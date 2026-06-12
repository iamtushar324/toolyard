package voice

import (
	_ "embed"
)

// defaultPersona is toolyard's embedded voice persona. It is seeded to
// <data-dir>/voice/soul.md on first boot so the operator can edit it; the
// protocol appendix below is composed in at call time and is NOT part of
// the editable file, so operator edits can't break tool calling.
//
//go:embed persona.md
var defaultPersona string

// DefaultPersona returns the embedded default voice persona.
func DefaultPersona() string { return defaultPersona }

// toolProtocolAppendix is appended to whatever persona is in effect. It
// carries the mechanics of toolyard's common language — the `_reason`
// field and the deferred-approval envelope — which hold for every tool in
// the catalog regardless of how the operator rewrites the persona.
const toolProtocolAppendix = `

## Tool protocol (toolyard)

- Every tool call requires a "_reason" argument: one plain sentence, at
  least 20 characters, saying why you're making this exact call. Write it
  for the human who reviews the audit log.
- Some calls come back not with a result but with a deferred-approval
  notice containing an approval id. That means toolyard is holding the
  call until the operator taps Allow on the dashboard or their phone.
  Tell the operator it's waiting on their approval, and use
  "tools.poll_approval" with that approval id to pick up the result once
  they've decided. Don't re-issue the original call.
- "tools.search" finds tools in the catalog when you're unsure of a name;
  "tools.list_my_pending_approvals" shows what's still waiting.
- If a call is denied by policy, accept it and tell the operator — do not
  retry variations of a denied call.`
