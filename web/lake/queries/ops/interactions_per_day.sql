SELECT date_trunc('day', event_at)::DATE AS day,
       COUNT(*) AS count
  FROM mart.fact_chat_interaction
 GROUP BY 1
 ORDER BY day ASC
 LIMIT 365
