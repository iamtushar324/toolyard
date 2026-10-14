package blobstore

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"io"
	"time"

	"github.com/tusharbhardwaj/toolyard/internal/store"
)

// dbStore keeps blobs in the database: inbox_blob_chunks holds the bytes in
// ChunkSize pieces (seq 0, 1, ...), and inbox_blob_objects holds one row per
// complete blob, written last in the same transaction. A blob exists only
// when its object row does.
//
// The SQL is plain and uses ? placeholders (the store rewrites them on
// Postgres), so the same code also runs on SQLite, where the tables exist
// but production uses Local instead.
type dbStore struct {
	db *sql.DB
}

// NewPostgres stores blobs in db's inbox_blob_objects and inbox_blob_chunks
// tables, in 1 MiB chunks.
func NewPostgres(db *sql.DB) Store { return &dbStore{db: db} }

// Put streams r into chunk rows with one reused ChunkSize buffer, so memory
// stays at about one chunk whatever the blob's size. The object row goes in
// last; when an identical Put commits first, this one rolls back and reports
// created=false.
func (p *dbStore) Put(ctx context.Context, sha string, r io.Reader, size int64) (bool, error) {
	if !ValidSHA(sha) {
		return false, errBadName(sha)
	}
	if size < 0 {
		return false, fmt.Errorf("blobstore: negative size %d", size)
	}
	created, err := p.put(ctx, sha, r, size)
	if err != nil && store.IsUniqueViolation(err) {
		// A concurrent Put of the same blob inserted the same chunk keys and
		// committed first: the blob is stored, which is all the caller needs.
		if ok, xerr := p.Exists(ctx, sha); xerr == nil && ok {
			return false, nil
		}
	}
	return created, err
}

func (p *dbStore) put(ctx context.Context, sha string, r io.Reader, size int64) (bool, error) {
	tx, err := p.db.BeginTx(ctx, nil)
	if err != nil {
		return false, err
	}
	defer tx.Rollback()
	var have bool
	if err := tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM inbox_blob_objects WHERE sha256 = ?)`, sha).Scan(&have); err != nil {
		return false, err
	}
	if have {
		return false, nil
	}
	// Chunks without an object row are leftovers of an incomplete write
	// (Put itself never commits one): replace them.
	if _, err := tx.ExecContext(ctx, `DELETE FROM inbox_blob_chunks WHERE sha256 = ?`, sha); err != nil {
		return false, err
	}
	buf := make([]byte, min(int64(ChunkSize), max(size, 1)))
	src := io.LimitReader(r, size+1) // one byte more than expected shows an overlong body
	var total, seq int64
	for {
		n, rerr := io.ReadFull(src, buf)
		if n > 0 {
			total += int64(n)
			if total > size {
				return false, fmt.Errorf("blobstore: %s has more than the expected %d bytes", short(sha), size)
			}
			if _, err := tx.ExecContext(ctx, `INSERT INTO inbox_blob_chunks(sha256, seq, data) VALUES (?, ?, ?)`, sha, seq, buf[:n]); err != nil {
				return false, err
			}
			seq++
		}
		if rerr == io.EOF || rerr == io.ErrUnexpectedEOF {
			break
		}
		if rerr != nil {
			return false, rerr
		}
	}
	if total != size {
		return false, fmt.Errorf("blobstore: %s has %d bytes, want %d", short(sha), total, size)
	}
	res, err := tx.ExecContext(ctx, `INSERT INTO inbox_blob_objects(sha256, size, chunks, created_at) VALUES (?, ?, ?, ?) ON CONFLICT DO NOTHING`,
		sha, size, seq, time.Now().UnixMilli())
	if err != nil {
		return false, err
	}
	if n, err := res.RowsAffected(); err != nil {
		return false, err
	} else if n == 0 {
		return false, nil // an identical Put committed first; roll ours back
	}
	if err := tx.Commit(); err != nil {
		return false, err
	}
	return true, nil
}

// Open reads the object row and returns a Blob that loads one chunk at a
// time, only when a Read needs it. ctx bounds every chunk query of the
// returned Blob.
func (p *dbStore) Open(ctx context.Context, sha string) (Blob, error) {
	if !ValidSHA(sha) {
		return nil, ErrNotFound
	}
	var size, chunks int64
	err := p.db.QueryRowContext(ctx, `SELECT size, chunks FROM inbox_blob_objects WHERE sha256 = ?`, sha).Scan(&size, &chunks)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	if want := (size + ChunkSize - 1) / ChunkSize; size < 0 || chunks != want {
		return nil, fmt.Errorf("blobstore: %s records %d chunks for %d bytes", short(sha), chunks, size)
	}
	return &dbBlob{db: p.db, ctx: ctx, sha: sha, size: size, seq: -1}, nil
}

// Exists reports whether the blob's object row is there.
func (p *dbStore) Exists(ctx context.Context, sha string) (bool, error) {
	if !ValidSHA(sha) {
		return false, nil
	}
	var ok bool
	if err := p.db.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM inbox_blob_objects WHERE sha256 = ?)`, sha).Scan(&ok); err != nil {
		return false, err
	}
	return ok, nil
}

// Delete removes the object row and its chunks in one transaction.
func (p *dbStore) Delete(ctx context.Context, sha string) error {
	if !ValidSHA(sha) {
		return nil
	}
	tx, err := p.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(ctx, `DELETE FROM inbox_blob_objects WHERE sha256 = ?`, sha); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM inbox_blob_chunks WHERE sha256 = ?`, sha); err != nil {
		return err
	}
	return tx.Commit()
}

// dbBlob reads a stored blob chunk by chunk. It holds at most one chunk:
// the one that covers the current offset (seq = off >> 20).
type dbBlob struct {
	db     *sql.DB
	ctx    context.Context
	sha    string
	size   int64
	off    int64
	seq    int64  // the chunk in buf; -1 when none is loaded
	buf    []byte // reused for every chunk
	closed bool
}

var errClosed = errors.New("blobstore: read of a closed blob")

func (b *dbBlob) Size() int64 { return b.size }

func (b *dbBlob) Read(p []byte) (int, error) {
	if b.closed {
		return 0, errClosed
	}
	if b.off >= b.size {
		return 0, io.EOF
	}
	if len(p) == 0 {
		return 0, nil
	}
	seq := b.off >> chunkShift
	if seq != b.seq {
		if err := b.load(seq); err != nil {
			return 0, err
		}
	}
	n := copy(p, b.buf[b.off-(seq<<chunkShift):])
	b.off += int64(n)
	return n, nil
}

// load reads chunk seq into buf and checks its length.
func (b *dbBlob) load(seq int64) error {
	b.seq = -1
	rows, err := b.db.QueryContext(b.ctx, `SELECT data FROM inbox_blob_chunks WHERE sha256 = ? AND seq = ?`, b.sha, seq)
	if err != nil {
		return err
	}
	defer rows.Close()
	if !rows.Next() {
		if err := rows.Err(); err != nil {
			return err
		}
		return fmt.Errorf("blobstore: chunk %d of %s is missing", seq, short(b.sha))
	}
	var data sql.RawBytes
	if err := rows.Scan(&data); err != nil {
		return err
	}
	want := min(int64(ChunkSize), b.size-(seq<<chunkShift))
	if int64(len(data)) != want {
		return fmt.Errorf("blobstore: chunk %d of %s has %d bytes, want %d", seq, short(b.sha), len(data), want)
	}
	// data is only valid until the rows close: copy it into the reused buffer.
	b.buf = append(b.buf[:0], data...)
	if err := rows.Close(); err != nil {
		return err
	}
	b.seq = seq
	return nil
}

func (b *dbBlob) Seek(offset int64, whence int) (int64, error) {
	if b.closed {
		return 0, errClosed
	}
	var abs int64
	switch whence {
	case io.SeekStart:
		abs = offset
	case io.SeekCurrent:
		abs = b.off + offset
	case io.SeekEnd:
		abs = b.size + offset
	default:
		return 0, errors.New("blobstore: invalid whence")
	}
	if abs < 0 {
		return 0, errors.New("blobstore: negative position")
	}
	b.off = abs
	return abs, nil
}

// Close drops the buffered chunk. Closing twice is fine.
func (b *dbBlob) Close() error {
	b.closed = true
	b.buf = nil
	return nil
}
