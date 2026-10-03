// Package githubapi implements the deliberately small GitHub App user-token
// surface exposed by Toolyard. Tokens never leave api.github.com.
package githubapi

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"regexp"
	"strings"
	"time"
)

const BaseURL = "https://api.github.com"

type Client struct {
	http  *http.Client
	token string
}

func New(h *http.Client, token string) *Client {
	if h == nil {
		h = &http.Client{Timeout: 30 * time.Second}
	}
	clone := *h
	clone.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	return &Client{http: &clone, token: token}
}

type APIError struct{ Status int }

func (e *APIError) Error() string { return fmt.Sprintf("GitHub returned HTTP %d", e.Status) }
func Unauthorized(err error) bool {
	var e *APIError
	return errors.As(err, &e) && e.Status == http.StatusUnauthorized
}

// Do does not retry writes. Response errors omit provider bodies, which can
// contain submitted text or credentials. Redirects never carry the bearer.
func (c *Client) Do(ctx context.Context, method, path string, body any, out any) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if c.token == "" || !strings.HasPrefix(path, "/") || strings.HasPrefix(path, "//") {
		return errors.New("GitHub requires a user connection and a relative API path")
	}
	var b []byte
	var err error
	if body != nil {
		b, err = json.Marshal(body)
		if err != nil {
			return err
		}
	}
	r, err := http.NewRequestWithContext(ctx, method, BaseURL+path, bytes.NewReader(b))
	if err != nil {
		return err
	}
	r.Header.Set("Authorization", "Bearer "+c.token)
	r.Header.Set("Accept", "application/vnd.github+json")
	r.Header.Set("X-GitHub-Api-Version", "2022-11-28")
	r.Header.Set("Content-Type", "application/json")
	r.Header.Set("User-Agent", "toolyard")
	res, err := c.http.Do(r)
	if err != nil {
		return errors.New("GitHub request failed; inspect the approval result before a retry")
	}
	defer res.Body.Close()
	if res.StatusCode < 200 || res.StatusCode >= 300 {
		return &APIError{Status: res.StatusCode}
	}
	if out == nil {
		return nil
	}
	data, err := io.ReadAll(io.LimitReader(res.Body, 8*1024*1024+1))
	if err != nil {
		return err
	}
	if len(data) > 8*1024*1024 {
		return errors.New("GitHub response exceeds the size limit")
	}
	return json.Unmarshal(data, out)
}

type User struct {
	ID    int64  `json:"id"`
	Login string `json:"login"`
}

func (c *Client) User(ctx context.Context) (*User, error) {
	var u User
	if err := c.Do(ctx, http.MethodGet, "/user", nil, &u); err != nil {
		return nil, err
	}
	if u.ID <= 0 || u.Login == "" {
		return nil, errors.New("GitHub did not verify the account")
	}
	return &u, nil
}

var repoPart = regexp.MustCompile(`^[A-Za-z0-9_.-]+$`)

func RepoPath(owner, repo string, number int) (string, error) {
	if !repoPart.MatchString(owner) || !repoPart.MatchString(repo) || owner == "." || owner == ".." || repo == "." || repo == ".." || number <= 0 {
		return "", errors.New("owner, repo, and a positive pull_number are required")
	}
	return fmt.Sprintf("/repos/%s/%s/pulls/%d", owner, repo, number), nil
}

type Pull struct {
	State string `json:"state"`
	Head  struct {
		SHA string `json:"sha"`
	} `json:"head"`
}

func (c *Client) Pull(ctx context.Context, path string) (*Pull, error) {
	var p Pull
	if err := c.Do(ctx, http.MethodGet, path, nil, &p); err != nil {
		return nil, err
	}
	if p.Head.SHA == "" {
		return nil, errors.New("GitHub did not return the pull request commit")
	}
	return &p, nil
}

// FindOperation reconciles a write after a process crash or uncertain HTTP
// result. A bounded scan fails closed instead of posting a possible duplicate.
func (c *Client) FindOperation(ctx context.Context, path, marker string, userID int64) (map[string]any, error) {
	for page := 1; page <= 10; page++ {
		var rows []map[string]any
		if err := c.Do(ctx, http.MethodGet, fmt.Sprintf("%s?per_page=100&page=%d", path, page), nil, &rows); err != nil {
			return nil, err
		}
		for _, row := range rows {
			body, _ := row["body"].(string)
			u, _ := row["user"].(map[string]any)
			id, _ := u["id"].(float64)
			if int64(id) == userID && strings.HasSuffix(body, marker) {
				return row, nil
			}
		}
		if len(rows) < 100 {
			return nil, nil
		}
	}
	return nil, errors.New("GitHub history is too large to exclude a duplicate; no write was sent")
}
