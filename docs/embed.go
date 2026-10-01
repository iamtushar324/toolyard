// Package docs embeds the agent-facing documents so the gateway serves
// exactly the text that lives in the repo: inbox.guide, GET /v1/guide and
// the toolyard-inbox skill all come from these files.
package docs

import _ "embed"

// AgentProtocol is docs/guidelines/agent-protocol.md.
//
//go:embed guidelines/agent-protocol.md
var AgentProtocol string

// InboxSkill is the toolyard-inbox Claude Code skill.
//
//go:embed guidelines/skill/toolyard-inbox/SKILL.md
var InboxSkill string

// OperatorGuide is docs/guidelines/operator.md, served at /v1/operator/guide
// and printed by `toolyard admin guide`.
//
//go:embed guidelines/operator.md
var OperatorGuide string
