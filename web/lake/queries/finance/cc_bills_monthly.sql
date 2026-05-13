SELECT bill_month,
       SUM(amount_due)              AS billed,
       SUM(COALESCE(amount_paid,0)) AS paid
  FROM mart.fact_credit_card_bill
 GROUP BY bill_month
 ORDER BY bill_month
