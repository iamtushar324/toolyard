package blobstore

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"io"
	"sync"
	"time"

	"github.com/tusharbhardwaj/toolyard/internal/store"
)

// dbStore keeps blobs in the database: inbox_blob_chunks holds the bytes in
// ChunkSize pieces (seq 0, 1, ...), and inbox_blob_objects holds one row per
// complete blob, written in the same transaction as its chunks. A blob
// exists only when its committed object row does.
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

// Opener opens stored blobs for reading. Every Store is an Opener.
type Opener interface {
	Open(ctx context.Context, sha string) (Blob, error)
}

// NewPostgresReader reads the blobs of the database store through q, for
// example a read-only *sql.Tx so that every read sees one snapshot. It never
// writes. Read and close each Blob it returns before q's transaction ends,
// and run no other query on q while a Read is in progress.
func NewPostgresReader(q store.Querier) Opener { return dbReader{q: q} }

type dbReader struct{ q store.Querier }

func (r dbReader) Open(ctx context.Context, sha string) (Blob, error) {
	return openDBBlob(ctx, r.q, sha)
}

// Execer runs one statement with ? placeholders inside a caller's
// transaction and reports how many rows it changed. PutIn writes through
// it.
type Execer func(ctx context.Context, query string, args ...any) (rowsAffected int64, err error)

// Put stores r in its own transaction (see PutIn). When an identical Put
// commits first, this one rolls back and reports created=false.
func (p *dbStore) Put(ctx context.Context, sha string, r io.Reader, size int64) (bool, error) {
	if !ValidSHA(sha) {
		return false, errBadName(sha)
	}
	if size < 0 {
		return false, fmt.Errorf("blobstore: negative size %d", size)
	}
	created, err := p.put(ctx, sha, r, size)
	if err != nil && store.IsUniqueViolation(err) {
		// A concurrent writer committed the same keys first: the blob is
		// stored, which is all the caller needs.
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
	exec := func(ctx context.Context, query string, args ...any) (int64, error) {
		res, err := tx.ExecContext(ctx, query, args...)
		if err != nil {
			return 0, err
		}
		return res.RowsAffected()
	}
	created, err := PutIn(ctx, exec, sha, r, size)
	if err != nil || !created {
		return false, err
	}
	if err := tx.Commit(); err != nil {
		return false, err
	}
	return true, nil
}

// PutIn writes blob sha (size bytes read from r) into the database store's
// tables through exec, inside the caller's transaction; the caller commits,
// or rolls back on an error. It streams r with one reused ChunkSize buffer,
// so memory stays at about one chunk whatever the blob's size.
//
// The object row goes in first (ON CONFLICT DO NOTHING): when it is already
// there, or an identical write commits it first, PutIn writes nothing and
// returns created=false. Taking the object row first means Put and Delete
// lock the same rows in the same order (object, then chunks), so they never
// deadlock, and a Put racing a Delete that has not committed waits for it
// instead of answering "already stored". A body shorter or longer than size
// is an error, after which the caller must roll back.
func PutIn(ctx context.Context, exec Execer, sha string, r io.Reader, size int64) (bool, error) {
	if !ValidSHA(sha) {
		return false, errBadName(sha)
	}
	if size < 0 {
		return false, fmt.Errorf("blobstore: negative size %d", size)
	}
	chunks := (size + ChunkSize - 1) / ChunkSize
	n, err := exec(ctx, `INSERT INTO inbox_blob_objects(sha256, size, chunks, created_at) VALUES (?, ?, ?, ?) ON CONFLICT DO NOTHING`,
		sha, size, chunks, time.Now().UnixMilli())
	if err != nil {
		return false, err
	}
	if n == 0 {
		return false, nil
	}
	// Chunks without an object row are leftovers of an incomplete write
	// (PutIn itself never commits one): replace them.
	if _, err := exec(ctx, `DELETE FROM inbox_blob_chunks WHERE sha256 = ?`, sha); err != nil {
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
			if _, err := exec(ctx, `INSERT INTO inbox_blob_chunks(sha256, seq, data) VALUES (?, ?, ?)`, sha, seq, buf[:n]); err != nil {
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
	if seq != chunks {
		return false, fmt.Errorf("blobstore: %s was written as %d chunks, want %d", short(sha), seq, chunks)
	}
	return true, nil
}

// Open reads the object row and returns a Blob that loads one chunk at a
// time, only when a Read needs it. ctx bounds every chunk query of the
// returned Blob.
func (p *dbStore) Open(ctx context.Context, sha string) (Blob, error) {
	return openDBBlob(ctx, p.db, sha)
}

func openDBBlob(ctx context.Context, q store.Querier, sha string) (Blob, error) {
	if !ValidSHA(sha) {
		return nil, ErrNotFound
	}
	var size, chunks int64
	err := q.QueryRowContext(ctx, `SELECT size, chunks FROM inbox_blob_objects WHERE sha256 = ?`, sha).Scan(&size, &chunks)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	if want := (size + ChunkSize - 1) / ChunkSize; size < 0 || chunks != want {
		return nil, fmt.Errorf("blobstore: %s records %d chunks for %d bytes", short(sha), chunks, size)
	}
	return &dbBlob{q: q, ctx: ctx, sha: sha, size: size, seq: -1}, nil
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

// Delete removes the object row, then its chunks, in one transaction: the
// same lock order as PutIn.
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
//
// Read, Seek and Close may run on different goroutines: for a multi-range
// request http.ServeContent reads in a goroutine that can still be running
// when the handler returns and closes the blob. mu serializes them, so
// Close never drops the buffer under a Read.
type dbBlob struct {
	q    store.Querier
	ctx  context.Context
	sha  string
	size int64 // set by Open, never changed

	mu     sync.Mutex // guards the fields below
	off    int64
	seq    int64  // the chunk in buf; -1 when none is loaded
	buf    []byte // reused for every chunk
	closed bool
}

var errClosed = errors.New("blobstore: read of a closed blob")

func (b *dbBlob) Size() int64 { return b.size }

func (b *dbBlob) Read(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
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
	// mu is held through load, so Close cannot have run; check anyway that
	// the chunk in buf is the one this Read needs.
	start := b.off - (seq << chunkShift)
	if b.closed || b.seq != seq || start < 0 || start >= int64(len(b.buf)) {
		return 0, errClosed
	}
	n := copy(p, b.buf[start:])
	b.off += int64(n)
	return n, nil
}

// load reads chunk seq into buf and checks its length. The caller holds mu.
func (b *dbBlob) load(seq int64) error {
	b.seq = -1
	rows, err := b.q.QueryContext(b.ctx, `SELECT data FROM inbox_blob_chunks WHERE sha256 = ? AND seq = ?`, b.sha, seq)
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
	b.mu.Lock()
	defer b.mu.Unlock()
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

// Close drops the buffered chunk. It waits for a Read in progress on
// another goroutine; that Read's caller then gets errClosed on its next
// call. Closing twice is fine.
func (b *dbBlob) Close() error {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.closed = true
	b.buf = nil
	b.seq = -1
	return nil
}
