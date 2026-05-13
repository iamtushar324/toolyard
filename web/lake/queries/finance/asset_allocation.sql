WITH latest AS (
  SELECT * FROM mart.fact_net_worth_snapshot
   ORDER BY as_of_date DESC LIMIT 1
)
SELECT * FROM (
  SELECT 'Bank'          AS category, bank_balance        AS value FROM latest
  UNION ALL SELECT 'Stocks',         stocks_value              FROM latest
  UNION ALL SELECT 'Mutual Funds',   mutual_funds_value        FROM latest
  UNION ALL SELECT 'Fixed Deposits', fd_value                  FROM latest
  UNION ALL SELECT 'ULIPs',          ulip_value                FROM latest
  UNION ALL SELECT 'Debt',           debt_instruments          FROM latest
  UNION ALL SELECT 'Gold',           gold_value                FROM latest
  UNION ALL SELECT 'Real Estate',    real_estate_value         FROM latest
  UNION ALL SELECT 'Crypto',         crypto_value              FROM latest
  UNION ALL SELECT 'Receivables',    receivables               FROM latest
)
WHERE value IS NOT NULL AND value > 0
ORDER BY value DESC
