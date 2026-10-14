-- 0040: Inbox blob bytes kept in the database (internal/blobstore).
--
-- Toolyard keeps the bytes of uploads, copied links and voice notes here, so
-- every replica reads the same files. A blob is split into 1 MiB chunks
-- (every chunk but the last is exactly 1 MiB, seq starts at 0). The
-- inbox_blob_objects row is written last, in the same transaction as the
-- chunks: a blob exists only when its object row does. size is in bytes and
-- chunks is the number of chunk rows. created_at is Unix milliseconds. The
-- metadata row (content type, source) stays in inbox_blobs.
--
-- Same tables as migrations/sqlite/0040_inbox_blob_chunks.sql (which SQLite
-- leaves empty, because it keeps blobs as files).
CREATE TABLE IF NOT EXISTS inbox_blob_objects (
    sha256     TEXT PRIMARY KEY,
    size       BIGINT NOT NULL,
    chunks     BIGINT NOT NULL,
    created_at BIGINT NOT NULL
);

CREATE TABLE IF NOT EXISTS inbox_blob_chunks (
    sha256 TEXT NOT NULL,
    seq    BIGINT NOT NULL,
    data   BYTEA NOT NULL,
    PRIMARY KEY (sha256, seq)
);
