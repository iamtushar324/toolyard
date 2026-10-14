// Package blobstore keeps the bytes of Inbox attachments (uploads, copied
// links, recorded voice notes), each named by the lowercase hex sha256 of its
// content. A Store holds bytes only: the metadata row (inbox_blobs: content
// type, source, size) stays with its callers and their transactions.
//
// Local keeps one file per blob under a directory; it is what SQLite
// deployments use, with the same layout toolyard always had. Postgres keeps
// each blob in the database as 1 MiB chunks (tables inbox_blob_objects and
// inbox_blob_chunks), so no replica needs a shared disk and a 50 MB file is
// never held in memory whole.
package blobstore

import (
	"context"
	"errors"
	"fmt"
	"io"

	"github.com/tusharbhardwaj/toolyard/internal/store"
)

// ChunkSize is the size of one chunk row in the Postgres store. Every chunk
// of a blob but the last is exactly ChunkSize bytes.
const ChunkSize = 1 << chunkShift

const chunkShift = 20

// ErrNotFound is returned by Open when a blob is not stored.
var ErrNotFound = errors.New("blob not found")

// Blob is one open stored blob. Read and Seek work like an *os.File, so a
// Blob can be served with http.ServeContent (Range requests included).
type Blob interface {
	io.ReadSeekCloser
	Size() int64
}

// Store keeps blob bytes by sha256.
type Store interface {
	// Put stores the bytes read from r under sha (lowercase hex sha256, verified by the
	// caller). size is the expected length. created is false when sha already existed.
	Put(ctx context.Context, sha string, r io.Reader, size int64) (created bool, err error)
	Open(ctx context.Context, sha string) (Blob, error) // ErrNotFound when missing
	Exists(ctx context.Context, sha string) (bool, error)
	Delete(ctx context.Context, sha string) error // no error when missing
}

// FileAdopter is implemented by stores that can take over a local file
// without copying it. Local does: it renames the file into place, which is
// how toolyard stored uploads and copied links before this package existed.
type FileAdopter interface {
	// AdoptFile moves the file at path into the store as blob sha. path must
	// be in the store's directory. The file at path is gone afterwards in
	// every case (moved or removed). created is false when sha already
	// existed.
	AdoptFile(ctx context.Context, sha, path string) (created bool, err error)
}

// ForDB returns the store that goes with db: the Postgres store when db is
// Postgres, and Local files under dir otherwise (SQLite, or a nil db).
func ForDB(db *store.DB, dir string) (Store, error) {
	if db != nil && db.Dialect.IsPostgres() {
		return NewPostgres(db.DB), nil
	}
	return NewLocal(dir)
}

// ValidSHA reports whether sha is a well-formed blob name: 64 lowercase hex
// characters. Stores never build a path or query from any other name.
func ValidSHA(sha string) bool {
	if len(sha) != 64 {
		return false
	}
	for i := 0; i < len(sha); i++ {
		c := sha[i]
		if !(c >= '0' && c <= '9' || c >= 'a' && c <= 'f') {
			return false
		}
	}
	return true
}

func errBadName(sha string) error {
	if len(sha) > 80 {
		sha = sha[:80]
	}
	return fmt.Errorf("blobstore: %q is not a sha256 blob name", sha)
}

// short is a blob name shortened for error messages.
func short(sha string) string {
	if len(sha) > 12 {
		return sha[:12]
	}
	return sha
}
