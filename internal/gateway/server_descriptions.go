package gateway

import (
	"strings"

	"github.com/mark3labs/mcp-go/mcp"
)

// Server descriptions (TEC-1817). Two upstreams can serve the same tools
// (bkt3_ds1 and bkt3_usm are one bk t3 on two hosts), so an agent needs a
// line that tells them apart. The admin's description wins; without one
// the upstream's own initialize instructions stand in.

const (
	// MaxServerDescription is the longest admin description, in runes.
	MaxServerDescription = 500
	// selfDescriptionLen is how much of an upstream's own initialize
	// instructions the fallback keeps, in runes.
	selfDescriptionLen = 300
)

// SetServerDescription records the admin's description of the upstream
// name ("" for none). The upstreams service calls it whenever it connects
// a server or follows another replica's change; it never reconnects.
func (g *Gateway) SetServerDescription(name, desc string) {
	desc = strings.TrimSpace(desc)
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

// ServerDescription is the description agents see for the upstream name:
// the admin's, else what the upstream said about itself at its last
// initialize (a shared upstream only; a per_user one has no connection of
// its own to ask), else "".
func (g *Gateway) ServerDescription(name string) string {
	g.mu.RLock()
	desc := g.serverDescs[name]
	u := g.upstreams[name]
	g.mu.RUnlock()
	if desc != "" {
		return desc
	}
	if u != nil {
		return u.selfDesc()
	}
	return ""
}

// selfDesc is the upstream's own description from its last initialize.
func (u *upstream) selfDesc() string {
	s, _ := u.selfDescription.Load().(string)
	return s
}

// initDescription is what an initialize result says about the server: its
// instructions, else its serverInfo description or title, cut to
// selfDescriptionLen runes. "" when it says nothing.
func initDescription(res *mcp.InitializeResult) string {
	if res == nil {
		return ""
	}
	for _, s := range []string{res.Instructions, res.ServerInfo.Description, res.ServerInfo.Title} {
		if s = strings.Join(strings.Fields(s), " "); s != "" {
			return clipRunes(s, selfDescriptionLen)
		}
	}
	return ""
}

// clipRunes cuts s to at most n runes, ending in "…" when it was cut.
func clipRunes(s string, n int) string {
	rs := []rune(s)
	if len(rs) <= n {
		return s
	}
	return strings.TrimSpace(string(rs[:n-1])) + "…"
}
