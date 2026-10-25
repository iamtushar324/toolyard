package inbox

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"mime"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"syscall"
	"time"

	"github.com/tusharbhardwaj/toolyard/internal/blobstore"
	"github.com/tusharbhardwaj/toolyard/internal/netguard"
	"github.com/tusharbhardwaj/toolyard/internal/store"
)

// Size caps per attachment type, in bytes.
const (
	MaxImageBytes = 10 << 20
	MaxVideoBytes = 50 << 20
	MaxFileBytes  = 25 << 20
)

var blobName = regexp.MustCompile(`^[0-9a-f]{64}$`)

// Snapshotter fetches a linked file once and keeps a copy in a blob store,
// named by its sha256. It refuses anything that resolves to a loopback,
// private, link-local or otherwise non-public address — checked on the
// dialled IP, after DNS, so a public name can't be pointed at an internal
// service.
type Snapshotter struct {
	// dir holds temporary files (downloads and uploads being hashed), and
	// on SQLite also the blobs themselves.
	dir    string
	db     *store.DB
	blobs  blobstore.Store
	client *http.Client
	// allowPrivate is for tests only.
	allowPrivate bool
}

// NewSnapshotter keeps copies in the blob store that goes with db
// (blobstore.ForDB): files named by their sha256 in dir on SQLite or with a
// nil db, 1 MiB chunks in the database on Postgres. dir is created if
// missing; temporary files always live there.
func NewSnapshotter(db *store.DB, dir string) (*Snapshotter, error) {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, err
	}
	blobs, err := blobstore.ForDB(db, dir)
	if err != nil {
		return nil, err
	}
	s := &Snapshotter{dir: dir, db: db, blobs: blobs}
	s.client = s.newClient()
	// Leftovers of a crash mid-download or mid-upload.
	s.CleanTemp(time.Hour)
	return s, nil
}

// tempPrefixes name the temporary files Snapshot, Put and uploads create.
var tempPrefixes = []string{"upload-", "fetch-", "put-"}

// CleanTemp deletes temporary files in the blob directory last modified
// more than olderThan ago and returns how many it removed. Blobs (named by
// their sha256) are never touched.
func (s *Snapshotter) CleanTemp(olderThan time.Duration) int {
	entries, err := os.ReadDir(s.dir)
	if err != nil {
		return 0
	}
	cutoff := time.Now().Add(-olderThan)
	n := 0
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || blobName.MatchString(name) {
			continue
		}
		temp := false
		for _, p := range tempPrefixes {
			temp = temp || strings.HasPrefix(name, p)
		}
		if !temp {
			continue
		}
		info, err := e.Info()
		if err != nil || !info.ModTime().Before(cutoff) {
			continue
		}
		if os.Remove(filepath.Join(s.dir, name)) == nil {
			n++
		}
	}
	return n
}

func (s *Snapshotter) newClient() *http.Client {
	dialer := &net.Dialer{
		Timeout: 10 * time.Second,
		Control: func(network, address string, _ syscall.RawConn) error {
			if s.allowPrivate {
				return nil
			}
			host, _, err := net.SplitHostPort(address)
			if err != nil {
				return err
			}
			ip := net.ParseIP(host)
			if ip == nil || !publicIP(ip) {
				return fmt.Errorf("%s is a private or local address, which toolyard won't fetch", host)
			}
			return nil
		},
	}
	tr := &http.Transport{
		Proxy:                 nil, // never route agent-chosen URLs through a local proxy
		DialContext:           dialer.DialContext,
		TLSHandshakeTimeout:   10 * time.Second,
		ResponseHeaderTimeout: 15 * time.Second,
		MaxIdleConns:          4,
		IdleConnTimeout:       30 * time.Second,
	}
	return &http.Client{
		Transport: tr,
		Timeout:   90 * time.Second,
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			if len(via) >= 3 {
				return errors.New("too many redirects")
			}
			if req.URL.Scheme != "https" && req.URL.Scheme != "http" {
				return errors.New("redirect to a non-http URL")
			}
			return nil
		},
	}
}

// publicIP reports whether ip is a public unicast address: the same check
// webhook delivery applies (netguard.Public).
func publicIP(ip net.IP) bool { return netguard.Public(ip) }

func capFor(kind string) int64 {
	switch kind {
	case "image":
		return MaxImageBytes
	case "video":
		return MaxVideoBytes
	default:
		return MaxFileBytes
	}
}

func typeAllowed(kind, ct string) bool {
	switch kind {
	case "image":
		switch ct {
		case "image/png", "image/jpeg", "image/gif", "image/webp":
			return true
		}
		return false
	case "video":
		return ct == "video/mp4" || ct == "video/webm" || ct == "video/quicktime"
	default:
		// Files are only ever offered as downloads, never rendered, but
		// refuse HTML/SVG anyway so a copy can't become a phishing page.
		return ct != "text/html" && ct != "image/svg+xml" && ct != "application/xhtml+xml"
	}
}

// AllowPrivateNetworks lets the fetcher reach loopback and private
// addresses. Only for operators who host evidence on their own LAN.
func (s *Snapshotter) AllowPrivateNetworks(allow bool) { s.allowPrivate = allow }

// Snapshot fetches rawURL and returns the stored copy's sha256.
func (s *Snapshotter) Snapshot(ctx context.Context, rawURL, kind string) (string, string, int64, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, nil)
	if err != nil {
		return "", "", 0, fmt.Errorf("bad URL: %v", err)
	}
	req.Header.Set("User-Agent", "toolyard-attachment-fetcher/1")
	resp, err := s.client.Do(req)
	if err != nil {
		return "", "", 0, fmt.Errorf("couldn't fetch it: %v", unwrapURLErr(err))
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", "", 0, fmt.Errorf("the server answered %s", resp.Status)
	}
	ct, _, _ := mime.ParseMediaType(resp.Header.Get("Content-Type"))
	if ct == "" {
		ct = "application/octet-stream"
	}
	limit := capFor(kind)
	if resp.ContentLength > limit {
		return "", "", 0, fmt.Errorf("%d MB; max %d MB for a %s", resp.ContentLength>>20, limit>>20, kind)
	}
	tmp, err := os.CreateTemp(s.dir, "fetch-*")
	if err != nil {
		return "", "", 0, err
	}
	defer os.Remove(tmp.Name())
	h := sha256.New()
	n, err := io.Copy(io.MultiWriter(tmp, h), io.LimitReader(resp.Body, limit+1))
	_ = tmp.Close()
	if err != nil {
		return "", "", 0, fmt.Errorf("download failed: %v", err)
	}
	if n > limit {
		return "", "", 0, fmt.Errorf("larger than %d MB, the limit for a %s", limit>>20, kind)
	}
	if ct == "application/octet-stream" || ct == "binary/octet-stream" {
		f, _ := os.Open(tmp.Name())
		if f != nil {
			head := make([]byte, 512)
			m, _ := io.ReadFull(f, head)
			f.Close()
			ct, _, _ = mime.ParseMediaType(http.DetectContentType(head[:m]))
		}
	}
	if !typeAllowed(kind, ct) {
		return "", "", 0, fmt.Errorf("content type %s isn't allowed for a %s", ct, kind)
	}
	sum := hex.EncodeToString(h.Sum(nil))
	// Local stores rename the temporary file into place (no copy); the
	// Postgres store copies it into chunks and the file is removed.
	if _, err := s.storeFile(ctx, sum, tmp.Name()); err != nil {
		return "", "", 0, err
	}
	if s.db != nil {
		_, _ = s.db.ExecContext(ctx, `INSERT INTO inbox_blobs(sha256, content_type, size, source_url, created_at) VALUES (?,?,?,?,?) ON CONFLICT DO NOTHING`,
			sum, ct, n, rawURL, time.Now().UnixMilli())
	}
	return sum, ct, n, nil
}

// Put stores generated bytes (e.g. a recorded voice note) as a blob.
func (s *Snapshotter) Put(ctx context.Context, data []byte, contentType, source string) (string, error) {
	sum := sha256.Sum256(data)
	name := hex.EncodeToString(sum[:])
	// Local writes a put-* temporary file and renames it, as before.
	if _, err := s.blobs.Put(ctx, name, bytes.NewReader(data), int64(len(data))); err != nil {
		return "", err
	}
	if s.db != nil {
		if _, err := s.db.ExecContext(ctx, `INSERT INTO inbox_blobs(sha256, content_type, size, source_url, created_at) VALUES (?,?,?,?,?) ON CONFLICT DO NOTHING`,
			name, contentType, len(data), source, time.Now().UnixMilli()); err != nil {
			return "", err
		}
	}
	return name, nil
}

// Open returns a stored copy and its content type. The returned Blob also
// has a ModTime method (see storedBlob) for HTTP caching.
func (s *Snapshotter) Open(ctx context.Context, sha string) (blobstore.Blob, string, error) {
	if !blobName.MatchString(sha) {
		return nil, "", ErrNotFound
	}
	var ct string
	var recorded int64
	if s.db != nil {
		if err := s.db.QueryRowContext(ctx, `SELECT content_type, created_at FROM inbox_blobs WHERE sha256 = ?`, sha).Scan(&ct, &recorded); err != nil {
			return nil, "", ErrNotFound
		}
	}
	b, err := s.blobs.Open(ctx, sha)
	if err != nil {
		return nil, "", ErrNotFound
	}
	return &storedBlob{Blob: b, recorded: recorded}, ct, nil
}

// storedBlob is an open blob with its modification time.
type storedBlob struct {
	blobstore.Blob
	recorded int64 // inbox_blobs.created_at (Unix ms); 0 when unknown
}

// ModTime is the file's modification time for a local blob (what the blob
// route always sent as Last-Modified), else when the blob was recorded.
func (b *storedBlob) ModTime() time.Time {
	if f, ok := b.Blob.(interface{ Stat() (os.FileInfo, error) }); ok {
		if st, err := f.Stat(); err == nil {
			return st.ModTime()
		}
	}
	if b.recorded > 0 {
		return time.UnixMilli(b.recorded)
	}
	return time.Time{}
}

func unwrapURLErr(err error) string {
	msg := err.Error()
	if i := strings.LastIndex(msg, ": "); i >= 0 && strings.Contains(msg, "private or local") {
		return msg[i+2:]
	}
	return msg
}

// Exists reports whether a stored copy's bytes are in the blob store.
func (s *Snapshotter) Exists(ctx context.Context, sha string) bool {
	if !blobName.MatchString(sha) {
		return false
	}
	ok, err := s.blobs.Exists(ctx, sha)
	return err == nil && ok
}

// Remove deletes a stored copy and its row. Callers decide that nothing
// references it any more (see Service.sweepUploads).
func (s *Snapshotter) Remove(ctx context.Context, sha string) error {
	if !blobName.MatchString(sha) {
		return ErrNotFound
	}
	if s.db != nil {
		if _, err := s.db.ExecContext(ctx, `DELETE FROM inbox_blobs WHERE sha256 = ?`, sha); err != nil {
			return err
		}
	}
	return s.blobs.Delete(ctx, sha)
}

// CreateUploadTemp opens a temporary file in the blob directory, so a
// finished upload can be renamed into place without a copy (Local).
func (s *Snapshotter) CreateUploadTemp() (*os.File, error) {
	return os.CreateTemp(s.dir, "upload-*")
}

// PrepareUpload copies a verified temporary file into a store that copies
// (Postgres) and leaves the file in place for Adopt. The upload calls it
// before it takes uploadMu, so other uploads don't wait for a copy of up to
// 50 MB. Stores that rename files instead (Local) do nothing here. created
// is true when this call stored the blob.
func (s *Snapshotter) PrepareUpload(ctx context.Context, tmpPath, sha string) (created bool, err error) {
	if _, ok := s.blobs.(blobstore.FileAdopter); ok {
		return false, nil
	}
	if !blobName.MatchString(sha) || !s.inDir(tmpPath) {
		return false, ErrNotFound
	}
	return s.putFile(ctx, sha, tmpPath)
}

// Adopt moves a verified temporary file into place as blob sha: a rename on
// Local, a copy into the database on Postgres (a cheap existence check when
// PrepareUpload already stored it). created is false when the blob already
// existed. The temporary file is gone afterwards in every case.
// It writes no row: the caller records inbox_blobs in its own transaction.
func (s *Snapshotter) Adopt(ctx context.Context, tmpPath, sha string) (created bool, err error) {
	if !blobName.MatchString(sha) || !s.inDir(tmpPath) {
		_ = os.Remove(tmpPath)
		return false, ErrNotFound
	}
	return s.storeFile(ctx, sha, tmpPath)
}

// DiscardBlob removes blob sha's bytes only (no row). Used to undo Adopt
// when the transaction that would have recorded it fails, and by the sweep
// after it deleted the row. A missing blob is not an error.
func (s *Snapshotter) DiscardBlob(ctx context.Context, sha string) error {
	if !blobName.MatchString(sha) {
		return nil
	}
	return s.blobs.Delete(ctx, sha)
}

// inDir reports whether path is a file directly in the blob directory.
func (s *Snapshotter) inDir(path string) bool {
	return filepath.Dir(path) == filepath.Clean(s.dir)
}

// putFile stores the file at path as blob sha through the blob store and
// leaves the file in place.
func (s *Snapshotter) putFile(ctx context.Context, sha, path string) (bool, error) {
	f, err := os.Open(path)
	if err != nil {
		return false, err
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil {
		return false, err
	}
	return s.blobs.Put(ctx, sha, f, st.Size())
}

// storeFile moves the temporary file at path into the blob store as blob
// sha and removes the file: Local renames it, other stores copy it.
func (s *Snapshotter) storeFile(ctx context.Context, sha, path string) (bool, error) {
	if a, ok := s.blobs.(blobstore.FileAdopter); ok {
		return a.AdoptFile(ctx, sha, path)
	}
	defer os.Remove(path)
	return s.putFile(ctx, sha, path)
}
