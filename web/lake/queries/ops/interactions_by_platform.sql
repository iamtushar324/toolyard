SELECT CASE WHEN platform IS NULL OR platform = ''
            THEN 'unknown' ELSE platform END AS platform,
       COUNT(*) AS count
  FROM mart.fact_chat_interaction
 GROUP BY 1
 ORDER BY count DESC
