package gateway

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"

	"github.com/mark3labs/mcp-go/mcp"
)

const trustedApprovalContext = "_toolyard_assertion_profile"

func trustedApprovalIdentity(cfg UpstreamConfig) string {
	if provider, ok := cfg.Trusted.(interface{ ApprovalIdentity() string }); ok {
		return provider.ApprovalIdentity()
	}
	// Non-signing test/providers still bind the exact reviewed projection.
	operations := map[string]string{}
	for _, tool := range cfg.Trusted.Catalog() {
		operations[tool.Name] = cfg.Trusted.Operation(tool.Name)
	}
	raw, _ := json.Marshal(struct {
		URL        string
		Tools      []mcp.Tool
		Operations map[string]string
	}{cfg.URL, cfg.Trusted.Catalog(), operations})
	digest := sha256.Sum256(raw)
	return hex.EncodeToString(digest[:])
}

// TrustedUpstream is a reusable, operator-configured assertion credential
// provider. The reviewed local catalog is a startup projection; authenticated
// remote discovery verifies it before execution. No caller chooses its profile.
type TrustedUpstream interface {
	Catalog() []mcp.Tool
	Operation(string) string
	Discover(context.Context, string) ([]mcp.Tool, error)
	Preflight(context.Context, string, string, map[string]any) error
	RequireApprovalBinding(context.Context, string, int64) error
	Call(context.Context, string, string, map[string]any) (json.RawMessage, error)
}

func (g *Gateway) addTrustedUpstream(cfg UpstreamConfig) error {
	if cfg.Transport != "http" || !cfg.PerUser || cfg.HeaderFunc != nil || cfg.IdentityHeader != "" ||
		cfg.Command != "" || len(cfg.Args) != 0 || len(cfg.Env) != 0 || cfg.EnvFunc != nil {
		return errors.New("assertion upstream configuration refused")
	}
	tools := cfg.Trusted.Catalog()
	if len(tools) == 0 {
		return errors.New("assertion upstream catalog is empty")
	}
	u := &upstream{cfg: cfg, pool: g, cachedTools: tools}
	g.mu.Lock()
	g.upstreams[cfg.Name] = u
	g.mu.Unlock()
	g.registerUpstreamTools(cfg.Name, tools, u)
	return nil
}
