#!/usr/bin/env python3
"""Build toolyard-grafana dashboard JSONs from the nova-grafana templates.

We keep the panel layouts, viz options, and thresholds from the original
nova dashboards and replace each panel's `targets` with a query against
the toolyard ClickHouse warehouse via the `grafana-clickhouse-datasource`
plugin. Each panel's SQL is captured below in the PANELS dict; the
script copies layout/title/viz from the nova JSON and swaps in the new
target.

Why a generator instead of hand-edited JSONs:
  * 27 portable panels across two dashboards is a lot of repetitive JSON
    plumbing — easy to typo, hard to review.
  * If the migration agent renames a mart column later, regen is one
    `python3 build_dashboards.py`; no per-panel hunt.
  * The PANELS dict below is a self-documenting map of "what each panel
    actually queries," which is the load-bearing part. Layout/viz come
    from nova, so we don't have to re-tune unit/threshold/legend.

Usage:
    python3 deploy/grafana/build_dashboards.py
Outputs:
    deploy/grafana/dashboards/finance-overview.json
    deploy/grafana/dashboards/investment-portfolio.json
    deploy/grafana/dashboards/spending-budget.json   (skeleton; raw.transactions pending)
"""

import json
import pathlib

REPO = pathlib.Path(__file__).resolve().parents[2]
NOVA = pathlib.Path("/home/tusharbhardwaj/.nova/main_agent_workspace/repo/nova/grafana/dashboards")
OUT = REPO / "deploy" / "grafana" / "dashboards"

# Direct ClickHouse datasource (no gateway hop). uid matches what
# provisioning/datasources/datasources.yaml assigns.
LAKE_DS = {"type": "grafana-clickhouse-datasource", "uid": "toolyard-lake"}

# grafana-clickhouse-datasource format enum (from the plugin's
# `pkg/plugin/types.go`): 0=timeseries, 1=table, 2=logs, 3=trace.
FMT_TIMESERIES = 0
FMT_TABLE = 1


# Reusable CTEs. We query dim/fact directly because the migration agent
# has been adding/removing the friendly mart.* views, and panels keep
# breaking when a view they depend on disappears. Dim/fact tables are
# the canonical layer and won't churn.

# Latest-valuation-per-investment, used everywhere we need "current value".
LATEST_V = (
    "latest_v AS ("
    " SELECT investment_key, market_value, as_of_date, "
    " ROW_NUMBER() OVER (PARTITION BY investment_key ORDER BY as_of_date DESC) rn "
    " FROM mart.fact_investment_valuation"
    ")"
)
# Latest-balance-per-account.
LATEST_B = (
    "latest_b AS ("
    " SELECT account_key, balance_amount, as_of_date, "
    " ROW_NUMBER() OVER (PARTITION BY account_key ORDER BY as_of_date DESC) rn "
    " FROM mart.fact_account_balance"
    ")"
)


def _risk_split_sql() -> str:
    """Risk-profile split SQL kept out of the PANELS dict because it
    builds a multi-CTE query whose inner expressions need single quotes
    that f-strings can't host (backslash escapes are illegal inside
    f-string braces). Plain concat avoids the gymnastics."""
    safe_inner = (
        "COALESCE((SELECT SUM(market_value) FROM iv "
        "WHERE asset_class IN ('fd','ulip')),0) "
        "+ COALESCE((SELECT SUM(balance_amount) FROM ab "
        "WHERE account_type IN ('savings','current')),0)"
    )
    return (
        "WITH " + LATEST_V + ", " + LATEST_B + ", "
        "iv AS ("
        " SELECT i.asset_class, v.market_value "
        " FROM mart.dim_investment i "
        " JOIN latest_v v ON v.investment_key = i.investment_key AND v.rn = 1 "
        " WHERE i.is_current = TRUE"
        "), "
        "ab AS ("
        " SELECT a.account_type, b.balance_amount "
        " FROM mart.dim_account a "
        " JOIN latest_b b ON b.account_key = a.account_key AND b.rn = 1 "
        " WHERE a.is_current = TRUE AND a.is_active = TRUE"
        ") "
        "SELECT 'Equity' AS risk, COALESCE(SUM(market_value),0) AS value "
        "FROM iv WHERE asset_class IN ('equity_india','equity_us') "
        "UNION ALL SELECT 'Safe' AS risk, " + safe_inner + " AS value "
        "UNION ALL SELECT 'Other' AS risk, COALESCE(SUM(market_value),0) AS value "
        "FROM iv WHERE asset_class NOT IN ('equity_india','equity_us','fd','ulip')"
    )


def ch_target(ref_id: str, sql: str, fmt: int) -> dict:
    """Build a Grafana panel target for the ClickHouse datasource.

    The grafana-clickhouse-datasource plugin runs `rawSql` directly
    against CH over the native protocol — no body/parser/columns
    plumbing needed (that was Infinity-era machinery). `format` selects
    the result shape Grafana renders into: timeseries panels expect a
    DateTime column followed by metrics; table panels render all
    columns as a grid.
    """
    return {
        "refId": ref_id,
        "datasource": LAKE_DS,
        "editorType": "sql",
        "queryType": "table" if fmt == FMT_TABLE else "timeseries",
        "rawSql": sql,
        "format": fmt,
        "meta": {"timezone": ""},
    }


# Panel-title -> {sql, fmt}. Title must match the nova JSON exactly so
# the loader can find it. All SQL targets dim/fact tables directly —
# the friendly mart.* views the migration agent shipped have been
# moving under us, so we don't depend on them.
PANELS: dict[str, dict] = {
    # ----- Finance Overview -----------------------------------------
    "Net Worth Over Time": {
        "fmt": FMT_TIMESERIES,
        "sql": (
            "SELECT toDateTime(as_of_date) AS time, net_worth "
            "FROM mart.fact_net_worth_snapshot ORDER BY time"
        ),
    },
    "Latest Net Worth": {
        "fmt": FMT_TABLE,
        "sql": (
            "SELECT net_worth FROM mart.fact_net_worth_snapshot "
            "ORDER BY as_of_date DESC LIMIT 1"
        ),
    },
    "Total Assets": {
        "fmt": FMT_TABLE,
        "sql": (
            "SELECT total_assets FROM mart.fact_net_worth_snapshot "
            "ORDER BY as_of_date DESC LIMIT 1"
        ),
    },
    "Monthly Burn": {
        "fmt": FMT_TABLE,
        # Recurring payments normalized to monthly equivalent.
        "sql": (
            "SELECT ROUND(SUM(CASE frequency "
            "WHEN 'monthly' THEN amount "
            "WHEN 'yearly' THEN amount/12.0 "
            "WHEN 'half_yearly' THEN amount/6.0 "
            "WHEN 'quarterly' THEN amount/3.0 "
            "ELSE amount END), 2) AS monthly_burn "
            "FROM mart.fact_recurring_schedule WHERE is_active = TRUE"
        ),
    },
    "Active Accounts": {
        "fmt": FMT_TABLE,
        "sql": (
            "SELECT count() AS active_accounts FROM mart.dim_account "
            "WHERE is_active = TRUE AND is_current = TRUE"
        ),
    },
    "Risk Profile Split": {
        "fmt": FMT_TABLE,
        # Two buckets matching nova's split: Equity (high-risk) vs Safe
        # (FD + savings + ULIP). Anything else falls into "Other".
        "sql": _risk_split_sql(),
    },
    "Category Breakdown": {
        "fmt": FMT_TABLE,
        "sql": (
            f"WITH {LATEST_V} "
            "SELECT i.asset_class AS category, "
            "COALESCE(SUM(v.market_value),0) AS value "
            "FROM mart.dim_investment i "
            "JOIN latest_v v ON v.investment_key = i.investment_key AND v.rn = 1 "
            "WHERE i.is_current = TRUE "
            "GROUP BY 1 ORDER BY 2 DESC"
        ),
    },
    "CC Bill Trend": {
        "fmt": FMT_TABLE,
        "sql": (
            "SELECT bill_month, SUM(amount_due) AS billed, SUM(amount_paid) AS paid "
            "FROM mart.fact_credit_card_bill GROUP BY 1 ORDER BY 1"
        ),
    },
    "Risk Profile Trend Over Time": {
        "fmt": FMT_TIMESERIES,
        # stocks+MF=Equity, fd+ulip+bank=Safe, gold+real_estate+crypto=Other.
        "sql": (
            "SELECT toDateTime(as_of_date) AS time, "
            "stocks_value + mutual_funds_value AS Equity, "
            "fd_value + ulip_value + bank_balance AS Safe, "
            "gold_value + real_estate_value + crypto_value AS Other "
            "FROM mart.fact_net_worth_snapshot ORDER BY time"
        ),
    },
    "Category Trend Over Time (Stacked)": {
        "fmt": FMT_TIMESERIES,
        "sql": (
            "SELECT toDateTime(as_of_date) AS time, "
            "bank_balance AS Bank, "
            "stocks_value AS Stocks, "
            "mutual_funds_value AS `Mutual Funds`, "
            "fd_value AS FD, "
            "ulip_value AS ULIP, "
            "gold_value AS Gold, "
            "real_estate_value AS `Real Estate`, "
            "crypto_value AS Crypto "
            "FROM mart.fact_net_worth_snapshot ORDER BY time"
        ),
    },
    "Individual Holdings Over Time": {
        "fmt": FMT_TIMESERIES,
        "sql": (
            "SELECT toDateTime(v.as_of_date) AS time, "
            "i.display_name AS name, v.market_value AS value "
            "FROM mart.fact_investment_valuation v "
            "JOIN mart.dim_investment i ON v.investment_key = i.investment_key "
            "WHERE i.is_current = TRUE "
            "ORDER BY 1, 2"
        ),
    },
    "Savings Accounts Over Time": {
        "fmt": FMT_TIMESERIES,
        "sql": (
            "SELECT toDateTime(b.as_of_date) AS time, "
            "a.display_name AS name, b.balance_amount AS balance "
            "FROM mart.fact_account_balance b "
            "JOIN mart.dim_account a ON b.account_key = a.account_key "
            "WHERE a.account_type = 'savings' AND a.is_current = TRUE "
            "ORDER BY 1, 2"
        ),
    },
    "FD & Fixed Income Over Time": {
        "fmt": FMT_TIMESERIES,
        "sql": (
            "SELECT toDateTime(v.as_of_date) AS time, "
            "i.display_name AS name, v.market_value AS value "
            "FROM mart.fact_investment_valuation v "
            "JOIN mart.dim_investment i ON v.investment_key = i.investment_key "
            "WHERE i.asset_class IN ('fd','ulip') AND i.is_current = TRUE "
            "ORDER BY 1, 2"
        ),
    },
    "Account Balances (Current)": {
        "fmt": FMT_TABLE,
        "sql": (
            f"WITH {LATEST_B} "
            "SELECT a.display_name AS account_name, a.account_type, a.institution, "
            "COALESCE(b.balance_amount, 0) AS current_balance, a.holder_name "
            "FROM mart.dim_account a "
            "LEFT JOIN latest_b b ON b.account_key = a.account_key AND b.rn = 1 "
            "WHERE a.is_active = TRUE AND a.is_current = TRUE "
            "AND COALESCE(b.balance_amount,0) > 0 "
            "ORDER BY current_balance DESC"
        ),
    },
    "Recurring Payments": {
        "fmt": FMT_TABLE,
        "sql": (
            "SELECT name, kind AS type, amount, frequency, "
            "CASE frequency "
            "WHEN 'yearly' THEN ROUND(amount/12.0) "
            "WHEN 'half_yearly' THEN ROUND(amount/6.0) "
            "WHEN 'quarterly' THEN ROUND(amount/3.0) "
            "ELSE amount END AS monthly_eq "
            "FROM mart.fact_recurring_schedule WHERE is_active = TRUE"
        ),
    },
    "Physical Assets": {
        "fmt": FMT_TABLE,
        "sql": (
            "SELECT category, SUM(purchase_price) AS total_value, count() AS items "
            "FROM mart.dim_physical_asset WHERE is_active = TRUE "
            "GROUP BY category ORDER BY total_value DESC"
        ),
    },

    # ----- Investment Portfolio -------------------------------------
    "Total Portfolio Value": {
        "fmt": FMT_TABLE,
        "sql": (
            f"WITH {LATEST_V} "
            "SELECT COALESCE(SUM(v.market_value),0) AS total "
            "FROM mart.dim_investment i "
            "JOIN latest_v v ON v.investment_key = i.investment_key AND v.rn = 1 "
            "WHERE i.is_current = TRUE"
        ),
    },
    "Total Holdings": {
        "fmt": FMT_TABLE,
        "sql": (
            f"WITH {LATEST_V} "
            "SELECT count() AS holdings "
            "FROM mart.dim_investment i "
            "JOIN latest_v v ON v.investment_key = i.investment_key AND v.rn = 1 "
            "WHERE i.is_current = TRUE AND v.market_value > 0"
        ),
    },
    "FD Value": {
        "fmt": FMT_TABLE,
        "sql": (
            f"WITH {LATEST_V} "
            "SELECT COALESCE(SUM(v.market_value),0) AS fd "
            "FROM mart.dim_investment i "
            "JOIN latest_v v ON v.investment_key = i.investment_key AND v.rn = 1 "
            "WHERE i.is_current = TRUE AND i.asset_class = 'fd'"
        ),
    },
    "Equity Value": {
        "fmt": FMT_TABLE,
        "sql": (
            f"WITH {LATEST_V} "
            "SELECT COALESCE(SUM(v.market_value),0) AS equity "
            "FROM mart.dim_investment i "
            "JOIN latest_v v ON v.investment_key = i.investment_key AND v.rn = 1 "
            "WHERE i.is_current = TRUE AND i.asset_class IN ('equity_india','equity_us')"
        ),
    },
    "Deployment Progress": {
        "fmt": FMT_TABLE,
        "sql": (
            "SELECT ROUND(deployed_amount * 1.0 / NULLIF(total_amount,0), 2) AS deployed "
            "FROM mart.fact_deployment_plan WHERE status = 'active' LIMIT 1"
        ),
    },
    "Portfolio by Asset Class": {
        "fmt": FMT_TABLE,
        "sql": (
            f"WITH {LATEST_V} "
            "SELECT CASE i.asset_class "
            "WHEN 'equity_india' THEN 'India Equity' "
            "WHEN 'equity_us' THEN 'US Equity' "
            "WHEN 'mutual_fund' THEN 'Mutual Funds' "
            "WHEN 'fd' THEN 'Fixed Deposits' "
            "WHEN 'ulip' THEN 'ULIP' "
            "WHEN 'peer_lending' THEN 'Peer Lending' "
            "WHEN 'gold_sgb' THEN 'Gold SGB' "
            "WHEN 'crypto' THEN 'Crypto' "
            "ELSE i.asset_class END AS class, "
            "SUM(v.market_value) AS value "
            "FROM mart.dim_investment i "
            "JOIN latest_v v ON v.investment_key = i.investment_key AND v.rn = 1 "
            "WHERE i.is_current = TRUE AND v.market_value > 0 "
            "GROUP BY 1 ORDER BY 2 DESC"
        ),
    },
    "India vs US Equity Split": {
        "fmt": FMT_TABLE,
        "sql": (
            f"WITH {LATEST_V} "
            "SELECT CASE i.asset_class "
            "WHEN 'equity_india' THEN 'India' "
            "WHEN 'equity_us' THEN 'US' END AS market, "
            "SUM(v.market_value) AS value "
            "FROM mart.dim_investment i "
            "JOIN latest_v v ON v.investment_key = i.investment_key AND v.rn = 1 "
            "WHERE i.is_current = TRUE AND i.asset_class IN ('equity_india','equity_us') "
            "GROUP BY 1"
        ),
    },
    "Holdings by Holder": {
        "fmt": FMT_TABLE,
        "sql": (
            f"WITH {LATEST_V} "
            "SELECT COALESCE(i.holder_name,'Unknown') AS holder, "
            "SUM(v.market_value) AS value "
            "FROM mart.dim_investment i "
            "JOIN latest_v v ON v.investment_key = i.investment_key AND v.rn = 1 "
            "WHERE i.is_current = TRUE AND v.market_value > 0 "
            "GROUP BY 1 ORDER BY 2 DESC"
        ),
    },
    "All Holdings": {
        "fmt": FMT_TABLE,
        "sql": (
            f"WITH {LATEST_V} "
            "SELECT i.display_name AS name, "
            "CASE i.asset_class "
            "WHEN 'equity_india' THEN 'India Equity' "
            "WHEN 'equity_us' THEN 'US Equity' "
            "WHEN 'mutual_fund' THEN 'Mutual Fund' "
            "WHEN 'fd' THEN 'FD' "
            "WHEN 'ulip' THEN 'ULIP' "
            "WHEN 'peer_lending' THEN 'Peer Lending' "
            "ELSE i.asset_class END AS class, "
            "v.market_value AS current_value, i.holder_name, i.plan_type "
            "FROM mart.dim_investment i "
            "JOIN latest_v v ON v.investment_key = i.investment_key AND v.rn = 1 "
            "WHERE i.is_current = TRUE AND v.market_value > 0 "
            "ORDER BY current_value DESC"
        ),
    },
    "Deployment Plan": {
        "fmt": FMT_TABLE,
        "sql": (
            "SELECT name, total_amount, deployed_amount, remaining_amount, "
            "strategy, status "
            "FROM mart.fact_deployment_plan "
            "ORDER BY CASE status WHEN 'active' THEN 0 ELSE 1 END"
        ),
    },
    "FD Maturity Timeline": {
        "fmt": FMT_TABLE,
        # dateDiff is CH's spelling of DuckDB's DATE_DIFF; today() is
        # the CH idiom for CURRENT_DATE. Args order is (unit, start,
        # end), same convention as DuckDB so the result semantics
        # carry over unchanged.
        "sql": (
            f"WITH {LATEST_V} "
            "SELECT i.display_name, v.market_value AS principal, "
            "i.interest_rate, "
            "a.institution, i.holder_name, i.maturity_date, "
            "dateDiff('day', today(), i.maturity_date) AS days_left "
            "FROM mart.dim_investment i "
            "LEFT JOIN mart.dim_account a "
            "  ON i.account_key = a.account_key AND a.is_current = TRUE "
            "LEFT JOIN latest_v v "
            "  ON v.investment_key = i.investment_key AND v.rn = 1 "
            "WHERE i.asset_class = 'fd' AND i.is_current = TRUE "
            "ORDER BY i.maturity_date"
        ),
    },
}


def rewrite_dashboard(src_name: str, out_name: str) -> tuple[int, int, list[str]]:
    """Load the nova source dashboard, swap each panel's targets onto the
    ClickHouse datasource, write the output JSON. Returns (ported,
    skipped, skipped_titles). A panel is "skipped" when its title isn't
    in PANELS — typically a Spending dashboard panel waiting on
    raw.transactions.
    """
    src = NOVA / src_name
    with src.open() as f:
        d = json.load(f)
    d.pop("id", None)
    # Force a fresh UID so importing alongside the nova original doesn't
    # collide on the legacy UID. Prefix with "tylk-" for traceability.
    d["uid"] = "tylk-" + d.get("uid", out_name.replace(".json", ""))
    d["title"] = d.get("title", out_name) + " (lake)"
    ported, skipped, skipped_titles = 0, 0, []
    for p in d.get("panels", []):
        title = p.get("title", "")
        spec = PANELS.get(title)
        if spec is None:
            skipped += 1
            if title:
                skipped_titles.append(title)
            continue
        p["datasource"] = LAKE_DS
        p["targets"] = [ch_target("A", spec["sql"], spec["fmt"])]
        ported += 1
    OUT.mkdir(parents=True, exist_ok=True)
    with (OUT / out_name).open("w") as f:
        json.dump(d, f, indent=2)
    return ported, skipped, skipped_titles


def write_spending_stub() -> None:
    """Write a minimal Spending & Budget dashboard that surfaces the
    'fin_transactions / fin_budgets pending migration' status. When the
    migration agent lands those tables, this file gets replaced by a
    full port (re-run this script with the real PANELS entries added)."""
    dash = {
        "uid": "tylk-spending-budget",
        "title": "Spending & Budget (lake) — pending migration",
        "tags": ["toolyard", "lake", "stub"],
        "schemaVersion": 39,
        "version": 1,
        "time": {"from": "now-30d", "to": "now"},
        "timezone": "browser",
        "refresh": "5m",
        "panels": [
            {
                "id": 1,
                "type": "text",
                "title": "Status",
                "gridPos": {"x": 0, "y": 0, "w": 24, "h": 6},
                "options": {
                    "mode": "markdown",
                    "content": (
                        "## Spending & Budget — pending data\n\n"
                        "This dashboard depends on `fin_transactions` and `fin_budgets`, "
                        "which haven't been migrated into the toolyard lake yet. "
                        "Once the migration agent lands `raw.transactions` and "
                        "`raw.budgets` (or matching `mart.*` views), re-run "
                        "`deploy/grafana/build_dashboards.py` to rebuild this dashboard "
                        "with the real panels.\n\n"
                        "In the meantime, `Finance Overview (lake)` and "
                        "`Investment Portfolio (lake)` are fully populated against "
                        "the existing lake tables."
                    ),
                },
            }
        ],
    }
    with (OUT / "spending-budget.json").open("w") as f:
        json.dump(dash, f, indent=2)


def main() -> None:
    finance_p, finance_s, finance_titles = rewrite_dashboard("finance-overview.json", "finance-overview.json")
    invest_p, invest_s, invest_titles = rewrite_dashboard("investment-portfolio.json", "investment-portfolio.json")
    write_spending_stub()
    print(f"finance-overview:   ported {finance_p}, skipped {finance_s}")
    if finance_titles:
        print("  skipped titles:", finance_titles)
    print(f"investment-portfolio: ported {invest_p}, skipped {invest_s}")
    if invest_titles:
        print("  skipped titles:", invest_titles)
    print("spending-budget:    stub (pending raw.transactions migration)")


if __name__ == "__main__":
    main()
