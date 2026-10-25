package skillhub

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"errors"
	"strings"
	"testing"
)

func TestParseRepoURL(t *testing.T) {
	cases := []struct {
		in           string
		owner, repo  string
		branch, path string
		rest         string
	}{
		{in: "anthropics/skills", owner: "anthropics", repo: "skills"},
		{in: "https://github.com/anthropics/skills", owner: "anthropics", repo: "skills"},
		{in: "https://github.com/anthropics/skills.git", owner: "anthropics", repo: "skills"},
		{in: "https://github.com/anthropics/skills/", owner: "anthropics", repo: "skills"},
		{in: "github.com/anthropics/skills", owner: "anthropics", repo: "skills"},
		{in: "git@github.com:anthropics/skills.git", owner: "anthropics", repo: "skills"},
		{in: "https://github.com/o/r/tree/main/Skills/agent-tooling", owner: "o", repo: "r", branch: "main", path: "Skills/agent-tooling", rest: "main/Skills/agent-tooling"},
		{in: "https://github.com/o/r/tree/main", owner: "o", repo: "r", branch: "main", rest: "main"},
		{in: "https://github.com/o/r/blob/main/Skills/pr/SKILL.md", owner: "o", repo: "r", branch: "main", path: "Skills/pr", rest: "main/Skills/pr"},
		{in: "https://github.com/o/r/blob/dev/SKILL.md", owner: "o", repo: "r", branch: "dev", rest: "dev"},
		{in: "https://www.github.com/o/r/tree/feature/x/skills?tab=readme", owner: "o", repo: "r", branch: "feature", path: "x/skills", rest: "feature/x/skills"},
	}
	for _, c := range cases {
		ref, rest, err := parseRepoURL(c.in)
		if err != nil {
			t.Errorf("%q: %v", c.in, err)
			continue
		}
		if ref.Owner != c.owner || ref.Repo != c.repo || ref.Branch != c.branch || ref.Path != c.path || rest != c.rest {
			t.Errorf("%q: got %+v rest %q", c.in, ref, rest)
		}
	}
	for _, bad := range []string{"", "anthropics", "https://gitlab.com/o/r", "ftp://github.com/o/r", "https://github.com/o", "o/..", "-bad/repo", "https://github.com/o/r/tree/main/../x"} {
		if _, _, err := parseRepoURL(bad); !errors.Is(err, ErrInvalid) {
			t.Errorf("%q: err %v, want ErrInvalid", bad, err)
		}
	}
}

func TestSplitTreeRestUsesBranchList(t *testing.T) {
	branches := []string{"main", "feature/x", "feature"}
	b, p := splitTreeRest("feature/x/skills/pr", branches)
	if b != "feature/x" || p != "skills/pr" {
		t.Fatalf("got %q %q", b, p)
	}
	b, p = splitTreeRest("main", branches)
	if b != "main" || p != "" {
		t.Fatalf("got %q %q", b, p)
	}
	b, p = splitTreeRest("unknown/a", branches)
	if b != "unknown" || p != "a" {
		t.Fatalf("got %q %q", b, p)
	}
}

func TestSlugs(t *testing.T) {
	if got := slugify("My_Skills.Repo"); got != "my-skills-repo" {
		t.Fatalf("slugify: %q", got)
	}
	if got := slugify("___"); got != "skills" {
		t.Fatalf("slugify empty: %q", got)
	}
	long := strings.Repeat("a", 45)
	if got := slugCandidate(slugify(long), 12); len(got) > maxSlugLen || !strings.HasSuffix(got, "-12") || !slugRe.MatchString(got) {
		t.Fatalf("slugCandidate: %q", got)
	}
}

// tarOf builds a gzip'd tar from entries (raw names, no top folder added).
func tarOf(t *testing.T, entries ...*tar.Header) []byte {
	t.Helper()
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	for _, h := range entries {
		if err := tw.WriteHeader(h); err != nil {
			t.Fatal(err)
		}
		if h.Typeflag == tar.TypeReg && h.Size > 0 {
			if _, err := tw.Write(bytes.Repeat([]byte("x"), int(h.Size))); err != nil {
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

func TestReadArchiveFilters(t *testing.T) {
	tree := map[string]string{
		"README.md":                         "outside the folder",
		"skills/a/SKILL.md":                 skillMD("a", "A skill.", "", "Body.\n"),
		"skills/a/notes.md":                 "notes\n",
		"skills/a/image.png":                "\x89PNG\x00\x00binary",
		"skills/a/bad-utf8.txt":             "\xff\xfe",
		"skills/a/node_modules/x/index.js":  "module.exports = 1\n",
		"skills/a/__pycache__/m.cpython.py": "x",
		"skills/a/tool.pyc":                 "x",
		"skills/a/.DS_Store":                "x",
		"skills/a/drafts/wip.md":            "draft",
		"skills/a/link":                     symlinkPrefix + "../../README.md",
		"skills/a/big.md":                   strings.Repeat("b", 2000),
	}
	data := buildTarball(t, "o-r-abc1234", tree)
	ex, err := newExcluder([]string{"drafts/"})
	if err != nil {
		t.Fatal(err)
	}
	lim := Limits{MaxFileBytes: 1000}.effective()
	fs, err := readArchive(bytes.NewReader(data), "skills", ex, lim)
	if err != nil {
		t.Fatal(err)
	}
	got := strings.Join(sortedPaths(fs.files), ",")
	if got != "a/SKILL.md,a/notes.md" {
		t.Fatalf("kept %s", got)
	}
	codes := map[string]string{}
	for p, w := range fs.omitted {
		codes[p] = w.Code
	}
	want := map[string]string{"a/image.png": "binary", "a/bad-utf8.txt": "binary", "a/link": "symlink", "a/big.md": "too_large"}
	for p, c := range want {
		if codes[p] != c {
			t.Errorf("%s: code %q, want %q (all: %v)", p, codes[p], c, codes)
		}
	}
	if len(codes) != len(want) {
		t.Errorf("omitted %v", codes)
	}
	if !strings.Contains(strings.Join(fs.excluded, ","), "a/drafts/wip.md") {
		t.Errorf("excluded %v", fs.excluded)
	}
	sk := fs.files["a/SKILL.md"]
	if !strings.HasPrefix(sk.sha, "sha256:") || len(sk.sha) != len("sha256:")+64 || sk.media != "text/markdown" {
		t.Errorf("candidate %+v", sk)
	}
}

func TestReadArchiveRefusesUnsafePaths(t *testing.T) {
	for _, name := range []string{"top/../../etc/passwd", "/abs/path", "top/a/../../x"} {
		data := tarOf(t, &tar.Header{Typeflag: tar.TypeReg, Name: name, Size: 3, Mode: 0o644})
		if _, err := readArchive(bytes.NewReader(data), "", nil, Limits{}.effective()); !errors.Is(err, errUnsafeArchive) {
			t.Errorf("%q: err %v, want errUnsafeArchive", name, err)
		}
	}
	// Hard links and devices are left out, never followed.
	data := tarOf(t,
		&tar.Header{Typeflag: tar.TypeLink, Name: "top/s/hard", Linkname: "top/s/SKILL.md"},
		&tar.Header{Typeflag: tar.TypeChar, Name: "top/s/dev", Devmajor: 1, Devminor: 3},
	)
	fs, err := readArchive(bytes.NewReader(data), "", nil, Limits{}.effective())
	if err != nil {
		t.Fatal(err)
	}
	if len(fs.files) != 0 || fs.omitted["s/hard"].Code != "symlink" || fs.omitted["s/dev"].Code != "special" {
		t.Fatalf("files %v omitted %v", fs.files, fs.omitted)
	}
}

func TestReadArchiveLimits(t *testing.T) {
	tree := map[string]string{}
	for i := 0; i < 5; i++ {
		tree["s/f"+string(rune('a'+i))+".md"] = strings.Repeat("x", 100)
	}
	data := buildTarball(t, "top", tree)
	if _, err := readArchive(bytes.NewReader(data), "", nil, Limits{MaxFiles: 4}.effective()); !errors.Is(err, ErrLimit) {
		t.Fatalf("file limit: %v", err)
	}
	if _, err := readArchive(bytes.NewReader(data), "", nil, Limits{MaxTotalBytes: 450}.effective()); !errors.Is(err, ErrLimit) {
		t.Fatalf("byte limit: %v", err)
	}
	if _, err := readArchive(bytes.NewReader(data), "", nil, Limits{MaxTotalBytes: 500, MaxFiles: 5}.effective()); err != nil {
		t.Fatalf("at the limits: %v", err)
	}
}

func TestFindSecret(t *testing.T) {
	// Built at run time so the source never holds a credential-shaped
	// string.
	cases := map[string]string{
		"gh" + "p_" + strings.Repeat("A1b2", 9):                            "a GitHub token",
		"github" + "_pat_" + strings.Repeat("x", 30):                       "a GitHub token",
		"AK" + "IA" + "ABCDEFGHIJKLMNOP":                                   "an AWS access key",
		"xo" + "xb-" + "1234567890-abcdef":                                 "a Slack token",
		"sk" + "-ant-" + strings.Repeat("k", 30):                           "an Anthropic API key",
		"sk" + "-proj-" + strings.Repeat("k", 30):                          "an OpenAI API key",
		"sk" + "-" + strings.Repeat("Z9", 20):                              "an API key",
		"-----BEGIN RSA " + "PRIVATE KEY-----\n" + strings.Repeat("M", 64): "a private key",
	}
	for content, want := range cases {
		if got := findSecret([]byte("text before " + content + " after")); got != want {
			t.Errorf("%q: got %q, want %q", content[:8], got, want)
		}
	}
	clean := []string{
		"export GITHUB_TOKEN=$(gh auth token)",
		"Use a key like sk-... from the console.",
		"AK" + "IA" + "IOSFODNN7EXAMPLE is AWS's documented example.",
		"-----BEGIN PRIVATE " + "KEY----- starts a PEM block.",
	}
	for _, c := range clean {
		if got := findSecret([]byte(c)); got != "" {
			t.Errorf("%q: flagged as %q", c, got)
		}
	}
}

func TestParseSkillMD(t *testing.T) {
	meta, warns, skip := parseSkillMD("s/pr/SKILL.md", "pr",
		[]byte(skillMD("pr", "Merge pull requests.", "license: MIT\nmetadata:\n  toolyard-runs: host\n  version: 3\n  created: 2026-01-02", "# PR\n")))
	if skip || len(warns) != 0 {
		t.Fatalf("skip %v warns %v", skip, warns)
	}
	if meta.name != "pr" || meta.runs != RunsHost {
		t.Fatalf("meta %+v", meta)
	}
	md, _ := meta.frontmatter["metadata"].(map[string]any)
	if v, _ := md["created"].(string); v != "2026-01-02" {
		t.Errorf("timestamp not kept as written: %#v", md["created"])
	}
	if md["version"] != int64(3) {
		t.Errorf("metadata.version = %#v", md["version"])
	}

	skipCases := map[string]string{
		skillMD("other", "x", "", ""):                "name_mismatch",
		"---\nname: pr\n---\nbody":                   "missing_field",
		"no frontmatter":                             "missing_frontmatter",
		"---\nname: pr\ndescription: [unclosed\n---": "invalid_yaml",
		"---\nname: pr\ndescription: x\n":            "missing_frontmatter",
	}
	for content, code := range skipCases {
		_, warns, skip := parseSkillMD("s/pr/SKILL.md", "pr", []byte(content))
		if !skip || len(warns) == 0 || warns[len(warns)-1].Code != code || warns[len(warns)-1].Severity != "skip" {
			t.Errorf("%q: skip %v warns %v, want %s", content, skip, warns, code)
		}
	}

	_, warns, skip = parseSkillMD("s/pr/SKILL.md", "pr", []byte(skillMD("pr", strings.Repeat("d", 1100), "extra: 1\nmetadata:\n  toolyard-runs: laptop", "")))
	if skip {
		t.Fatal("served skill skipped")
	}
	codes := []string{}
	for _, w := range warns {
		codes = append(codes, w.Code)
	}
	if got := strings.Join(codes, ","); got != "description_too_long,unknown_field,invalid_runs" {
		t.Fatalf("warnings %s", got)
	}
	// BOM and CRLF line ends are accepted.
	if _, _, skip := parseSkillMD("SKILL.md", "pr", []byte("\ufeff---\r\nname: pr\r\ndescription: d\r\n---\r\nbody")); skip {
		t.Fatal("BOM/CRLF SKILL.md skipped")
	}
}

func TestYAMLAliasBomb(t *testing.T) {
	var b strings.Builder
	b.WriteString("name: pr\ndescription: d\na: &a [x, x, x, x, x, x, x, x, x, x]\n")
	prev := "a"
	for i := 0; i < 8; i++ {
		cur := "l" + string(rune('a'+i))
		b.WriteString(cur + ": &" + cur + " [*" + prev + ", *" + prev + ", *" + prev + ", *" + prev + ", *" + prev + ", *" + prev + ", *" + prev + ", *" + prev + ", *" + prev + ", *" + prev + "]\n")
		prev = cur
	}
	if _, err := parseYAMLMap(b.String()); err == nil {
		t.Fatal("alias expansion was not capped")
	}
}

func TestExcluder(t *testing.T) {
	ex, err := newExcluder([]string{"*.log", "docs/internal/", "**/fixtures/**", "scratch[0-9].md"})
	if err != nil {
		t.Fatal(err)
	}
	for p, want := range map[string]bool{
		"a/SKILL.md":               false,
		"a/run.log":                true,
		"a/.git/config":            true,
		"a/node_modules/x/y.js":    true,
		"docs/internal/x.md":       true,
		"a/docs/internal/x.md":     false,
		"a/fixtures/one.json":      true,
		"a/b/fixtures/c/two.json":  true,
		"a/scratch1.md":            true,
		"a/scratchX.md":            false,
		"a/__pycache__/m.pyc":      true,
		"a/.DS_Store":              true,
		"a/notes.DS_Store.md":      false,
		"a/internal/docs/x.md":     false,
		"a/node_modules_backup.md": false,
	} {
		if got := ex.excluded(p); got != want {
			t.Errorf("%s: excluded %v, want %v", p, got, want)
		}
	}
	if _, err := validateGlobs([]string{"bad["}); !errors.Is(err, ErrInvalid) {
		t.Fatalf("bad glob: %v", err)
	}
}

func TestDiscoverNestedAndRoot(t *testing.T) {
	fs := newFileSet(Limits{}.effective())
	add := func(p, c string) {
		if err := fs.add(p, []byte(c)); err != nil {
			t.Fatal(err)
		}
	}
	add("SKILL.md", skillMD("toolbox", "Root skill.", "", ""))
	add("outer/SKILL.md", skillMD("outer", "Outer.", "", ""))
	add("outer/ref.md", "ref")
	add("outer/inner/SKILL.md", skillMD("inner", "Inner.", "", ""))
	add("outer/inner/x.md", "x")
	add("wrong/SKILL.md", skillMD("misnamed", "Bad.", "", ""))
	fs.omit("outer/inner/blob.bin", "binary", "omitted")
	b, err := discover(fs, "", "toolbox", nil)
	if err != nil {
		t.Fatal(err)
	}
	var paths []string
	for _, sk := range b.skills {
		paths = append(paths, sk.path)
	}
	if got := strings.Join(paths, "|"); got != "|outer|outer/inner" {
		t.Fatalf("skills %q", got)
	}
	inner := map[string]string{}
	for _, f := range b.files {
		inner[f.path] = f.skill
	}
	if inner["outer/inner/x.md"] != "outer/inner" || inner["outer/ref.md"] != "outer" || inner["wrong/SKILL.md"] != "" {
		t.Fatalf("innermost skills %v", inner)
	}
	var codes []string
	for _, w := range b.warnings {
		codes = append(codes, w.Code+":"+w.Path)
	}
	if got := strings.Join(codes, ","); got != "name_mismatch:wrong/SKILL.md,binary:outer/inner/blob.bin" {
		t.Fatalf("warnings %s", got)
	}
	if b.skills[1].filesDigest == "" || b.skills[1].filesDigest == b.skills[2].filesDigest {
		t.Fatal("file-set digests missing or equal")
	}
	if _, err := discover(fs, "", "toolbox", nil); err != nil {
		t.Fatal(err)
	}
	fs.lim.skills = 2
	if _, err := discover(fs, "", "toolbox", nil); !errors.Is(err, ErrLimit) {
		t.Fatalf("skill limit: %v", err)
	}
}

func TestSliceLines(t *testing.T) {
	text := "a\nb\nc\nd\n"
	if got, total, more := sliceLines(text, 1, 2); got != "b\nc\n" || total != 4 || !more {
		t.Fatalf("got %q %d %v", got, total, more)
	}
	if got, total, more := sliceLines(text, 2, 0); got != "c\nd\n" || total != 4 || more {
		t.Fatalf("got %q %d %v", got, total, more)
	}
	if got, total, _ := sliceLines("x", 5, 1); got != "" || total != 1 {
		t.Fatalf("got %q %d", got, total)
	}
}

func TestSafeRepoPath(t *testing.T) {
	for p, want := range map[string]bool{
		"skills/a/SKILL.md": true,
		"a":                 true,
		"":                  false,
		"/abs":              false,
		"a/../b":            false,
		"a/./b":             false,
		"a//b":              false,
		"a\\b":              false,
		"a/\x00":            false,
	} {
		if got := safeRepoPath(p); got != want {
			t.Errorf("safeRepoPath(%q) = %v, want %v", p, got, want)
		}
	}
}
