WITH latest AS (
  SELECT account_key, balance_amount
    FROM mart.fact_account_balance
   QUALIFY ROW_NUMBER() OVER (PARTITION BY account_key ORDER BY as_of_date DESC) = 1
)
SELECT da.display_name   AS account_name,
       da.account_type,
       da.institution,
       da.currency,
       l.balance_amount  AS current_balance,
       da.is_active      AS active
  FROM mart.dim_account da
  LEFT JOIN latest l ON l.account_key = da.account_key
 ORDER BY l.balance_amount DESC NULLS LAST
