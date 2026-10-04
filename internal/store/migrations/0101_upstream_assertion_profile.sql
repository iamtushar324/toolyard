-- A private service-owned credential profile on a normal HTTP per_user server.
-- The profile ID is public metadata, never a secret path or a caller session ID.
ALTER TABLE upstream_servers ADD COLUMN assertion_profile TEXT;
