-- 0051: the language of a code-mode run over REST (v0.1, TEC-1629).
--
-- POST /v1/code/run takes JavaScript (text/javascript, or JSON
-- "language": "javascript") as well as Starlark; a resume runs the script
-- again in the language it started in. Runs stored before this migration
-- were Starlark.
--
-- Same change as migrations/sqlite/0051_code_runs_language.sql.
ALTER TABLE code_runs ADD COLUMN IF NOT EXISTS language TEXT NOT NULL DEFAULT 'starlark';
