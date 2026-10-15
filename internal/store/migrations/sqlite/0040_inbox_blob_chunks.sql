-- 0040: Inbox blob bytes kept in the database (internal/blobstore).
--
-- On Postgres, toolyard keeps the bytes of uploads, copied links and voice
-- notes in the database, so every replica reads the same files. A blob is
-- split into 1 MiB chunks (every chunk but the last is exactly 1 MiB, seq
-- starts at 0). The inbox_blob_objects row is written first, in the same
-- transaction as the chunks: a blob exists only when its object row does.
-- size is in bytes and chunks is the number of chunk rows. created_at is
-- Unix milliseconds. The metadata row (content type, source) stays in
-- inbox_blobs.
--
-- SQLite keeps blobs as files under <data-dir>/inbox/blobs and leaves these
-- tables empty. They exist here so both engines have the same schema.
-- Rollback: an older binary ignores both tables.
CREATE TABLE IF NOT EXISTS inbox_blob_objects (
    sha256     TEXT PRIMARY KEY,
    size       INTEGER NOT NULL,
    chunks     INTEGER NOT NULL,
    created_at INTEGER NOT NULL
);

CREATE TABLE IF NOT EXISTS inbox_blob_chunks (
    sha256 TEXT NOT NULL,
    seq    INTEGER NOT NULL,
    data   BLOB NOT NULL,
    PRIMARY KEY (sha256, seq)
);
