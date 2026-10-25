package skillhub

// GitHub access: the instance-wide default read token, and rate limits.
//
// Without a token GitHub allows 60 API calls an hour per egress address,
// and even an unchanged branch head (a 304 to a conditional request)
// counts. A few public sources polled every five minutes, plus an admin
// trying things, use that up. An admin can therefore store one default
// read token (the setting skills_github_token, sealed with secrets.key
// like a source's own token, never returned and never logged): every
// source without its own token (auth none) then reads with it, 5,000 calls
// an hour, and conditional requests that answer 304 stop counting. A
// source's own token always wins. publicClient is the one place that picks
// the credential of a source without its own.
//
// A rate limit answer pauses every call with that credential until it ends
// (rateLimits, one shared backoff per credential, not per source): the
// poller skips the sources that read with a paused credential, a sync that
// meets one is not counted as the source's failure, and the admin and
// agent routes answer 429 with when to try again (ErrRateLimited,
// RateLimitedUntil).

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/tusharbhardwaj/toolyard/internal/settings"
)

// ErrRateLimited matches (errors.Is) every error that is a GitHub rate
// limit; RateLimitedUntil says when it ends.
var ErrRateLimited = errors.New("skillhub: GitHub rate limit reached")

// RateLimitedUntil returns when the GitHub rate limit behind err ends.
func RateLimitedUntil(err error) (time.Time, bool) { return retryAfter(err) }

// RateLimitError is the error a GitHub rate limit that ends at until gives,
// for fakes and tests of the callers.
func RateLimitError(until time.Time) error {
	return &ghError{status: http.StatusTooManyRequests, retryAt: until}
}

// RateLimit is GitHub's core API rate limit for one credential.
type RateLimit struct {
	Limit     int       `json:"limit"`
	Remaining int       `json:"remaining"`
	Used      int       `json:"used"`
	Reset     time.Time `json:"reset"`
}

// GitHubAccess is what the dashboard's GitHub access panel shows. It never
// carries the token.
type GitHubAccess struct {
	// Set reports a default read token.
	Set bool `json:"set"`
	// Rate is the rate limit of the credential the sources without their
	// own token read with: the default token, else this server's address.
	Rate *RateLimit `json:"rate,omitempty"`
	// RateError says why Rate could not be read.
	RateError string `json:"rate_error,omitempty"`
	// PausedUntil is set while this node holds back the calls of that
	// credential after GitHub answered with a rate limit.
	PausedUntil *time.Time `json:"paused_until,omitempty"`
}

// settingsWriter is the part of the settings service that stores the
// default token (internal/settings.Service implements it).
type settingsWriter interface {
	Set(ctx context.Context, key string, value any) error
}

func defaultTokenAAD() []byte { return []byte("skillhub-default-token") }

// defaultToken opens the default read token, or "" when none is set or it
// cannot be opened (then sources without a token read anonymously, and the
// failure is logged; the token itself never is).
func (s *Service) defaultToken() string {
	if s.settings == nil {
		return ""
	}
	sealed := strings.TrimSpace(s.settings.GetString(settings.SkillsGitHubToken, ""))
	if sealed == "" || s.cipher == nil {
		return ""
	}
	tok, err := s.cipher.Open(sealed, defaultTokenAAD())
	if err != nil {
		s.badTokenMu.Lock()
		first := s.badToken != sealed
		s.badToken = sealed
		s.badTokenMu.Unlock()
		if first {
			s.log.Warn("skillhub: the default GitHub token cannot be opened (was secrets.key replaced?); sources without a token read anonymously until it is set again")
		}
		return ""
	}
	return string(tok)
}

// publicClient is the client of a source without its own token: the
// default read token when one is set, else anonymous.
func (s *Service) publicClient() *ghClient {
	tok := s.defaultToken()
	c := s.newClient(tok)
	c.isDefault = tok != ""
	return c
}

// GitHubAccess reports whether a default token is set and the rate limit
// of the credential sources without their own token use. GitHub's
// /rate_limit endpoint does not count against the limit.
func (s *Service) GitHubAccess(ctx context.Context) (*GitHubAccess, error) {
	c := s.publicClient()
	out := &GitHubAccess{Set: c.isDefault}
	if until, ok := s.limits.blockedUntil(c.key); ok {
		u := until.UTC()
		out.PausedUntil = &u
	}
	rl, err := c.rateLimit(ctx)
	if err != nil {
		out.RateError = cleanMessage(err.Error())
		return out, nil
	}
	out.Rate = rl
	return out, nil
}

// SetGitHubToken stores (or, with "", clears) the default read token. A
// new token is checked with one call GitHub does not count (/rate_limit)
// and refused when GitHub rejects it; nothing is stored then. The poller
// is woken, so sources paused on the old credential's rate limit go on.
func (s *Service) SetGitHubToken(ctx context.Context, by, token string) (*GitHubAccess, error) {
	w, ok := s.settings.(settingsWriter)
	if !ok || s.settings == nil {
		return nil, errors.New("skillhub: this server cannot store settings")
	}
	token = strings.TrimSpace(token)
	if token == "" {
		if err := w.Set(ctx, settings.SkillsGitHubToken, ""); err != nil {
			return nil, fmt.Errorf("skillhub: clear the default token: %w", err)
		}
		s.log.Info("skillhub: default GitHub token cleared", "by", by)
		s.pokePoller()
		return s.GitHubAccess(ctx)
	}
	if strings.ContainsAny(token, " \t\r\n") || len(token) > 1024 {
		return nil, fmt.Errorf("%w: that does not look like a GitHub token", ErrInvalid)
	}
	rl, err := s.newClient(token).rateLimit(ctx)
	switch {
	case isGHStatus(err, http.StatusUnauthorized, http.StatusForbidden):
		return nil, fmt.Errorf("%w: GitHub rejected the token", ErrInvalid)
	case err != nil:
		return nil, fmt.Errorf("skillhub: check the token with GitHub: %w", err)
	}
	sealed, err := s.seal(token, defaultTokenAAD())
	if err != nil {
		return nil, err
	}
	if err := w.Set(ctx, settings.SkillsGitHubToken, sealed); err != nil {
		return nil, fmt.Errorf("skillhub: store the default token: %w", err)
	}
	s.log.Info("skillhub: default GitHub token set", "by", by, "limit", rl.Limit)
	s.pokePoller()
	return &GitHubAccess{Set: true, Rate: rl}, nil
}

// rateLimit reads the credential's core rate limit. It skips the
// credential's pause (the endpoint does not count), and a limit that is
// used up pauses the credential until it resets.
func (c *ghClient) rateLimit(ctx context.Context) (*RateLimit, error) {
	ctx, cancel := context.WithTimeout(ctx, apiTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.endpoint("/rate_limit", nil), nil)
	if err != nil {
		return nil, fmt.Errorf("github: build request: %w", err)
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("X-GitHub-Api-Version", githubAPIVersion)
	req.Header.Set("User-Agent", userAgent)
	if c.token != "" {
		req.Header.Set("Authorization", "Bearer "+c.token)
	}
	resp, err := c.hc.Do(req)
	if err != nil {
		return nil, cleanTransportError(err)
	}
	if resp.StatusCode != http.StatusOK {
		return nil, statusError(resp)
	}
	defer drain(resp)
	type core struct {
		Limit     int   `json:"limit"`
		Remaining int   `json:"remaining"`
		Used      int   `json:"used"`
		Reset     int64 `json:"reset"`
	}
	var body struct {
		Resources struct {
			Core core `json:"core"`
		} `json:"resources"`
		Rate core `json:"rate"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 64<<10)).Decode(&body); err != nil {
		return nil, fmt.Errorf("github: read the rate limit: %w", err)
	}
	k := body.Resources.Core
	if k.Limit == 0 {
		k = body.Rate
	}
	rl := &RateLimit{Limit: k.Limit, Remaining: k.Remaining, Used: k.Used, Reset: time.Unix(k.Reset, 0).UTC()}
	if rl.Limit > 0 && rl.Remaining == 0 {
		c.limits.block(c.key, rl.Reset)
	}
	return rl, nil
}
