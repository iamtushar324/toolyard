package inbox

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/url"
	"regexp"
	"strings"
	"unicode/utf8"
)

// Limits. These are the numbers the agent protocol promises; keep them in
// sync with docs/guidelines/agent-protocol.md (a test checks the key ones).
const (
	MaxTitle        = 60
	MaxSummary      = 200
	MaxMessage      = 4000
	MaxFact         = 600
	MaxAudioWords   = 75
	MaxTools        = 12
	MaxToolSummary  = 200
	MaxAttachments  = 12
	MaxCaption      = 300
	MaxSpoken       = 120
	MaxMarkdown     = 20000
	MaxCodeBody     = 60000
	MaxTableCols    = 12
	MaxTableRows    = 200
	MaxChartSeries  = 6
	MaxChartPoints  = 500
	MinOptions      = 2
	MaxOptions      = 4
	MaxOptionLabel  = 80
	MaxOptionDetail = 200
	DefaultTTL      = 1800
	MinTTL          = 60
	MaxTTL          = 86400
)

// Problem is one thing wrong with a request, with the JSON path it's at.
type Problem struct {
	Path    string `json:"path"`
	Message string `json:"message"`
}

// Catalog tells the inbox how the gateway would treat a tool call.
// Implemented by the gateway.
type Catalog interface {
	// Access returns "open", "restricted", "denied" or "unknown", plus the
	// tool's upstream.
	Access(ctx context.Context, agentID, tool string, args map[string]any) (access, upstream string)
}

// Tool access values.
const (
	AccessOpen       = "open"
	AccessRestricted = "restricted"
	AccessDenied     = "denied"
	AccessUnknown    = "unknown"
)

// Submission is the wire form agents send to inbox.request / inbox.ask /
// inbox.post. Params stay raw here so a bad constraint is reported with
// its path instead of failing the whole decode.
type Submission struct {
	Kind        string           `json:"kind,omitempty"`
	SessionID   string           `json:"session_id,omitempty"`
	RelatedID   string           `json:"request_id,omitempty"`
	Title       string           `json:"title"`
	Summary     string           `json:"summary"`
	Message     string           `json:"message"`
	Facts       *Facts           `json:"facts,omitempty"`
	Audio       Audio            `json:"audio"`
	Urgency     string           `json:"urgency"`
	Tools       []SubmissionTool `json:"tools,omitempty"`
	Options     []Option         `json:"options,omitempty"`
	Attachments []Attachment     `json:"attachments,omitempty"`
	TTLSeconds  int              `json:"ttl_seconds,omitempty"`
	DryRun      bool             `json:"dry_run,omitempty"`

	IdempotencyKey string `json:"idempotency_key,omitempty"`
}

// SubmissionTool is one tool in a Submission.
type SubmissionTool struct {
	Tool     string         `json:"tool"`
	Required bool           `json:"required"`
	Summary  string         `json:"summary"`
	Params   map[string]any `json:"params"`
	After    []string       `json:"after,omitempty"`
}

// DecodeSubmission decodes a Submission from a generic map (MCP arguments).
func DecodeSubmission(args map[string]any) (*Submission, error) {
	b, err := json.Marshal(args)
	if err != nil {
		return nil, err
	}
	var s Submission
	if err := json.Unmarshal(b, &s); err != nil {
		return nil, fmt.Errorf("request isn't in the expected shape: %v", err)
	}
	return &s, nil
}

var (
	urlInText = regexp.MustCompile(`(?i)\bhttps?://|\bwww\.`)
	hashLike  = regexp.MustCompile(`\b[0-9a-f]{7,40}\b`)
	idLike    = regexp.MustCompile(`\b[a-z]{2,4}_[A-Za-z0-9]{6,}\b`)
)

// ValidateOptions tune validation.
type ValidateOptions struct {
	// AllowPrivateMedia accepts media links on private networks (an
	// operator opt-in for home/LAN setups).
	AllowPrivateMedia bool
}

// Validate checks a submission and converts it into a Request. The request
// is returned even when there are problems, so dry runs can still compute
// flags and a preview.
func Validate(ctx context.Context, cat Catalog, agentID string, s *Submission, opt ...ValidateOptions) (*Request, []Problem, []Problem) {
	var vo ValidateOptions
	if len(opt) > 0 {
		vo = opt[0]
	}
	var probs, warns []Problem
	add := func(path, format string, a ...any) { probs = append(probs, Problem{path, fmt.Sprintf(format, a...)}) }
	warn := func(path, format string, a ...any) { warns = append(warns, Problem{path, fmt.Sprintf(format, a...)}) }
	if err := validateIdempotencyKey(s.IdempotencyKey); err != nil {
		add("idempotency_key", "%s", err)
	}

	r := &Request{
		Kind:       s.Kind,
		SessionID:  strings.TrimSpace(s.SessionID),
		RelatedID:  strings.TrimSpace(s.RelatedID),
		Title:      strings.TrimSpace(s.Title),
		Summary:    strings.TrimSpace(s.Summary),
		Message:    strings.TrimSpace(s.Message),
		Facts:      s.Facts,
		Audio:      Audio{Script: strings.TrimSpace(s.Audio.Script)},
		Urgency:    strings.TrimSpace(s.Urgency),
		Options:    s.Options,
		TTLSeconds: s.TTLSeconds,
	}

	switch r.Kind {
	case KindAccess, KindQuestion, KindBlocker, KindUpdate:
	default:
		add("kind", "unknown kind %q", r.Kind)
	}

	checkText(add, "title", r.Title, MaxTitle, "Verb + object + target, e.g. \"Deploy api to prod\".")
	checkText(add, "summary", r.Summary, MaxSummary, "One line for the inbox card.")
	checkText(add, "message", r.Message, MaxMessage, "Write it in the first person: what you want to do and why.")

	switch r.Urgency {
	case UrgencyNow, UrgencySoon, UrgencyDigest, UrgencyFYI:
	case "":
		add("urgency", "missing; use now, soon, digest or fyi")
	default:
		add("urgency", "%q isn't one of now, soon, digest, fyi", r.Urgency)
	}
	if r.Kind == KindUpdate && (r.Urgency == UrgencyNow || r.Urgency == UrgencySoon) {
		warn("urgency", "updates are shown as fyi; urgency lowered")
		r.Urgency = UrgencyFYI
	}

	// Voice note.
	script := r.Audio.Script
	if script == "" {
		add("audio.script", "missing. Every request needs a voice note: at most %d words, first person, written to be heard.", MaxAudioWords)
	} else {
		if n := len(strings.Fields(script)); n > MaxAudioWords {
			add("audio.script", "%d words; max %d. Keep what you want, why it's safe, one number, and the exact ask.", n, MaxAudioWords)
		}
		if urlInText.MatchString(script) {
			add("audio.script", "contains a URL. The script is read aloud; put links in attachments.")
		}
		if hashLike.MatchString(strings.ToLower(script)) || idLike.MatchString(script) {
			warn("audio.script", "looks like it contains a hash or ID. Those don't read well aloud; describe them instead (\"the merge commit\").")
		}
	}

	// Facts: required for access requests.
	if r.Kind == KindAccess {
		if r.Facts == nil {
			add("facts", "missing; give why_now, if_it_goes_wrong and undo")
		} else {
			r.Facts.WhyNow = strings.TrimSpace(r.Facts.WhyNow)
			r.Facts.IfItGoesWrong = strings.TrimSpace(r.Facts.IfItGoesWrong)
			r.Facts.Undo = strings.TrimSpace(r.Facts.Undo)
			checkText(add, "facts.why_now", r.Facts.WhyNow, MaxFact, "")
			checkText(add, "facts.if_it_goes_wrong", r.Facts.IfItGoesWrong, MaxFact, "")
			checkText(add, "facts.undo", r.Facts.Undo, MaxFact, "Say how you'd undo it, or say plainly that it can't be undone.")
		}
	}

	// Tools.
	switch r.Kind {
	case KindAccess:
		if len(s.Tools) == 0 {
			add("tools", "an access request needs at least one tool")
		}
		if len(s.Tools) > MaxTools {
			add("tools", "%d tools; max %d. Split the task, or leave out tools you don't need.", len(s.Tools), MaxTools)
		}
		names := map[string]bool{}
		for _, st := range s.Tools {
			names[strings.TrimSpace(st.Tool)] = true
		}
		for i, st := range s.Tools {
			p := fmt.Sprintf("tools[%d]", i)
			tr := ToolRequest{Tool: strings.TrimSpace(st.Tool), Required: st.Required, Summary: strings.TrimSpace(st.Summary), After: st.After, Params: map[string]Constraint{}}
			if tr.Tool == "" {
				add(p+".tool", "missing")
			} else if cat != nil {
				access, upstream := cat.Access(ctx, agentID, tr.Tool, nil)
				tr.Upstream = upstream
				switch access {
				case AccessUnknown:
					add(p+".tool", "%q isn't a toolyard tool. Use the catalog name, e.g. from tools.search.", tr.Tool)
				case AccessDenied:
					add(p+".tool", "%q is blocked for you by your owner. Don't ask for it.", tr.Tool)
				case AccessOpen:
					warn(p+".tool", "%q doesn't need permission; you can call it directly.", tr.Tool)
				}
			}
			checkText(add, p+".summary", tr.Summary, MaxToolSummary, "Say in plain words what this call will do.")
			for name, raw := range st.Params {
				c, err := ParseConstraint(raw)
				if err != nil {
					add(p+".params."+name, "%v", err)
					continue
				}
				tr.Params[name] = c
			}
			for j, a := range tr.After {
				if !names[a] {
					add(fmt.Sprintf("%s.after[%d]", p, j), "%q isn't one of the tools in this request", a)
				}
			}
			r.Tools = append(r.Tools, tr)
		}
		if len(s.Options) > 0 {
			add("options", "options are for inbox.ask; an access request lists tools")
		}
		switch {
		case r.TTLSeconds == 0:
			r.TTLSeconds = DefaultTTL
		case r.TTLSeconds < MinTTL || r.TTLSeconds > MaxTTL:
			add("ttl_seconds", "must be between %d and %d", MinTTL, MaxTTL)
		}
	case KindQuestion, KindBlocker:
		if len(s.Tools) > 0 {
			add("tools", "a question can't carry tools; send an access request for permissions")
		}
		if len(s.Options) < MinOptions || len(s.Options) > MaxOptions {
			add("options", "give %d to %d options", MinOptions, MaxOptions)
		}
		for i := range r.Options {
			r.Options[i].Label = strings.TrimSpace(r.Options[i].Label)
			r.Options[i].Detail = strings.TrimSpace(r.Options[i].Detail)
			checkText(add, fmt.Sprintf("options[%d].label", i), r.Options[i].Label, MaxOptionLabel, "")
			if utf8.RuneCountInString(r.Options[i].Detail) > MaxOptionDetail {
				add(fmt.Sprintf("options[%d].detail", i), "too long; max %d characters", MaxOptionDetail)
			}
		}
		r.TTLSeconds = 0
	case KindUpdate:
		if len(s.Tools) > 0 || len(s.Options) > 0 {
			add("kind", "an update can't carry tools or options")
		}
		r.TTLSeconds = 0
	}

	// Attachments.
	if len(s.Attachments) > MaxAttachments {
		add("attachments", "%d attachments; max %d", len(s.Attachments), MaxAttachments)
	}
	for i, a := range s.Attachments {
		p := fmt.Sprintf("attachments[%d]", i)
		a.Blob, a.PosterBlob, a.ContentType, a.Size, a.FetchError = "", "", "", 0, ""
		validateAttachment(add, p, &a, vo.AllowPrivateMedia)
		r.Attachments = append(r.Attachments, a)
	}
	return r, probs, warns
}

func checkText(add func(string, string, ...any), path, v string, max int, hint string) {
	n := utf8.RuneCountInString(v)
	switch {
	case n == 0:
		if hint != "" {
			add(path, "missing. %s", hint)
		} else {
			add(path, "missing")
		}
	case n > max:
		add(path, "%d characters; max %d", n, max)
	}
}

func validateAttachment(add func(string, string, ...any), p string, a *Attachment, allowPrivate bool) {
	if utf8.RuneCountInString(a.Caption) > MaxCaption {
		add(p+".caption", "max %d characters", MaxCaption)
	}
	if utf8.RuneCountInString(a.Spoken) > MaxSpoken {
		add(p+".spoken", "max %d characters", MaxSpoken)
	}
	bodyLimit := func(field, v string, max int) {
		n := len(v)
		if n == 0 {
			add(p+"."+field, "missing")
		} else if n > max {
			add(p+"."+field, "%d bytes; max %d. Trim it to the part that matters.", n, max)
		}
	}
	switch a.Type {
	case "markdown":
		bodyLimit("body", a.Body, MaxMarkdown)
	case "table":
		if a.Title == "" {
			add(p+".title", "missing")
		}
		if len(a.Columns) == 0 || len(a.Columns) > MaxTableCols {
			add(p+".columns", "give 1 to %d columns", MaxTableCols)
		}
		if len(a.Rows) == 0 || len(a.Rows) > MaxTableRows {
			add(p+".rows", "give 1 to %d rows", MaxTableRows)
		}
		for j, row := range a.Rows {
			if len(row) != len(a.Columns) {
				add(fmt.Sprintf("%s.rows[%d]", p, j), "has %d cells; the table has %d columns", len(row), len(a.Columns))
				break
			}
		}
	case "chart":
		if a.Title == "" {
			add(p+".title", "missing")
		}
		if a.Chart != "line" && a.Chart != "bar" {
			add(p+".chart", "use \"line\" or \"bar\"")
		}
		if len(a.Series) == 0 || len(a.Series) > MaxChartSeries {
			add(p+".series", "give 1 to %d series", MaxChartSeries)
		}
		for j, s := range a.Series {
			if len(s.Values) == 0 || len(s.Values) > MaxChartPoints {
				add(fmt.Sprintf("%s.series[%d].values", p, j), "give 1 to %d values", MaxChartPoints)
			}
			if len(a.X) > 0 && len(a.X) != len(s.Values) {
				add(fmt.Sprintf("%s.series[%d].values", p, j), "has %d values but x has %d labels", len(s.Values), len(a.X))
			}
		}
	case "diff":
		if a.File == "" {
			add(p+".file", "missing")
		}
		bodyLimit("patch", a.Patch, MaxCodeBody)
	case "code":
		if a.File == "" && a.Title == "" {
			add(p+".file", "give a file name or a title")
		}
		bodyLimit("body", a.Body, MaxCodeBody)
	case "log":
		bodyLimit("body", a.Body, MaxCodeBody)
	case "image", "video", "file":
		checkMediaURL(add, p+".url", a.URL, allowPrivate)
		if a.Type == "file" && a.Name == "" {
			add(p+".name", "missing")
		}
		if a.Type == "video" && a.PosterURL != "" {
			checkMediaURL(add, p+".poster_url", a.PosterURL, allowPrivate)
		}
	case "link":
		if a.Label == "" {
			add(p+".label", "missing")
		}
		if u, err := url.Parse(a.URL); err != nil || (u.Scheme != "https" && u.Scheme != "http") || u.Host == "" {
			add(p+".url", "must be an http(s) URL")
		}
	case "":
		add(p+".type", "missing")
	default:
		add(p+".type", "unknown type %q (markdown, table, chart, diff, code, log, image, video, file, link)", a.Type)
	}
}

// checkMediaURL rejects URLs toolyard can't or mustn't fetch. The fetcher
// repeats the network-level check after DNS resolution.
func checkMediaURL(add func(string, string, ...any), path, raw string, allowPrivate bool) {
	u, err := url.Parse(raw)
	if err != nil || (u.Scheme != "https" && u.Scheme != "http") || u.Host == "" {
		add(path, "must be an http(s) URL toolyard can reach")
		return
	}
	if allowPrivate {
		return
	}
	host := strings.ToLower(u.Hostname())
	if host == "localhost" || strings.HasSuffix(host, ".localhost") || strings.HasSuffix(host, ".local") || strings.HasSuffix(host, ".internal") {
		add(path, "%s isn't reachable from toolyard. Host the file somewhere public, or ask your owner where to put it.", host)
		return
	}
	if ip := net.ParseIP(host); ip != nil && !publicIP(ip) {
		add(path, "%s is a private or local address, which toolyard won't fetch", host)
	}
}
