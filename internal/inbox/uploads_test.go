package inbox

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/tusharbhardwaj/toolyard/internal/store"
)

type uploadEnv struct {
	*testEnv
	db    *store.DB
	snaps *Snapshotter
}

// newUploadEnv is newEnv with a blob store (and, with fetch, the URL
// fetcher wired to the same store, allowed to reach httptest servers).
func newUploadEnv(t *testing.T, fetch bool) *uploadEnv {
	t.Helper()
	db, err := store.Open(filepath.Join(t.TempDir(), "uploads.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	snaps, err := NewSnapshotter(db, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	e := &testEnv{now: time.Date(2026, 10, 13, 9, 0, 0, 0, time.UTC)}
	opts := Options{
		DB:      db,
		Catalog: testCatalog,
		Blobs:   snaps,
		Now:     func() time.Time { e.mu.Lock(); defer e.mu.Unlock(); return e.now },
	}
	if fetch {
		snaps.allowPrivate = true
		snaps.client = snaps.newClient()
		opts.Fetcher = snaps
		opts.SnapshotEnabled = func() bool { return true }
		opts.AllowPrivateMedia = true
	}
	svc, err := New(context.Background(), opts)
	if err != nil {
		t.Fatal(err)
	}
	e.svc = svc
	t.Cleanup(svc.Flush)
	return &uploadEnv{testEnv: e, db: db, snaps: snaps}
}

// testPNG returns bytes that sniff as image/png, distinct per seed.
func testPNG(seed int) []byte {
	b := []byte("\x89PNG\r\n\x1a\n\x00\x00\x00\rIHDR")
	return append(b, []byte(fmt.Sprintf("fake image body %d %s", seed, strings.Repeat("z", 64)))...)
}

func hexSum(b []byte) string {
	s := sha256.Sum256(b)
	return hex.EncodeToString(s[:])
}

func pngSpec(data []byte) UploadSpec {
	return UploadSpec{Name: "shot.png", ContentType: "image/png", Size: int64(len(data)), SHA256: hexSum(data), Kind: "image"}
}

// upload runs the whole flow and returns the ref.
func (u *uploadEnv) upload(t *testing.T, agent string, spec UploadSpec, data []byte) string {
	t.Helper()
	ctx := context.Background()
	tk, err := u.svc.CreateUpload(ctx, agent, spec)
	if err != nil {
		t.Fatalf("create upload: %v", err)
	}
	if tk.AlreadyUploaded {
		return tk.Ref
	}
	p, err := u.svc.ClaimUpload(ctx, tk.Ticket)
	if err != nil {
		t.Fatalf("open upload: %v", err)
	}
	res, err := u.svc.CompleteUpload(ctx, p, spec.ContentType, bytes.NewReader(data))
	if err != nil {
		t.Fatalf("complete upload: %v", err)
	}
	if res.Ref != tk.Ref || res.SHA256 != spec.SHA256 {
		t.Fatalf("result %+v doesn't match ticket %+v", res, tk)
	}
	return res.Ref
}

func isUploadErr(err error) bool {
	var ue *UploadError
	return errors.As(err, &ue)
}

func TestUploadTicketIsSingleUse(t *testing.T) {
	u := newUploadEnv(t, false)
	ctx := context.Background()
	data := testPNG(1)
	tk, err := u.svc.CreateUpload(ctx, "ag_1", pngSpec(data))
	if err != nil || !strings.HasPrefix(tk.Ticket, uploadTicketPrefix) || tk.Ref != UploadRefPrefix+hexSum(data) {
		t.Fatalf("create: %+v %v", tk, err)
	}
	var stored int
	_ = u.db.QueryRow(`SELECT COUNT(*) FROM inbox_upload_tickets WHERE token_hash=?`, tk.Ticket).Scan(&stored)
	if stored != 0 {
		t.Fatal("the raw ticket must never be stored")
	}
	p, err := u.svc.ClaimUpload(ctx, tk.Ticket)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := u.svc.ClaimUpload(ctx, tk.Ticket); !errors.Is(err, ErrUploadBusy) { // a concurrent second PUT
		t.Fatalf("second claim while the first runs: %v, want ErrUploadBusy", err)
	}
	res, err := u.svc.CompleteUpload(ctx, p, "image/png", bytes.NewReader(data))
	if err != nil || res.ContentType != "image/png" || res.Size != int64(len(data)) || res.AgentID != "ag_1" {
		t.Fatalf("complete: %+v %v", res, err)
	}
	if _, err := u.svc.ClaimUpload(ctx, tk.Ticket); !errors.Is(err, ErrUploadGone) {
		t.Fatalf("reclaim a used ticket: %v, want ErrUploadGone", err)
	}
	if _, err := u.svc.CompleteUpload(ctx, p, "image/png", bytes.NewReader(data)); !errors.Is(err, ErrUploadGone) {
		t.Fatalf("completing a used claim again: %v, want ErrUploadGone", err)
	}
	if !u.snaps.Exists(ctx, hexSum(data)) {
		t.Fatal("blob not stored")
	}
	if _, err := u.svc.ClaimUpload(ctx, "tyu_"+strings.Repeat("A", 43)); !errors.Is(err, ErrUploadUnknown) {
		t.Fatalf("unknown ticket: %v", err)
	}
	if _, err := u.svc.ClaimUpload(ctx, "nope"); !errors.Is(err, ErrUploadUnknown) {
		t.Fatalf("malformed ticket: %v", err)
	}
}

func TestUploadTicketExpires(t *testing.T) {
	u := newUploadEnv(t, false)
	ctx := context.Background()
	data := testPNG(2)
	tk, err := u.svc.CreateUpload(ctx, "ag_1", pngSpec(data))
	if err != nil {
		t.Fatal(err)
	}
	if tk.ExpiresAt != u.now.Add(UploadTicketTTL).UnixMilli() {
		t.Fatalf("expiry %d, want 15 minutes", tk.ExpiresAt)
	}
	late, err := u.svc.CreateUpload(ctx, "ag_1", pngSpec(testPNG(20)))
	if err != nil {
		t.Fatal(err)
	}
	u.advance(UploadTicketTTL - time.Minute)
	p, err := u.svc.ClaimUpload(ctx, tk.Ticket)
	if err != nil {
		t.Fatal(err)
	}
	u.advance(2 * time.Minute) // past the ticket's expiry, inside the claim
	if _, err := u.svc.ClaimUpload(ctx, late.Ticket); !errors.Is(err, ErrUploadGone) {
		t.Fatalf("expired ticket: %v, want ErrUploadGone", err)
	}
	// The sweep must not delete a ticket whose PUT is still running ...
	u.svc.Sweep(ctx)
	// ... and a body claimed before expiry still counts when it finishes.
	if _, err := u.svc.CompleteUpload(ctx, p, "image/png", bytes.NewReader(data)); err != nil {
		t.Fatalf("complete a claim taken before expiry: %v", err)
	}
}

// A store failure after the claim releases it: no burned ticket, no
// orphan file, and the same ticket works on retry.
func TestUploadClaimReleasedOnStoreFailure(t *testing.T) {
	u := newUploadEnv(t, false)
	ctx := context.Background()
	data := testPNG(21)
	tk, err := u.svc.CreateUpload(ctx, "ag_1", pngSpec(data))
	if err != nil {
		t.Fatal(err)
	}
	p, err := u.svc.ClaimUpload(ctx, tk.Ticket)
	if err != nil {
		t.Fatal(err)
	}
	u.svc.uploadRecordHook = func() error { return errors.New("disk full") }
	if _, err := u.svc.CompleteUpload(ctx, p, "image/png", bytes.NewReader(data)); err == nil {
		t.Fatal("the store failure should surface")
	}
	if u.snaps.Exists(ctx, hexSum(data)) {
		t.Fatal("a failed store must not leave a file without its row")
	}
	var owners int
	_ = u.db.QueryRow(`SELECT COUNT(*) FROM inbox_uploads`).Scan(&owners)
	if owners != 0 {
		t.Fatal("no owner row after a failed store")
	}
	u.svc.uploadRecordHook = nil
	p, err = u.svc.ClaimUpload(ctx, tk.Ticket)
	if err != nil {
		t.Fatalf("the claim should be released after a failure: %v", err)
	}
	if _, err := u.svc.CompleteUpload(ctx, p, "image/png", bytes.NewReader(data)); err != nil {
		t.Fatalf("retry with the same ticket: %v", err)
	}
	// A bad body releases the claim too.
	tk2, _ := u.svc.CreateUpload(ctx, "ag_1", pngSpec(testPNG(22)))
	p2, _ := u.svc.ClaimUpload(ctx, tk2.Ticket)
	if _, err := u.svc.CompleteUpload(ctx, p2, "image/png", bytes.NewReader([]byte("short"))); !isUploadErr(err) {
		t.Fatalf("short body: %v", err)
	}
	if _, err := u.svc.ClaimUpload(ctx, tk2.Ticket); err != nil {
		t.Fatalf("claim after a bad body: %v", err)
	}
}

func TestUploadSlotsAreBounded(t *testing.T) {
	u := newUploadEnv(t, false)
	var releases []func()
	for i := 0; i < MaxConcurrentUploads; i++ {
		rel, ok := u.svc.AcquireUploadSlot()
		if !ok {
			t.Fatalf("slot %d refused", i)
		}
		releases = append(releases, rel)
	}
	if _, ok := u.svc.AcquireUploadSlot(); ok {
		t.Fatal("a slot beyond the cap was granted")
	}
	releases[0]()
	releases[0]() // releasing twice frees one slot only
	if _, ok := u.svc.AcquireUploadSlot(); !ok {
		t.Fatal("a released slot should be reusable")
	}
	if _, ok := u.svc.AcquireUploadSlot(); ok {
		t.Fatal("a double release must not free two slots")
	}
}

func TestUploadRejectsSizeHashAndTypeMismatches(t *testing.T) {
	u := newUploadEnv(t, false)
	ctx := context.Background()
	data := testPNG(3)
	try := func(spec UploadSpec, ct string, body []byte) error {
		t.Helper()
		tk, err := u.svc.CreateUpload(ctx, "ag_1", spec)
		if err != nil {
			t.Fatalf("create: %v", err)
		}
		p, err := u.svc.ClaimUpload(ctx, tk.Ticket)
		if err != nil {
			t.Fatal(err)
		}
		_, err = u.svc.CompleteUpload(ctx, p, ct, bytes.NewReader(body))
		return err
	}
	if err := try(pngSpec(data), "image/png", data[:len(data)-1]); !isUploadErr(err) || !strings.Contains(err.Error(), "bytes") {
		t.Errorf("short body: %v", err)
	}
	if err := try(pngSpec(data), "image/png", append(append([]byte{}, data...), 'x')); !isUploadErr(err) {
		t.Errorf("long body: %v", err)
	}
	other := testPNG(4)
	other = other[:len(data)]
	spec := pngSpec(data)
	if err := try(spec, "image/png", other); !isUploadErr(err) || !strings.Contains(err.Error(), "sha256 mismatch") {
		t.Errorf("hash mismatch: %v", err)
	}
	if err := try(pngSpec(data), "image/jpeg", data); !isUploadErr(err) || !strings.Contains(err.Error(), "Content-Type") {
		t.Errorf("header type mismatch: %v", err)
	}

	// HTML declared as a PNG, with a matching hash and size.
	html := []byte("<html><body><script>alert(document.cookie)</script></body></html>")
	if err := try(UploadSpec{Name: "x.png", ContentType: "image/png", Size: int64(len(html)), SHA256: hexSum(html)}, "image/png", html); !isUploadErr(err) || !strings.Contains(err.Error(), "not an image") {
		t.Errorf("html disguised as png: %v", err)
	}
	// A real PNG declared as JPEG.
	if err := try(UploadSpec{Name: "x.jpg", ContentType: "image/jpeg", Size: int64(len(data)), SHA256: hexSum(data)}, "", data); !isUploadErr(err) || !strings.Contains(err.Error(), "image/png") {
		t.Errorf("png declared as jpeg: %v", err)
	}
	// HTML or SVG smuggled in as a plain file.
	if err := try(UploadSpec{Name: "notes.txt", ContentType: "text/plain", Size: int64(len(html)), SHA256: hexSum(html), Kind: "file"}, "text/plain", html); !isUploadErr(err) {
		t.Errorf("html as text file: %v", err)
	}
	svg := []byte(`<?xml version="1.0"?><svg xmlns="http://www.w3.org/2000/svg"><script>alert(1)</script></svg>`)
	if err := try(UploadSpec{Name: "d.txt", ContentType: "text/plain", Size: int64(len(svg)), SHA256: hexSum(svg), Kind: "file"}, "text/plain", svg); !isUploadErr(err) {
		t.Errorf("svg as text file: %v", err)
	}
	// A file declared as an image must really be one (images render inline).
	if err := try(UploadSpec{Name: "x.png", ContentType: "image/png", Size: int64(len(html)), SHA256: hexSum(html), Kind: "file"}, "image/png", html); !isUploadErr(err) {
		t.Errorf("html file declared as png: %v", err)
	}
	// A CSV is a fine file.
	csv := []byte("sku,units\nA-1,12\nB-2,7\n")
	if err := try(UploadSpec{Name: "units.csv", ContentType: "text/csv", Size: int64(len(csv)), SHA256: hexSum(csv), Kind: "file"}, "text/csv; charset=utf-8", csv); err != nil {
		t.Errorf("csv file: %v", err)
	}
	var n int
	_ = u.db.QueryRow(`SELECT COUNT(*) FROM inbox_uploads WHERE agent_id='ag_1'`).Scan(&n)
	if n != 1 {
		t.Fatalf("only the csv should be stored, got %d uploads", n)
	}
}

func TestCreateUploadValidates(t *testing.T) {
	u := newUploadEnv(t, false)
	ctx := context.Background()
	data := testPNG(5)
	cases := map[string]UploadSpec{
		"bad sha":      {Name: "a.png", ContentType: "image/png", Size: 10, SHA256: "ABC"},
		"upper sha":    {Name: "a.png", ContentType: "image/png", Size: 10, SHA256: strings.ToUpper(hexSum(data))},
		"zero size":    {Name: "a.png", ContentType: "image/png", Size: 0, SHA256: hexSum(data)},
		"too big":      {Name: "a.png", ContentType: "image/png", Size: MaxImageBytes + 1, SHA256: hexSum(data)},
		"svg image":    {Name: "a.svg", ContentType: "image/svg+xml", Size: 10, SHA256: hexSum(data)},
		"html file":    {Name: "a.html", ContentType: "text/html", Size: 10, SHA256: hexSum(data), Kind: "file"},
		"bad kind":     {Name: "a.png", ContentType: "image/png", Size: 10, SHA256: hexSum(data), Kind: "audio"},
		"no name":      {Name: " ", ContentType: "image/png", Size: 10, SHA256: hexSum(data)},
		"no type":      {Name: "a.png", ContentType: "", Size: 10, SHA256: hexSum(data)},
		"video as img": {Name: "a.mp4", ContentType: "video/mp4", Size: 10, SHA256: hexSum(data)},
		"quote type":   {Name: "a.png", ContentType: "image/png'; rm -rf ~; '", Size: 10, SHA256: hexSum(data)},
		"subst type":   {Name: "a.png", ContentType: "image/png$(id)", Size: 10, SHA256: hexSum(data)},
		"tick type":    {Name: "a.png", ContentType: "image/`id`png", Size: 10, SHA256: hexSum(data)},
		"param quote":  {Name: "a.png", ContentType: "image/png; x=\"a b\"", Size: 10, SHA256: hexSum(data)},
	}
	for name, spec := range cases {
		if _, err := u.svc.CreateUpload(ctx, "ag_1", spec); !isUploadErr(err) {
			t.Errorf("%s: %v, want an UploadError", name, err)
		}
	}
	if _, err := u.svc.CreateUpload(ctx, "ag_1", UploadSpec{Name: "rec.mp4", ContentType: "video/mp4", Size: MaxImageBytes + 1, SHA256: hexSum(data), Kind: "video"}); err != nil {
		t.Errorf("an 11 MB video is fine: %v", err)
	}
	if tk, err := u.svc.CreateUpload(ctx, "ag_1", UploadSpec{Name: "../../etc/shot.png", ContentType: "IMAGE/PNG; q=1", Size: 10, SHA256: hexSum(data)}); err != nil || tk.ContentType != "image/png" {
		t.Errorf("name and type normalization: %+v %v", tk, err)
	}
	var name string
	_ = u.db.QueryRow(`SELECT name FROM inbox_upload_tickets ORDER BY created_at DESC, rowid DESC LIMIT 1`).Scan(&name)
	if name != "shot.png" {
		t.Errorf("stored name %q, want the base name", name)
	}
	noBlobs, err := New(ctx, Options{DB: u.db, Catalog: testCatalog})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := noBlobs.CreateUpload(ctx, "ag_1", pngSpec(data)); !errors.Is(err, ErrUploadUnavailable) {
		t.Errorf("no blob store: %v", err)
	}
}

func TestUploadTicketCapPerAgent(t *testing.T) {
	u := newUploadEnv(t, false)
	ctx := context.Background()
	for i := 0; i < MaxOpenUploadTickets; i++ {
		if _, err := u.svc.CreateUpload(ctx, "ag_1", pngSpec(testPNG(100+i))); err != nil {
			t.Fatalf("ticket %d: %v", i, err)
		}
	}
	if _, err := u.svc.CreateUpload(ctx, "ag_1", pngSpec(testPNG(999))); !errors.Is(err, ErrUploadTooMany) {
		t.Fatalf("ticket %d: %v, want ErrUploadTooMany", MaxOpenUploadTickets+1, err)
	}
	if _, err := u.svc.CreateUpload(ctx, "ag_2", pngSpec(testPNG(999))); err != nil {
		t.Fatalf("another agent has its own cap: %v", err)
	}
	u.advance(UploadTicketTTL + time.Second)
	if _, err := u.svc.CreateUpload(ctx, "ag_1", pngSpec(testPNG(999))); err != nil {
		t.Fatalf("expired tickets don't count: %v", err)
	}
}

func TestUploadAlreadyUploaded(t *testing.T) {
	u := newUploadEnv(t, false)
	ctx := context.Background()
	data := testPNG(6)
	ref := u.upload(t, "ag_1", pngSpec(data), data)
	tk, err := u.svc.CreateUpload(ctx, "ag_1", pngSpec(data))
	if err != nil || !tk.AlreadyUploaded || tk.Ticket != "" || tk.Ref != ref {
		t.Fatalf("same agent, same bytes: %+v %v", tk, err)
	}
	tk, err = u.svc.CreateUpload(ctx, "ag_2", pngSpec(data))
	if err != nil || tk.AlreadyUploaded || tk.Ticket == "" {
		t.Fatalf("another agent must upload its own copy: %+v %v", tk, err)
	}
}

func uploadPost(att ...Attachment) *Submission {
	return &Submission{Kind: KindUpdate, Title: "Checkout fix is live", Summary: "The fix shipped; screenshot attached.",
		Message: "I shipped the checkout fix. The screenshot shows the page after the deploy.", Audio: Audio{Script: "The checkout fix is live."},
		Urgency: UrgencyFYI, Attachments: att}
}

func TestUploadRefBelongsToItsAgent(t *testing.T) {
	u := newUploadEnv(t, false)
	ctx := context.Background()
	data := testPNG(7)
	ref := u.upload(t, "ag_1", pngSpec(data), data)

	res, err := u.svc.Submit(ctx, "ag_2", uploadPost(Attachment{Type: "image", Upload: ref, Alt: "checkout"}))
	if err != nil {
		t.Fatal(err)
	}
	if res.OK || len(res.Problems) != 1 || res.Problems[0].Path != "attachments[0].upload" {
		t.Fatalf("another agent's ref must be refused: %+v", res)
	}
	if u.svc.BlobBelongsToAgent(ctx, "ag_2", hexSum(data)) {
		t.Fatal("ag_2 must not be able to download ag_1's upload")
	}
	if !u.svc.BlobBelongsToAgent(ctx, "ag_1", hexSum(data)) {
		t.Fatal("ag_1 owns its upload")
	}

	res, err = u.svc.Submit(ctx, "ag_1", uploadPost(Attachment{Type: "image", Upload: ref, Alt: "checkout"}))
	if err != nil || !res.OK {
		t.Fatalf("own ref: %+v %v", res, err)
	}
}

func TestUploadAttachmentValidation(t *testing.T) {
	u := newUploadEnv(t, false)
	ctx := context.Background()
	data := testPNG(8)
	ref := u.upload(t, "ag_1", pngSpec(data), data)
	cases := map[string]struct {
		att  Attachment
		path string
	}{
		"url and upload": {Attachment{Type: "image", URL: "https://example.com/a.png", Upload: ref}, "attachments[0].upload"},
		"bad ref":        {Attachment{Type: "image", Upload: "upload:xyz"}, "attachments[0].upload"},
		"bare sha":       {Attachment{Type: "image", Upload: hexSum(data)}, "attachments[0].upload"},
		"image as video": {Attachment{Type: "video", Upload: ref}, "attachments[0].upload"},
		"poster on img":  {Attachment{Type: "image", Upload: ref, PosterUpload: ref}, "attachments[0].poster_upload"},
		"two posters":    {Attachment{Type: "video", URL: "https://example.com/a.mp4", PosterURL: "https://example.com/p.png", PosterUpload: ref}, "attachments[0].poster_upload"},
		"upload on link": {Attachment{Type: "link", Label: "PR", URL: "https://example.com/pr/1", Upload: ref}, "attachments[0].upload"},
	}
	for name, c := range cases {
		sub := uploadPost(c.att)
		sub.Kind, sub.DryRun = KindQuestion, true
		sub.SchemaVersion, sub.Prompt, sub.Question = 2, "Does the checkout page look right to you?", &Question{Type: "free_text"}
		res, err := u.svc.Submit(ctx, "ag_1", sub)
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		found := false
		for _, p := range res.Problems {
			found = found || p.Path == c.path
		}
		if res.OK || !found {
			t.Errorf("%s: want a problem at %s, got %+v", name, c.path, res.Problems)
		}
	}
	// A dry run with a good ref passes, and an image upload may be a video poster.
	sub := uploadPost(Attachment{Type: "image", Upload: ref})
	sub.Kind, sub.DryRun = KindQuestion, true
	sub.SchemaVersion, sub.Prompt, sub.Question = 2, "Does the checkout page look right to you?", &Question{Type: "free_text"}
	if res, err := u.svc.Submit(ctx, "ag_1", sub); err != nil || !res.OK {
		t.Fatalf("dry run with a good ref: %+v %v", res, err)
	}
}

// An uploaded image is stored exactly like a fetched one, on all three
// submission kinds.
func TestUploadedImageMatchesURLSnapshot(t *testing.T) {
	data := testPNG(9)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "image/png")
		_, _ = w.Write(data)
	}))
	defer srv.Close()
	u := newUploadEnv(t, true)
	ctx := context.Background()
	ref := u.upload(t, "ag_1", pngSpec(data), data)
	both := []Attachment{
		{Type: "image", URL: srv.URL + "/shot.png", Alt: "fetched", Caption: "The page after the fix."},
		{Type: "image", Upload: ref, Alt: "uploaded", Caption: "The page after the fix."},
	}

	request := deploySubmission()
	request.Attachments = both
	ask := &Submission{SchemaVersion: 2, Kind: KindQuestion, Prompt: "Does the checkout page look right to you?", Question: &Question{Type: "free_text"}, Attachments: both}
	post := uploadPost(both...)
	for name, sub := range map[string]*Submission{"request": request, "ask": ask, "post": post} {
		res, err := u.svc.Submit(ctx, "ag_1", sub)
		if err != nil || !res.OK {
			t.Fatalf("%s: %+v %v", name, res, err)
		}
		u.svc.Flush()
		r, err := u.svc.Get(ctx, res.RequestID)
		if err != nil {
			t.Fatal(err)
		}
		fetched, uploaded := r.Attachments[0], r.Attachments[1]
		if uploaded.Blob != hexSum(data) || uploaded.ContentType != "image/png" || uploaded.Size != int64(len(data)) || uploaded.FetchError != "" {
			t.Fatalf("%s: uploaded attachment %+v", name, uploaded)
		}
		if fetched.Blob != uploaded.Blob || fetched.ContentType != uploaded.ContentType || fetched.Size != uploaded.Size {
			t.Fatalf("%s: fetched %+v and uploaded %+v differ", name, fetched, uploaded)
		}
		if uploaded.Upload != ref || uploaded.URL != "" {
			t.Fatalf("%s: the ref should be kept as sent: %+v", name, uploaded)
		}
		f, ct, err := u.snaps.Open(ctx, uploaded.Blob)
		if err != nil || ct != "image/png" {
			t.Fatalf("%s: open blob: %v %s", name, err, ct)
		}
		f.Close()
	}
	var used int64
	_ = u.db.QueryRow(`SELECT COALESCE(used_at, 0) FROM inbox_uploads WHERE sha256=? AND agent_id='ag_1'`, hexSum(data)).Scan(&used)
	if used == 0 {
		t.Fatal("a cited upload should be marked used")
	}
}

func TestSweepRemovesUncitedUploads(t *testing.T) {
	u := newUploadEnv(t, false)
	ctx := context.Background()
	lonely, cited, shared := testPNG(10), testPNG(11), testPNG(12)
	u.upload(t, "ag_1", pngSpec(lonely), lonely)
	citedRef := u.upload(t, "ag_1", pngSpec(cited), cited)
	u.upload(t, "ag_1", pngSpec(shared), shared)
	u.upload(t, "ag_2", pngSpec(shared), shared)
	if _, err := u.svc.CreateUpload(ctx, "ag_1", pngSpec(testPNG(13))); err != nil { // never used
		t.Fatal(err)
	}
	res, err := u.svc.Submit(ctx, "ag_1", uploadPost(Attachment{Type: "image", Upload: citedRef}))
	if err != nil || !res.OK {
		t.Fatalf("post: %+v %v", res, err)
	}
	// Exercise the fallback check too: forget that it was cited.
	_, _ = u.db.Exec(`UPDATE inbox_uploads SET used_at=NULL`)

	u.advance(time.Hour)
	u.svc.Sweep(ctx)
	var n int
	_ = u.db.QueryRow(`SELECT COUNT(*) FROM inbox_upload_tickets`).Scan(&n)
	if n != 0 {
		t.Fatalf("expired tickets should be purged, %d left", n)
	}
	_ = u.db.QueryRow(`SELECT COUNT(*) FROM inbox_uploads`).Scan(&n)
	if n != 4 {
		t.Fatalf("uploads younger than 24h must stay, %d left", n)
	}

	u.advance(UploadRetention)
	u.svc.Sweep(ctx)
	exists := func(sha string) bool {
		var one int
		return u.db.QueryRow(`SELECT 1 FROM inbox_uploads WHERE sha256=?`, sha).Scan(&one) == nil
	}
	if exists(hexSum(lonely)) || u.snaps.Exists(ctx, hexSum(lonely)) {
		t.Fatal("an upload nobody cited should be deleted with its blob")
	}
	if _, _, err := u.snaps.Open(ctx, hexSum(lonely)); err == nil {
		t.Fatal("the blob row should be gone too")
	}
	if !exists(hexSum(cited)) || !u.snaps.Exists(ctx, hexSum(cited)) {
		t.Fatal("a cited upload must stay")
	}
	if !u.svc.BlobBelongsToAgent(ctx, "ag_1", hexSum(cited)) {
		t.Fatal("the owner can still download a cited upload")
	}
	if exists(hexSum(shared)) || u.snaps.Exists(ctx, hexSum(shared)) {
		t.Fatal("both agents' uncited copies should be gone")
	}
}

func TestSweepKeepsBlobAnotherAgentStillHolds(t *testing.T) {
	u := newUploadEnv(t, false)
	ctx := context.Background()
	data := testPNG(14)
	u.upload(t, "ag_1", pngSpec(data), data)
	u.advance(UploadRetention / 2)
	u.upload(t, "ag_2", pngSpec(data), data)
	u.advance(UploadRetention/2 + time.Minute)
	u.svc.Sweep(ctx)
	var n int
	_ = u.db.QueryRow(`SELECT COUNT(*) FROM inbox_uploads WHERE sha256=?`, hexSum(data)).Scan(&n)
	if n != 1 || !u.snaps.Exists(ctx, hexSum(data)) {
		t.Fatalf("ag_2's younger upload keeps the blob (rows %d)", n)
	}
	if u.svc.BlobBelongsToAgent(ctx, "ag_1", hexSum(data)) {
		t.Fatal("ag_1's expired upload no longer grants access")
	}
}

// The sweep deletes only blobs an upload created: a blob that a URL
// snapshot or a recorded voice note wrote first keeps its file.
func TestSweepKeepsURLAndVoiceBlobs(t *testing.T) {
	u := newUploadEnv(t, false)
	ctx := context.Background()
	voice, fetched := testPNG(30), testPNG(31)
	if _, err := u.snaps.Put(ctx, voice, "image/png", "voice:rq_1"); err != nil {
		t.Fatal(err)
	}
	if _, err := u.snaps.Put(ctx, fetched, "image/png", "https://example.com/shot.png"); err != nil {
		t.Fatal(err)
	}
	u.upload(t, "ag_1", pngSpec(voice), voice)
	u.upload(t, "ag_1", pngSpec(fetched), fetched)
	u.advance(UploadRetention + 2*time.Hour)
	u.svc.Sweep(ctx)
	var rows int
	_ = u.db.QueryRow(`SELECT COUNT(*) FROM inbox_uploads`).Scan(&rows)
	if rows != 0 {
		t.Fatalf("the uncited upload rows should go, %d left", rows)
	}
	for name, b := range map[string][]byte{"voice": voice, "url": fetched} {
		f, _, err := u.snaps.Open(ctx, hexSum(b))
		if err != nil {
			t.Fatalf("%s blob was deleted: %v", name, err)
		}
		f.Close()
	}
}

// A submission that resolves an upload touches it, so a sweep running at
// the same time skips it instead of deleting it under the citation.
func TestSweepSkipsUploadsASubmissionJustResolved(t *testing.T) {
	u := newUploadEnv(t, false)
	ctx := context.Background()
	data := testPNG(32)
	ref := u.upload(t, "ag_1", pngSpec(data), data)
	u.advance(UploadRetention + time.Hour) // stale by age
	sub := uploadPost(Attachment{Type: "image", Upload: ref})
	sub.Kind, sub.DryRun = KindQuestion, true
	sub.SchemaVersion, sub.Prompt, sub.Question = 2, "Does the checkout page look right to you?", &Question{Type: "free_text"}
	if res, err := u.svc.Submit(ctx, "ag_1", sub); err != nil || !res.OK {
		t.Fatalf("dry run: %+v %v", res, err)
	}
	u.svc.Sweep(ctx)
	if !u.snaps.Exists(ctx, hexSum(data)) {
		t.Fatal("an upload a submission just resolved must survive the sweep")
	}
	// Sending it for real marks it cited in the same transaction.
	res, err := u.svc.Submit(ctx, "ag_1", uploadPost(Attachment{Type: "image", Upload: ref}))
	if err != nil || !res.OK {
		t.Fatalf("post: %+v %v", res, err)
	}
	u.advance(UploadRetention)
	u.svc.Sweep(ctx)
	if !u.snaps.Exists(ctx, hexSum(data)) {
		t.Fatal("a cited upload must survive")
	}
	// A resolved-but-never-sent upload goes once the grace period passes.
	other := testPNG(33)
	oref := u.upload(t, "ag_1", pngSpec(other), other)
	u.advance(UploadRetention + time.Hour)
	sub.Attachments = []Attachment{{Type: "image", Upload: oref}}
	if res, err := u.svc.Submit(ctx, "ag_1", sub); err != nil || !res.OK {
		t.Fatalf("dry run 2: %+v %v", res, err)
	}
	u.advance(2 * uploadTouchGrace)
	u.svc.Sweep(ctx)
	if u.snaps.Exists(ctx, hexSum(other)) {
		t.Fatal("an upload only dry-run cited should go after the grace period")
	}
}

// An upload-sourced blob whose owner rows are gone is collected too.
func TestSweepCollectsOwnerlessUploadBlobs(t *testing.T) {
	u := newUploadEnv(t, false)
	ctx := context.Background()
	data := testPNG(34)
	sha := hexSum(data)
	if _, err := u.snaps.Put(ctx, data, "image/png", "voice:placeholder"); err != nil {
		t.Fatal(err)
	}
	// Re-label it as an upload blob written by the service clock.
	if _, err := u.db.Exec(`UPDATE inbox_blobs SET source_url=?, created_at=? WHERE sha256=?`, UploadRefPrefix+sha, u.now.UnixMilli(), sha); err != nil {
		t.Fatal(err)
	}
	u.advance(time.Hour)
	u.svc.Sweep(ctx)
	if !u.snaps.Exists(ctx, sha) {
		t.Fatal("a young ownerless blob stays")
	}
	u.advance(UploadRetention)
	u.svc.Sweep(ctx)
	if u.snaps.Exists(ctx, sha) {
		t.Fatal("an old ownerless upload blob should be collected")
	}
}

// Release and completion act only on the caller's own claim: a PUT whose
// claim expired cannot drop or use the newer claim of the PUT after it.
func TestUploadClaimTokenOwnsTheClaim(t *testing.T) {
	u := newUploadEnv(t, false)
	ctx := context.Background()
	data := testPNG(40)
	tk, err := u.svc.CreateUpload(ctx, "ag_1", pngSpec(data))
	if err != nil {
		t.Fatal(err)
	}
	old, err := u.svc.ClaimUpload(ctx, tk.Ticket)
	if err != nil {
		t.Fatal(err)
	}
	u.advance(UploadClaimTTL + time.Second) // the first PUT hangs past its claim
	fresh, err := u.svc.ClaimUpload(ctx, tk.Ticket)
	if err != nil {
		t.Fatalf("an expired claim should be reclaimable: %v", err)
	}
	u.svc.ReleaseUpload(old)
	if _, err := u.svc.ClaimUpload(ctx, tk.Ticket); !errors.Is(err, ErrUploadBusy) {
		t.Fatalf("the stale holder must not release the new claim: %v", err)
	}
	if _, err := u.svc.CompleteUpload(ctx, old, "image/png", bytes.NewReader(data)); !errors.Is(err, ErrUploadGone) {
		t.Fatalf("the stale holder must not complete: %v", err)
	}
	if _, err := u.svc.ClaimUpload(ctx, tk.Ticket); !errors.Is(err, ErrUploadBusy) {
		t.Fatalf("the stale holder's failure must not release the new claim: %v", err)
	}
	if _, err := u.svc.CompleteUpload(ctx, fresh, "image/png", bytes.NewReader(data)); err != nil {
		t.Fatalf("the current holder completes: %v", err)
	}
	if !u.snaps.Exists(ctx, hexSum(data)) {
		t.Fatal("blob missing after the current holder completed")
	}
}

func TestAgentUploadSlots(t *testing.T) {
	u := newUploadEnv(t, false)
	r1, ok1 := u.svc.AcquireAgentUploadSlot("ag_1")
	_, ok2 := u.svc.AcquireAgentUploadSlot("ag_1")
	if !ok1 || !ok2 {
		t.Fatal("two bodies per agent are allowed")
	}
	if _, ok := u.svc.AcquireAgentUploadSlot("ag_1"); ok {
		t.Fatal("a third body for the same agent must be refused")
	}
	if _, ok := u.svc.AcquireAgentUploadSlot("ag_2"); !ok {
		t.Fatal("another agent has its own slots")
	}
	r1()
	r1()
	if _, ok := u.svc.AcquireAgentUploadSlot("ag_1"); !ok {
		t.Fatal("a released slot is reusable")
	}
	if _, ok := u.svc.AcquireAgentUploadSlot("ag_1"); ok {
		t.Fatal("a double release must free one slot only")
	}
}

func TestCleanTempRemovesOnlyStaleTempFiles(t *testing.T) {
	dir := t.TempDir()
	snaps, err := NewSnapshotter(nil, dir)
	if err != nil {
		t.Fatal(err)
	}
	old := time.Now().Add(-2 * time.Hour)
	blob := hexSum([]byte("kept blob"))
	files := map[string]bool{ // name -> should survive
		"upload-123": false, "fetch-9": false, "put-77": false,
		"upload-fresh": true, blob: true, "notes.txt": true,
	}
	for name := range files {
		path := filepath.Join(dir, name)
		if err := os.WriteFile(path, []byte("x"), 0o600); err != nil {
			t.Fatal(err)
		}
		if name != "upload-fresh" {
			_ = os.Chtimes(path, old, old)
		}
	}
	if n := snaps.CleanTemp(time.Hour); n != 3 {
		t.Fatalf("removed %d, want 3", n)
	}
	for name, keep := range files {
		_, err := os.Stat(filepath.Join(dir, name))
		if keep != (err == nil) {
			t.Errorf("%s: kept=%v, want %v", name, err == nil, keep)
		}
	}
	// Startup runs the same cleanup.
	stale := filepath.Join(dir, "fetch-startup")
	_ = os.WriteFile(stale, []byte("x"), 0o600)
	_ = os.Chtimes(stale, old, old)
	if _, err := NewSnapshotter(nil, dir); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(stale); err == nil {
		t.Fatal("startup should remove stale temp files")
	}
}
