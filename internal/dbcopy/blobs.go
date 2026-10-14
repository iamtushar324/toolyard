package dbcopy

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"hash"
	"io"
	"io/fs"
	"net"
	"os"
	"path/filepath"
	"sort"

	"github.com/jackc/pgx/v5/pgconn"

	"github.com/tusharbhardwaj/toolyard/internal/blobstore"
	"github.com/tusharbhardwaj/toolyard/internal/store"
)

// sourceBlob is one blob file named by an inbox_blobs row, already checked
// against its name.
type sourceBlob struct {
	sha  string
	path string
	size int64
}

// blobRows reads every inbox_blobs.sha256 of the source, sorted.
func blobRows(ctx context.Context, src store.Querier) ([]string, error) {
	shas, err := readNames(ctx, src, `SELECT sha256 FROM inbox_blobs`)
	if err != nil {
		return nil, fmt.Errorf("read the source's inbox_blobs: %w", err)
	}
	return shas, nil
}

// blobName shortens a name that is not a sha256 for the report.
func blobName(sha string) string {
	if len(sha) > 80 {
		return sha[:80] + "..."
	}
	return sha
}

// scanSourceBlobs checks the file of every inbox_blobs row: it must exist
// in dir and hash to its name. It records missing and mismatched files, and
// files in dir that no row names (orphans, reported only), and returns the
// good ones. It only reads.
func scanSourceBlobs(ctx context.Context, src store.Querier, dir string, br *BlobReport) ([]sourceBlob, error) {
	shas, err := blobRows(ctx, src)
	if err != nil {
		return nil, err
	}
	br.Rows = len(shas)
	known := setOf(shas)
	var out []sourceBlob
	for _, sha := range shas {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		if !blobstore.ValidSHA(sha) {
			br.MismatchedFiles = append(br.MismatchedFiles, blobName(sha))
			continue
		}
		path := filepath.Join(dir, sha)
		sum, n, err := hashFile(path)
		if errors.Is(err, fs.ErrNotExist) {
			br.MissingFiles = append(br.MissingFiles, sha)
			continue
		}
		if err != nil {
			return nil, fmt.Errorf("read blob file %s: %w", sha, err)
		}
		if sum != sha {
			br.MismatchedFiles = append(br.MismatchedFiles, sha)
			continue
		}
		br.Bytes += n
		out = append(out, sourceBlob{sha: sha, path: path, size: n})
	}
	orphans, err := orphanFiles(dir, known)
	if err != nil {
		return nil, err
	}
	br.OrphanFiles = orphans
	return out, nil
}

// orphanFiles lists the blob-named files in dir that no row names. Temp
// files (upload-*, fetch-*, put-*) and other names are not blobs.
func orphanFiles(dir string, known map[string]bool) ([]string, error) {
	entries, err := os.ReadDir(dir)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("list the blob folder: %w", err)
	}
	var out []string
	for _, e := range entries {
		if e.Type().IsRegular() && blobstore.ValidSHA(e.Name()) && !known[e.Name()] {
			out = append(out, e.Name())
		}
	}
	sort.Strings(out)
	return out, nil
}

func hashFile(path string) (string, int64, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", 0, err
	}
	defer f.Close()
	h := sha256.New()
	n, err := io.Copy(h, f)
	if err != nil {
		return "", 0, err
	}
	return fmt.Sprintf("%x", h.Sum(nil)), n, nil
}

// errBlobChanged is a blob file whose bytes no longer hash to its name.
var errBlobChanged = errors.New("the blob file changed")

// verifyingReader hashes what it reads and, at the end of the file, fails
// with errBlobChanged unless the bytes hash to want. The blob store then
// sees an error before it is done, so the copy rolls back before it
// commits. Once failed, it keeps failing.
type verifyingReader struct {
	r       io.Reader
	h       hash.Hash
	want    string
	matched bool // the whole file was read and hashed to want
	failed  bool
}

func (v *verifyingReader) Read(p []byte) (int, error) {
	if v.failed {
		return 0, errBlobChanged
	}
	n, err := v.r.Read(p)
	_, _ = v.h.Write(p[:n])
	if err == io.EOF {
		if fmt.Sprintf("%x", v.h.Sum(nil)) != v.want {
			v.failed, v.matched = true, false
			return n, errBlobChanged
		}
		v.matched = true
	}
	return n, err
}

// copyBlobs writes every checked blob into the blob store tables through
// exec, inside the copy's transaction, hashing the bytes again on the way:
// a file that changed since the check fails the copy before it commits. It
// returns how many blobs it wrote and how many were already there.
func copyBlobs(ctx context.Context, blobs []sourceBlob, exec blobstore.Execer) (copied, present int, err error) {
	for _, b := range blobs {
		if err := ctx.Err(); err != nil {
			return copied, present, loadError("store the blobs", err)
		}
		f, err := os.Open(b.path)
		if err != nil {
			return copied, present, fmt.Errorf("db copy: open blob file %s: %w. Nothing was written.", b.sha, err)
		}
		vr := &verifyingReader{r: f, h: sha256.New(), want: b.sha}
		created, err := blobstore.PutIn(ctx, exec, b.sha, vr, b.size)
		_ = f.Close()
		if err != nil || (created && !vr.matched) {
			if err == nil || errors.Is(err, errBlobChanged) || vr.failed {
				return copied, present, fmt.Errorf("db copy: blob file %s changed during the copy (its bytes no longer match its name). Nothing was written.", b.sha)
			}
			return copied, present, blobError(b.sha, err)
		}
		if !created {
			present++
			continue
		}
		copied++
	}
	return copied, present, nil
}

// blobError describes a failed blob write without quoting a value. The
// blob store's own errors hold only the blob name and sizes.
func blobError(sha string, err error) error {
	var pe *pgconn.PgError
	var ne net.Error
	if errors.As(err, &pe) || errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) || errors.As(err, &ne) {
		return loadError("store blob "+sha, err)
	}
	return fmt.Errorf("db copy: store blob %s: %v. Nothing was written.", sha, err)
}

// verifyBlobs reads every source blob back from the target store and
// checks that its sha256 equals its name. The source folder is only
// listed: rows without a file there, and orphan files, are reported.
func verifyBlobs(ctx context.Context, src store.Querier, dir string, bs blobstore.Opener, br *BlobReport) error {
	shas, err := blobRows(ctx, src)
	if err != nil {
		return fmt.Errorf("db verify: %w", err)
	}
	br.Rows = len(shas)
	for _, sha := range shas {
		if err := ctx.Err(); err != nil {
			return err
		}
		if blobstore.ValidSHA(sha) {
			if _, err := os.Stat(filepath.Join(dir, sha)); errors.Is(err, fs.ErrNotExist) {
				br.MissingFiles = append(br.MissingFiles, sha)
			}
		}
		b, err := bs.Open(ctx, sha)
		if errors.Is(err, blobstore.ErrNotFound) {
			br.TargetMissing = append(br.TargetMissing, blobName(sha))
			continue
		}
		if err != nil {
			return fmt.Errorf("db verify: open target blob %s: %w", blobName(sha), err)
		}
		h := sha256.New()
		n, err := io.Copy(h, b)
		size := b.Size()
		_ = b.Close()
		if err != nil {
			if cerr := ctx.Err(); cerr != nil {
				return cerr
			}
			// The store checks each chunk's length: a damaged blob fails
			// to read. That is a difference to report, not a reason to
			// stop comparing.
			br.TargetMismatched = append(br.TargetMismatched, sha)
			continue
		}
		if fmt.Sprintf("%x", h.Sum(nil)) != sha || n != size {
			br.TargetMismatched = append(br.TargetMismatched, sha)
			continue
		}
		br.Verified++
		br.Bytes += n
	}
	orphans, err := orphanFiles(dir, setOf(shas))
	if err != nil {
		return fmt.Errorf("db verify: %w", err)
	}
	br.OrphanFiles = orphans
	return nil
}
