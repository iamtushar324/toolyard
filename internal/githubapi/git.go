package githubapi

// Git data and pull request calls for the skills hub's change proposals
// (skills.propose_change): read a commit and its trees, write a tree, a
// commit and a branch, open a pull request, and find the pull request of a
// branch again. As for the rest of the package: no write is retried, errors
// omit provider bodies, and the bearer never follows a redirect. A caller
// that gets an Uncertain error from a write reconciles (GetBranch,
// FindPullByHead) before it writes again.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"regexp"
	"strings"
)

var (
	shaPattern    = regexp.MustCompile(`^[0-9a-f]{40}(?:[0-9a-f]{24})?$`)
	branchPattern = regexp.MustCompile(`^[A-Za-z0-9._-]+(?:/[A-Za-z0-9._-]+)*$`)
)

// NotFound reports a 404 answer.
func NotFound(err error) bool { return StatusOf(err) == http.StatusNotFound }

// StatusOf is the HTTP status of an APIError, or 0.
func StatusOf(err error) int {
	var e *APIError
	if errors.As(err, &e) {
		return e.Status
	}
	return 0
}

// Uncertain reports whether a failed write may still have reached GitHub:
// the request got no answer (a network failure, a deadline), or GitHub
// answered with a server error. Reconcile before writing again.
func Uncertain(err error) bool {
	if err == nil {
		return false
	}
	if errors.Is(err, errTransport) || errors.Is(err, context.DeadlineExceeded) || errors.Is(err, context.Canceled) {
		return true
	}
	return StatusOf(err) >= 500
}

// repoBase is "/repos/<owner>/<repo>" for a safe owner and repository.
func repoBase(owner, repo string) (string, error) {
	if !repoPart.MatchString(owner) || !repoPart.MatchString(repo) || owner == "." || owner == ".." || repo == "." || repo == ".." {
		return "", errors.New("a valid GitHub owner and repository are required")
	}
	return "/repos/" + owner + "/" + repo, nil
}

func checkSHA(sha string) error {
	if !shaPattern.MatchString(sha) {
		return errors.New("a full lowercase commit SHA is required")
	}
	return nil
}

// checkBranch accepts a branch name made of safe path segments.
func checkBranch(name string) error {
	if len(name) > 200 || !branchPattern.MatchString(name) || strings.Contains(name, "..") || strings.HasSuffix(name, ".lock") {
		return errors.New("the branch name is not allowed")
	}
	for _, seg := range strings.Split(name, "/") {
		if strings.HasPrefix(seg, ".") || strings.HasSuffix(seg, ".lock") {
			return errors.New("the branch name is not allowed")
		}
	}
	return nil
}

// Commit is a Git commit object.
type Commit struct {
	SHA     string `json:"sha"`
	Message string `json:"message"`
	Tree    struct {
		SHA string `json:"sha"`
	} `json:"tree"`
	Parents []struct {
		SHA string `json:"sha"`
	} `json:"parents"`
}

// GetCommit reads a commit object.
func (c *Client) GetCommit(ctx context.Context, owner, repo, sha string) (*Commit, error) {
	base, err := repoBase(owner, repo)
	if err != nil {
		return nil, err
	}
	if err := checkSHA(sha); err != nil {
		return nil, err
	}
	var out Commit
	if err := c.Do(ctx, http.MethodGet, base+"/git/commits/"+sha, nil, &out); err != nil {
		return nil, err
	}
	if out.SHA == "" || out.Tree.SHA == "" {
		return nil, errors.New("GitHub did not return the commit")
	}
	return &out, nil
}

// TreeEntry is one entry of a tree: mode 100644 (file), 100755
// (executable), 120000 (symbolic link), 040000 (folder) or 160000
// (submodule).
type TreeEntry struct {
	Path string `json:"path"`
	Mode string `json:"mode"`
	Type string `json:"type"` // blob | tree | commit
	SHA  string `json:"sha"`
	Size int64  `json:"size,omitempty"`
}

// Tree is a Git tree.
type Tree struct {
	SHA       string      `json:"sha"`
	Tree      []TreeEntry `json:"tree"`
	Truncated bool        `json:"truncated"`
}

// GetTree reads a tree; recursive lists every entry below it.
func (c *Client) GetTree(ctx context.Context, owner, repo, sha string, recursive bool) (*Tree, error) {
	base, err := repoBase(owner, repo)
	if err != nil {
		return nil, err
	}
	if err := checkSHA(sha); err != nil {
		return nil, err
	}
	path := base + "/git/trees/" + sha
	if recursive {
		path += "?recursive=1"
	}
	var out Tree
	if err := c.Do(ctx, http.MethodGet, path, nil, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// TreeChange is one entry of a new tree: a file with Content (its full
// text), or, with Delete, the removal of a file.
type TreeChange struct {
	Path    string
	Mode    string // 100644 or 100755
	Content string
	Delete  bool
}

// CreateTree writes a tree: baseTree with changes applied. It returns the
// new tree's SHA. GitHub writes each content as a blob.
func (c *Client) CreateTree(ctx context.Context, owner, repo, baseTree string, changes []TreeChange) (string, error) {
	base, err := repoBase(owner, repo)
	if err != nil {
		return "", err
	}
	if err := checkSHA(baseTree); err != nil {
		return "", err
	}
	if len(changes) == 0 {
		return "", errors.New("a tree change needs at least one file")
	}
	entries := make([]map[string]any, 0, len(changes))
	for _, ch := range changes {
		if ch.Path == "" || strings.HasPrefix(ch.Path, "/") {
			return "", errors.New("tree paths must be relative")
		}
		if ch.Mode != "100644" && ch.Mode != "100755" {
			return "", fmt.Errorf("tree entry %s: mode %q is not a file mode", ch.Path, ch.Mode)
		}
		e := map[string]any{"path": ch.Path, "mode": ch.Mode, "type": "blob"}
		if ch.Delete {
			e["sha"] = nil // GitHub removes the path
		} else {
			e["content"] = ch.Content
		}
		entries = append(entries, e)
	}
	var out struct {
		SHA string `json:"sha"`
	}
	if err := c.Do(ctx, http.MethodPost, base+"/git/trees", map[string]any{"base_tree": baseTree, "tree": entries}, &out); err != nil {
		return "", err
	}
	if out.SHA == "" {
		return "", errors.New("GitHub did not return the tree")
	}
	return out.SHA, nil
}

// CreateCommit writes a commit object of tree with parents. The author
// and committer are the token's user.
func (c *Client) CreateCommit(ctx context.Context, owner, repo, message, tree string, parents []string) (string, error) {
	base, err := repoBase(owner, repo)
	if err != nil {
		return "", err
	}
	if err := checkSHA(tree); err != nil {
		return "", err
	}
	for _, p := range parents {
		if err := checkSHA(p); err != nil {
			return "", err
		}
	}
	var out struct {
		SHA string `json:"sha"`
	}
	if err := c.Do(ctx, http.MethodPost, base+"/git/commits", map[string]any{"message": message, "tree": tree, "parents": parents}, &out); err != nil {
		return "", err
	}
	if out.SHA == "" {
		return "", errors.New("GitHub did not return the commit")
	}
	return out.SHA, nil
}

// Ref is a Git reference.
type Ref struct {
	Ref    string `json:"ref"`
	Object struct {
		SHA  string `json:"sha"`
		Type string `json:"type"`
	} `json:"object"`
}

// GetBranch reads refs/heads/<branch>; it returns nil and no error when the
// branch does not exist.
func (c *Client) GetBranch(ctx context.Context, owner, repo, branch string) (*Ref, error) {
	base, err := repoBase(owner, repo)
	if err != nil {
		return nil, err
	}
	if err := checkBranch(branch); err != nil {
		return nil, err
	}
	var raw json.RawMessage
	err = c.Do(ctx, http.MethodGet, base+"/git/ref/heads/"+branch, nil, &raw)
	if NotFound(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var out Ref
	if len(raw) == 0 || raw[0] != '{' || json.Unmarshal(raw, &out) != nil || out.Ref != "refs/heads/"+branch || out.Object.SHA == "" {
		// A list answers a prefix of other refs: this exact branch
		// does not exist.
		return nil, nil
	}
	return &out, nil
}

// CreateBranch makes refs/heads/<branch> point at sha. GitHub refuses
// (HTTP 422) a branch that already exists.
func (c *Client) CreateBranch(ctx context.Context, owner, repo, branch, sha string) (*Ref, error) {
	base, err := repoBase(owner, repo)
	if err != nil {
		return nil, err
	}
	if err := checkBranch(branch); err != nil {
		return nil, err
	}
	if err := checkSHA(sha); err != nil {
		return nil, err
	}
	var out Ref
	if err := c.Do(ctx, http.MethodPost, base+"/git/refs", map[string]any{"ref": "refs/heads/" + branch, "sha": sha}, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// PullRequest is the part of a pull request the hub reads.
type PullRequest struct {
	Number  int    `json:"number"`
	HTMLURL string `json:"html_url"`
	State   string `json:"state"`
	Title   string `json:"title"`
	// User is who opened it.
	User struct {
		ID    int64  `json:"id"`
		Login string `json:"login"`
	} `json:"user"`
	Head struct {
		Ref string `json:"ref"`
		SHA string `json:"sha"`
	} `json:"head"`
	Base struct {
		Ref string `json:"ref"`
	} `json:"base"`
}

// NewPull opens a pull request from Head (a branch of the same
// repository) into Base.
type NewPull struct {
	Title string
	Head  string
	Base  string
	Body  string
}

// CreatePull opens a pull request. GitHub refuses (HTTP 422) a second open
// pull request for the same head and base.
func (c *Client) CreatePull(ctx context.Context, owner, repo string, p NewPull) (*PullRequest, error) {
	base, err := repoBase(owner, repo)
	if err != nil {
		return nil, err
	}
	if err := checkBranch(p.Head); err != nil {
		return nil, err
	}
	if strings.TrimSpace(p.Title) == "" || strings.TrimSpace(p.Base) == "" {
		return nil, errors.New("a pull request needs a title and a base branch")
	}
	var out PullRequest
	if err := c.Do(ctx, http.MethodPost, base+"/pulls", map[string]any{"title": p.Title, "head": p.Head, "base": p.Base, "body": p.Body}, &out); err != nil {
		return nil, err
	}
	if out.HTMLURL == "" || out.Number <= 0 {
		return nil, errors.New("GitHub did not return the pull request")
	}
	return &out, nil
}

// FindPullByHead returns the newest pull request (open or closed) whose
// head is branch in the same repository, or nil when there is none.
func (c *Client) FindPullByHead(ctx context.Context, owner, repo, branch string) (*PullRequest, error) {
	base, err := repoBase(owner, repo)
	if err != nil {
		return nil, err
	}
	if err := checkBranch(branch); err != nil {
		return nil, err
	}
	q := url.Values{"head": {owner + ":" + branch}, "state": {"all"}, "per_page": {"10"}}
	var rows []PullRequest
	if err := c.Do(ctx, http.MethodGet, base+"/pulls?"+q.Encode(), nil, &rows); err != nil {
		return nil, err
	}
	for i := range rows {
		if rows[i].Head.Ref == branch && rows[i].HTMLURL != "" {
			return &rows[i], nil
		}
	}
	return nil, nil
}
