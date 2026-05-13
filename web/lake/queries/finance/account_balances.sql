WITH latest AS (
  SELECT account_key, balance_amount
    FROM mart.fact_account_balance
   QUALIFY ROW_NUMBER() OVER (PARTITION BY account_key ORDER BY as_of_date DESC) = 1
)
SELECT da.display_name   AS account_name,
       l.balance_amount  AS current_balance
  FROM mart.dim_account da
  JOIN latest l ON l.account_key = da.account_key
 WHERE da.is_active
   AND l.balance_amount > 0
 ORDER BY l.balance_amount DESC
 LIMIT 20
