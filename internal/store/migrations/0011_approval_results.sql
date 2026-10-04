-- 0011: cache the executed tool result on the approval row so an
-- approved tool fires immediately on the human's tap and the agent
-- collects the result via tools.poll_approval / tools.wait_for_approval
-- (or the legacy _approval_id re-call path) without ever re-dispatching
-- the original tool itself.
--
-- result_envelope is a JSON blob shaped like an MCP CallToolResult:
--   {"is_error": bool, "text_content": "...", "structured_content": ...}
-- result_executed_at is the millisecond UTC timestamp the executor
-- finished. NULL means "approved but the executor hasn't completed yet"
-- (or "this row pre-dates the auto-execute feature").
-- result_error is the toolyard-side execution failure when the executor
-- itself errored before it could even invoke the tool (e.g., the upstream
-- vanished). Distinct from is_error, which is the tool's own logical
-- failure.

ALTER TABLE approval_requests ADD COLUMN result_envelope TEXT;
ALTER TABLE approval_requests ADD COLUMN result_is_error INTEGER;
ALTER TABLE approval_requests ADD COLUMN result_executed_at INTEGER;
ALTER TABLE approval_requests ADD COLUMN result_error TEXT;
