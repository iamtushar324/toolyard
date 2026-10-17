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

// CodeOpenAPI is docs/openapi/code.json: code mode over REST as OpenAPI
// 3.1, served at GET /v1/code/openapi.json.
//
//go:embed openapi/code.json
var CodeOpenAPI string

// TyScript is docs/ty/ty, the `ty` command for code mode over REST
// (Python 3, standard library only), served at GET /v1/code/ty.
//
//go:embed ty/ty
var TyScript string
