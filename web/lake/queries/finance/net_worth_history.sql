SELECT as_of_date AS snapshot_date,
       net_worth,
       total_assets,
       total_liabilities
  FROM mart.fact_net_worth_snapshot
 ORDER BY as_of_date
