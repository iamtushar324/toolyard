package skillhub

// GitHubConnections is the TokenSource of AuthGitHub sources and of change
// proposals: a person's own toolyard GitHub connection (a per-user GitHub
// server on the Servers page, connected in My connections). toolyard keeps
// those tokens; this file only finds the person's live one. Wire it with
// Admin.SetTokenSource and ChangeOptions.Tokens.

import (
	"context"
	"errors"
	"fmt"
	"strings"
)

// ErrNoGitHubConnection means the person has no usable toolyard GitHub
// connection (none, signed out, or it needs a new sign-in).
var ErrNoGitHubConnection = errors.New("skillhub: no usable GitHub connection; connect GitHub in My connections")

// GitHubConnections reads people's toolyard GitHub connections. Servers,
// Freshen and Token are required; Active is optional.
type GitHubConnections struct {
	// Servers lists the enabled per-user GitHub servers, in the order to
	// try them.
	Servers func(ctx context.Context) ([]string, error)
	// Freshen refreshes the person's token on server when it has expired
	// (the OAuth service's FreshenUserToken). An error skips the server.
	Freshen func(ctx context.Context, server, userID string) error
	// Token returns the person's access token on server, "" when there is
	// no usable one.
	Token func(ctx context.Context, server, userID string) (string, error)
	// Active, when set, reports whether the person may still act: a
	// blocked or removed person's connection is never used.
	Active func(ctx context.Context, userID string) bool
}

// TokenSource returns the person's first usable GitHub token.
func (g *GitHubConnections) TokenSource() TokenSource {
	return func(ctx context.Context, userID string) (string, error) {
		if g == nil || g.Servers == nil || g.Token == nil || userID == "" {
			return "", ErrNoGitHubConnection
		}
		if g.Active != nil && !g.Active(ctx, userID) {
			return "", ErrNoGitHubConnection
		}
		servers, err := g.Servers(ctx)
		if err != nil {
			return "", fmt.Errorf("skillhub: list the GitHub servers: %w", err)
		}
		for _, server := range servers {
			if g.Freshen != nil {
				if err := g.Freshen(ctx, server, userID); err != nil {
					continue
				}
			}
			tok, err := g.Token(ctx, server, userID)
			if err != nil {
				continue
			}
			if tok = strings.TrimSpace(tok); tok != "" {
				return tok, nil
			}
		}
		return "", ErrNoGitHubConnection
	}
}

// InspectAs is Inspect with userID's toolyard GitHub connection: the
// wizard's "Use my GitHub connection" for a private repository. The token
// is used for this read only and never stored.
func (s *Service) InspectAs(ctx context.Context, userID, rawURL string) (*RepoInfo, error) {
	ts := s.tokenSource()
	if ts == nil {
		return nil, fmt.Errorf("%w: reading with a GitHub connection is not available on this server", ErrInvalid)
	}
	tok, err := ts(ctx, userID)
	if err != nil || tok == "" {
		return nil, fmt.Errorf("%w: you have no usable GitHub connection; connect GitHub in My connections, or use a read-only token", ErrInvalid)
	}
	info, err := s.Inspect(ctx, rawURL, tok)
	if errors.Is(err, ErrNeedsToken) {
		ref, _, _ := parseRepoURL(rawURL)
		return nil, fmt.Errorf("%w: your GitHub connection cannot read %s/%s; install the toolyard GitHub App on that repository (Contents: read, Metadata: read)",
			ErrInvalid, ref.Owner, ref.Repo)
	}
	return info, err
}
