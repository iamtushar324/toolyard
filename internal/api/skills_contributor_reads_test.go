package api

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/tusharbhardwaj/toolyard/internal/sealbox"
	"github.com/tusharbhardwaj/toolyard/internal/skillhub"
)

// skillsGitHubRecorder serves one public repository, acme/skills, at one
// commit (the part of the GitHub API inspect, a dry run and a sync use),
// and records the Authorization header of every API call.
type skillsGitHubRecorder struct {
	srv  *httptest.Server
	mu   sync.Mutex
	auth []string
}

func newSkillsGitHubRecorder(t *testing.T) *skillsGitHubRecorder {
	t.Helper()
	const sha = "cccccccccccccccccccccccccccccccccccccccc"
	tree := map[string]string{
		"skills/pr-review/SKILL.md": "---\nname: pr-review\ndescription: Review a pull request.\n---\nRead the diff.\n",
		"skills/pr-review/notes.md": "Check the tests.\n",
	}
	g := &skillsGitHubRecorder{}
	answer := func(w http.ResponseWriter, v any) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(v)
	}
	api := func(h http.HandlerFunc) http.HandlerFunc {
		return func(w http.ResponseWriter, r *http.Request) {
			g.mu.Lock()
			g.auth = append(g.auth, r.Header.Get("Authorization"))
			g.mu.Unlock()
			h(w, r)
		}
	}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /repos/acme/skills", api(func(w http.ResponseWriter, r *http.Request) {
		answer(w, map[string]any{"name": "skills", "private": false, "default_branch": "main",
			"html_url": "https://github.com/acme/skills", "owner": map[string]any{"login": "acme"}})
	}))
	mux.HandleFunc("GET /repos/acme/skills/branches", api(func(w http.ResponseWriter, r *http.Request) {
		answer(w, []map[string]any{{"name": "main"}})
	}))
	mux.HandleFunc("GET /repos/acme/skills/git/ref/heads/main", api(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("ETag", `"`+sha+`"`)
		answer(w, map[string]any{"ref": "refs/heads/main", "object": map[string]any{"sha": sha, "type": "commit"}})
	}))
	mux.HandleFunc("GET /repos/acme/skills/tarball/{sha}", api(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, g.srv.URL+"/_codeload/acme/skills/"+r.PathValue("sha"), http.StatusFound)
	}))
	mux.HandleFunc("GET /_codeload/acme/skills/{sha}", func(w http.ResponseWriter, r *http.Request) {
		var buf bytes.Buffer
		gz := gzip.NewWriter(&buf)
		tw := tar.NewWriter(gz)
		top := "acme-skills-ccccccc"
		_ = tw.WriteHeader(&tar.Header{Typeflag: tar.TypeDir, Name: top + "/", Mode: 0o755, ModTime: time.Unix(0, 0)})
		for _, p := range []string{"skills/pr-review/SKILL.md", "skills/pr-review/notes.md"} {
			_ = tw.WriteHeader(&tar.Header{Typeflag: tar.TypeReg, Name: top + "/" + p, Size: int64(len(tree[p])), Mode: 0o644, ModTime: time.Unix(0, 0)})
			_, _ = tw.Write([]byte(tree[p]))
		}
		_ = tw.Close()
		_ = gz.Close()
		_, _ = w.Write(buf.Bytes())
	})
	// The default token's check when an admin stores it (not a read made
	// for anyone, so not recorded).
	mux.HandleFunc("GET /rate_limit", func(w http.ResponseWriter, r *http.Request) {
		core := map[string]any{"limit": 5000, "remaining": 4999, "used": 1, "reset": time.Now().Add(time.Hour).Unix()}
		answer(w, map[string]any{"resources": map[string]any{"core": core}, "rate": core})
	})
	g.srv = httptest.NewServer(mux)
	t.Cleanup(g.srv.Close)
	return g
}

// since returns the Authorization headers recorded after mark.
func (g *skillsGitHubRecorder) since(mark int) []string {
	g.mu.Lock()
	defer g.mu.Unlock()
	return append([]string(nil), g.auth[mark:]...)
}

func (g *skillsGitHubRecorder) mark() int {
	g.mu.Lock()
	defer g.mu.Unlock()
	return len(g.auth)
}

// skillsTokenSettings is a settings store the default GitHub token can be
// written to.
type skillsTokenSettings struct {
	mu sync.Mutex
	m  map[string]string
}

func (s *skillsTokenSettings) GetString(key, fallback string) string {
	s.mu.Lock()
	defer s.mu.Unlock()
	if v, ok := s.m[key]; ok {
		return v
	}
	return fallback
}

func (s *skillsTokenSettings) Set(_ context.Context, key string, value any) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	v, _ := value.(string)
	s.m[key] = v
	return nil
}

// Every GitHub read the API makes for a skill contributor is anonymous,
// with the admins' default token set: inspect, preview, add (and its first
// sync) and Sync now. An admin's preview reads with the default token.
func TestSkillContributorReadsNeverUseTheDefaultToken(t *testing.T) {
	e := newAccessTestServer(t)
	gh := newSkillsGitHubRecorder(t)
	cipher, err := sealbox.NewCipher(bytes.Repeat([]byte{7}, sealbox.MasterKeySize))
	if err != nil {
		t.Fatal(err)
	}
	svc, err := skillhub.New(skillhub.Options{DB: e.db, Cipher: cipher, Settings: &skillsTokenSettings{m: map[string]string{}},
		GitHubAPI: gh.srv.URL, HTTPClient: gh.srv.Client()})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(svc.Wait)
	t.Cleanup(cancel)
	if err := svc.Start(ctx); err != nil {
		t.Fatal(err)
	}
	e.srv.skillsAdmin = svc
	adminCk := e.cookieFor(t, e.admin.ID)
	contrib := e.member(t, "user_c", "casey@example.com")
	if err := svc.SetContributor(ctx, e.admin.ID, contrib.ID, true); err != nil {
		t.Fatal(err)
	}
	contribCk := e.cookieFor(t, contrib.ID)
	if rec := e.do(t, adminCk, http.MethodPut, "/v1/skills/admin/github-token", `{"token":"`+skillsDefaultTokenSentinel+`"}`); rec.Code != http.StatusOK {
		t.Fatalf("set the default token: %d %s", rec.Code, rec.Body.String())
	}

	mark := gh.mark()
	if rec := e.do(t, contribCk, http.MethodPost, "/v1/skills/sources/inspect", `{"url":"acme/skills"}`); rec.Code != http.StatusOK {
		t.Fatalf("contributor inspect: %d %s", rec.Code, rec.Body.String())
	}
	rec := e.do(t, contribCk, http.MethodPost, "/v1/skills/sources/preview", `{"url":"acme/skills","path":"skills"}`)
	if rec.Code != http.StatusOK || skillsJSON(t, rec)["skill_count"] != float64(1) {
		t.Fatalf("contributor preview: %d %s", rec.Code, rec.Body.String())
	}
	rec = e.do(t, contribCk, http.MethodPost, "/v1/skills/sources", `{"url":"acme/skills","path":"skills","mode":"review"}`)
	if rec.Code != http.StatusCreated {
		t.Fatalf("contributor create: %d %s", rec.Code, rec.Body.String())
	}
	src, _ := skillsJSON(t, rec)["source"].(map[string]any)
	id, _ := src["id"].(string)
	if id == "" || src["approval"] != "pending" {
		t.Fatalf("created source %v", src)
	}
	// The first sync runs in the background: wait for its check.
	deadline := time.Now().Add(10 * time.Second)
	for {
		got := skillsJSON(t, e.do(t, contribCk, http.MethodGet, "/v1/skills/sources/"+id, ""))
		if got["last_checked_at"] != nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("the first sync recorded no check: %v", got)
		}
		time.Sleep(20 * time.Millisecond)
	}
	if rec := e.do(t, contribCk, http.MethodPost, "/v1/skills/sources/"+id+"/sync", ""); rec.Code != http.StatusOK {
		t.Fatalf("contributor sync: %d %s", rec.Code, rec.Body.String())
	}
	calls := gh.since(mark)
	if len(calls) == 0 {
		t.Fatal("no GitHub calls were made")
	}
	for _, a := range calls {
		if a != "" {
			t.Fatalf("a read made for a contributor sent %q (calls %q)", a, calls)
		}
	}

	// The default token is set and an admin's dry run reads with it.
	mark = gh.mark()
	if rec := e.do(t, adminCk, http.MethodPost, "/v1/skills/sources/preview", `{"url":"acme/skills","path":"skills"}`); rec.Code != http.StatusOK {
		t.Fatalf("admin preview: %d %s", rec.Code, rec.Body.String())
	}
	if got := strings.Join(gh.since(mark), ","); !strings.Contains(got, "Bearer "+skillsDefaultTokenSentinel) {
		t.Fatalf("an admin's preview did not read with the default token: %q", got)
	}
}
