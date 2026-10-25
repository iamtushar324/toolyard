package skillhub

import (
	"fmt"
	"net/url"
	"path"
	"regexp"
	"strings"
	"unicode/utf8"
)

var (
	ownerRe = regexp.MustCompile(`^[A-Za-z0-9](?:[A-Za-z0-9-]{0,38})$`)
	repoRe  = regexp.MustCompile(`^[A-Za-z0-9._-]{1,100}$`)
	shaRe   = regexp.MustCompile(`^(?:[0-9a-f]{40}|[0-9a-f]{64})$`)
	slugRe  = regexp.MustCompile(`^[a-z0-9]+(-[a-z0-9]+)*$`)
)

// maxSlugLen caps a source slug.
const maxSlugLen = 40

// ParseURL reads a GitHub repository reference: owner/repo,
// https://github.com/owner/repo (with or without .git),
// .../tree/<branch>/<path> or .../blob/<branch>/<path>/SKILL.md. A branch
// name may hold "/", which only the repository's branch list can settle:
// ParseURL guesses the first segment is the branch; Inspect resolves it.
func (s *Service) ParseURL(raw string) (RepoRef, error) {
	ref, _, err := parseRepoURL(raw)
	return ref, err
}

// parseRepoURL parses raw (see ParseURL) and also returns the unresolved
// "<branch>/<path>" of a tree or blob URL ("" for a repository URL).
func parseRepoURL(raw string) (RepoRef, string, error) {
	s := strings.TrimSpace(raw)
	if s == "" {
		return RepoRef{}, "", fmt.Errorf("%w: a GitHub repository URL or owner/repo is required", ErrInvalid)
	}
	var p string
	switch {
	case strings.HasPrefix(s, "git@github.com:"):
		p = strings.TrimPrefix(s, "git@github.com:")
	case strings.Contains(s, "://"):
		u, err := url.Parse(s)
		if err != nil {
			return RepoRef{}, "", fmt.Errorf("%w: %q is not a URL", ErrInvalid, raw)
		}
		if u.Scheme != "https" && u.Scheme != "http" {
			return RepoRef{}, "", fmt.Errorf("%w: %q is not an https URL", ErrInvalid, raw)
		}
		if h := strings.ToLower(u.Hostname()); h != "github.com" && h != "www.github.com" {
			return RepoRef{}, "", fmt.Errorf("%w: only github.com repositories are supported (got %s)", ErrInvalid, u.Hostname())
		}
		p = u.Path
	case strings.HasPrefix(strings.ToLower(s), "github.com/"), strings.HasPrefix(strings.ToLower(s), "www.github.com/"):
		p = s[strings.IndexByte(s, '/')+1:]
		if i := strings.IndexAny(p, "?#"); i >= 0 {
			p = p[:i]
		}
	default:
		p = s
	}
	segs := splitPath(p)
	if len(segs) < 2 {
		return RepoRef{}, "", fmt.Errorf("%w: %q does not name a repository (owner/repo)", ErrInvalid, raw)
	}
	ref := RepoRef{Owner: segs[0], Repo: strings.TrimSuffix(segs[1], ".git")}
	if !ownerRe.MatchString(ref.Owner) || !repoRe.MatchString(ref.Repo) || ref.Repo == "." || ref.Repo == ".." {
		return RepoRef{}, "", fmt.Errorf("%w: %q is not a valid owner/repo", ErrInvalid, ref.Owner+"/"+ref.Repo)
	}
	var rest string
	if len(segs) > 3 {
		switch segs[2] {
		case "tree":
			rest = strings.Join(segs[3:], "/")
		case "blob":
			// A file URL: the skill is the file's folder.
			rest = path.Dir(strings.Join(segs[3:], "/"))
			if rest == "." {
				rest = ""
			}
		}
	}
	if rest != "" {
		if _, err := cleanFolder(rest); err != nil {
			return RepoRef{}, "", err
		}
		b, f := splitTreeRest(rest, nil)
		ref.Branch, ref.Path = b, f
	}
	return ref, rest, nil
}

// splitTreeRest splits "<branch>/<path>" at the longest prefix that is one
// of branches; with no match (or no branch list) the first segment is the
// branch.
func splitTreeRest(rest string, branches []string) (branch, folder string) {
	segs := splitPath(rest)
	if len(segs) == 0 {
		return "", ""
	}
	if len(branches) > 0 {
		known := make(map[string]bool, len(branches))
		for _, b := range branches {
			known[b] = true
		}
		for i := len(segs); i >= 1; i-- {
			if cand := strings.Join(segs[:i], "/"); known[cand] {
				return cand, strings.Join(segs[i:], "/")
			}
		}
	}
	return segs[0], strings.Join(segs[1:], "/")
}

// validBranch reports a branch name toolyard accepts (git's ref name
// rules): at most 255 bytes of UTF-8, no "..", "//" or "@{", no control
// character, space or any of ~^:?*[\, no leading or trailing "/", no
// segment starting with ".", not ending in "." or ".lock", no leading
// "-", and not "@".
func validBranch(b string) bool {
	if b == "" || b == "@" || len(b) > 255 || !utf8.ValidString(b) {
		return false
	}
	if strings.Contains(b, "..") || strings.Contains(b, "//") || strings.Contains(b, "@{") ||
		strings.HasPrefix(b, "/") || strings.HasPrefix(b, "-") || strings.HasSuffix(b, "/") ||
		strings.HasSuffix(b, ".") || strings.HasSuffix(b, ".lock") {
		return false
	}
	for _, r := range b {
		if r < 0x20 || r == 0x7f || strings.ContainsRune(" ~^:?*[\\", r) {
			return false
		}
	}
	for _, seg := range strings.Split(b, "/") {
		if strings.HasPrefix(seg, ".") {
			return false
		}
	}
	return true
}

// splitPath splits p at "/" and drops empty segments.
func splitPath(p string) []string {
	var out []string
	for _, seg := range strings.Split(p, "/") {
		if seg != "" {
			out = append(out, seg)
		}
	}
	return out
}

// cleanFolder normalises a folder path inside a repository: no leading or
// trailing "/", no "." or ".." segments, "" for the root.
func cleanFolder(p string) (string, error) {
	segs := splitPath(strings.TrimSpace(p))
	for _, seg := range segs {
		if seg == "." || seg == ".." || !validPath(seg) {
			return "", fmt.Errorf("%w: folder %q must be a plain path inside the repository", ErrInvalid, p)
		}
	}
	return strings.Join(segs, "/"), nil
}

// slugify makes a default slug from a repository name.
func slugify(name string) string {
	var b strings.Builder
	dash := true
	for _, r := range strings.ToLower(name) {
		if r >= 'a' && r <= 'z' || r >= '0' && r <= '9' {
			b.WriteRune(r)
			dash = false
			continue
		}
		if !dash {
			b.WriteByte('-')
			dash = true
		}
	}
	out := strings.Trim(b.String(), "-")
	if len(out) > maxSlugLen {
		out = strings.Trim(out[:maxSlugLen], "-")
	}
	if out == "" {
		out = "skills"
	}
	return out
}

// slugCandidate is the n-th try at a unique slug: base, base-2, base-3, ...
func slugCandidate(base string, n int) string {
	if n <= 1 {
		return base
	}
	suffix := fmt.Sprintf("-%d", n)
	if len(base)+len(suffix) > maxSlugLen {
		base = strings.Trim(base[:maxSlugLen-len(suffix)], "-")
	}
	return base + suffix
}

// escapePath escapes each segment of a repository path for a URL path.
func escapePath(p string) string {
	segs := strings.Split(p, "/")
	for i, seg := range segs {
		segs[i] = url.PathEscape(seg)
	}
	return strings.Join(segs, "/")
}

// repoPath joins the source folder and a path inside it.
func repoPath(folder, rel string) string {
	switch {
	case folder == "":
		return rel
	case rel == "":
		return folder
	}
	return folder + "/" + rel
}
