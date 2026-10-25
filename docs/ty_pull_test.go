package docs

// `ty skills pull` (docs/ty/ty) against a fake toolyard: it asks for a
// bundle with the token, downloads it without the token, and unpacks it
// only when every member stays inside its skill folder. The script is
// Python 3 (standard library only); the test skips when python3 is not
// installed, except in CI.

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

type tyMember struct {
	name, body, link string
	typ              byte
}

func tyBundle(t *testing.T, members []tyMember) []byte {
	t.Helper()
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	for _, m := range members {
		typ := m.typ
		if typ == 0 {
			typ = tar.TypeReg
		}
		hdr := &tar.Header{Name: m.name, Mode: 0o644, Typeflag: typ, Linkname: m.link, Format: tar.FormatPAX}
		if typ == tar.TypeReg {
			hdr.Size = int64(len(m.body))
		}
		if err := tw.WriteHeader(hdr); err != nil {
			t.Fatal(err)
		}
		if typ == tar.TypeReg {
			if _, err := tw.Write([]byte(m.body)); err != nil {
				t.Fatal(err)
			}
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := gz.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

// tyFake answers POST /v1/skills/bundle with a ticket path and serves the
// bundle once at that path.
type tyFake struct {
	t      *testing.T
	srv    *httptest.Server
	mu     sync.Mutex
	bundle []byte
	asked  []string
	errors []string
}

func newTyFake(t *testing.T) *tyFake {
	f := &tyFake{t: t}
	f.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()
		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/v1/skills/bundle":
			if r.Header.Get("Authorization") != "Bearer ty-test-token" {
				f.errors = append(f.errors, "bundle request without the token")
				http.Error(w, `{"error":"unauthorized"}`, http.StatusUnauthorized)
				return
			}
			var body struct {
				Skills []string `json:"skills"`
			}
			_ = json.NewDecoder(r.Body).Decode(&body)
			f.asked = append(f.asked, body.Skills...)
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(map[string]any{
				"url":    "/v1/skills/bundle/ticket-1",
				"skills": []map[string]string{{"name": "deploy", "uri": "skill://team/ops/deploy/SKILL.md", "source": "team", "repo": "acme/skills@0123456789ab", "commit": "0123456789abcdef0123456789abcdef01234567"}},
			})
		case r.Method == http.MethodGet && r.URL.Path == "/v1/skills/bundle/ticket-1":
			if r.Header.Get("Authorization") != "" {
				f.errors = append(f.errors, "the download carried the token")
			}
			w.Header().Set("Content-Type", "application/gzip")
			_, _ = w.Write(f.bundle)
		default:
			f.errors = append(f.errors, "unexpected "+r.Method+" "+r.URL.Path)
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(f.srv.Close)
	return f
}

// runTy runs `ty skills pull` and returns its exit code, stdout and stderr.
func (f *tyFake) runTy(t *testing.T, dir string, args ...string) (int, string, string) {
	t.Helper()
	python, err := exec.LookPath("python3")
	if err != nil {
		if os.Getenv("CI") != "" {
			t.Fatal("python3 is required in CI to test ty")
		}
		t.Skip("python3 is not installed")
	}
	home := t.TempDir()
	tokenFile := filepath.Join(home, "token")
	if err := os.WriteFile(tokenFile, []byte("ty-test-token\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	script, err := filepath.Abs(filepath.Join("ty", "ty"))
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(python, append([]string{"-I", script, "skills", "pull", "deploy", "--dir", dir}, args...)...)
	cmd.Dir = home
	cmd.Env = []string{"HOME=" + home, "PATH=" + os.Getenv("PATH"), "TOOLYARD_URL=" + f.srv.URL, "TOOLYARD_TOKEN_FILE=" + tokenFile}
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	err = cmd.Run()
	code := 0
	if ee, ok := err.(*exec.ExitError); ok {
		code = ee.ExitCode()
	} else if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(stdout.String()+stderr.String(), "ty-test-token") {
		t.Fatal("ty printed the token")
	}
	return code, stdout.String(), stderr.String()
}

func (f *tyFake) check(t *testing.T) {
	t.Helper()
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, e := range f.errors {
		t.Error(e)
	}
}

func listDir(t *testing.T, dir string) []string {
	t.Helper()
	var out []string
	_ = filepath.Walk(dir, func(p string, info os.FileInfo, err error) error {
		if err == nil && p != dir {
			rel, _ := filepath.Rel(dir, p)
			out = append(out, filepath.ToSlash(rel))
		}
		return nil
	})
	return out
}

func TestTySkillsPullInstallsSkills(t *testing.T) {
	f := newTyFake(t)
	dir := filepath.Join(t.TempDir(), "skills")
	f.bundle = tyBundle(t, []tyMember{
		{name: "deploy/SKILL.md", body: "---\nname: deploy\ndescription: Deploy\n---\nBody\n"},
		{name: "deploy/references/checklist.md", body: "one\n"},
	})
	code, out, errOut := f.runTy(t, dir)
	if code != 0 {
		t.Fatalf("exit %d: %s", code, errOut)
	}
	var got struct {
		Dir    string `json:"dir"`
		Skills []struct {
			Name     string `json:"name"`
			Files    int    `json:"files"`
			Replaced bool   `json:"replaced"`
			Commit   string `json:"commit"`
		} `json:"skills"`
	}
	if err := json.Unmarshal([]byte(out), &got); err != nil {
		t.Fatalf("output %q: %v", out, err)
	}
	if len(got.Skills) != 1 || got.Skills[0].Name != "deploy" || got.Skills[0].Files != 2 || got.Skills[0].Replaced {
		t.Fatalf("output %+v", got)
	}
	if b, err := os.ReadFile(filepath.Join(dir, "deploy", "references", "checklist.md")); err != nil || string(b) != "one\n" {
		t.Fatalf("checklist %q %v", b, err)
	}
	if entries := listDir(t, dir); len(entries) != 4 { // deploy, deploy/SKILL.md, deploy/references, deploy/references/checklist.md
		t.Fatalf("left behind: %v", entries)
	}

	// An existing skill folder stays unless --force.
	if err := os.WriteFile(filepath.Join(dir, "deploy", "local-note.md"), []byte("mine\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	f.bundle = tyBundle(t, []tyMember{{name: "deploy/SKILL.md", body: "---\nname: deploy\ndescription: Deploy v2\n---\n"}})
	if code, _, errOut := f.runTy(t, dir); code != 2 || !strings.Contains(errOut, "--force") {
		t.Fatalf("without --force: exit %d %s", code, errOut)
	}
	if _, err := os.Stat(filepath.Join(dir, "deploy", "local-note.md")); err != nil {
		t.Fatal("the existing folder was changed without --force")
	}
	if code, out, errOut := f.runTy(t, dir, "--force"); code != 0 || !strings.Contains(out, `"replaced": true`) {
		t.Fatalf("with --force: exit %d %s %s", code, out, errOut)
	}
	if _, err := os.Stat(filepath.Join(dir, "deploy", "local-note.md")); !os.IsNotExist(err) {
		t.Fatal("--force kept the old folder's files")
	}
	if entries := listDir(t, dir); len(entries) != 2 {
		t.Fatalf("after --force: %v", entries)
	}
	f.check(t)
}

func TestTySkillsPullRefusesUnsafeBundles(t *testing.T) {
	cases := map[string][]tyMember{
		"parent folder": {{name: "deploy/SKILL.md", body: "x"}, {name: "deploy/../../escape.md", body: "x"}},
		"absolute path": {{name: "/tmp/ty-escape.md", body: "x"}},
		"symlink":       {{name: "deploy/SKILL.md", body: "x"}, {name: "deploy/passwd", typ: tar.TypeSymlink, link: "/etc/passwd"}},
		"hard link":     {{name: "deploy/SKILL.md", body: "x"}, {name: "deploy/hard", typ: tar.TypeLink, link: "deploy/SKILL.md"}},
		"device":        {{name: "deploy/null", typ: tar.TypeChar}},
		"other folder":  {{name: "deploy/SKILL.md", body: "x"}, {name: "other/SKILL.md", body: "x"}},
		"backslash":     {{name: `deploy\..\..\escape.md`, body: "x"}},
	}
	for name, members := range cases {
		t.Run(name, func(t *testing.T) {
			f := newTyFake(t)
			root := t.TempDir()
			dir := filepath.Join(root, "skills")
			f.bundle = tyBundle(t, members)
			code, _, errOut := f.runTy(t, dir)
			if code != 2 || !strings.Contains(errOut, "refusing the bundle") {
				t.Fatalf("exit %d: %s", code, errOut)
			}
			// Nothing was installed and nothing was left behind.
			if entries := listDir(t, dir); len(entries) != 0 {
				t.Fatalf("installed or left behind: %v", entries)
			}
			if entries := listDir(t, root); len(entries) != 1 {
				t.Fatalf("wrote outside --dir: %v", entries)
			}
			if _, err := os.Lstat("/tmp/ty-escape.md"); err == nil {
				t.Fatal("wrote an absolute path")
			}
			f.check(t)
		})
	}
}
