package api

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"github.com/tusharbhardwaj/toolyard/internal/hooks"
	"github.com/tusharbhardwaj/toolyard/internal/mempalace"
)

func expectNotGranted(t *testing.T, name string, rec *httptest.ResponseRecorder) {
	t.Helper()
	if rec.Code != http.StatusForbidden || decodeJSON(t, rec)["error"] != "server not granted" {
		t.Errorf("%s: got %d %s, want 403 server not granted", name, rec.Code, rec.Body.String())
	}
}

func expectOK(t *testing.T, name string, rec *httptest.ResponseRecorder) map[string]any {
	t.Helper()
	if rec.Code != http.StatusOK {
		t.Fatalf("%s: got %d %s, want 200", name, rec.Code, rec.Body.String())
	}
	return decodeJSON(t, rec)
}

func countTarget(calls []string, target string) int {
	n := 0
	for _, c := range calls {
		if c == target {
			n++
		}
	}
	return n
}

// The bearer REST twins of the mempalace and notes tools honour the same
// per-user grants the gateway enforces on the tools themselves.
func TestBearerRoutesHonourMemberGrants(t *testing.T) {
	e := newAccessTestServer(t)
	ctx := t.Context()
	m := e.member(t, "user_m", "m@beknown.work")
	memberTok, _ := e.agentFor(t, m.ID)
	adminTok, _ := e.agentFor(t, e.admin.ID)
	ingest := `{"entry":"remember this","topic":"t","wing":"w"}`
	note := func(p string) string { return `{"path":"` + p + `","content":"# hi"}` }

	// A member with no grants: every write refused, nothing dispatched,
	// no file written.
	expectNotGranted(t, "mempalace ingest", e.bearer(t, memberTok, http.MethodPost, "/v1/mempalace/ingest", ingest))
	expectNotGranted(t, "notes publish", e.bearer(t, memberTok, http.MethodPost, "/v1/notes/publish", note("team/hello.md")))
	expectNotGranted(t, "notes sync", e.bearer(t, memberTok, http.MethodPost, "/v1/notes/sync", ""))
	if calls := e.disp.Calls(); len(calls) != 0 {
		t.Errorf("dispatched despite 403: %v", calls)
	}
	if _, err := os.Stat(filepath.Join(e.notesDir, "team", "hello.md")); err == nil {
		t.Error("note written despite 403")
	}

	// An admin's agent is unaffected.
	if got := expectOK(t, "admin mempalace ingest", e.bearer(t, adminTok, http.MethodPost, "/v1/mempalace/ingest", ingest)); got["ok"] != true {
		t.Errorf("admin ingest = %v", got)
	}
	expectOK(t, "admin notes publish", e.bearer(t, adminTok, http.MethodPost, "/v1/notes/publish", note("admin/note.md")))
	if _, err := os.Stat(filepath.Join(e.notesDir, "admin", "note.md")); err != nil {
		t.Errorf("admin note not written: %v", err)
	}
	expectOK(t, "admin notes sync", e.bearer(t, adminTok, http.MethodPost, "/v1/notes/sync", ""))
	if n := countTarget(e.disp.Calls(), mempalace.DiaryWriteTool); n == 0 {
		t.Errorf("admin ingest never reached %s: %v", mempalace.DiaryWriteTool, e.disp.Calls())
	}

	// Grant mempalace only: ingest works, notes still refused.
	if err := e.access.SetGroups(ctx, m.ID, []string{mempalaceGroup}, e.admin.ID); err != nil {
		t.Fatal(err)
	}
	before := countTarget(e.disp.Calls(), mempalace.DiaryWriteTool)
	if got := expectOK(t, "granted mempalace ingest", e.bearer(t, memberTok, http.MethodPost, "/v1/mempalace/ingest", ingest)); got["ok"] != true {
		t.Errorf("member ingest = %v", got)
	}
	if after := countTarget(e.disp.Calls(), mempalace.DiaryWriteTool); after != before+1 {
		t.Errorf("member ingest dispatch count %d -> %d", before, after)
	}
	expectNotGranted(t, "notes publish with mempalace only", e.bearer(t, memberTok, http.MethodPost, "/v1/notes/publish", note("team/hello.md")))
	expectNotGranted(t, "notes sync with mempalace only", e.bearer(t, memberTok, http.MethodPost, "/v1/notes/sync", ""))

	// Grant notes too: the note lands.
	if err := e.access.SetGroups(ctx, m.ID, []string{mempalaceGroup, notesGroup}, e.admin.ID); err != nil {
		t.Fatal(err)
	}
	expectOK(t, "granted notes publish", e.bearer(t, memberTok, http.MethodPost, "/v1/notes/publish", note("team/hello.md")))
	if _, err := os.Stat(filepath.Join(e.notesDir, "team", "hello.md")); err != nil {
		t.Errorf("member note not written after grant: %v", err)
	}
	expectOK(t, "granted notes sync", e.bearer(t, memberTok, http.MethodPost, "/v1/notes/sync", ""))

	// Group names are the ones the gateway uses.
	if mempalaceGroup != "mempalace" || notesGroup != "notes" {
		t.Errorf("groups = %q, %q", mempalaceGroup, notesGroup)
	}
}

// Hook ingest always records the event; the MemPalace forward needs the
// mempalace grant.
func TestHooksIngestSkipsMemoryWithoutGrant(t *testing.T) {
	e := newAccessTestServer(t)
	ctx := t.Context()
	m := e.member(t, "user_m", "m@beknown.work")
	memberTok, memberAgent := e.agentFor(t, m.ID)
	adminTok, adminAgent := e.agentFor(t, e.admin.ID)
	body := `{"source":"codex","event_name":"UserPromptSubmit","text":"remember this"}`

	got := expectOK(t, "member hook", e.bearer(t, memberTok, http.MethodPost, "/v1/hooks/ingest", body))
	if got["memory_ingested"] != false || got["event_id"] == "" {
		t.Errorf("member hook without grant = %v", got)
	}
	if calls := e.disp.Calls(); len(calls) != 0 {
		t.Errorf("memory forwarded without grant: %v", calls)
	}
	events, err := e.hooks.List(ctx, hooks.Query{AgentID: memberAgent})
	if err != nil || len(events) != 1 || events[0].MemoryIngested {
		t.Errorf("hook event not recorded as expected: %v %+v", err, events)
	}

	if err := e.access.SetGroups(ctx, m.ID, []string{mempalaceGroup}, e.admin.ID); err != nil {
		t.Fatal(err)
	}
	got = expectOK(t, "granted member hook", e.bearer(t, memberTok, http.MethodPost, "/v1/hooks/ingest", body))
	if got["memory_ingested"] != true {
		t.Errorf("member hook with grant = %v", got)
	}
	if n := countTarget(e.disp.Calls(), mempalace.DiaryWriteTool); n != 1 {
		t.Errorf("forwards after grant = %d, want 1 (%v)", n, e.disp.Calls())
	}

	got = expectOK(t, "admin hook", e.bearer(t, adminTok, http.MethodPost, "/v1/hooks/ingest", body))
	if got["memory_ingested"] != true {
		t.Errorf("admin hook = %v", got)
	}
	events, err = e.hooks.List(ctx, hooks.Query{AgentID: adminAgent})
	if err != nil || len(events) != 1 || !events[0].MemoryIngested {
		t.Errorf("admin hook event: %v %+v", err, events)
	}
}
