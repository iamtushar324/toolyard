package inbox

import (
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

	"github.com/tusharbhardwaj/toolyard/internal/store"
)

// Size caps per attachment type, in bytes.
const (
	MaxImageBytes = 10 << 20
	MaxVideoBytes = 50 << 20
	MaxFileBytes  = 25 << 20
)

var blobName = regexp.MustCompile(`^[0-9a-f]{64}$`)

// Snapshotter fetches a linked file once and keeps a copy under dir, named
// by its sha256. It refuses anything that resolves to a loopback, private,
// link-local or otherwise non-public address — checked on the dialled IP,
// after DNS, so a public name can't be pointed at an internal service.
type Snapshotter struct {
	dir    string
	db     *store.DB
	client *http.Client
	// allowPrivate is for tests only.
	allowPrivate bool
}

// NewSnapshotter stores copies in dir (created if missing).
func NewSnapshotter(db *store.DB, dir string) (*Snapshotter, error) {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, err
	}
	s := &Snapshotter{dir: dir, db: db}
	s.client = s.newClient()
	return s, nil
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

// publicIP reports whether ip is a globally routable unicast address.
func publicIP(ip net.IP) bool {
	if ip.IsLoopback() || ip.IsPrivate() || ip.IsUnspecified() || ip.IsLinkLocalUnicast() ||
		ip.IsLinkLocalMulticast() || ip.IsInterfaceLocalMulticast() || ip.IsMulticast() {
		return false
	}
	if v4 := ip.To4(); v4 != nil {
		switch {
		case v4[0] == 0, v4[0] >= 240: // "this network", reserved
			return false
		case v4[0] == 100 && v4[1]&0xc0 == 64: // 100.64.0.0/10 carrier-grade NAT
			return false
		case v4[0] == 192 && v4[1] == 0 && v4[2] == 0: // 192.0.0.0/24
			return false
		case v4[0] == 198 && (v4[1] == 18 || v4[1] == 19): // benchmarking
			return false
		}
		return true
	}
	// 64:ff9b::/96 NAT64 could map to private v4; treat as non-public.
	if len(ip) == net.IPv6len && ip[0] == 0x00 && ip[1] == 0x64 && ip[2] == 0xff && ip[3] == 0x9b {
		return false
	}
	return true
}

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
	dst := filepath.Join(s.dir, sum)
	if _, err := os.Stat(dst); err != nil {
		if err := os.Rename(tmp.Name(), dst); err != nil {
			return "", "", 0, err
		}
	}
	if s.db != nil {
		_, _ = s.db.ExecContext(ctx, `INSERT OR IGNORE INTO inbox_blobs(sha256, content_type, size, source_url, created_at) VALUES (?,?,?,?,?)`,
			sum, ct, n, rawURL, time.Now().UnixMilli())
	}
	return sum, ct, n, nil
}

// Put stores generated bytes (e.g. a recorded voice note) as a blob.
func (s *Snapshotter) Put(ctx context.Context, data []byte, contentType, source string) (string, error) {
	sum := sha256.Sum256(data)
	name := hex.EncodeToString(sum[:])
	dst := filepath.Join(s.dir, name)
	if _, err := os.Stat(dst); err != nil {
		tmp, err := os.CreateTemp(s.dir, "put-*")
		if err != nil {
			return "", err
		}
		if _, err := tmp.Write(data); err != nil {
			tmp.Close()
			os.Remove(tmp.Name())
			return "", err
		}
		_ = tmp.Close()
		if err := os.Rename(tmp.Name(), dst); err != nil {
			os.Remove(tmp.Name())
			return "", err
		}
	}
	if s.db != nil {
		if _, err := s.db.ExecContext(ctx, `INSERT OR IGNORE INTO inbox_blobs(sha256, content_type, size, source_url, created_at) VALUES (?,?,?,?,?)`,
			name, contentType, len(data), source, time.Now().UnixMilli()); err != nil {
			return "", err
		}
	}
	return name, nil
}

// Open returns a stored copy and its content type.
func (s *Snapshotter) Open(ctx context.Context, sha string) (*os.File, string, error) {
	if !blobName.MatchString(sha) {
		return nil, "", ErrNotFound
	}
	var ct string
	if s.db != nil {
		if err := s.db.QueryRowContext(ctx, `SELECT content_type FROM inbox_blobs WHERE sha256 = ?`, sha).Scan(&ct); err != nil {
			return nil, "", ErrNotFound
		}
	}
	f, err := os.Open(filepath.Join(s.dir, sha))
	if err != nil {
		return nil, "", ErrNotFound
	}
	return f, ct, nil
}

func unwrapURLErr(err error) string {
	msg := err.Error()
	if i := strings.LastIndex(msg, ": "); i >= 0 && strings.Contains(msg, "private or local") {
		return msg[i+2:]
	}
	return msg
}
