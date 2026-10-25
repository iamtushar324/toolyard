package skillhub

// fakeGitData serves the GitHub Git data and pull request API that change
// proposals write through (internal/githubapi), on an httptest server. It
// reads the base commits from a fakeGitHub (the sync fake) and keeps what
// toolyard writes: trees, commits, branches and pull requests. The
// githubapi client always calls https://api.github.com; client() returns
// an HTTP client that sends those requests here instead.

import (
	"crypto/sha1"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"sort"
	"strings"
	"sync"
	"testing"
)

type fakeGitTree struct {
	files map[string]string // repository path -> content
	modes map[string]string // repository path -> mode ("" means 100644)
	dir   string            // the folder this tree is; "" is the root
}

type fakeGitCommit struct {
	tree    string
	message string
	parents []string
}

type fakeGitPull struct {
	number                  int
	title, head, base, body string
	userID                  int64 // who opened it
}

// The GitHub account behind the fake's token.
const (
	fakeGitUserID    = 101
	fakeGitUserLogin = "creator"
)

// fakeGitFail makes one call to a route fail: after the first `after`
// calls, the next one answers status; with apply, it carries the write out
// first (an answer lost on the way back).
type fakeGitFail struct {
	status int
	apply  bool
	after  int
}

type fakeGitData struct {
	t     *testing.T
	gh    *fakeGitHub
	srv   *httptest.Server
	token string

	mu      sync.Mutex
	modes   map[string]string // modes of the base commits' files
	trees   map[string]fakeGitTree
	commits map[string]fakeGitCommit
	refs    map[string]string // branch -> commit, for branches toolyard made
	pulls   []fakeGitPull
	fail    map[string]fakeGitFail // "POST refs", "POST pulls", "GET pulls"
	calls   map[string]int
	auth    map[string]bool
	written []map[string]any // the entries of every tree toolyard wrote
}

func newFakeGitData(t *testing.T, gh *fakeGitHub, token string) *fakeGitData {
	t.Helper()
	f := &fakeGitData{t: t, gh: gh, token: token, modes: map[string]string{}, trees: map[string]fakeGitTree{},
		commits: map[string]fakeGitCommit{}, refs: map[string]string{}, fail: map[string]fakeGitFail{},
		calls: map[string]int{}, auth: map[string]bool{}}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /repos/{o}/{r}/git/commits/{sha}", f.api("GET commit", f.getCommit))
	mux.HandleFunc("POST /repos/{o}/{r}/git/commits", f.api("POST commits", f.postCommit))
	mux.HandleFunc("GET /repos/{o}/{r}/git/trees/{sha}", f.api("GET tree", f.getTree))
	mux.HandleFunc("POST /repos/{o}/{r}/git/trees", f.api("POST trees", f.postTree))
	mux.HandleFunc("GET /repos/{o}/{r}/git/ref/{ref...}", f.api("GET ref", f.getRef))
	mux.HandleFunc("POST /repos/{o}/{r}/git/refs", f.api("POST refs", f.postRef))
	mux.HandleFunc("GET /repos/{o}/{r}/pulls", f.api("GET pulls", f.getPulls))
	mux.HandleFunc("POST /repos/{o}/{r}/pulls", f.api("POST pulls", f.postPull))
	mux.HandleFunc("GET /user", func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		f.calls["GET user"]++
		f.auth[r.Header.Get("Authorization")] = true
		f.mu.Unlock()
		if r.Header.Get("Authorization") != "Bearer "+f.token {
			http.Error(w, `{"message":"Bad credentials"}`, http.StatusUnauthorized)
			return
		}
		writeJSON(w, map[string]any{"id": fakeGitUserID, "login": fakeGitUserLogin})
	})
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		t.Errorf("fake GitHub: unexpected %s %s", r.Method, r.URL.Path)
		http.Error(w, `{"message":"Not Found"}`, http.StatusNotFound)
	})
	f.srv = httptest.NewServer(mux)
	t.Cleanup(f.srv.Close)
	return f
}

// client sends the githubapi client's requests (always to
// https://api.github.com) to this fake; any other host fails.
func (f *fakeGitData) client() *http.Client {
	target, _ := url.Parse(f.srv.URL)
	return &http.Client{Transport: fakeGitTransport{target: target, base: f.srv.Client().Transport}}
}

type fakeGitTransport struct {
	target *url.URL
	base   http.RoundTripper
}

func (t fakeGitTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	if r.URL.Host != "api.github.com" || r.URL.Scheme != "https" {
		return nil, fmt.Errorf("request to an unexpected host %s", r.URL.Host)
	}
	out := r.Clone(r.Context())
	out.URL.Scheme, out.URL.Host, out.Host = t.target.Scheme, t.target.Host, t.target.Host
	return t.base.RoundTrip(out)
}

func (f *fakeGitData) api(name string, h func(w http.ResponseWriter, r *http.Request)) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		f.calls[name]++
		f.auth[r.Header.Get("Authorization")] = true
		f.mu.Unlock()
		if r.Header.Get("Authorization") != "Bearer "+f.token {
			http.Error(w, `{"message":"Bad credentials"}`, http.StatusUnauthorized)
			return
		}
		if r.PathValue("o") != f.gh.owner || r.PathValue("r") != f.gh.repo {
			http.Error(w, `{"message":"Not Found"}`, http.StatusNotFound)
			return
		}
		h(w, r)
	}
}

func (f *fakeGitData) count(name string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.calls[name]
}

func (f *fakeGitData) totalCalls() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	n := 0
	for _, c := range f.calls {
		n += c
	}
	return n
}

// takeFail returns the failure planned for this call of route, if any.
func (f *fakeGitData) takeFail(route string) (fakeGitFail, bool) {
	ff, ok := f.fail[route]
	if !ok {
		return fakeGitFail{}, false
	}
	if ff.after > 0 {
		ff.after--
		f.fail[route] = ff
		return fakeGitFail{}, false
	}
	delete(f.fail, route)
	return ff, true
}

func fakeSHA(parts ...string) string {
	sum := sha1.Sum([]byte(strings.Join(parts, "\x00")))
	return hex.EncodeToString(sum[:])
}

func fakeSorted(m map[string]string) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

func fakeMode(modes map[string]string, p string) string {
	if m := modes[p]; m != "" {
		return m
	}
	return "100644"
}

// treeLocked registers the tree of dir in files and returns its SHA: a
// hash of the files under dir, so equal contents give equal SHAs.
func (f *fakeGitData) treeLocked(files, modes map[string]string, dir string) string {
	var parts []string
	for _, p := range fakeSorted(files) {
		if p != dir && inFolder(dir, p) {
			parts = append(parts, relTo(dir, p), fakeMode(modes, p), files[p])
		}
	}
	sha := fakeSHA(append([]string{"tree"}, parts...)...)
	f.trees[sha] = fakeGitTree{files: files, modes: modes, dir: dir}
	return sha
}

func (f *fakeGitData) getCommit(w http.ResponseWriter, r *http.Request) {
	sha := r.PathValue("sha")
	f.gh.mu.Lock()
	base, ok := f.gh.commits[sha]
	parent := f.gh.parents[sha]
	files := map[string]string{}
	for p, c := range base {
		files[p] = c
	}
	f.gh.mu.Unlock()
	f.mu.Lock()
	defer f.mu.Unlock()
	if ok {
		modes := map[string]string{}
		for p, m := range f.modes {
			modes[p] = m
		}
		parents := []map[string]string{}
		if parent != "" {
			parents = append(parents, map[string]string{"sha": parent})
		}
		writeJSON(w, map[string]any{"sha": sha, "message": "a commit", "tree": map[string]string{"sha": f.treeLocked(files, modes, "")}, "parents": parents})
		return
	}
	c, ok := f.commits[sha]
	if !ok {
		http.Error(w, `{"message":"Not Found"}`, http.StatusNotFound)
		return
	}
	parents := []map[string]string{}
	for _, p := range c.parents {
		parents = append(parents, map[string]string{"sha": p})
	}
	writeJSON(w, map[string]any{"sha": sha, "message": c.message, "tree": map[string]string{"sha": c.tree}, "parents": parents})
}

func (f *fakeGitData) getTree(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	sha := r.PathValue("sha")
	t, ok := f.trees[sha]
	if !ok {
		http.Error(w, `{"message":"Not Found"}`, http.StatusNotFound)
		return
	}
	recursive := r.URL.Query().Get("recursive") != ""
	entries := []map[string]any{}
	dirs := map[string]bool{}
	addDir := func(rel string) {
		if !dirs[rel] {
			dirs[rel] = true
			entries = append(entries, map[string]any{"path": rel, "mode": "040000", "type": "tree", "sha": f.treeLocked(t.files, t.modes, repoPath(t.dir, rel))})
		}
	}
	for _, p := range fakeSorted(t.files) {
		if p == t.dir || !inFolder(t.dir, p) {
			continue
		}
		rel := relTo(t.dir, p)
		segs := strings.Split(rel, "/")
		if !recursive && len(segs) > 1 {
			addDir(segs[0])
			continue
		}
		for i := 1; i < len(segs); i++ {
			addDir(strings.Join(segs[:i], "/"))
		}
		entries = append(entries, map[string]any{"path": rel, "mode": fakeMode(t.modes, p), "type": "blob",
			"sha": fakeSHA("blob", t.files[p]), "size": len(t.files[p])})
	}
	writeJSON(w, map[string]any{"sha": sha, "tree": entries, "truncated": false})
}

func (f *fakeGitData) postTree(w http.ResponseWriter, r *http.Request) {
	var body struct {
		BaseTree string           `json:"base_tree"`
		Tree     []map[string]any `json:"tree"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		http.Error(w, `{"message":"Problems parsing JSON"}`, http.StatusBadRequest)
		return
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	base, ok := f.trees[body.BaseTree]
	if !ok || base.dir != "" {
		http.Error(w, `{"message":"base_tree is not a root tree"}`, http.StatusUnprocessableEntity)
		return
	}
	files, modes := map[string]string{}, map[string]string{}
	for p, c := range base.files {
		files[p] = c
	}
	for p, m := range base.modes {
		modes[p] = m
	}
	for _, e := range body.Tree {
		p, _ := e["path"].(string)
		mode, _ := e["mode"].(string)
		if e["type"] != "blob" || p == "" || (mode != "100644" && mode != "100755") {
			http.Error(w, `{"message":"bad tree entry"}`, http.StatusUnprocessableEntity)
			return
		}
		if c, has := e["content"]; has {
			files[p], modes[p] = c.(string), mode
			continue
		}
		if v, has := e["sha"]; has && v == nil {
			if _, exists := files[p]; !exists {
				http.Error(w, `{"message":"path does not exist"}`, http.StatusUnprocessableEntity)
				return
			}
			delete(files, p)
			delete(modes, p)
			continue
		}
		http.Error(w, `{"message":"bad tree entry"}`, http.StatusUnprocessableEntity)
		return
	}
	f.written = append(f.written, body.Tree...)
	w.WriteHeader(http.StatusCreated)
	writeJSON(w, map[string]any{"sha": f.treeLocked(files, modes, "")})
}

func (f *fakeGitData) postCommit(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Message string   `json:"message"`
		Tree    string   `json:"tree"`
		Parents []string `json:"parents"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		http.Error(w, `{"message":"Problems parsing JSON"}`, http.StatusBadRequest)
		return
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if _, ok := f.trees[body.Tree]; !ok {
		http.Error(w, `{"message":"tree not found"}`, http.StatusUnprocessableEntity)
		return
	}
	sha := fakeSHA(append([]string{"commit", body.Message, body.Tree}, body.Parents...)...)
	f.commits[sha] = fakeGitCommit{tree: body.Tree, message: body.Message, parents: body.Parents}
	w.WriteHeader(http.StatusCreated)
	writeJSON(w, map[string]any{"sha": sha})
}

func (f *fakeGitData) branchHead(branch string) string {
	if sha := f.refs[branch]; sha != "" {
		return sha
	}
	f.gh.mu.Lock()
	defer f.gh.mu.Unlock()
	return f.gh.branches[branch]
}

func (f *fakeGitData) getRef(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	branch, ok := strings.CutPrefix(r.PathValue("ref"), "heads/")
	sha := ""
	if ok {
		sha = f.branchHead(branch)
	}
	if sha == "" {
		http.Error(w, `{"message":"Not Found"}`, http.StatusNotFound)
		return
	}
	writeJSON(w, map[string]any{"ref": "refs/heads/" + branch, "object": map[string]string{"sha": sha, "type": "commit"}})
}

func (f *fakeGitData) postRef(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Ref string `json:"ref"`
		SHA string `json:"sha"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		http.Error(w, `{"message":"Problems parsing JSON"}`, http.StatusBadRequest)
		return
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	branch, ok := strings.CutPrefix(body.Ref, "refs/heads/")
	if !ok || f.branchHead(branch) != "" {
		http.Error(w, `{"message":"Reference already exists"}`, http.StatusUnprocessableEntity)
		return
	}
	if _, ok := f.commits[body.SHA]; !ok {
		http.Error(w, `{"message":"Object does not exist"}`, http.StatusUnprocessableEntity)
		return
	}
	ff, failing := f.takeFail("POST refs")
	if failing && !ff.apply {
		http.Error(w, `{"message":"Server Error"}`, ff.status)
		return
	}
	f.refs[branch] = body.SHA
	if failing {
		http.Error(w, `{"message":"Server Error"}`, ff.status)
		return
	}
	w.WriteHeader(http.StatusCreated)
	writeJSON(w, map[string]any{"ref": body.Ref, "object": map[string]string{"sha": body.SHA, "type": "commit"}})
}

func (f *fakeGitData) pullJSON(p fakeGitPull) map[string]any {
	login := fakeGitUserLogin
	if p.userID != fakeGitUserID {
		login = "someone-else"
	}
	return map[string]any{"number": p.number, "html_url": fmt.Sprintf("https://github.com/%s/%s/pull/%d", f.gh.owner, f.gh.repo, p.number),
		"state": "open", "title": p.title, "head": map[string]string{"ref": p.head, "sha": f.refs[p.head]}, "base": map[string]string{"ref": p.base},
		"user": map[string]any{"id": p.userID, "login": login}}
}

func (f *fakeGitData) getPulls(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if ff, failing := f.takeFail("GET pulls"); failing {
		http.Error(w, `{"message":"Server Error"}`, ff.status)
		return
	}
	head := r.URL.Query().Get("head")
	out := []map[string]any{}
	for _, p := range f.pulls {
		if f.gh.owner+":"+p.head == head {
			out = append(out, f.pullJSON(p))
		}
	}
	writeJSON(w, out)
}

func (f *fakeGitData) postPull(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Title string `json:"title"`
		Head  string `json:"head"`
		Base  string `json:"base"`
		Body  string `json:"body"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		http.Error(w, `{"message":"Problems parsing JSON"}`, http.StatusBadRequest)
		return
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.refs[body.Head] == "" || f.branchHead(body.Base) == "" {
		http.Error(w, `{"message":"Validation Failed"}`, http.StatusUnprocessableEntity)
		return
	}
	for _, p := range f.pulls {
		if p.head == body.Head {
			http.Error(w, `{"message":"A pull request already exists"}`, http.StatusUnprocessableEntity)
			return
		}
	}
	ff, failing := f.takeFail("POST pulls")
	if failing && !ff.apply {
		http.Error(w, `{"message":"Server Error"}`, ff.status)
		return
	}
	p := fakeGitPull{number: len(f.pulls) + 1, title: body.Title, head: body.Head, base: body.Base, body: body.Body, userID: fakeGitUserID}
	f.pulls = append(f.pulls, p)
	if failing {
		http.Error(w, `{"message":"Server Error"}`, ff.status)
		return
	}
	w.WriteHeader(http.StatusCreated)
	writeJSON(w, f.pullJSON(p))
}

// branchTree is the files and modes at the head of a branch toolyard made.
func (f *fakeGitData) branchTree(branch string) (files, modes map[string]string, commit fakeGitCommit, ok bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	sha := f.refs[branch]
	if commit, ok = f.commits[sha]; !ok {
		return nil, nil, commit, false
	}
	t := f.trees[commit.tree]
	return t.files, t.modes, commit, true
}

// plantBranch makes branch point at a new commit on parent whose tree is
// the root tree of files (a branch someone else prepared).
func (f *fakeGitData) plantBranch(branch, parent, message string, files map[string]string) string {
	f.mu.Lock()
	defer f.mu.Unlock()
	tree := f.treeLocked(files, map[string]string{}, "")
	sha := fakeSHA("planted", branch, tree)
	f.commits[sha] = fakeGitCommit{tree: tree, message: message, parents: []string{parent}}
	f.refs[branch] = sha
	return sha
}

// plantPull opens a pull request from branch as userID.
func (f *fakeGitData) plantPull(branch, base string, userID int64) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.pulls = append(f.pulls, fakeGitPull{number: len(f.pulls) + 1, title: "planted", head: branch, base: base, userID: userID})
}
