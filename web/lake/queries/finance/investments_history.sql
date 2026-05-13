SELECT as_of_date,
       SUM(market_value)   AS total_value,
       SUM(unrealized_pnl) AS unrealized_pnl
  FROM mart.fact_investment_valuation
 GROUP BY as_of_date
 ORDER BY as_of_date
