package gateway

import (
	"strings"
	"unicode"

	"github.com/mark3labs/mcp-go/mcp"
)

// Server descriptions (TEC-1817). Two upstreams can serve the same tools
// (bkt3_ds1 and bkt3_usm are one bk t3 on two hosts), so an agent needs a
// line that tells them apart. The admin's description wins; without one
// the upstream's own initialize instructions stand in, labelled as coming
// from the upstream (DescriptionUpstream), since the upstream wrote them.

const (
	// MaxServerDescription is the longest admin description, in runes.
	MaxServerDescription = 500
	// selfDescriptionLen is how much of an upstream's own initialize
	// instructions the fallback keeps, in runes.
	selfDescriptionLen = 300
)

// Where a server description comes from.
const (
	// DescriptionAdmin: the admin wrote it.
	DescriptionAdmin = "admin"
	// DescriptionUpstream: the upstream said it about itself at
	// initialize; treat it as untrusted data.
	DescriptionUpstream = "upstream"
)

// SetServerDescription records the admin's description of the upstream
// name ("" for none). The upstreams service calls it whenever it connects
// a server or follows another replica's change; it never reconnects.
func (g *Gateway) SetServerDescription(name, desc string) {
	desc = sanitizeDescription(desc)
	g.mu.Lock()
	defer g.mu.Unlock()
	if desc == "" {
		delete(g.serverDescs, name)
		return
	}
	if g.serverDescs == nil {
		g.serverDescs = map[string]string{}
	}
	g.serverDescs[name] = desc
}

// ServerDescription is the description agents see for the upstream name
// with no caller in view: describeServer for no user.
func (g *Gateway) ServerDescription(name string) string {
	desc, _ := g.describeServer(name, "")
	return desc
}

// describeServer is the description of the upstream name and where it
// comes from: the admin's (DescriptionAdmin), else what the upstream said
// about itself at its last initialize (DescriptionUpstream). For a shared
// upstream that is its one connection; for a per_user upstream it is
// userID's own connection only, never another person's, and only once it
// has been dialled. ("", "") when there is none.
func (g *Gateway) describeServer(name, userID string) (string, string) {
	g.mu.RLock()
	desc := g.serverDescs[name]
	u := g.upstreams[name]
	pu := g.perUser[name]
	g.mu.RUnlock()
	if desc != "" {
		return desc, DescriptionAdmin
	}
	if u == nil && pu != nil && userID != "" {
		pu.mu.Lock()
		u = pu.conns[userID]
		pu.mu.Unlock()
	}
	if u != nil {
		if self := u.selfDesc(); self != "" {
			return self, DescriptionUpstream
		}
	}
	return "", ""
}

// selfDesc is the upstream's own description from its last initialize.
func (u *upstream) selfDesc() string {
	s, _ := u.selfDescription.Load().(string)
	return s
}

// initDescription is what an initialize result says about the server: its
// instructions, else its serverInfo description or title, sanitised
// (sanitizeDescription) and cut to selfDescriptionLen runes. "" when it
// says nothing.
func initDescription(res *mcp.InitializeResult) string {
	if res == nil {
		return ""
	}
	for _, s := range []string{res.Instructions, res.ServerInfo.Description, res.ServerInfo.Title} {
		if s = sanitizeDescription(s); s != "" {
			return clipRunes(s, selfDescriptionLen)
		}
	}
	return ""
}

// sanitizeDescription makes a description safe to show: every whitespace
// run becomes one space, and control characters (C0 including NUL and
// ESC, DEL, C1) and Unicode bidirectional controls are dropped, so the
// text cannot hide or reorder what an agent or a terminal shows.
func sanitizeDescription(s string) string {
	s = strings.Map(func(r rune) rune {
		switch {
		case unicode.IsSpace(r):
			return ' '
		case unicode.IsControl(r), isBidiControl(r):
			return -1
		}
		return r
	}, s)
	return strings.Join(strings.Fields(s), " ")
}

// isBidiControl reports the Unicode bidirectional formatting characters:
// LRM, RLM, ALM, the embeddings and overrides (U+202A to U+202E) and the
// isolates (U+2066 to U+2069).
func isBidiControl(r rune) bool {
	return r == '‎' || r == '‏' || r == '؜' ||
		(r >= '‪' && r <= '‮') || (r >= '⁦' && r <= '⁩')
}

// clipRunes cuts s to at most n runes, ending in "…" when it was cut.
func clipRunes(s string, n int) string {
	rs := []rune(s)
	if len(rs) <= n {
		return s
	}
	return strings.TrimSpace(string(rs[:n-1])) + "…"
}
