package gateway

import "context"

// SkillsPublisher was the gateway-side dependency of the old skills
// workspace tools (skills.publish, skills.install, skills.list_tags and the
// base64 skills.bundle), implemented by *internal/skills.Service.
//
// Deprecated: the skills hub replaced that workspace (RegisterSkillHub,
// docs/design/skills-hub.md). The type only keeps cmd/gateway compiling
// until its old wiring is removed with internal/skills; delete this file
// then.
type SkillsPublisher interface {
	Publish(ctx context.Context, agentID, slug, skillMD, agentsYAML string, scripts map[string]string, topic string) (any, error)
	Install(ctx context.Context, agentID, slug, mode, target string) (any, error)
	List(ctx context.Context, tagFilter []string) (any, error)
	ListTags(ctx context.Context) (any, error)
	Get(ctx context.Context, slug string, includeFiles bool) (any, error)
	Bundle(ctx context.Context, slugs []string) (any, error)
	SkillsDir() string
}

// RegisterSkillsBuiltins registered the old skills workspace tools. It
// now registers nothing: skills.list, skills.get and skills.bundle belong
// to the skills hub (RegisterSkillHub), and the workspace's own tools are
// gone.
//
// Deprecated: remove the call with the old skills wiring in cmd/gateway.
func (g *Gateway) RegisterSkillsBuiltins(SkillsPublisher) {}
