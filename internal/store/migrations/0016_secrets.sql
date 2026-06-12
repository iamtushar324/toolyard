-- Secrets broker: credentials stored AES-256-GCM-encrypted, referenced as
-- secret://NAME in upstream configs and resolved only at dial time.
CREATE TABLE IF NOT EXISTS secrets (
  name        TEXT PRIMARY KEY,          -- ^[A-Z][A-Z0-9_]{0,63}$
  value_enc   TEXT NOT NULL,             -- base64(nonce||ct||tag) AES-256-GCM, AAD-bound to name
  description TEXT,
  created_at  INTEGER NOT NULL,
  updated_at  INTEGER NOT NULL,
  last_used_at INTEGER
);

-- Static headers for http upstreams (composed with OAuth headers at dial time).
ALTER TABLE upstream_servers ADD COLUMN headers_json TEXT;
