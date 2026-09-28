// Package inbox is the owner-facing side of toolyard's permission model.
//
// An agent that needs restricted tools sends one access request describing
// its task in its own words: a message, a voice-note script, evidence
// attachments, and every tool it needs with the parameters it will use.
// Toolyard checks the request in the background (rule flags, optionally a
// judge model), puts it in the owner's inbox, and — when the owner allows
// some or all of the tools — issues one scoped, single-use grant per tool.
// The agent then calls each tool with `_grant` and the gateway redeems it.
//
// The same inbox also carries questions, blockers and updates, so an owner
// running many agents in parallel has one place to look.
//
// The agent-facing contract is docs/guidelines/agent-protocol.md; this
// package implements it.
package inbox

// Request kinds.
const (
	KindAccess   = "access"
	KindQuestion = "question"
	KindBlocker  = "blocker"
	KindUpdate   = "update"
)

// Request statuses.
const (
	StatusPending   = "pending"
	StatusApproved  = "approved"
	StatusDenied    = "denied"
	StatusReturned  = "returned"
	StatusAnswered  = "answered"
	StatusRead      = "read"
	StatusCancelled = "cancelled"
	StatusExpired   = "expired"
)

// Urgencies.
const (
	UrgencyNow    = "now"
	UrgencySoon   = "soon"
	UrgencyDigest = "digest"
	UrgencyFYI    = "fyi"
)

// Per-tool decisions on an access request.
const (
	ToolPending = ""
	ToolAllowed = "allowed"
	ToolRefused = "refused"
)

// Flag levels.
const (
	LevelRed   = "red"
	LevelAmber = "amber"
)

// Flag is something toolyard wants the owner to notice. Flags are the only
// thing toolyard adds to a request by default; everything else on the page
// is the agent's own words.
type Flag struct {
	Level  string `json:"level"`
	Label  string `json:"label"`
	Why    string `json:"why"`
	Source string `json:"source"` // "rule" | "judge"
}

// Facts are the three things every access request must answer.
type Facts struct {
	WhyNow        string `json:"why_now"`
	IfItGoesWrong string `json:"if_it_goes_wrong"`
	Undo          string `json:"undo"`
}

// Audio is the voice-note script. Toolyard (the dashboard) turns it into
// speech; the agent never handles audio.
type Audio struct {
	Script string `json:"script"`
}

// Option is one choice on a question or blocker.
type Option struct {
	Label  string `json:"label"`
	Detail string `json:"detail,omitempty"`
}

// Series is one line or bar series in a chart attachment.
type Series struct {
	Name   string    `json:"name"`
	Values []float64 `json:"values"`
}

// Attachment is one piece of evidence. Which fields apply depends on Type;
// see validate.go for the rules per type.
type Attachment struct {
	Type    string `json:"type"`
	Title   string `json:"title,omitempty"`
	Caption string `json:"caption,omitempty"`
	Spoken  string `json:"spoken,omitempty"`

	// markdown, code, log
	Body string `json:"body,omitempty"`
	// table
	Columns []string `json:"columns,omitempty"`
	Rows    [][]any  `json:"rows,omitempty"`
	// chart
	Chart  string   `json:"chart,omitempty"` // line | bar
	Unit   string   `json:"unit,omitempty"`
	X      []any    `json:"x,omitempty"`
	Series []Series `json:"series,omitempty"`
	// diff, code
	File     string `json:"file,omitempty"`
	Patch    string `json:"patch,omitempty"`
	Language string `json:"language,omitempty"`
	// image, video, file, link
	URL       string `json:"url,omitempty"`
	Alt       string `json:"alt,omitempty"`
	PosterURL string `json:"poster_url,omitempty"`
	Name      string `json:"name,omitempty"`
	Label     string `json:"label,omitempty"`

	// Set by toolyard when it copies a linked file.
	Blob        string `json:"blob,omitempty"`
	PosterBlob  string `json:"poster_blob,omitempty"`
	ContentType string `json:"content_type,omitempty"`
	Size        int64  `json:"size,omitempty"`
	FetchError  string `json:"fetch_error,omitempty"`
}

// ToolRequest is one tool the agent needs, with the parameters it will
// call it with. Summary is the agent's own description of the call.
type ToolRequest struct {
	Tool     string                `json:"tool"`
	Required bool                  `json:"required"`
	Summary  string                `json:"summary"`
	Params   map[string]Constraint `json:"params"`
	After    []string              `json:"after,omitempty"`

	// Set by toolyard.
	Upstream string `json:"upstream,omitempty"`
	Flags    []Flag `json:"flags,omitempty"`
	Decision string `json:"decision,omitempty"` // "" | allowed | refused
	GrantID  string `json:"grant_id,omitempty"`
	// Explanation is the judge's plain reading of the call, shown only
	// when the owner asks ("Ask toolyard").
	Explanation string `json:"explanation,omitempty"`
}

// Activity is one line on a request's timeline.
type Activity struct {
	At   int64  `json:"at"`
	Text string `json:"text"`
}

// Request is the full stored shape of an inbox item.
type Request struct {
	ID        string `json:"id"`
	AgentID   string `json:"agent_id"`
	SessionID string `json:"session_id,omitempty"`
	Kind      string `json:"kind"`
	Status    string `json:"status"`
	Urgency   string `json:"urgency"`
	RelatedID string `json:"related_id,omitempty"`

	Title       string        `json:"title"`
	Summary     string        `json:"summary"`
	Message     string        `json:"message"`
	Facts       *Facts        `json:"facts,omitempty"`
	Audio       Audio         `json:"audio"`
	Tools       []ToolRequest `json:"tools,omitempty"`
	Options     []Option      `json:"options,omitempty"`
	Attachments []Attachment  `json:"attachments,omitempty"`
	TTLSeconds  int           `json:"ttl_seconds,omitempty"`

	// Set by toolyard.
	Flags        []Flag   `json:"flags,omitempty"` // request-level (questions, blockers)
	Checked      bool     `json:"checked"`         // background checks finished
	ToolyardSum  string   `json:"toolyard_summary,omitempty"`
	DryRunCount  int      `json:"dry_run_count"`
	DroppedFlags []string `json:"dropped_flags,omitempty"`
	OwnerNote    string   `json:"owner_note,omitempty"`
	Answer       string   `json:"answer,omitempty"`
	SnoozedUntil int64    `json:"snoozed_until,omitempty"`
	// RequestedUrgency is what the agent asked for when toolyard lowered
	// it; Downgraded says why.
	RequestedUrgency string     `json:"requested_urgency,omitempty"`
	Downgraded       string     `json:"downgraded,omitempty"`
	RemindedAt       int64      `json:"reminded_at,omitempty"`
	DigestedAt       int64      `json:"digested_at,omitempty"`
	DecidedBy        string     `json:"decided_by,omitempty"`
	DecidedAt        int64      `json:"decided_at,omitempty"`
	CreatedAt        int64      `json:"created_at"`
	UpdatedAt        int64      `json:"updated_at"`
	ExpiresAt        int64      `json:"expires_at"`
	GrantsExpire     int64      `json:"grants_expire_at,omitempty"`
	Activity         []Activity `json:"activity,omitempty"`
}

// AllFlags returns request-level flags plus every tool's flags.
func (r *Request) AllFlags() []Flag {
	out := append([]Flag(nil), r.Flags...)
	for _, t := range r.Tools {
		out = append(out, t.Flags...)
	}
	return out
}

// IsOpen reports whether the request still needs the owner.
func (r *Request) IsOpen() bool { return r.Status == StatusPending }

func (r *Request) addActivity(at int64, text string) {
	r.Activity = append(r.Activity, Activity{At: at, Text: text})
}
