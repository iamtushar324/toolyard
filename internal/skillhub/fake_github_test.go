package skillhub

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha1"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sort"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/tusharbhardwaj/toolyard/internal/sealbox"
	"github.com/tusharbhardwaj/toolyard/internal/store/storetest"
)

// symlinkPrefix marks fake file content that is a symbolic link (the rest
// is its target).
const symlinkPrefix = "symlink:"

// fakeGitHub serves the part of the GitHub REST API the hub uses, for one
// repository, from commits the test makes. The tarball endpoint redirects
// to /_codeload/ on the same server, as GitHub redirects to
// codeload.github.com.
type fakeGitHub struct {
	t   *testing.T
	srv *httptest.Server

	mu            sync.Mutex
	owner, repo   string
	defaultBranch string
	private       bool
	token         string                       // required for every API call when private
	branches      map[string]string            // name -> head sha
	commits       map[string]map[string]string // sha -> repo path -> content
	parents       map[string]string            // sha -> parent sha
	n             int
	calls         map[string]int
	codeloadAuth  []string // Authorization headers the archive download got
	retryAfter    int      // when > 0, the next API call answers 429 with Retry-After
}

func newFakeGitHub(t *testing.T, owner, repo string) *fakeGitHub {
	t.Helper()
	f := &fakeGitHub{
		t: t, owner: owner, repo: repo, defaultBranch: "main",
		branches: map[string]string{}, commits: map[string]map[string]string{},
		parents: map[string]string{}, calls: map[string]int{},
	}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /repos/{o}/{r}", f.api(f.handleRepo))
	mux.HandleFunc("GET /repos/{o}/{r}/branches", f.api(f.handleBranches))
	mux.HandleFunc("GET /repos/{o}/{r}/commits/{ref...}", f.api(f.handleHead))
	mux.HandleFunc("GET /repos/{o}/{r}/tarball/{sha}", f.api(f.handleTarball))
	mux.HandleFunc("GET /repos/{o}/{r}/compare/{spec}", f.api(f.handleCompare))
	mux.HandleFunc("GET /repos/{o}/{r}/contents/{path...}", f.api(f.handleContents))
	mux.HandleFunc("GET /_codeload/{o}/{r}/{sha}", f.handleCodeload)
	f.srv = httptest.NewServer(mux)
	t.Cleanup(f.srv.Close)
	return f
}

// commit makes a commit on branch from its head's tree: set adds or
// replaces files, del removes them. It returns the new SHA.
func (f *fakeGitHub) commit(branch string, set map[string]string, del ...string) string {
	f.mu.Lock()
	defer f.mu.Unlock()
	parent := f.branches[branch]
	tree := map[string]string{}
	for p, c := range f.commits[parent] {
		tree[p] = c
	}
	for p, c := range set {
		tree[p] = c
	}
	for _, p := range del {
		delete(tree, p)
	}
	f.n++
	sum := sha1.Sum([]byte(fmt.Sprintf("commit-%d", f.n)))
	sha := hex.EncodeToString(sum[:])
	f.commits[sha] = tree
	f.parents[sha] = parent
	f.branches[branch] = sha
	return sha
}

func (f *fakeGitHub) count(name string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.calls[name]
}

// api wraps an API handler: repository and credential checks, call counts
// and the rate limit switch.
func (f *fakeGitHub) api(h func(w http.ResponseWriter, r *http.Request)) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		retry := f.retryAfter
		f.retryAfter = 0
		private, token := f.private, f.token
		f.mu.Unlock()
		if retry > 0 {
			w.Header().Set("Retry-After", strconv.Itoa(retry))
			http.Error(w, `{"message":"secondary rate limit"}`, http.StatusTooManyRequests)
			return
		}
		if r.PathValue("o") != f.owner || r.PathValue("r") != f.repo {
			http.Error(w, `{"message":"Not Found"}`, http.StatusNotFound)
			return
		}
		if private && r.Header.Get("Authorization") != "Bearer "+token {
			http.Error(w, `{"message":"Not Found"}`, http.StatusNotFound)
			return
		}
		h(w, r)
	}
}

func (f *fakeGitHub) bump(name string) {
	f.mu.Lock()
	f.calls[name]++
	f.mu.Unlock()
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(v)
}

func (f *fakeGitHub) handleRepo(w http.ResponseWriter, r *http.Request) {
	f.bump("repo")
	f.mu.Lock()
	defer f.mu.Unlock()
	writeJSON(w, map[string]any{
		"name": f.repo, "private": f.private, "default_branch": f.defaultBranch,
		"html_url": "https://github.com/" + f.owner + "/" + f.repo,
		"owner":    map[string]any{"login": f.owner},
	})
}

func (f *fakeGitHub) handleBranches(w http.ResponseWriter, r *http.Request) {
	f.bump("branches")
	f.mu.Lock()
	names := make([]string, 0, len(f.branches))
	for n := range f.branches {
		names = append(names, n)
	}
	f.mu.Unlock()
	sort.Strings(names)
	per, _ := strconv.Atoi(r.URL.Query().Get("per_page"))
	page, _ := strconv.Atoi(r.URL.Query().Get("page"))
	if per <= 0 {
		per = 30
	}
	if page <= 0 {
		page = 1
	}
	out := []map[string]any{}
	for i := (page - 1) * per; i < len(names) && i < page*per; i++ {
		out = append(out, map[string]any{"name": names[i]})
	}
	writeJSON(w, out)
}

func (f *fakeGitHub) handleHead(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	sha, ok := f.branches[r.PathValue("ref")]
	f.mu.Unlock()
	if !ok {
		f.bump("head")
		http.Error(w, `{"message":"No commit found for SHA"}`, http.StatusUnprocessableEntity)
		return
	}
	etag := `"` + sha + `"`
	if r.Header.Get("If-None-Match") == etag {
		f.bump("head304")
		w.WriteHeader(http.StatusNotModified)
		return
	}
	f.bump("head")
	if r.Header.Get("Accept") != "application/vnd.github.sha" {
		http.Error(w, `{"message":"want the sha media type"}`, http.StatusBadRequest)
		return
	}
	w.Header().Set("ETag", etag)
	_, _ = w.Write([]byte(sha))
}

func (f *fakeGitHub) handleTarball(w http.ResponseWriter, r *http.Request) {
	f.bump("tarball")
	sha := r.PathValue("sha")
	http.Redirect(w, r, f.srv.URL+"/_codeload/"+f.owner+"/"+f.repo+"/"+sha+"?token=short-lived-grant", http.StatusFound)
}

func (f *fakeGitHub) handleCodeload(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	f.calls["codeload"]++
	f.codeloadAuth = append(f.codeloadAuth, r.Header.Get("Authorization"))
	tree, ok := f.commits[r.PathValue("sha")]
	f.mu.Unlock()
	if !ok {
		http.NotFound(w, r)
		return
	}
	_, _ = w.Write(buildTarball(f.t, f.owner+"-"+f.repo+"-"+r.PathValue("sha")[:7], tree))
}

func (f *fakeGitHub) handleCompare(w http.ResponseWriter, r *http.Request) {
	f.bump("compare")
	base, head, ok := strings.Cut(r.PathValue("spec"), "...")
	f.mu.Lock()
	defer f.mu.Unlock()
	bt, bok := f.commits[base]
	ht, hok := f.commits[head]
	if !ok || !bok || !hok {
		http.Error(w, `{"message":"Not Found"}`, http.StatusNotFound)
		return
	}
	status := "diverged"
	if base == head {
		status = "identical"
	}
	for c := f.parents[head]; c != "" && status == "diverged"; c = f.parents[c] {
		if c == base {
			status = "ahead"
		}
	}
	files := []map[string]any{}
	for _, p := range sortedPaths(ht) {
		old, had := bt[p]
		switch {
		case !had:
			files = append(files, map[string]any{"filename": p, "status": "added"})
		case old != ht[p]:
			files = append(files, map[string]any{"filename": p, "status": "modified"})
		}
	}
	for _, p := range sortedPaths(bt) {
		if _, still := ht[p]; !still {
			files = append(files, map[string]any{"filename": p, "status": "removed"})
		}
	}
	writeJSON(w, map[string]any{"status": status, "files": files})
}

func (f *fakeGitHub) handleContents(w http.ResponseWriter, r *http.Request) {
	f.bump("contents")
	f.mu.Lock()
	content, ok := f.commits[r.URL.Query().Get("ref")][r.PathValue("path")]
	f.mu.Unlock()
	if !ok {
		http.Error(w, `{"message":"Not Found"}`, http.StatusNotFound)
		return
	}
	if target, isLink := strings.CutPrefix(content, symlinkPrefix); isLink {
		writeJSON(w, map[string]any{"type": "symlink", "target": target, "size": len(target)})
		return
	}
	writeJSON(w, map[string]any{
		"type": "file", "encoding": "base64", "size": len(content),
		"content": base64.StdEncoding.EncodeToString([]byte(content)),
	})
}

// buildTarball makes a GitHub-style archive: a pax global header, then
// every file under one top folder.
func buildTarball(t testing.TB, top string, tree map[string]string) []byte {
	t.Helper()
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	must := func(err error) {
		if err != nil {
			t.Fatalf("build tarball: %v", err)
		}
	}
	must(tw.WriteHeader(&tar.Header{Typeflag: tar.TypeXGlobalHeader, Name: "pax_global_header", PAXRecords: map[string]string{"comment": top}}))
	must(tw.WriteHeader(&tar.Header{Typeflag: tar.TypeDir, Name: top + "/", Mode: 0o755, ModTime: time.Unix(0, 0)}))
	for _, p := range sortedPaths(tree) {
		c := tree[p]
		if target, isLink := strings.CutPrefix(c, symlinkPrefix); isLink {
			must(tw.WriteHeader(&tar.Header{Typeflag: tar.TypeSymlink, Name: top + "/" + p, Linkname: target, Mode: 0o777, ModTime: time.Unix(0, 0)}))
			continue
		}
		must(tw.WriteHeader(&tar.Header{Typeflag: tar.TypeReg, Name: top + "/" + p, Size: int64(len(c)), Mode: 0o644, ModTime: time.Unix(0, 0)}))
		_, err := tw.Write([]byte(c))
		must(err)
	}
	must(tw.Close())
	must(gz.Close())
	return buf.Bytes()
}

// skillMD is a SKILL.md with the given name and description, extra
// frontmatter lines and a body.
func skillMD(name, desc, extra, body string) string {
	fm := "---\nname: " + name + "\ndescription: " + desc + "\n"
	if extra != "" {
		fm += extra + "\n"
	}
	return fm + "---\n" + body
}

// staticSettings answers GetString from a map.
type staticSettings map[string]string

func (s staticSettings) GetString(key, fallback string) string {
	if v, ok := s[key]; ok {
		return v
	}
	return fallback
}

// newTestService returns a started Service on a fresh test database that
// talks to gh.
func newTestService(t *testing.T, gh *fakeGitHub, opts ...func(*Options)) *Service {
	t.Helper()
	db := storetest.Open(t)
	cipher, err := sealbox.NewCipher(bytes.Repeat([]byte{7}, sealbox.MasterKeySize))
	if err != nil {
		t.Fatal(err)
	}
	o := Options{DB: db, Cipher: cipher, Settings: staticSettings{}, GitHubAPI: gh.srv.URL, HTTPClient: gh.srv.Client()}
	for _, fn := range opts {
		fn(&o)
	}
	svc, err := New(o)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(svc.Wait)
	t.Cleanup(cancel)
	if err := svc.Start(ctx); err != nil {
		t.Fatal(err)
	}
	return svc
}

// admin and member are viewers the tests use.
var (
	adminViewer  = Viewer{UserID: "usr_admin", Admin: true, OnHost: true}
	memberViewer = Viewer{UserID: "usr_member", AgentID: "agt_member"}
)

// mustCreate creates a source and syncs it once.
func mustCreate(t *testing.T, svc *Service, in SourceInput) (*Source, *SyncResult) {
	t.Helper()
	ctx := context.Background()
	src, err := svc.CreateSource(ctx, "usr_admin", in)
	if err != nil {
		t.Fatalf("CreateSource: %v", err)
	}
	res, err := svc.SyncNow(ctx, src.ID)
	if err != nil {
		t.Fatalf("SyncNow: %v", err)
	}
	return src, res
}
