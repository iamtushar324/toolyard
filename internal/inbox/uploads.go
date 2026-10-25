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
	"os"
	"path"
	"regexp"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/tusharbhardwaj/toolyard/internal/store"
)

// Native uploads let an agent attach a screenshot, recording or file it
// holds locally, without hosting it anywhere public:
//
//  1. inbox.upload {name, content_type, size, sha256, kind} returns a
//     single-use ticket URL, bound to this agent and to exactly those bytes.
//  2. The agent PUTs the bytes to /v1/uploads/{ticket}. Toolyard claims the
//     ticket, streams the body to a temporary file while hashing it, checks
//     the size, the sha256 and the real (sniffed) type, and then — in one
//     transaction — records the blob, records that this agent uploaded it
//     and uses the ticket. Any failure releases the claim.
//  3. An attachment cites it as {"upload": "upload:<sha256>"} instead of a
//     url. Only the agent that uploaded it can cite it.
//
// Uploads no request cites within UploadRetention are removed by Sweep.
const (
	UploadTicketTTL      = 15 * time.Minute
	MaxOpenUploadTickets = 50
	UploadRetention      = 24 * time.Hour
	UploadRefPrefix      = "upload:"
	// MaxConcurrentUploads is how many upload bodies the server reads at
	// once; further PUTs are refused (503) rather than queued.
	MaxConcurrentUploads = 8
	// MaxConcurrentUploadsPerAgent keeps one agent from taking every slot.
	MaxConcurrentUploadsPerAgent = 2
	// staleTempAge: temporary files older than this in the blob directory
	// are leftovers of a crash and are deleted (startup and Sweep).
	staleTempAge = time.Hour
	// UploadClaimTTL bounds a claim whose holder died without releasing it.
	UploadClaimTTL = 6 * time.Minute
	// uploadTouchGrace: the sweep skips an upload a submission resolved
	// this recently, so it never deletes one that is about to be cited.
	uploadTouchGrace   = time.Hour
	uploadTicketPrefix = "tyu_"
	maxUploadName      = 200
	uploadSniffBytes   = 4096
)

// Upload errors. UploadError is a problem with what the agent sent (400);
// the sentinels map to 404, 409, 410 and 503.
var (
	ErrUploadUnknown     = errors.New("unknown upload ticket")
	ErrUploadGone        = errors.New("this upload ticket was already used or has expired; call inbox.upload for a new one")
	ErrUploadBusy        = errors.New("another upload with this ticket is in progress")
	ErrUploadTooMany     = fmt.Errorf("you already have %d open upload tickets; use or let some expire first", MaxOpenUploadTickets)
	ErrUploadUnavailable = errors.New("uploads are not available on this gateway")
	ErrUploadServerBusy  = errors.New("too many uploads in progress; retry shortly")
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

// PendingUpload is a ticket claimed for one PUT. Complete it with
// CompleteUpload, which releases the claim on any failure.
type PendingUpload struct {
	hash        string
	claim       string
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

// uploadBlobs is the blob store an upload needs (Snapshotter implements
// it): a temporary file next to the blobs, and a move into the store (a
// rename on Local, a copy into database chunks on Postgres).
type uploadBlobs interface {
	CreateUploadTemp() (*os.File, error)
	PrepareUpload(ctx context.Context, tmpPath, sha string) (created bool, err error)
	Adopt(ctx context.Context, tmpPath, sha string) (created bool, err error)
	DiscardBlob(ctx context.Context, sha string) error
	Exists(ctx context.Context, sha string) bool
	CleanTemp(olderThan time.Duration) int
}

func (s *Service) uploadStore() (uploadBlobs, bool) {
	b, ok := s.opts.Blobs.(uploadBlobs)
	return b, ok
}

// mediaTypeRE is the RFC 6838 type/subtype grammar, lowercased.
var mediaTypeRE = regexp.MustCompile(`^[a-z0-9][a-z0-9!#&^_.+-]{0,126}/[a-z0-9][a-z0-9!#&^_.+-]{0,126}$`)

// normalizeContentType returns the bare, lowercased media type, or "" when
// ct isn't a well-formed media type. Parameters are dropped.
func normalizeContentType(ct string) string {
	mt, _, err := mime.ParseMediaType(strings.TrimSpace(ct))
	if err != nil {
		return ""
	}
	mt = strings.ToLower(mt)
	if !mediaTypeRE.MatchString(mt) {
		return ""
	}
	return mt
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

// AcquireUploadSlot reserves one of MaxConcurrentUploads body readers. It
// never blocks: ok is false when every slot is taken.
func (s *Service) AcquireUploadSlot() (release func(), ok bool) {
	select {
	case s.uploadSlots <- struct{}{}:
		var once sync.Once
		return func() { once.Do(func() { <-s.uploadSlots }) }, true
	default:
		return func() {}, false
	}
}

// AcquireAgentUploadSlot reserves one of an agent's
// MaxConcurrentUploadsPerAgent body readers, on top of the global slots.
func (s *Service) AcquireAgentUploadSlot(agentID string) (release func(), ok bool) {
	s.agentUploadsMu.Lock()
	defer s.agentUploadsMu.Unlock()
	if s.agentUploads[agentID] >= MaxConcurrentUploadsPerAgent {
		return func() {}, false
	}
	s.agentUploads[agentID]++
	var once sync.Once
	return func() {
		once.Do(func() {
			s.agentUploadsMu.Lock()
			defer s.agentUploadsMu.Unlock()
			if s.agentUploads[agentID]--; s.agentUploads[agentID] <= 0 {
				delete(s.agentUploads, agentID)
			}
		})
	}, true
}

// CreateUpload validates an agent's declared upload and issues a ticket.
// When this agent already uploaded the same bytes, it returns the ref with
// AlreadyUploaded and no ticket.
func (s *Service) CreateUpload(ctx context.Context, agentID string, spec UploadSpec) (*UploadTicket, error) {
	blobs, ok := s.uploadStore()
	if !ok {
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
	// The type ends up in a shell command we hand back, so anything beyond
	// the media-type grammar is refused outright, not just stripped.
	if strings.ContainsAny(spec.ContentType, "'`$\\\"\r\n") {
		return nil, uploadErr("content_type: give a plain MIME type such as image/png")
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
	if err == nil && blobs.Exists(ctx, sha) {
		if haveSize != spec.Size {
			return nil, uploadErr("size: you already uploaded this sha256 with %d bytes", haveSize)
		}
		// Refresh the retention clock: the agent is about to cite it.
		_, _ = s.db.ExecContext(ctx, `UPDATE inbox_uploads SET created_at=?, touched_at=? WHERE sha256=? AND agent_id=?`, now.UnixMilli(), now.UnixMilli(), sha, agentID)
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

// ClaimUpload marks a ticket as in progress for one PUT, before its body
// is read. A second PUT on the same ticket gets ErrUploadBusy until the
// first finishes; a used or expired ticket gets ErrUploadGone.
func (s *Service) ClaimUpload(ctx context.Context, ticket string) (*PendingUpload, error) {
	if _, ok := s.uploadStore(); !ok {
		return nil, ErrUploadUnavailable
	}
	if !strings.HasPrefix(ticket, uploadTicketPrefix) || len(ticket) < len(uploadTicketPrefix)+43 || len(ticket) > 128 {
		return nil, ErrUploadUnknown
	}
	want := hashUploadTicket(ticket)
	now := s.now().UnixMilli()
	cb := make([]byte, 16)
	if _, err := rand.Read(cb); err != nil {
		return nil, err
	}
	claim := hex.EncodeToString(cb)
	res, err := s.db.ExecContext(ctx, `UPDATE inbox_upload_tickets SET claimed_until=?, claim_token=?
		WHERE token_hash=? AND used_at IS NULL AND expires_at>? AND (claimed_until IS NULL OR claimed_until<=?)`,
		s.now().Add(UploadClaimTTL).UnixMilli(), claim, want, now, now)
	if err != nil {
		return nil, err
	}
	claimed, _ := res.RowsAffected()
	var p PendingUpload
	var got string
	var exp int64
	var used, until sql.NullInt64
	err = s.db.QueryRowContext(ctx, `SELECT token_hash, agent_id, sha256, size, content_type, kind, name, expires_at, used_at, claimed_until FROM inbox_upload_tickets WHERE token_hash=?`, want).
		Scan(&got, &p.AgentID, &p.SHA256, &p.Size, &p.ContentType, &p.Kind, &p.Name, &exp, &used, &until)
	if errors.Is(err, sql.ErrNoRows) || (err == nil && subtle.ConstantTimeCompare([]byte(got), []byte(want)) != 1) {
		return nil, ErrUploadUnknown
	}
	if err != nil {
		return nil, err
	}
	if claimed != 1 {
		if used.Valid || exp <= now {
			return nil, ErrUploadGone
		}
		return nil, ErrUploadBusy
	}
	p.hash, p.claim = want, claim
	return &p, nil
}

// ReleaseUpload drops a claim after a failed PUT, so the agent can retry
// with the same ticket until it expires.
func (s *Service) ReleaseUpload(p *PendingUpload) {
	if p == nil || p.hash == "" {
		return
	}
	// Only this caller's claim: a newer claim (taken after this one
	// expired) is left alone.
	if _, err := s.db.ExecContext(context.Background(), `UPDATE inbox_upload_tickets SET claimed_until=NULL, claim_token=NULL WHERE token_hash=? AND claim_token=? AND used_at IS NULL`, p.hash, p.claim); err != nil {
		log.Printf("inbox: release upload claim: %v", err)
	}
}

// headWriter keeps the first len(buf) bytes written to it.
type headWriter struct {
	buf []byte
	n   int
}

func (h *headWriter) Write(p []byte) (int, error) {
	if h.n < len(h.buf) {
		h.n += copy(h.buf[h.n:], p)
	}
	return len(p), nil
}

// CompleteUpload streams the body of a claimed PUT to a temporary file
// while hashing it, checks it against the ticket and stores it. The blob
// row, the owner row and the ticket's used_at are written in one
// transaction. On any failure the claim is released. contentType is the
// request's Content-Type header ("" is accepted).
func (s *Service) CompleteUpload(ctx context.Context, p *PendingUpload, contentType string, body io.Reader) (res *UploadResult, err error) {
	if p == nil || p.hash == "" {
		return nil, ErrUploadUnknown
	}
	blobs, ok := s.uploadStore()
	if !ok {
		return nil, ErrUploadUnavailable
	}
	defer func() {
		if err != nil {
			s.ReleaseUpload(p)
		}
	}()
	if ct := normalizeContentType(contentType); contentType != "" && ct != p.ContentType {
		return nil, uploadErr("Content-Type %s doesn't match the ticket (%s)", contentType, p.ContentType)
	}
	tmp, err := blobs.CreateUploadTemp()
	if err != nil {
		return nil, err
	}
	tmpPath := tmp.Name()
	adopted := false
	defer func() {
		if !adopted {
			_ = os.Remove(tmpPath)
		}
	}()
	h := sha256.New()
	head := &headWriter{buf: make([]byte, uploadSniffBytes)}
	n, err := io.Copy(io.MultiWriter(tmp, h, head), io.LimitReader(body, p.Size+1))
	if cerr := tmp.Close(); err == nil && cerr != nil {
		err = cerr
	}
	if err != nil {
		var mbe *http.MaxBytesError
		if errors.As(err, &mbe) {
			return nil, uploadErr("the body is larger than the declared %d bytes", p.Size)
		}
		return nil, uploadErr("couldn't read the body: %v", err)
	}
	if n != p.Size {
		return nil, uploadErr("the body is %d bytes; the ticket says %d", n, p.Size)
	}
	if got := hex.EncodeToString(h.Sum(nil)); got != p.SHA256 {
		return nil, uploadErr("sha256 mismatch: the body hashes to %s, the ticket says %s", got, p.SHA256)
	}
	stored, err := sniffUpload(p.Kind, p.ContentType, head.buf[:head.n])
	if err != nil {
		return nil, err
	}

	// A store that copies (Postgres) gets the bytes before uploadMu, so
	// other uploads don't wait for a copy of up to 50 MB. Adopt, under the
	// lock, then finds them stored (or stores them again if the sweep
	// removed them in between). Local does nothing here and renames the
	// file in Adopt.
	prepared, err := blobs.PrepareUpload(ctx, tmpPath, p.SHA256)
	if err != nil {
		return nil, err
	}
	s.uploadMu.Lock()
	defer s.uploadMu.Unlock()
	created, err := blobs.Adopt(ctx, tmpPath, p.SHA256)
	adopted = true // Adopt consumed (or removed) the temporary file
	created = created || prepared
	if err == nil {
		err = s.recordUpload(ctx, p, stored)
	}
	if err != nil {
		if created {
			// Never leave bytes without their row; a blob some other
			// writer recorded meanwhile keeps its bytes.
			var one int
			if s.db.QueryRowContext(context.Background(), `SELECT 1 FROM inbox_blobs WHERE sha256=?`, p.SHA256).Scan(&one) != nil {
				if derr := blobs.DiscardBlob(context.Background(), p.SHA256); derr != nil {
					log.Printf("inbox: discard upload blob %s: %v", p.SHA256[:12], derr)
				}
			}
		}
		return nil, err
	}
	return &UploadResult{Ref: UploadRefPrefix + p.SHA256, SHA256: p.SHA256, Size: p.Size, ContentType: stored, AgentID: p.AgentID, Name: p.Name}, nil
}

// recordUpload writes the blob row, the owner row and the ticket's used_at
// as one unit, so a stored upload always has an owner and a used ticket.
func (s *Service) recordUpload(ctx context.Context, p *PendingUpload, stored string) error {
	if s.uploadRecordHook != nil {
		if err := s.uploadRecordHook(); err != nil {
			return err
		}
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	now := s.now().UnixMilli()
	// The claim was taken before expiry; a body that finishes after the
	// ticket's expiry still counts (Sweep never deletes a claimed ticket).
	res, err := tx.ExecContext(ctx, `UPDATE inbox_upload_tickets SET used_at=?, claimed_until=NULL, claim_token=NULL WHERE token_hash=? AND used_at IS NULL AND claim_token=?`, now, p.hash, p.claim)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n != 1 {
		return ErrUploadGone
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO inbox_blobs(sha256, content_type, size, source_url, created_at) VALUES (?,?,?,?,?) ON CONFLICT DO NOTHING`,
		p.SHA256, stored, p.Size, UploadRefPrefix+p.SHA256, now); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO inbox_uploads(sha256, agent_id, name, content_type, kind, size, created_at) VALUES (?,?,?,?,?,?,?)
		ON CONFLICT(sha256, agent_id) DO UPDATE SET name=excluded.name, content_type=excluded.content_type, kind=excluded.kind, size=excluded.size, created_at=excluded.created_at`,
		p.SHA256, p.AgentID, p.Name, stored, p.Kind, p.Size, now); err != nil {
		return err
	}
	return tx.Commit()
}

// sniffUpload checks the first bytes against the declared type and returns
// the content type to store. Images must really be PNG, JPEG, WebP or GIF
// and match the declared type; videos must look like MP4, WebM or
// QuickTime; files may be anything except HTML, SVG or XHTML, and a file
// declared as an image or video must really be one (those render inline).
func sniffUpload(kind, declared string, head []byte) (string, error) {
	sniffed := normalizeContentType(http.DetectContentType(head))
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
		mp4OrWebM := sniffed == "video/mp4" || sniffed == "video/webm"
		switch {
		case mp4OrWebM && sniffed == declared:
			return declared, nil
		case declared == "video/quicktime" && (sniffed == "video/mp4" || looksQuickTime(head)):
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
	if looksLikeMarkup(sniffed, head) {
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

func looksLikeMarkup(sniffed string, head []byte) bool {
	if sniffed == "text/html" || sniffed == "image/svg+xml" || sniffed == "application/xhtml+xml" {
		return true
	}
	if !strings.HasPrefix(sniffed, "text/") {
		return false
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
// type it is cited as. It touches the row first, so a concurrent sweep
// skips it (uploadTouchGrace) instead of deleting it under the citation.
func (s *Service) uploadFor(ctx context.Context, agentID, ref, kind string) (sha, ct string, size int64, err error) {
	sha, ok := ParseUploadRef(ref)
	if !ok {
		return "", "", 0, fmt.Errorf("use \"upload:<sha256>\" exactly as inbox.upload returned it")
	}
	notYours := fmt.Errorf("no finished upload with this ref belongs to you; call inbox.upload, PUT the file, then cite the ref")
	res, err := s.db.ExecContext(ctx, `UPDATE inbox_uploads SET touched_at=? WHERE sha256=? AND agent_id=?`, s.now().UnixMilli(), sha, agentID)
	if err != nil {
		return "", "", 0, err
	}
	if n, _ := res.RowsAffected(); n != 1 {
		return "", "", 0, notYours
	}
	err = s.db.QueryRowContext(ctx, `SELECT content_type, size FROM inbox_uploads WHERE sha256=? AND agent_id=?`, sha, agentID).Scan(&ct, &size)
	if errors.Is(err, sql.ErrNoRows) {
		return "", "", 0, notYours
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
	// Markdown images in the message and markdown attachments: only the
	// agent's own image uploads show inline (InlineImages).
	r.InlineImages = nil
	seen := map[string]bool{}
	for _, src := range r.inlineImageSources() {
		if seen[src.Ref] {
			continue
		}
		seen[src.Ref] = true
		if len(r.InlineImages) == MaxInlineImages {
			*probs = append(*probs, Problem{src.Path, fmt.Sprintf("more than %d inline images; attach the rest", MaxInlineImages)})
			break
		}
		sha, ct, _, err := s.uploadFor(ctx, agentID, src.Ref, "image")
		if err != nil {
			*probs = append(*probs, Problem{src.Path, "image " + src.Ref + ": " + err.Error()})
			continue
		}
		r.InlineImages = append(r.InlineImages, InlineImage{Ref: src.Ref, Blob: sha, ContentType: ct})
	}
}

// markUploadsUsedTx records, inside the transaction that stores r, that r
// cites its uploads, so Sweep keeps them.
func (s *Service) markUploadsUsedTx(ctx context.Context, tx *sql.Tx, r *Request) error {
	now := s.now().UnixMilli()
	var refs []string
	for _, a := range r.Attachments {
		refs = append(refs, a.Upload, a.PosterUpload)
	}
	for _, img := range r.InlineImages {
		refs = append(refs, img.Ref)
	}
	for _, ref := range refs {
		if sha, ok := ParseUploadRef(ref); ok {
			if _, err := tx.ExecContext(ctx, `UPDATE inbox_uploads SET used_at=COALESCE(used_at, ?), touched_at=? WHERE sha256=? AND agent_id=?`, now, now, sha, r.AgentID); err != nil {
				return err
			}
		}
	}
	return nil
}

// sweepUploads purges expired tickets, uploads that no request cited
// within UploadRetention, and upload-sourced blobs left without an owner.
// Each deletion re-checks citations inside one transaction.
func (s *Service) sweepUploads(ctx context.Context) {
	now := s.now()
	if _, err := s.db.ExecContext(ctx, `DELETE FROM inbox_upload_tickets WHERE expires_at <= ? AND (claimed_until IS NULL OR claimed_until <= ?)`, now.UnixMilli(), now.UnixMilli()); err != nil {
		log.Printf("inbox: purge upload tickets: %v", err)
	}
	cutoff := now.Add(-UploadRetention).UnixMilli()
	touchCutoff := now.Add(-uploadTouchGrace).UnixMilli()
	type up struct{ sha, agent string }
	var stale []up
	rows, err := s.db.QueryContext(ctx, `SELECT sha256, agent_id FROM inbox_uploads WHERE used_at IS NULL AND created_at < ? AND COALESCE(touched_at, 0) < ? LIMIT 500`, cutoff, touchCutoff)
	if err != nil {
		log.Printf("inbox: sweep uploads: %v", err)
		return
	}
	for rows.Next() {
		var u up
		if rows.Scan(&u.sha, &u.agent) == nil {
			stale = append(stale, u)
		}
	}
	rows.Close()
	// Blobs an upload wrote whose owner rows are all gone (e.g. a crash
	// between steps in an older build).
	rows, err = s.db.QueryContext(ctx, `SELECT b.sha256 FROM inbox_blobs b WHERE b.source_url = 'upload:' || b.sha256 AND b.created_at < ?
		AND NOT EXISTS (SELECT 1 FROM inbox_uploads u WHERE u.sha256 = b.sha256) LIMIT 500`, cutoff)
	if err == nil {
		for rows.Next() {
			var u up
			if rows.Scan(&u.sha) == nil {
				stale = append(stale, u)
			}
		}
		rows.Close()
	}
	for _, u := range stale {
		s.sweepUploadLocked(ctx, u.sha, u.agent, cutoff, touchCutoff)
	}
	if b, ok := s.uploadStore(); ok {
		if n := b.CleanTemp(staleTempAge); n > 0 {
			log.Printf("inbox: removed %d stale temporary blob file(s)", n)
		}
	}
}

// sweepUploadLocked runs one sweep deletion and its byte removal under
// uploadMu, so no upload of the same sha can adopt a file in between.
func (s *Service) sweepUploadLocked(ctx context.Context, sha, agent string, cutoff, touchCutoff int64) {
	s.uploadMu.Lock()
	defer s.uploadMu.Unlock()
	dropBytes, err := s.sweepOneUpload(ctx, sha, agent, cutoff, touchCutoff)
	if err != nil {
		log.Printf("inbox: sweep upload %s: %v", sha[:12], err)
		return
	}
	if dropBytes {
		if b, ok := s.uploadStore(); ok {
			if err := b.DiscardBlob(ctx, sha); err != nil {
				log.Printf("inbox: sweep upload %s: delete bytes: %v", sha[:12], err)
			}
		}
	}
}

// sweepBeforeDelete, when set (tests), runs inside sweepOneUpload's
// transaction for blob sha after a check and before the delete it guards:
// stage "upload" before the upload row's delete, "blob" before the blob
// row's.
var sweepBeforeDelete func(ctx context.Context, tx *sql.Tx, stage, sha string)

// sweepOneUpload deletes one stale upload row (agent "" = an ownerless
// blob) and, when nothing else holds or cites it, its blob row. It reports
// whether the caller should delete the bytes. All checks run in the same
// transaction as the deletes, and each DELETE repeats the conditions it
// was checked on, so a row that changed in between (cited, touched,
// re-uploaded, or held by a new upload) is kept.
func (s *Service) sweepOneUpload(ctx context.Context, sha, agent string, cutoff, touchCutoff int64) (bool, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return false, err
	}
	defer tx.Rollback()
	if agent != "" {
		var one int
		err := tx.QueryRowContext(ctx, `SELECT 1 FROM inbox_uploads WHERE sha256=? AND agent_id=? AND used_at IS NULL AND created_at < ? AND COALESCE(touched_at, 0) < ?`, sha, agent, cutoff, touchCutoff).Scan(&one)
		if errors.Is(err, sql.ErrNoRows) {
			return false, nil // cited, touched or re-uploaded since the scan
		}
		if err != nil {
			return false, err
		}
		cited, err := blobCitedTx(ctx, tx, s.db.Dialect, agent, sha)
		if err != nil {
			return false, err
		}
		if cited {
			if _, err := tx.ExecContext(ctx, `UPDATE inbox_uploads SET used_at=? WHERE sha256=? AND agent_id=? AND used_at IS NULL`, s.now().UnixMilli(), sha, agent); err != nil {
				return false, err
			}
			return false, tx.Commit()
		}
		if sweepBeforeDelete != nil {
			sweepBeforeDelete(ctx, tx, "upload", sha)
		}
		res, err := tx.ExecContext(ctx, `DELETE FROM inbox_uploads WHERE sha256=? AND agent_id=? AND used_at IS NULL AND created_at < ? AND COALESCE(touched_at, 0) < ?`, sha, agent, cutoff, touchCutoff)
		if err != nil {
			return false, err
		}
		n, err := res.RowsAffected()
		if err != nil {
			return false, err
		}
		if n != 1 {
			return false, nil // changed since the check: keep it
		}
	}
	drop := true
	var one int
	if tx.QueryRowContext(ctx, `SELECT 1 FROM inbox_uploads WHERE sha256=? LIMIT 1`, sha).Scan(&one) == nil {
		drop = false // another agent's upload still holds it
	}
	if drop {
		cited, err := blobCitedTx(ctx, tx, s.db.Dialect, "", sha)
		if err != nil {
			return false, err
		}
		drop = !cited
	}
	var source string
	if drop && (tx.QueryRowContext(ctx, `SELECT source_url FROM inbox_blobs WHERE sha256=?`, sha).Scan(&source) != nil || source != UploadRefPrefix+sha) {
		drop = false // a URL snapshot, a voice note, or already gone
	}
	if drop {
		if sweepBeforeDelete != nil {
			sweepBeforeDelete(ctx, tx, "blob", sha)
		}
		res, err := tx.ExecContext(ctx, `DELETE FROM inbox_blobs WHERE sha256=? AND source_url=? AND NOT EXISTS (SELECT 1 FROM inbox_uploads u WHERE u.sha256=?)`,
			sha, UploadRefPrefix+sha, sha)
		if err != nil {
			return false, err
		}
		n, err := res.RowsAffected()
		if err != nil {
			return false, err
		}
		drop = n == 1
	}
	if err := tx.Commit(); err != nil {
		return false, err
	}
	return drop, nil
}

// blobCitedTx reports whether any request (of agentID, or of anyone when
// agentID is "") references sha as a blob, poster blob or voice blob: an
// object member named blob or poster_blob, at any depth of the request
// doc, whose value is the string sha. An error means "don't know"; the
// sweep then deletes nothing.
func blobCitedTx(ctx context.Context, tx *sql.Tx, d store.Dialect, agentID, sha string) (bool, error) {
	var found int
	var err error
	switch {
	case d.IsPostgres():
		// The same match as the json_tree queries below (and as
		// BlobBelongsToAgent): 'strict $.**' yields every item of the doc
		// once, and jsonb equality with a JSON string matches only a
		// string.
		const cited = `EXISTS (SELECT 1 FROM jsonb_path_query(CAST(replace(r.doc, '\u0000', '\ufffd') AS jsonb), 'strict $.**') AS o(v)
			WHERE jsonb_typeof(o.v) = 'object'
			AND ((o.v -> 'blob') = to_jsonb(CAST(? AS TEXT)) OR (o.v -> 'poster_blob') = to_jsonb(CAST(? AS TEXT))))`
		if agentID != "" {
			err = tx.QueryRowContext(ctx, `SELECT 1 FROM inbox_requests r WHERE r.agent_id=? AND `+cited+` LIMIT 1`, agentID, sha, sha).Scan(&found)
		} else {
			err = tx.QueryRowContext(ctx, `SELECT 1 FROM inbox_requests r WHERE `+cited+` LIMIT 1`, sha, sha).Scan(&found)
		}
	case agentID != "":
		err = tx.QueryRowContext(ctx, `SELECT 1 FROM inbox_requests r, json_tree(r.doc) j WHERE r.agent_id=? AND j.key IN ('blob','poster_blob') AND j.type='text' AND j.value=? LIMIT 1`, agentID, sha).Scan(&found)
	default:
		err = tx.QueryRowContext(ctx, `SELECT 1 FROM inbox_requests r, json_tree(r.doc) j WHERE j.key IN ('blob','poster_blob') AND j.type='text' AND j.value=? LIMIT 1`, sha).Scan(&found)
	}
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return found == 1, nil
}
