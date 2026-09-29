package inbox

import (
	"regexp"
	"sort"
	"strconv"
	"strings"

	"github.com/tusharbhardwaj/toolyard/docs"
)

// Guide topics map to the numbered sections of agent-protocol.md. The
// section number is the stable key; a test fails if a section goes
// missing.
var guideSections = map[int]string{
	1:  "restricted",
	2:  "coaching",
	3:  "requests",
	4:  "tools",
	5:  "voice",
	6:  "attachments",
	7:  "urgency",
	8:  "dry-run",
	9:  "grants",
	10: "updates",
	11: "never",
	12: "examples",
}

var topicAliases = map[string]string{
	"check": "restricted", "permission_required": "coaching", "request": "requests", "format": "requests",
	"params": "tools", "parameters": "tools", "audio": "voice", "voice-note": "voice", "evidence": "attachments",
	"media": "attachments", "dry_run": "dry-run", "dryrun": "dry-run", "wait": "grants", "waiting": "grants",
	"grant": "grants", "update": "updates", "done": "updates", "example": "examples",
}

var sectionHead = regexp.MustCompile(`(?m)^## (\d+)\. `)

// Guide serves the agent protocol by topic.
type Guide struct {
	intro    string
	sections map[string]string
	full     string
	hosting  func() string
}

// NewGuide splits the embedded protocol. hosting returns the owner's note
// on where agents should host files (may be nil).
func NewGuide(hosting func() string) *Guide {
	return newGuide(docs.AgentProtocol, hosting)
}

func newGuide(text string, hosting func() string) *Guide {
	g := &Guide{sections: map[string]string{}, full: text, hosting: hosting}
	idx := sectionHead.FindAllStringSubmatchIndex(text, -1)
	if len(idx) == 0 {
		g.intro = text
		return g
	}
	g.intro = strings.TrimSpace(text[:idx[0][0]])
	for i, m := range idx {
		end := len(text)
		if i+1 < len(idx) {
			end = idx[i+1][0]
		}
		n, _ := strconv.Atoi(text[m[2]:m[3]])
		if name, ok := guideSections[n]; ok {
			g.sections[name] = strings.TrimSpace(strings.TrimSuffix(strings.TrimSpace(text[m[0]:end]), "---"))
		}
	}
	return g
}

// Topics lists the topic names, in protocol order.
func (g *Guide) Topics() []string {
	nums := make([]int, 0, len(guideSections))
	for n := range guideSections {
		nums = append(nums, n)
	}
	sort.Ints(nums)
	out := []string{}
	for _, n := range nums {
		if _, ok := g.sections[guideSections[n]]; ok {
			out = append(out, guideSections[n])
		}
	}
	return append(out, "hosting", "all")
}

// Topic returns the text for a topic. "" returns the overview.
func (g *Guide) Topic(topic string) (string, bool) {
	t := strings.ToLower(strings.TrimSpace(topic))
	if a, ok := topicAliases[t]; ok {
		t = a
	}
	switch t {
	case "", "overview", "short":
		return g.intro + "\n\nTopics: " + strings.Join(g.Topics(), ", ") + `. Call inbox.guide({"topic": "<name>"}) for one.`, true
	case "all":
		return g.full, true
	case "hosting":
		note := ""
		if g.hosting != nil {
			note = strings.TrimSpace(g.hosting())
		}
		if note == "" {
			note = "Your owner hasn't said where to host files. Use a link that already exists and is public (a CI artifact, a PR attachment, object storage), send the evidence as data instead (table, chart, diff, log), or ask with inbox.ask."
		}
		return "## Hosting media\n\n" + note, true
	}
	s, ok := g.sections[t]
	return s, ok
}
