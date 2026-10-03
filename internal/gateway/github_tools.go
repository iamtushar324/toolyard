package gateway

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"

	"github.com/mark3labs/mcp-go/mcp"
	"github.com/tusharbhardwaj/toolyard/internal/approval"
	"github.com/tusharbhardwaj/toolyard/internal/githubapi"
)

// githubSnapshot is gateway-authored and persisted with the exact proposed
// arguments. Agents cannot supply it. A different connection or PR commit
// invalidates the proposal, even if its text still looks the same.
type githubSnapshot struct {
	Owner      string `json:"owner_user_id"`
	ID         int64  `json:"github_id"`
	Login      string `json:"github_login"`
	Head       string `json:"head_sha"`
	Connection string `json:"connection_fingerprint"`
	Operation  string `json:"operation_id"`
}

func (g *Gateway) addGitHubUpstream(cfg UpstreamConfig) error {
	if !cfg.PerUser || cfg.PerUserAuth == nil || cfg.HeaderFunc == nil || cfg.URL != githubapi.BaseURL {
		return errors.New("GitHub requires per-user OAuth at https://api.github.com")
	}
	pu := &perUserGroup{cfg: cfg, pool: g, conns: map[string]*upstream{}, toolsLoaded: true}
	g.mu.Lock()
	g.perUser[cfg.Name] = pu
	g.mu.Unlock()
	for _, name := range []string{"get_pull_request", "list_pull_request_files", "list_pull_request_comments", "list_pull_request_reviews", "list_review_comments", "create_pull_request_comment", "submit_pull_request_review", "create_review_comment"} {
		write := strings.HasPrefix(name, "create_") || strings.HasPrefix(name, "submit_")
		opts := []mcp.ToolOption{
			mcp.WithDescription("Use your own GitHub App connection. Writes require your explicit approval. Approving reviews are human-only."),
			mcp.WithString("owner", mcp.Required()), mcp.WithString("repo", mcp.Required()),
			mcp.WithNumber("pull_number", mcp.Required()),
			mcp.WithString(ReasonField, mcp.Required(), mcp.Description("Why this call is needed")),
		}
		if strings.HasPrefix(name, "list_") {
			opts = append(opts, mcp.WithNumber("page", mcp.Description("Page number; 100 results per page")))
		}
		if write {
			opts = append(opts, mcp.WithString("body", mcp.Required()))
		}
		if name == "submit_pull_request_review" {
			opts = append(opts, mcp.WithString("event", mcp.Required(), mcp.Enum("COMMENT", "REQUEST_CHANGES")))
		}
		if name == "create_review_comment" {
			opts = append(opts, mcp.WithString("path", mcp.Required()), mcp.WithNumber("line", mcp.Required()), mcp.WithString("side", mcp.Required(), mcp.Enum("LEFT", "RIGHT")))
		}
		tool := mcp.NewTool(cfg.Name+"."+name, opts...)
		entry := toolEntry{tool: tool, upstream: cfg.Name, originalName: name, reasonField: ReasonField, personalGitHub: true, requireHuman: write}
		entry.handle = func(ctx context.Context, args map[string]any) (*mcp.CallToolResult, error) {
			return g.callGitHub(ctx, pu, name, args)
		}
		g.registerEntry(entry)
	}
	return nil
}

// SetGitHubHTTPClient supplies a test transport; production uses the default
// client and the API origin remains fixed in githubapi.
func (g *Gateway) SetGitHubHTTPClient(c *http.Client) { g.githubHTTP = c }

func (g *Gateway) githubClient(ctx context.Context, pu *perUserGroup) (*githubapi.Client, string, string, error) {
	uid, err := g.ownerUser(ctx, agentIDFromContext(ctx))
	if err != nil || uid == "" {
		return nil, uid, "", errors.New("GitHub requires an agent with a user owner")
	}
	if err := pu.readyUser(ctx, uid); err != nil {
		return nil, uid, "", fmt.Errorf("connect your GitHub account in My connections: %w", err)
	}
	h := pu.cfg.HeaderFunc(WithUpstreamUser(ctx, uid))
	var token string
	for k, v := range h {
		if strings.EqualFold(k, "Authorization") {
			token = strings.TrimPrefix(v, "Bearer ")
			break
		}
	}
	if token == "" {
		return nil, uid, "", ErrNoUserConnection
	}
	sum := sha256.Sum256([]byte(token))
	return githubapi.New(g.githubHTTP, token), uid, hex.EncodeToString(sum[:]), nil
}

func githubArgs(tool string, args map[string]any) (string, error) {
	allowed := map[string]bool{"owner": true, "repo": true, "pull_number": true, approval.PersonalGitHubField: true}
	write := strings.HasPrefix(tool, "create_") || strings.HasPrefix(tool, "submit_")
	if strings.HasPrefix(tool, "list_") {
		allowed["page"] = true
		if page, has := args["page"]; has {
			if _, ok := githubNumber(page); !ok {
				return "", errors.New("page must be a positive integer")
			}
		}
	}
	if write {
		allowed["body"] = true
	}
	if tool == "submit_pull_request_review" {
		allowed["event"] = true
	}
	if tool == "create_review_comment" {
		allowed["path"], allowed["line"], allowed["side"] = true, true, true
	}
	for k := range args {
		if !allowed[k] {
			return "", fmt.Errorf("unsupported GitHub argument %q", k)
		}
	}
	owner, _ := args["owner"].(string)
	repo, _ := args["repo"].(string)
	n, ok := githubNumber(args["pull_number"])
	if !ok {
		return "", errors.New("pull_number must be a positive integer")
	}
	path, err := githubapi.RepoPath(owner, repo, n)
	if err != nil {
		return "", err
	}
	if write {
		body, ok := args["body"].(string)
		if !ok || strings.TrimSpace(body) == "" || len(body) > 60000 {
			return "", errors.New("body must contain between 1 and 60000 bytes")
		}
	}
	if tool == "submit_pull_request_review" && args["event"] != "COMMENT" && args["event"] != "REQUEST_CHANGES" {
		return "", errors.New("only COMMENT and REQUEST_CHANGES reviews are supported; approving reviews require a human on GitHub")
	}
	if tool == "create_review_comment" {
		file, _ := args["path"].(string)
		_, ok := githubNumber(args["line"])
		if file == "" || !ok || (args["side"] != "LEFT" && args["side"] != "RIGHT") {
			return "", errors.New("an inline comment requires path, positive line, and LEFT or RIGHT side")
		}
	}
	return path, nil
}

func githubNumber(v any) (int, bool) {
	var n float64
	switch x := v.(type) {
	case float64:
		n = x
	case int:
		n = float64(x)
	case json.Number:
		n, _ = x.Float64()
	default:
		return 0, false
	}
	return int(n), n > 0 && n <= 2147483647 && n == float64(int(n))
}

func (g *Gateway) prepareGitHub(ctx context.Context, entry toolEntry, args map[string]any) error {
	if _, supplied := args[approval.PersonalGitHubField]; supplied {
		return errors.New("GitHub approval context is reserved for Toolyard")
	}
	path, err := githubArgs(entry.originalName, args)
	if err != nil {
		return err
	}
	if !entry.requireHuman {
		return nil
	}
	g.mu.RLock()
	pu := g.perUser[entry.upstream]
	g.mu.RUnlock()
	if pu == nil {
		return ErrUpstreamNotFound
	}
	c, uid, fingerprint, err := g.githubClient(ctx, pu)
	if err != nil {
		return err
	}
	u, err := c.User(ctx)
	if err != nil {
		return g.githubAuthError(ctx, pu, uid, err)
	}
	p, err := c.Pull(ctx, path)
	if err != nil {
		return err
	}
	if p.State != "open" {
		return errors.New("GitHub writes require an open pull request")
	}
	s := githubSnapshot{Owner: uid, ID: u.ID, Login: u.Login, Head: p.Head.SHA, Connection: fingerprint}
	op, _ := json.Marshal([]any{agentIDFromContext(ctx), entry.tool.Name, args, s})
	sum := sha256.Sum256(op)
	s.Operation = hex.EncodeToString(sum[:])
	b, _ := json.Marshal(s)
	var snapshot map[string]any
	_ = json.Unmarshal(b, &snapshot)
	args[approval.PersonalGitHubField] = snapshot
	return nil
}

func (g *Gateway) githubAuthError(ctx context.Context, pu *perUserGroup, uid string, err error) error {
	if githubapi.Unauthorized(err) {
		pu.cfg.PerUserAuth.MarkUserUnauthorized(ctx, pu.cfg.Name, uid, "GitHub rejected the user connection")
	}
	return err
}

func (g *Gateway) callGitHub(ctx context.Context, pu *perUserGroup, tool string, args map[string]any) (*mcp.CallToolResult, error) {
	path, err := githubArgs(tool, args)
	if err != nil {
		return mcp.NewToolResultError(err.Error()), nil
	}
	c, uid, fingerprint, err := g.githubClient(ctx, pu)
	if err != nil {
		return mcp.NewToolResultError(err.Error()), nil
	}
	write := strings.HasPrefix(tool, "create_") || strings.HasPrefix(tool, "submit_")
	if !write {
		switch tool {
		case "list_pull_request_files":
			path += "/files?per_page=100"
		case "list_pull_request_comments":
			path = strings.Replace(path, "/pulls/", "/issues/", 1) + "/comments?per_page=100"
		case "list_pull_request_reviews":
			path += "/reviews?per_page=100"
		case "list_review_comments":
			path += "/comments?per_page=100"
		}
		if strings.HasPrefix(tool, "list_") {
			page := 1
			if n, ok := githubNumber(args["page"]); ok {
				page = n
			}
			path += fmt.Sprintf("&page=%d", page)
		}
		var out any
		err := c.Do(ctx, http.MethodGet, path, nil, &out)
		if err != nil {
			return mcp.NewToolResultError(g.githubAuthError(ctx, pu, uid, err).Error()), nil
		}
		return mcp.NewToolResultJSON(out)
	}
	// Reconciliation and creation are serialized in this single-instance
	// service. The persisted marker also survives a process restart.
	g.githubWriteMu.Lock()
	defer g.githubWriteMu.Unlock()
	var s githubSnapshot
	b, _ := json.Marshal(args[approval.PersonalGitHubField])
	if json.Unmarshal(b, &s) != nil || s.Owner == "" || s.Operation == "" || s.Owner != uid || s.Connection != fingerprint {
		return mcp.NewToolResultError("the GitHub connection changed; request fresh approval"), nil
	}
	u, err := c.User(ctx)
	if err != nil {
		return mcp.NewToolResultError(g.githubAuthError(ctx, pu, uid, err).Error()), nil
	}
	if u.ID != s.ID {
		return mcp.NewToolResultError("the GitHub account changed; request fresh approval"), nil
	}
	p, err := c.Pull(ctx, path)
	if err != nil {
		return mcp.NewToolResultError(err.Error()), nil
	}
	if p.Head.SHA != s.Head || p.State != "open" {
		return mcp.NewToolResultError("the pull request changed; request fresh approval"), nil
	}
	endpoint := path + "/reviews"
	marker := "\n\n<!-- toolyard:" + s.Operation + " -->"
	payload := map[string]any{"body": args["body"].(string) + marker}
	switch tool {
	case "create_pull_request_comment":
		endpoint = strings.Replace(path, "/pulls/", "/issues/", 1) + "/comments"
	case "submit_pull_request_review":
		payload["event"], payload["commit_id"] = args["event"], s.Head
	case "create_review_comment":
		endpoint = path + "/comments"
		payload["commit_id"], payload["path"], payload["line"], payload["side"] = s.Head, args["path"], args["line"], args["side"]
	}
	existing, err := c.FindOperation(ctx, endpoint, marker, s.ID)
	if err != nil {
		return mcp.NewToolResultError(err.Error()), nil
	}
	if existing != nil {
		return mcp.NewToolResultJSON(existing)
	}
	latest, latestUID, latestFingerprint, err := g.githubClient(ctx, pu)
	if err != nil || latestUID != uid || latestFingerprint != fingerprint {
		return mcp.NewToolResultError("the GitHub connection changed; request fresh approval"), nil
	}
	c = latest
	var out map[string]any
	if err := c.Do(ctx, http.MethodPost, endpoint, payload, &out); err != nil {
		return mcp.NewToolResultError(g.githubAuthError(ctx, pu, uid, err).Error()), nil
	}
	return mcp.NewToolResultJSON(out)
}
