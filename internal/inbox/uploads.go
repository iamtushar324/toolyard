package inbox

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"database/sql"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"log"
	"mime"
	"net/http"
	"path"
	"strings"
	"time"
	"unicode/utf8"
)

// Native uploads let an agent attach a screenshot, recording or file it
// holds locally, without hosting it anywhere public:
//
//  1. inbox.upload {name, content_type, size, sha256, kind} returns a
//     single-use ticket URL, bound to this agent and to exactly those bytes.
//  2. The agent PUTs the bytes to /v1/uploads/{ticket}. Toolyard checks the
//     size, the sha256 and the real (sniffed) type, stores the blob and
//     records that this agent uploaded it.
//  3. An attachment cites it as {"upload": "upload:<sha256>"} instead of a
//     url. Only the agent that uploaded it can cite it.
//
// Uploads no request cites within UploadRetention are removed by Sweep.
const (
	UploadTicketTTL      = 15 * time.Minute
	MaxOpenUploadTickets = 50
	UploadRetention      = 24 * time.Hour
	UploadRefPrefix      = "upload:"
	uploadTicketPrefix   = "tyu_"
	maxUploadName        = 200
)

// Upload errors. UploadError is a problem with what the agent sent (400);
// the sentinels map to 404, 410 and 429-style refusals.
var (
	ErrUploadUnknown     = errors.New("unknown upload ticket")
	ErrUploadGone        = errors.New("this upload ticket was already used or has expired; call inbox.upload for a new one")
	ErrUploadTooMany     = fmt.Errorf("you already have %d open upload tickets; use or let some expire first", MaxOpenUploadTickets)
	ErrUploadUnavailable = errors.New("uploads are not available on this gateway")
)

// UploadError is a validation problem with an upload request or body.
type UploadError struct{ Msg string }

func (e *UploadError) Error() string { return e.Msg }

func uploadErr(format string, a ...any) error { return &UploadError{Msg: fmt.Sprintf(format, a...)} }

// UploadSpec is what an agent declares before it uploads.
type UploadSpec struct {
	Name        string
	ContentType string
	Size        int64
	SHA256      string
	Kind        string // image | video | file (default image)
}

// UploadTicket is the answer to inbox.upload. Ticket is the bearer
// credential for the PUT; it is shown once and only its hash is stored.
type UploadTicket struct {
	Ticket          string
	Ref             string
	ContentType     string
	Size            int64
	ExpiresAt       int64
	AlreadyUploaded bool
}

// PendingUpload is a ticket looked up for a PUT, before its body is read.
type PendingUpload struct {
	hash        string
	AgentID     string
	SHA256      string
	Size        int64
	ContentType string
	Kind        string
	Name        string
}

// UploadResult describes a stored upload.
type UploadResult struct {
	Ref         string `json:"ref"`
	SHA256      string `json:"sha256"`
	Size        int64  `json:"size"`
	ContentType string `json:"content_type"`
	AgentID     string `json:"-"`
	Name        string `json:"-"`
}

// blobRemover is implemented by Snapshotter; Sweep uses it to delete the
// file of an upload nothing cites.
type blobRemover interface {
	Remove(ctx context.Context, sha string) error
}

func normalizeContentType(ct string) string {
	mt, _, err := mime.ParseMediaType(strings.TrimSpace(ct))
	if err != nil {
		return ""
	}
	return strings.ToLower(mt)
}

// ParseUploadRef returns the sha256 in an "upload:<sha256>" reference.
func ParseUploadRef(ref string) (string, bool) {
	sha, ok := strings.CutPrefix(strings.TrimSpace(ref), UploadRefPrefix)
	if !ok || !blobName.MatchString(sha) {
		return "", false
	}
	return sha, true
}

func hashUploadTicket(t string) string {
	h := sha256.Sum256([]byte(t))
	return hex.EncodeToString(h[:])
}

func validUploadKind(k string) bool { return k == "image" || k == "video" || k == "file" }

// CreateUpload validates an agent's declared upload and issues a ticket.
// When this agent already uploaded the same bytes, it returns the ref with
// AlreadyUploaded and no ticket.
func (s *Service) CreateUpload(ctx context.Context, agentID string, spec UploadSpec) (*UploadTicket, error) {
	if s.opts.Blobs == nil {
		return nil, ErrUploadUnavailable
	}
	if agentID == "" {
		return nil, uploadErr("uploads need an enrolled agent")
	}
	kind := strings.TrimSpace(spec.Kind)
	if kind == "" {
		kind = "image"
	}
	if !validUploadKind(kind) {
		return nil, uploadErr("kind: use image, video or file")
	}
	name := strings.TrimSpace(spec.Name)
	if name != "" {
		name = path.Base(strings.ReplaceAll(name, "\\", "/"))
	}
	if name == "" || name == "." || name == "/" {
		return nil, uploadErr("name: give the file name, e.g. checkout-screenshot.png")
	}
	if utf8.RuneCountInString(name) > maxUploadName || strings.ContainsAny(name, "\r\n\"") {
		return nil, uploadErr("name: at most %d characters, no quotes or line breaks", maxUploadName)
	}
	ct := normalizeContentType(spec.ContentType)
	if ct == "" {
		return nil, uploadErr("content_type: give a MIME type such as image/png")
	}
	if !typeAllowed(kind, ct) {
		switch kind {
		case "image":
			return nil, uploadErr("content_type %s isn't allowed for an image; use image/png, image/jpeg, image/webp or image/gif", ct)
		case "video":
			return nil, uploadErr("content_type %s isn't allowed for a video; use video/mp4, video/webm or video/quicktime", ct)
		default:
			return nil, uploadErr("content_type %s isn't allowed for a file", ct)
		}
	}
	if spec.Size <= 0 {
		return nil, uploadErr("size: give the exact size in bytes")
	}
	if limit := capFor(kind); spec.Size > limit {
		return nil, uploadErr("size: %d bytes; max %d MB for a %s", spec.Size, limit>>20, kind)
	}
	sha := strings.TrimSpace(spec.SHA256)
	if !blobName.MatchString(sha) {
		return nil, uploadErr("sha256: give the file's sha256 as 64 lowercase hex characters (sha256sum <file>)")
	}
	now := s.now()

	var have string
	var haveSize int64
	err := s.db.QueryRowContext(ctx, `SELECT content_type, size FROM inbox_uploads WHERE sha256=? AND agent_id=?`, sha, agentID).Scan(&have, &haveSize)
	if err == nil && s.blobExists(ctx, sha) {
		if haveSize != spec.Size {
			return nil, uploadErr("size: you already uploaded this sha256 with %d bytes", haveSize)
		}
		// Refresh the retention clock: the agent is about to cite it.
		_, _ = s.db.ExecContext(ctx, `UPDATE inbox_uploads SET created_at=? WHERE sha256=? AND agent_id=? AND used_at IS NULL`, now.UnixMilli(), sha, agentID)
		return &UploadTicket{Ref: UploadRefPrefix + sha, ContentType: have, Size: haveSize, AlreadyUploaded: true}, nil
	}
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return nil, err
	}

	var open int
	if err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM inbox_upload_tickets WHERE agent_id=? AND used_at IS NULL AND expires_at>?`, agentID, now.UnixMilli()).Scan(&open); err != nil {
		return nil, err
	}
	if open >= MaxOpenUploadTickets {
		return nil, ErrUploadTooMany
	}
	raw := make([]byte, 32)
	if _, err := rand.Read(raw); err != nil {
		return nil, err
	}
	ticket := uploadTicketPrefix + base64.RawURLEncoding.EncodeToString(raw)
	exp := now.Add(UploadTicketTTL).UnixMilli()
	if _, err := s.db.ExecContext(ctx, `INSERT INTO inbox_upload_tickets(token_hash, agent_id, sha256, size, content_type, kind, name, created_at, expires_at) VALUES (?,?,?,?,?,?,?,?,?)`,
		hashUploadTicket(ticket), agentID, sha, spec.Size, ct, kind, name, now.UnixMilli(), exp); err != nil {
		return nil, err
	}
	return &UploadTicket{Ticket: ticket, Ref: UploadRefPrefix + sha, ContentType: ct, Size: spec.Size, ExpiresAt: exp}, nil
}

// blobExists reports whether the blob's bytes are still on disk.
func (s *Service) blobExists(ctx context.Context, sha string) bool {
	o, ok := s.opts.Blobs.(interface {
		Exists(ctx context.Context, sha string) bool
	})
	if !ok {
		return true
	}
	return o.Exists(ctx, sha)
}

// OpenUpload looks up a ticket for a PUT. It does not use the ticket.
func (s *Service) OpenUpload(ctx context.Context, ticket string) (*PendingUpload, error) {
	if s.opts.Blobs == nil {
		return nil, ErrUploadUnavailable
	}
	if !strings.HasPrefix(ticket, uploadTicketPrefix) || len(ticket) < len(uploadTicketPrefix)+43 || len(ticket) > 128 {
		return nil, ErrUploadUnknown
	}
	want := hashUploadTicket(ticket)
	var p PendingUpload
	var got string
	var exp int64
	var used sql.NullInt64
	err := s.db.QueryRowContext(ctx, `SELECT token_hash, agent_id, sha256, size, content_type, kind, name, expires_at, used_at FROM inbox_upload_tickets WHERE token_hash=?`, want).
		Scan(&got, &p.AgentID, &p.SHA256, &p.Size, &p.ContentType, &p.Kind, &p.Name, &exp, &used)
	if errors.Is(err, sql.ErrNoRows) || (err == nil && subtle.ConstantTimeCompare([]byte(got), []byte(want)) != 1) {
		return nil, ErrUploadUnknown
	}
	if err != nil {
		return nil, err
	}
	if used.Valid || exp <= s.now().UnixMilli() {
		return nil, ErrUploadGone
	}
	p.hash = want
	return &p, nil
}

// CompleteUpload reads the body of a PUT for p, checks it against the
// ticket, stores it and uses the ticket. contentType is the request's
// Content-Type header ("" is accepted).
func (s *Service) CompleteUpload(ctx context.Context, p *PendingUpload, contentType string, body io.Reader) (*UploadResult, error) {
	if p == nil || p.hash == "" {
		return nil, ErrUploadUnknown
	}
	if ct := normalizeContentType(contentType); contentType != "" && ct != p.ContentType {
		return nil, uploadErr("Content-Type %s doesn't match the ticket (%s)", contentType, p.ContentType)
	}
	data, err := io.ReadAll(io.LimitReader(body, p.Size+1))
	if err != nil {
		var mbe *http.MaxBytesError
		if errors.As(err, &mbe) {
			return nil, uploadErr("the body is larger than the declared %d bytes", p.Size)
		}
		return nil, uploadErr("couldn't read the body: %v", err)
	}
	if int64(len(data)) != p.Size {
		return nil, uploadErr("the body is %d bytes; the ticket says %d", len(data), p.Size)
	}
	sum := sha256.Sum256(data)
	if got := hex.EncodeToString(sum[:]); got != p.SHA256 {
		return nil, uploadErr("sha256 mismatch: the body hashes to %s, the ticket says %s", got, p.SHA256)
	}
	stored, err := sniffUpload(p.Kind, p.ContentType, data)
	if err != nil {
		return nil, err
	}
	now := s.now().UnixMilli()
	res, err := s.db.ExecContext(ctx, `UPDATE inbox_upload_tickets SET used_at=? WHERE token_hash=? AND used_at IS NULL AND expires_at>?`, now, p.hash, now)
	if err != nil {
		return nil, err
	}
	if n, _ := res.RowsAffected(); n != 1 {
		return nil, ErrUploadGone
	}
	sha, err := s.opts.Blobs.Put(ctx, data, stored, UploadRefPrefix+p.SHA256)
	if err != nil {
		return nil, err
	}
	if sha != p.SHA256 {
		return nil, fmt.Errorf("stored blob hash %s differs from %s", sha, p.SHA256)
	}
	if _, err := s.db.ExecContext(ctx, `INSERT INTO inbox_uploads(sha256, agent_id, name, content_type, kind, size, created_at) VALUES (?,?,?,?,?,?,?)
		ON CONFLICT(sha256, agent_id) DO UPDATE SET name=excluded.name, content_type=excluded.content_type, kind=excluded.kind, size=excluded.size, created_at=excluded.created_at`,
		p.SHA256, p.AgentID, p.Name, stored, p.Kind, p.Size, now); err != nil {
		return nil, err
	}
	return &UploadResult{Ref: UploadRefPrefix + p.SHA256, SHA256: p.SHA256, Size: p.Size, ContentType: stored, AgentID: p.AgentID, Name: p.Name}, nil
}

// sniffUpload checks the bytes against the declared type and returns the
// content type to store. Images must really be PNG, JPEG, WebP or GIF and
// match the declared type; videos must look like MP4, WebM or QuickTime;
// files may be anything except HTML, SVG or XHTML, and a file declared as
// an image or video must really be one (those render inline).
func sniffUpload(kind, declared string, data []byte) (string, error) {
	sniffed := normalizeContentType(http.DetectContentType(data))
	checkImage := func() (string, error) {
		switch sniffed {
		case "image/png", "image/jpeg", "image/gif", "image/webp":
		default:
			return "", uploadErr("the bytes are %s, not an image (png, jpeg, webp or gif)", sniffed)
		}
		if sniffed != declared {
			return "", uploadErr("the bytes are %s but the ticket says %s; ask for a ticket with the real type", sniffed, declared)
		}
		return sniffed, nil
	}
	checkVideo := func() (string, error) {
		switch {
		case (sniffed == "video/mp4" || sniffed == "video/webm") && (sniffed == declared || declared == "video/quicktime" && sniffed == "video/mp4"):
			return declared, nil
		case declared == "video/quicktime" && looksQuickTime(data):
			return declared, nil
		}
		return "", uploadErr("the bytes are %s, not a %s video", sniffed, declared)
	}
	switch kind {
	case "image":
		return checkImage()
	case "video":
		return checkVideo()
	}
	if strings.HasPrefix(declared, "image/") {
		return checkImage()
	}
	if strings.HasPrefix(declared, "video/") {
		return checkVideo()
	}
	if looksLikeMarkup(sniffed, data) {
		return "", uploadErr("HTML, SVG and XHTML files aren't accepted; send it as a code or markdown attachment instead")
	}
	return declared, nil
}

// looksQuickTime reports whether data starts with a QuickTime atom.
func looksQuickTime(data []byte) bool {
	if len(data) < 12 {
		return false
	}
	switch string(data[4:8]) {
	case "ftyp", "moov", "mdat", "wide", "free", "skip", "pnot":
		return true
	}
	return false
}

func looksLikeMarkup(sniffed string, data []byte) bool {
	if sniffed == "text/html" || sniffed == "image/svg+xml" || sniffed == "application/xhtml+xml" {
		return true
	}
	if !strings.HasPrefix(sniffed, "text/") {
		return false
	}
	head := data
	if len(head) > 4096 {
		head = head[:4096]
	}
	lower := bytes.ToLower(head)
	for _, tag := range [][]byte{[]byte("<svg"), []byte("<html"), []byte("<!doctype html"), []byte("<script")} {
		if bytes.Contains(lower, tag) {
			return true
		}
	}
	return false
}

// uploadFor returns an upload agentID owns, checked against the attachment
// type it is cited as.
func (s *Service) uploadFor(ctx context.Context, agentID, ref, kind string) (sha, ct string, size int64, err error) {
	sha, ok := ParseUploadRef(ref)
	if !ok {
		return "", "", 0, fmt.Errorf("use \"upload:<sha256>\" exactly as inbox.upload returned it")
	}
	err = s.db.QueryRowContext(ctx, `SELECT content_type, size FROM inbox_uploads WHERE sha256=? AND agent_id=?`, sha, agentID).Scan(&ct, &size)
	if errors.Is(err, sql.ErrNoRows) {
		return "", "", 0, fmt.Errorf("no finished upload with this ref belongs to you; call inbox.upload, PUT the file, then cite the ref")
	}
	if err != nil {
		return "", "", 0, err
	}
	if !typeAllowed(kind, ct) {
		return "", "", 0, fmt.Errorf("the upload is %s, which isn't allowed for a %s", ct, kind)
	}
	if limit := capFor(kind); size > limit {
		return "", "", 0, fmt.Errorf("the upload is %d MB; max %d MB for a %s", size>>20, limit>>20, kind)
	}
	return sha, ct, size, nil
}

// resolveUploads fills Blob, ContentType and Size for attachments that cite
// an upload. No network is involved.
func (s *Service) resolveUploads(ctx context.Context, agentID string, r *Request, probs *[]Problem) {
	for i := range r.Attachments {
		a := &r.Attachments[i]
		if a.Type != "image" && a.Type != "video" && a.Type != "file" {
			continue
		}
		if a.Upload != "" && a.URL == "" {
			sha, ct, size, err := s.uploadFor(ctx, agentID, a.Upload, a.Type)
			if err != nil {
				*probs = append(*probs, Problem{fmt.Sprintf("attachments[%d].upload", i), err.Error()})
			} else {
				a.Blob, a.ContentType, a.Size = sha, ct, size
			}
		}
		if a.Type == "video" && a.PosterUpload != "" && a.PosterURL == "" {
			sha, _, _, err := s.uploadFor(ctx, agentID, a.PosterUpload, "image")
			if err != nil {
				*probs = append(*probs, Problem{fmt.Sprintf("attachments[%d].poster_upload", i), err.Error()})
			} else {
				a.PosterBlob = sha
			}
		}
	}
}

// markUploadsUsed records that a stored request cites its uploads, so
// Sweep keeps them.
func (s *Service) markUploadsUsed(ctx context.Context, r *Request) {
	now := s.now().UnixMilli()
	for _, a := range r.Attachments {
		for _, ref := range []string{a.Upload, a.PosterUpload} {
			if sha, ok := ParseUploadRef(ref); ok {
				_, _ = s.db.ExecContext(ctx, `UPDATE inbox_uploads SET used_at=? WHERE sha256=? AND agent_id=? AND used_at IS NULL`, now, sha, r.AgentID)
			}
		}
	}
}

// sweepUploads purges expired tickets and uploads that no request cited
// within UploadRetention. A blob is deleted only when it came from an
// upload, no other agent's upload holds it and no request references it.
func (s *Service) sweepUploads(ctx context.Context) {
	now := s.now()
	if _, err := s.db.ExecContext(ctx, `DELETE FROM inbox_upload_tickets WHERE expires_at <= ?`, now.UnixMilli()); err != nil {
		log.Printf("inbox: purge upload tickets: %v", err)
	}
	rows, err := s.db.QueryContext(ctx, `SELECT sha256, agent_id FROM inbox_uploads WHERE used_at IS NULL AND created_at < ? LIMIT 500`, now.Add(-UploadRetention).UnixMilli())
	if err != nil {
		log.Printf("inbox: sweep uploads: %v", err)
		return
	}
	type up struct{ sha, agent string }
	var stale []up
	for rows.Next() {
		var u up
		if rows.Scan(&u.sha, &u.agent) == nil {
			stale = append(stale, u)
		}
	}
	rows.Close()
	for _, u := range stale {
		if s.blobCited(ctx, u.agent, u.sha) {
			_, _ = s.db.ExecContext(ctx, `UPDATE inbox_uploads SET used_at=? WHERE sha256=? AND agent_id=?`, now.UnixMilli(), u.sha, u.agent)
			continue
		}
		if _, err := s.db.ExecContext(ctx, `DELETE FROM inbox_uploads WHERE sha256=? AND agent_id=?`, u.sha, u.agent); err != nil {
			continue
		}
		var other int
		if s.db.QueryRowContext(ctx, `SELECT 1 FROM inbox_uploads WHERE sha256=? LIMIT 1`, u.sha).Scan(&other) == nil {
			continue
		}
		if s.blobCited(ctx, "", u.sha) {
			continue
		}
		var source string
		if s.db.QueryRowContext(ctx, `SELECT source_url FROM inbox_blobs WHERE sha256=?`, u.sha).Scan(&source) != nil || source != UploadRefPrefix+u.sha {
			continue
		}
		if rm, ok := s.opts.Blobs.(blobRemover); ok {
			if err := rm.Remove(ctx, u.sha); err != nil {
				log.Printf("inbox: remove upload blob %s: %v", u.sha[:12], err)
			}
		}
	}
}

// blobCited reports whether any request (of agentID, or of anyone when
// agentID is "") references sha as a blob or poster blob.
func (s *Service) blobCited(ctx context.Context, agentID, sha string) bool {
	var found int
	var err error
	if agentID != "" {
		err = s.db.QueryRowContext(ctx, `SELECT 1 FROM inbox_requests r, json_tree(r.doc) j WHERE r.agent_id=? AND j.key IN ('blob','poster_blob') AND j.type='text' AND j.value=? LIMIT 1`, agentID, sha).Scan(&found)
	} else {
		err = s.db.QueryRowContext(ctx, `SELECT 1 FROM inbox_requests r, json_tree(r.doc) j WHERE j.key IN ('blob','poster_blob') AND j.type='text' AND j.value=? LIMIT 1`, sha).Scan(&found)
	}
	return err == nil && found == 1
}
