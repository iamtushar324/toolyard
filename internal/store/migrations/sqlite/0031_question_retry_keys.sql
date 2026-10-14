-- Stable submission keys are scoped to the agent. Old request documents have none.
CREATE UNIQUE INDEX inbox_client_request_key
ON inbox_requests(agent_id, json_extract(doc, '$.client_request_id'))
WHERE json_extract(doc, '$.client_request_id') IS NOT NULL
  AND json_extract(doc, '$.client_request_id') != '';
