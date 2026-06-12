-- tool_policies: explicit per-tool / per-upstream gating set from the UI.
-- Supersedes the unused expression-based `policies` table (which had zero Go
-- references). action is one of allow | ask | deny; scope is tool | upstream.
-- target is the WRAPPED tool name (e.g. "github.create_issue") for tool
-- scope, or the upstream name for upstream scope.
CREATE TABLE IF NOT EXISTS tool_policies (
    id          TEXT PRIMARY KEY,
    scope       TEXT NOT NULL,            -- 'tool' | 'upstream'
    target      TEXT NOT NULL,
    action      TEXT NOT NULL,            -- 'allow' | 'ask' | 'deny'
    note        TEXT,
    created_at  INTEGER NOT NULL,
    updated_at  INTEGER NOT NULL,
    UNIQUE(scope, target)
);
CREATE INDEX IF NOT EXISTS idx_tool_policies_scope ON tool_policies(scope, target);

-- The expression-based table never shipped a Go evaluator; drop it.
DROP TABLE IF EXISTS policies;

-- One-time migration: user-enabled, non-destructive `kind=tool` auto rules
-- become explicit `allow` tool policies, then those rules are disabled so the
-- learned-auto and explicit-policy systems don't fight. Destructive-marker
-- tool names are excluded (an allow on those needs an explicit force later).
INSERT OR IGNORE INTO tool_policies(id, scope, target, action, note, created_at, updated_at)
SELECT 'tp_mig_' || id, 'tool', tool_name, 'allow', 'migrated from auto-approval rule',
       CAST(strftime('%s','now') AS INTEGER) * 1000,
       CAST(strftime('%s','now') AS INTEGER) * 1000
FROM auto_approval_rules
WHERE kind = 'tool' AND enabled = 1 AND tool_name IS NOT NULL
  AND lower(tool_name) NOT LIKE '%delete%'
  AND lower(tool_name) NOT LIKE '%destroy%'
  AND lower(tool_name) NOT LIKE '%drop%'
  AND lower(tool_name) NOT LIKE '%remove%'
  AND lower(tool_name) NOT LIKE '%purge%'
  AND lower(tool_name) NOT LIKE '%wipe%';

UPDATE auto_approval_rules SET enabled = 0
WHERE kind = 'tool' AND enabled = 1 AND tool_name IS NOT NULL
  AND tool_name IN (SELECT target FROM tool_policies WHERE scope = 'tool');
