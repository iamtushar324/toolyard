SELECT table_schema AS schema,
       table_name   AS name,
       table_type   AS kind
  FROM information_schema.tables
 WHERE table_schema NOT IN ('information_schema','pg_catalog')
 ORDER BY table_schema, table_name
