-- Per-client extra authorize-URL query params (e.g. Linear's actor=app).
-- NULL = legacy default (BeginCallback appends access_type=offline &
-- prompt=consent); non-NULL JSON object replaces those defaults entirely.
ALTER TABLE oauth_clients ADD COLUMN authorize_extra_json TEXT;
