package skillhub

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode"
)

const (
	defaultGitHubAPI = "https://api.github.com"
	// codeloadHost serves GitHub's archive downloads (the tarball
	// endpoint redirects there).
	codeloadHost     = "codeload.github.com"
	githubAPIVersion = "2022-11-28"
	userAgent        = "toolyard-skillhub"
	// maxBranches caps the branch list Inspect reads.
	maxBranches = 1000
	// maxJSONBytes caps a JSON answer from GitHub.
	maxJSONBytes = 16 << 20
	// maxArchiveBytes caps the uncompressed size of a repository archive
	// toolyard reads through (it keeps only the source folder's files).
	maxArchiveBytes = 4 << 30
	// apiTimeout bounds one API call; archiveTimeout one archive download.
	apiTimeout     = 30 * time.Second
	archiveTimeout = 5 * time.Minute
	// maxCompareFiles is the most files GitHub lists in a comparison; a
	// list that long may be cut, so the update downloads everything.
	maxCompareFiles = 300
	// maxIncrementalFiles is the most changed files under the source folder
	// an incremental update fetches one by one.
	maxIncrementalFiles = 100
	// maxRateBlock caps how long a rate limit answer pauses calls.
	maxRateBlock = time.Hour
	// maxAPIRedirects caps the redirects an API call follows (a renamed
	// or transferred repository answers 301 or 307).
	maxAPIRedirects = 3
)

// ghError is a GitHub answer toolyard could not use. It never holds a
// token: messages come from GitHub's JSON "message" field.
type ghError struct {
	status  int
	msg     string
	retryAt time.Time // set for a rate limit
}

// Is makes a rate limit answer match ErrRateLimited (errors.Is), however
// it is wrapped.
func (e *ghError) Is(target error) bool { return target == ErrRateLimited && !e.retryAt.IsZero() }

func (e *ghError) Error() string {
	if !e.retryAt.IsZero() {
		return fmt.Sprintf("github: rate limited until %s", e.retryAt.UTC().Format(time.RFC3339))
	}
	if e.msg == "" {
		return fmt.Sprintf("github: HTTP %d", e.status)
	}
	return fmt.Sprintf("github: HTTP %d: %s", e.status, e.msg)
}

// isGHStatus reports a ghError with one of the statuses.
func isGHStatus(err error, statuses ...int) bool {
	var ge *ghError
	if !errors.As(err, &ge) {
		return false
	}
	for _, s := range statuses {
		if ge.status == s {
			return true
		}
	}
	return false
}

// retryAfter returns when a rate-limited call may be tried again.
func retryAfter(err error) (time.Time, bool) {
	var ge *ghError
	if errors.As(err, &ge) && !ge.retryAt.IsZero() {
		return ge.retryAt, true
	}
	return time.Time{}, false
}

// rateLimits remembers, per credential, until when GitHub said to wait.
type rateLimits struct {
	mu    sync.Mutex
	until map[string]time.Time
	now   func() time.Time
}

func (r *rateLimits) blockedUntil(key string) (time.Time, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	t, ok := r.until[key]
	if !ok {
		return time.Time{}, false
	}
	if !r.now().Before(t) {
		delete(r.until, key)
		return time.Time{}, false
	}
	return t, true
}

func (r *rateLimits) block(key string, until time.Time) {
	now := r.now()
	if until.After(now.Add(maxRateBlock)) {
		until = now.Add(maxRateBlock)
	}
	if !until.After(now) {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if until.After(r.until[key]) {
		r.until[key] = until
	}
}

// ghClient calls the GitHub REST API with one credential.
type ghClient struct {
	base   *url.URL
	hc     *http.Client // never follows redirects
	token  string
	key    string // rate-limit key of the credential (never the token)
	limits *rateLimits
	now    func() time.Time
	// isDefault: the token is the instance-wide default read token
	// (github_access.go), not a source's own.
	isDefault bool
}

// newClient returns a client for token ("" reads anonymously).
func (s *Service) newClient(token string) *ghClient {
	key := "anonymous"
	if token != "" {
		sum := sha256.Sum256([]byte(token))
		key = "token:" + hex.EncodeToString(sum[:8])
	}
	return &ghClient{base: s.apiBase, hc: s.hc, token: token, key: key, limits: s.limits, now: s.now}
}

// endpoint is the API URL of an escaped path ("/repos/o/r").
func (c *ghClient) endpoint(p string, q url.Values) string {
	u := strings.TrimRight(c.base.String(), "/") + p
	if len(q) > 0 {
		u += "?" + q.Encode()
	}
	return u
}

// do sends one API request. With follow it follows the redirects of a
// renamed or transferred repository, only to the API's own scheme and
// host (the credential is kept, as for the first request); without it
// the caller handles a redirect. A rate limit answer (and a call made
// while GitHub said to wait) is a ghError that carries retryAt. The
// caller closes the body.
func (c *ghClient) do(ctx context.Context, rawURL, accept string, header http.Header, follow bool) (*http.Response, error) {
	for hops := 0; ; hops++ {
		resp, err := c.send(ctx, rawURL, accept, header)
		if err != nil || !follow || !isRedirect(resp.StatusCode) {
			return resp, err
		}
		loc, lerr := resp.Request.URL.Parse(resp.Header.Get("Location"))
		drain(resp)
		if lerr != nil || resp.Header.Get("Location") == "" || hops >= maxAPIRedirects ||
			loc.Scheme != c.base.Scheme || !strings.EqualFold(loc.Host, c.base.Host) {
			return nil, &ghError{status: resp.StatusCode, msg: "the repository moved somewhere toolyard does not follow"}
		}
		rawURL = loc.String()
	}
}

func isRedirect(status int) bool {
	switch status {
	case http.StatusMovedPermanently, http.StatusFound, http.StatusSeeOther, http.StatusTemporaryRedirect, http.StatusPermanentRedirect:
		return true
	}
	return false
}

// send sends one request (see do).
func (c *ghClient) send(ctx context.Context, rawURL, accept string, header http.Header) (*http.Response, error) {
	if until, blocked := c.limits.blockedUntil(c.key); blocked {
		return nil, &ghError{status: http.StatusTooManyRequests, retryAt: until}
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, nil)
	if err != nil {
		return nil, fmt.Errorf("github: build request: %w", err)
	}
	req.Header.Set("Accept", accept)
	req.Header.Set("X-GitHub-Api-Version", githubAPIVersion)
	req.Header.Set("User-Agent", userAgent)
	for k, vs := range header {
		for _, v := range vs {
			req.Header.Add(k, v)
		}
	}
	if c.token != "" {
		req.Header.Set("Authorization", "Bearer "+c.token)
	}
	resp, err := c.hc.Do(req)
	if err != nil {
		return nil, cleanTransportError(err)
	}
	c.noteLimits(resp)
	if resp.StatusCode == http.StatusTooManyRequests || resp.StatusCode == http.StatusForbidden {
		if until, ok := c.limitedUntil(resp); ok {
			drain(resp)
			c.limits.block(c.key, until)
			return nil, &ghError{status: resp.StatusCode, retryAt: until}
		}
	}
	return resp, nil
}

// noteLimits pauses this credential when GitHub reports no calls left.
func (c *ghClient) noteLimits(resp *http.Response) {
	if resp.Header.Get("X-RateLimit-Remaining") != "0" {
		return
	}
	if reset, err := strconv.ParseInt(resp.Header.Get("X-RateLimit-Reset"), 10, 64); err == nil {
		c.limits.block(c.key, time.Unix(reset, 0))
	}
}

// limitedUntil reads a rate limit answer: Retry-After (seconds), or
// X-RateLimit-Remaining 0 with X-RateLimit-Reset.
func (c *ghClient) limitedUntil(resp *http.Response) (time.Time, bool) {
	if ra := strings.TrimSpace(resp.Header.Get("Retry-After")); ra != "" {
		if secs, err := strconv.Atoi(ra); err == nil && secs >= 0 {
			return c.now().Add(time.Duration(secs) * time.Second), true
		}
		if t, err := http.ParseTime(ra); err == nil {
			return t, true
		}
	}
	if resp.Header.Get("X-RateLimit-Remaining") == "0" {
		if reset, err := strconv.ParseInt(resp.Header.Get("X-RateLimit-Reset"), 10, 64); err == nil {
			return time.Unix(reset, 0), true
		}
	}
	return time.Time{}, false
}

// statusError reads GitHub's error message from a failed answer and
// closes it.
func statusError(resp *http.Response) error {
	defer drain(resp)
	var body struct {
		Message string `json:"message"`
	}
	_ = json.NewDecoder(io.LimitReader(resp.Body, 64<<10)).Decode(&body)
	return &ghError{status: resp.StatusCode, msg: cleanMessage(body.Message)}
}

// cleanMessage keeps a GitHub message printable and short.
func cleanMessage(m string) string {
	m = strings.Map(func(r rune) rune {
		if unicode.IsControl(r) {
			return ' '
		}
		return r
	}, strings.TrimSpace(m))
	if len(m) > 200 {
		m = m[:200]
	}
	return m
}

// drain discards the rest of a body and closes it.
func drain(resp *http.Response) {
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 64<<10))
	_ = resp.Body.Close()
}

// cleanTransportError drops the URL from a transport error: an archive
// URL carries a temporary token in its query.
func cleanTransportError(err error) error {
	var ue *url.Error
	if errors.As(err, &ue) {
		host := ""
		if u, perr := url.Parse(ue.URL); perr == nil {
			host = u.Host
		}
		return fmt.Errorf("github: %s %s: %w", ue.Op, host, ue.Err)
	}
	return fmt.Errorf("github: %w", err)
}

// getJSON GETs an API path and decodes its JSON answer into out.
func (c *ghClient) getJSON(ctx context.Context, p string, q url.Values, out any) error {
	ctx, cancel := context.WithTimeout(ctx, apiTimeout)
	defer cancel()
	resp, err := c.do(ctx, c.endpoint(p, q), "application/vnd.github+json", nil, true)
	if err != nil {
		return err
	}
	if resp.StatusCode != http.StatusOK {
		return statusError(resp)
	}
	defer drain(resp)
	if err := json.NewDecoder(io.LimitReader(resp.Body, maxJSONBytes)).Decode(out); err != nil {
		return fmt.Errorf("github: read %s: %w", p, err)
	}
	return nil
}

// ghRepo is what toolyard reads about a repository.
type ghRepo struct {
	Name          string `json:"name"`
	Private       bool   `json:"private"`
	DefaultBranch string `json:"default_branch"`
	HTMLURL       string `json:"html_url"`
	Owner         struct {
		Login string `json:"login"`
	} `json:"owner"`
}

func repoAPIPath(owner, repo string) string {
	return "/repos/" + url.PathEscape(owner) + "/" + url.PathEscape(repo)
}

// repo reads a repository's metadata.
func (c *ghClient) repo(ctx context.Context, owner, repo string) (*ghRepo, error) {
	var r ghRepo
	if err := c.getJSON(ctx, repoAPIPath(owner, repo), nil, &r); err != nil {
		return nil, err
	}
	return &r, nil
}

// branches lists branch names, up to maxBranches; more reports a longer
// list.
func (c *ghClient) branches(ctx context.Context, owner, repo string) (names []string, more bool, err error) {
	const perPage = 100
	for page := 1; ; page++ {
		var batch []struct {
			Name string `json:"name"`
		}
		q := url.Values{"per_page": {strconv.Itoa(perPage)}, "page": {strconv.Itoa(page)}}
		if err := c.getJSON(ctx, repoAPIPath(owner, repo)+"/branches", q, &batch); err != nil {
			return nil, false, err
		}
		for _, b := range batch {
			names = append(names, b.Name)
		}
		if len(batch) < perPage {
			return names, false, nil
		}
		if len(names) >= maxBranches {
			return names[:maxBranches], true, nil
		}
	}
}

// head reads the commit SHA of a branch, through the branch's ref (so a
// tag or a commit SHA never matches a branch name). With etag it sends
// If-None-Match; a 304 answer (notModified) costs no rate limit. fresh
// sends neither and asks caches not to answer: GitHub may serve a public
// repository's ref from a cache up to 60 seconds old.
func (c *ghClient) head(ctx context.Context, owner, repo, branch, etag string, fresh bool) (sha, newETag string, notModified bool, err error) {
	if !validBranch(branch) {
		return "", "", false, fmt.Errorf("%w: branch %q is not a valid branch name", ErrInvalid, branch)
	}
	ctx, cancel := context.WithTimeout(ctx, apiTimeout)
	defer cancel()
	h := http.Header{}
	switch {
	case fresh:
		h.Set("Cache-Control", "no-cache")
	case etag != "":
		h.Set("If-None-Match", etag)
	}
	resp, err := c.do(ctx, c.endpoint(repoAPIPath(owner, repo)+"/git/ref/heads/"+escapePath(branch), nil), "application/vnd.github+json", h, true)
	if err != nil {
		return "", "", false, err
	}
	switch resp.StatusCode {
	case http.StatusNotModified:
		drain(resp)
		return "", etag, true, nil
	case http.StatusOK:
	default:
		return "", "", false, statusError(resp)
	}
	defer drain(resp)
	var ref struct {
		Ref    string `json:"ref"`
		Object struct {
			SHA  string `json:"sha"`
			Type string `json:"type"`
		} `json:"object"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 64<<10)).Decode(&ref); err != nil {
		return "", "", false, fmt.Errorf("github: read branch %q: %w", branch, err)
	}
	sha = strings.ToLower(ref.Object.SHA)
	if ref.Ref != "refs/heads/"+branch || ref.Object.Type != "commit" || !shaRe.MatchString(sha) {
		return "", "", false, &ghError{status: http.StatusNotFound, msg: fmt.Sprintf("no branch named %q", branch)}
	}
	return sha, resp.Header.Get("ETag"), false, nil
}

// ghCompare is a comparison of two commits.
type ghCompare struct {
	Status string          `json:"status"` // ahead | behind | diverged | identical
	Files  []ghChangedFile `json:"files"`
}

// ghChangedFile is one file of a comparison.
type ghChangedFile struct {
	Filename         string `json:"filename"`
	Status           string `json:"status"` // added | removed | modified | renamed | copied | changed | unchanged
	PreviousFilename string `json:"previous_filename"`
}

// compare lists the files changed between two commits.
func (c *ghClient) compare(ctx context.Context, owner, repo, base, head string) (*ghCompare, error) {
	var cmp ghCompare
	p := repoAPIPath(owner, repo) + "/compare/" + url.PathEscape(base) + "..." + url.PathEscape(head)
	if err := c.getJSON(ctx, p, nil, &cmp); err != nil {
		return nil, err
	}
	return &cmp, nil
}

// ghContent is one entry of the contents API.
type ghContent struct {
	Path     string `json:"path"`
	Type     string `json:"type"` // file | dir | symlink | submodule
	Encoding string `json:"encoding"`
	Content  string `json:"content"`
	Size     int64  `json:"size"`
}

// contents reads one file at a commit. A file of more than 1 MB comes
// back without content (Encoding "none").
func (c *ghClient) contents(ctx context.Context, owner, repo, filePath, ref string) (*ghContent, []byte, error) {
	var ct ghContent
	q := url.Values{"ref": {ref}}
	if err := c.getJSON(ctx, repoAPIPath(owner, repo)+"/contents/"+escapePath(filePath), q, &ct); err != nil {
		return nil, nil, err
	}
	if ct.Type != "file" || ct.Encoding != "base64" {
		return &ct, nil, nil
	}
	data, err := base64.StdEncoding.DecodeString(strings.NewReplacer("\n", "", "\r", "").Replace(ct.Content))
	if err != nil {
		return nil, nil, fmt.Errorf("github: decode %s: %w", filePath, err)
	}
	return &ct, data, nil
}

// archive downloads the tarball of a commit and passes its (gzip) body to
// read. The API answers with a redirect to codeload.github.com; toolyard
// follows it only to that host or to the API's own host (a test server),
// and never sends the token there (the redirect URL carries its own
// short-lived grant). A redirect to another API URL of the same host (a
// renamed or transferred repository) is followed with the token.
func (c *ghClient) archive(ctx context.Context, owner, repo, sha string, read func(io.Reader) error) error {
	ctx, cancel := context.WithTimeout(ctx, archiveTimeout)
	defer cancel()
	const accept = "application/vnd.github+json"
	resp, err := c.do(ctx, c.endpoint(repoAPIPath(owner, repo)+"/tarball/"+url.PathEscape(sha), nil), accept, nil, false)
	for hops := 0; err == nil && isRedirect(resp.StatusCode); hops++ {
		loc, lerr := resp.Request.URL.Parse(resp.Header.Get("Location"))
		drain(resp)
		switch {
		case lerr != nil || resp.Header.Get("Location") == "" || hops >= maxAPIRedirects:
			return errors.New("github: the archive redirect has no usable Location")
		case c.isAPIURL(loc):
			resp, err = c.send(ctx, loc.String(), accept, nil)
		case c.archiveHostAllowed(loc):
			resp, err = c.download(ctx, loc)
			if err == nil && isRedirect(resp.StatusCode) {
				drain(resp)
				return errors.New("github: the archive download redirected again")
			}
		default:
			return fmt.Errorf("github: the archive redirect points at %s, which toolyard does not download from", loc.Host)
		}
	}
	if err != nil {
		return err
	}
	if resp.StatusCode != http.StatusOK {
		return statusError(resp)
	}
	defer drain(resp)
	return read(resp.Body)
}

// download GETs an archive URL without the token.
func (c *ghClient) download(ctx context.Context, u *url.URL) (*http.Response, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
	if err != nil {
		return nil, errors.New("github: the archive redirect is not a valid URL")
	}
	req.Header.Set("User-Agent", userAgent)
	resp, err := c.hc.Do(req)
	if err != nil {
		return nil, cleanTransportError(err)
	}
	return resp, nil
}

// isAPIURL reports a repository URL of the API itself (same scheme and
// host, under /repos/ or /repositories/).
func (c *ghClient) isAPIURL(u *url.URL) bool {
	if u.Scheme != c.base.Scheme || !strings.EqualFold(u.Host, c.base.Host) {
		return false
	}
	base := strings.TrimRight(c.base.Path, "/")
	return strings.HasPrefix(u.Path, base+"/repos/") || strings.HasPrefix(u.Path, base+"/repositories/")
}

// archiveHostAllowed reports whether toolyard may download an archive from
// u: https://codeload.github.com, or the API base's own scheme and host.
func (c *ghClient) archiveHostAllowed(u *url.URL) bool {
	if u.Scheme == "https" && strings.EqualFold(u.Host, codeloadHost) {
		return true
	}
	return u.Scheme == c.base.Scheme && strings.EqualFold(u.Host, c.base.Host)
}
