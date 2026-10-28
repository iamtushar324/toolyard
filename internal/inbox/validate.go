package inbox

import (
	"bytes"
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
	MaxTitle       = 60
	MaxSummary     = 200
	MaxMessage     = 20000
	MaxFact        = 600
	MaxAudioWords  = 75
	MaxTools       = 50 // schema_version 3; MaxToolsLegacy before
	MaxToolsLegacy = 12
	MaxToolSummary = 200
	MaxAttachments = 24
	MaxCaption     = 300
	MaxSpoken      = 120
	MaxMarkdown    = 20000
	MaxCodeBody    = 60000
	MaxTableCols   = 12
	MaxTableRows   = 200
	MaxChartSeries = 6
	MaxChartPoints = 500
	MaxPieSlices   = 24
	MaxChartLabel  = 80 // an x label, a series name, the unit
	// MaxSubmissionBytes bounds a whole submission as JSON, and
	// MaxParamsBytes one call's params.
	MaxSubmissionBytes = 1 << 20
	MaxParamsBytes     = 64 << 10
	// MaxCallbackRefLen bounds a callback_ref (the same bound as the
	// _callback_ref argument).
	MaxCallbackRefLen = 512
	MinOptions        = 2
	MaxOptions        = 4
	MaxOptionLabel    = 80
	MaxOptionDetail   = 200
	DefaultTTL        = 1800
	MinTTL            = 60
	MaxTTL            = 86400
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

// ToolNamer is the optional part of a Catalog that maps a name an agent
// may use for a tool to the catalog name: the gateway lists tools under
// OpenAI-safe names ("inbox_request" for "inbox.request") and accepts
// both. Requests and checks keep the catalog name, so a grant names the
// tool as policies and audit do. Only a tool agentID may use is resolved:
// any other name (unknown, or a tool outside the agent's scope) comes back
// unchanged, so no answer reveals a tool the agent cannot see.
type ToolNamer interface {
	CanonicalTool(ctx context.Context, agentID, name string) string
}

// canonicalTool is name as the catalog names it for agentID, when cat is
// a ToolNamer.
func canonicalTool(ctx context.Context, cat Catalog, agentID, name string) string {
	if namer, ok := cat.(ToolNamer); ok && name != "" {
		return namer.CanonicalTool(ctx, agentID, name)
	}
	return name
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
	CallbackRef       string       `json:"callback_ref,omitempty"`
	CallbackRefs      []string     `json:"callback_refs,omitempty"`
	PendingTTLSeconds int          `json:"pending_ttl_seconds,omitempty"`
	SchemaVersion     int          `json:"schema_version,omitempty"`
	ClientRequestID   string       `json:"client_request_id,omitempty"`
	Prompt            string       `json:"prompt,omitempty"`
	Context           string       `json:"context,omitempty"`
	Question          *Question    `json:"question,omitempty"`
	Task              *TaskContext `json:"task,omitempty"`
	Blocking          bool         `json:"blocking,omitempty"`

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
	// Questions are the schema version 3 questions (questions_v3.go).
	Questions []QuestionSpec `json:"questions,omitempty"`
}

// SubmissionTool is one tool in a Submission.
type SubmissionTool struct {
	CallID          string         `json:"call_id"`
	Target          string         `json:"target"`
	ExpectedEffects string         `json:"expected_effects"`
	Operation       string         `json:"operation"`
	AffectedScope   string         `json:"affected_scope,omitempty"`
	MaterialRisks   string         `json:"material_risks,omitempty"`
	Undo            string         `json:"undo,omitempty"`
	Tool            string         `json:"tool"`
	Required        bool           `json:"required"`
	Summary         string         `json:"summary"`
	Params          map[string]any `json:"params"`
	After           []string       `json:"after,omitempty"`
}

// jsonSize is the size of v as the agent sent it: JSON without HTML
// escaping, so "<", ">" and "&" count one byte each, not six (\u003c).
func jsonSize(v any) (int, error) {
	var b bytes.Buffer
	enc := json.NewEncoder(&b)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(v); err != nil {
		return 0, err
	}
	return b.Len() - 1, nil // Encode ends with a newline
}

// DecodeSubmission decodes a Submission from a generic map (MCP arguments).
func DecodeSubmission(args map[string]any) (*Submission, error) {
	b, err := json.Marshal(args)
	if err != nil {
		return nil, err
	}
	var s Submission
	dec := json.NewDecoder(bytes.NewReader(b))
	dec.UseNumber()
	if err := dec.Decode(&s); err != nil {
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
	s = normalizeQuestion(s)
	var vo ValidateOptions
	if len(opt) > 0 {
		vo = opt[0]
	}
	var probs, warns []Problem
	add := func(path, format string, a ...any) { probs = append(probs, Problem{path, fmt.Sprintf(format, a...)}) }
	warn := func(path, format string, a ...any) { warns = append(warns, Problem{path, fmt.Sprintf(format, a...)}) }
	rich := s.SchemaVersion == SchemaVersionRich

	r := &Request{
		CallbackRef: s.CallbackRef, ExecutionMode: "agent", PendingTTLSeconds: s.PendingTTLSeconds,
		SchemaVersion: s.SchemaVersion, Revision: 1, ClientRequestID: s.ClientRequestID, Question: s.Question, Task: s.Task, Blocking: s.Blocking,
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
	// Question kinds check their version in validateQuestion.
	if r.Kind != KindQuestion && r.Kind != KindBlocker {
		switch s.SchemaVersion {
		case 0, 1, 2, SchemaVersionRich:
		default:
			add("schema_version", "use 1, 2 or 3")
		}
	}
	r.CallbackRefs = validateCallbackRefs(add, s.CallbackRefs)
	if len(s.CallbackRef) > MaxCallbackRefLen {
		add("callback_ref", "at most %d bytes", MaxCallbackRefLen)
	}
	if n, err := jsonSize(s); err == nil && n > MaxSubmissionBytes {
		add("", "the request is %d bytes as JSON; max %d. Attach large evidence as an upload instead.", n, MaxSubmissionBytes)
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
	if script != "" {
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

	// Facts and task objective: required for access requests.
	if r.Kind == KindAccess {
		if r.Task == nil {
			add("task.objective", "missing; explain the task this batch advances")
		} else {
			checkText(add, "task.objective", r.Task.Objective, MaxFact, "Explain the task this batch advances.")
		}
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
		maxTools := MaxToolsLegacy
		if rich {
			maxTools = MaxTools
		}
		if len(s.Tools) > maxTools {
			add("tools", "%d tools; max %d (%d with schema_version 3). Split the task, or leave out tools you don't need.", len(s.Tools), maxTools, MaxTools)
		}
		names := map[string]bool{}
		for _, st := range s.Tools {
			names[st.CallID] = true
		}
		for i, st := range s.Tools {
			p := fmt.Sprintf("tools[%d]", i)
			tr := ToolRequest{CallID: st.CallID, Target: strings.TrimSpace(st.Target), ExpectedEffects: strings.TrimSpace(st.ExpectedEffects), Operation: st.Operation, AffectedScope: strings.TrimSpace(st.AffectedScope), MaterialRisks: strings.TrimSpace(st.MaterialRisks), Undo: strings.TrimSpace(st.Undo), Tool: strings.TrimSpace(st.Tool), Required: st.Required, Summary: strings.TrimSpace(st.Summary), After: st.After, Params: map[string]Constraint{}}
			supplied := tr.Tool
			tr.Tool = canonicalTool(ctx, cat, agentID, supplied)
			if tr.Tool == "" {
				add(p+".tool", "missing")
			} else if cat != nil {
				access, upstream := cat.Access(ctx, agentID, tr.Tool, exactArgs(st.Params))
				tr.Upstream = upstream
				switch access {
				case AccessUnknown:
					// Named as the agent named it: an unknown tool and one
					// outside its scope read the same.
					tr.Tool = supplied
					add(p+".tool", "%q isn't a toolyard tool. Use the catalog name, e.g. from tools.search.", supplied)
				case AccessDenied:
					add(p+".tool", "%q is blocked for you by your owner. Don't ask for it.", tr.Tool)
				case AccessOpen:
					warn(p+".tool", "%q doesn't need permission; you can call it directly.", tr.Tool)
				}
			}
			validateCallContext(add, p, &tr)
			if n, err := jsonSize(st.Params); err == nil && n > MaxParamsBytes {
				add(p+".params", "%d bytes as JSON; max %d", n, MaxParamsBytes)
			}
			checkText(add, p+".summary", tr.Summary, MaxToolSummary, "Say in plain words what this call will do.")
			for name, raw := range st.Params {
				if !browserNumbersSafe(raw) {
					add(p+".params."+name, "numeric scope exceeds supported browser precision; use a tool with string identifiers or supported numeric bounds")
					continue
				}
				c, err := ParseConstraint(raw)
				if err != nil {
					add(p+".params."+name, "%v", err)
					continue
				}
				if c.Op == "any" || c.Op == "limit" && c.Pattern == "" {
					add(p+".params."+name, "unbounded scope; provide an exact value or enforced bounded constraint")
					continue
				}
				tr.Params[name] = c
			}
			for j, a := range tr.After {
				if !names[a] {
					add(fmt.Sprintf("%s.after[%d]", p, j), "%q isn't one of the call IDs in this request", a)
				}
			}
			r.Tools = append(r.Tools, tr)
		}
		validateCallIDs(add, r.Tools)
		if validator, ok := cat.(ScopeValidator); ok {
			for i, t := range r.Tools {
				for _, problem := range validator.ValidateScope(ctx, agentID, t) {
					add(fmt.Sprintf("tools[%d].%s", i, problem.Path), "%s", problem.Message)
				}
			}
		}
		if len(s.Options) > 0 {
			add("options", "options are for inbox.ask; an access request lists tools")
		}
		if rich {
			r.Question = nil
		}
		if r.PendingTTLSeconds == 0 {
			r.PendingTTLSeconds = int(RequestTTL.Seconds())
		}
		if r.PendingTTLSeconds < MinTTL || r.PendingTTLSeconds > 7*86400 {
			add("pending_ttl_seconds", "must be between 60 and 604800 seconds")
		}
		switch {
		case r.TTLSeconds == 0:
			r.TTLSeconds = DefaultTTL
		case r.TTLSeconds < MinTTL || r.TTLSeconds > MaxTTL:
			add("ttl_seconds", "must be between %d and %d", MinTTL, MaxTTL)
		}
	case KindQuestion, KindBlocker:
		if len(s.Tools) > 0 {
			add("tools", "a question can't carry tools; send an access request for permissions (it may carry questions too)")
		}
		if rich {
			// Options and the single question are mapped to questions[]
			// below.
			r.Question, r.Options = nil, nil
			validateTask(r, add)
		} else {
			validateQuestion(r, s, add)
		}
		r.TTLSeconds = 0
	case KindUpdate:
		if len(s.Tools) > 0 || len(s.Options) > 0 {
			add("kind", "an update can't carry tools or options")
		}
		r.TTLSeconds = 0
	}

	// Questions (schema version 3): an access item may carry questions,
	// and a question or blocker carries one or more.
	switch {
	case rich && r.Kind == KindUpdate:
		if len(s.Questions) > 0 || s.Question != nil {
			add("questions", "an update can't carry questions")
		}
	case rich && (r.Kind == KindQuestion || r.Kind == KindBlocker):
		qs := legacyQuestions(s)
		if len(qs) == 0 {
			add("questions", "give at least one question")
		}
		r.Questions = validateQuestionSpecs(add, qs)
	case rich && r.Kind == KindAccess:
		r.Questions = validateQuestionSpecs(add, legacyQuestions(s))
	case len(s.Questions) > 0:
		add("questions", "questions[] needs schema_version 3")
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

	// Markdown images show inline only when they cite the agent's own
	// upload; anything else is rendered as a link.
	warnImages := func(path, text string) {
		_, others := markdownImages(text)
		if len(others) > 0 {
			warn(path, "%d image(s) like %q won't show inline: upload the file with inbox.upload and cite ![alt](upload:<sha256>); other sources show as links", len(others), limitText(others[0], 80))
		}
	}
	warnImages("message", r.Message)
	for i, a := range r.Attachments {
		if a.Type == "markdown" {
			warnImages(fmt.Sprintf("attachments[%d].body", i), a.Body)
		}
	}
	return r, probs, warns
}

// validateCallbackRefs checks callback_refs (more receivers next to
// callback_ref) and returns them trimmed.
func validateCallbackRefs(add func(string, string, ...any), refs []string) []string {
	if len(refs) > MaxCallbackRefs {
		add("callback_refs", "%d receivers; max %d", len(refs), MaxCallbackRefs)
	}
	var out []string
	for i, ref := range refs {
		ref = strings.TrimSpace(ref)
		if ref == "" || len(ref) > MaxCallbackRefLen {
			add(fmt.Sprintf("callback_refs[%d]", i), "give a callback_ref from callbacks.register (at most %d bytes)", MaxCallbackRefLen)
			continue
		}
		out = append(out, ref)
	}
	return out
}

// exactArgs returns the parameters of a requested call that have an exact
// value, as call arguments, so the catalog judges argument-dependent access
// the way the call will route (a tool whose replace mode needs permission
// while an append does not). Nil when no parameter has an exact value.
func exactArgs(raw map[string]any) map[string]any {
	var out map[string]any
	for name, v := range raw {
		c, err := ParseConstraint(v)
		if err != nil || c.Op != "eq" {
			continue
		}
		if out == nil {
			out = map[string]any{}
		}
		out[name] = c.Eq
	}
	return out
}

// checkText validates required agent narrative, including known placeholders.
func checkText(add func(string, string, ...any), path, v string, max int, hint string) {
	checkBoundedText(add, path, v, max, hint)
	if v != "" && utf8.RuneCountInString(v) <= max && isPlaceholder(v) {
		add(path, "placeholder text; replace it with concrete task information")
	}
}

// Choice labels are user answers, not explanations. "None" and "Test" can
// be meaningful choices and still require nonempty, bounded text.
func checkBoundedText(add func(string, string, ...any), path, v string, max int, hint string) {
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
		validateChart(add, p, a)
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
		switch {
		case a.URL != "" && a.Upload != "":
			add(p+".upload", "give url or upload, not both")
		case a.Upload != "":
			checkUploadRef(add, p+".upload", a.Upload)
		default:
			checkMediaURL(add, p+".url", a.URL, allowPrivate)
		}
		if a.Type == "file" && a.Name == "" {
			add(p+".name", "missing")
		}
		if a.Type != "video" && a.PosterUpload != "" {
			add(p+".poster_upload", "only a video takes a poster")
		}
		if a.Type == "video" {
			switch {
			case a.PosterURL != "" && a.PosterUpload != "":
				add(p+".poster_upload", "give poster_url or poster_upload, not both")
			case a.PosterUpload != "":
				checkUploadRef(add, p+".poster_upload", a.PosterUpload)
			case a.PosterURL != "":
				checkMediaURL(add, p+".poster_url", a.PosterURL, allowPrivate)
			}
		}
	case "link":
		if a.Upload != "" || a.PosterUpload != "" {
			add(p+".upload", "only image, video and file attachments take an upload")
		}
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

// Chart kinds the dashboard draws.
var chartKinds = []string{"line", "bar", "stacked_bar", "area", "pie", "scatter"}

// validateChart checks a chart attachment's shape. Every chart has a title
// and 1 to MaxChartSeries series of 1 to MaxChartPoints values; x, when
// given, labels each value. A stacked bar stacks non-negative values. A pie
// is one series of 1 to MaxPieSlices non-negative values with a label in x
// for each slice. A scatter's x holds the numeric x coordinates its
// series' values (the y coordinates) pair with.
func validateChart(add func(string, string, ...any), p string, a *Attachment) {
	if a.Title == "" {
		add(p+".title", "missing")
	}
	known := false
	for _, k := range chartKinds {
		known = known || a.Chart == k
	}
	if !known {
		add(p+".chart", "use one of %s", strings.Join(chartKinds, ", "))
	}
	if len(a.Series) == 0 || len(a.Series) > MaxChartSeries {
		add(p+".series", "give 1 to %d series", MaxChartSeries)
	}
	if utf8.RuneCountInString(a.Unit) > MaxChartLabel {
		add(p+".unit", "max %d characters", MaxChartLabel)
	}
	for j, x := range a.X {
		switch v := x.(type) {
		case string:
			if utf8.RuneCountInString(v) > MaxChartLabel {
				add(fmt.Sprintf("%s.x[%d]", p, j), "max %d characters", MaxChartLabel)
			}
		default:
			if _, ok := answerNumber(x); !ok {
				add(fmt.Sprintf("%s.x[%d]", p, j), "an x label is a string or a number")
			}
		}
	}
	for j, s := range a.Series {
		if utf8.RuneCountInString(s.Name) > MaxChartLabel {
			add(fmt.Sprintf("%s.series[%d].name", p, j), "max %d characters", MaxChartLabel)
		}
		if len(s.Values) == 0 || len(s.Values) > MaxChartPoints {
			add(fmt.Sprintf("%s.series[%d].values", p, j), "give 1 to %d values", MaxChartPoints)
		}
		if len(a.X) > 0 && len(a.X) != len(s.Values) {
			add(fmt.Sprintf("%s.series[%d].values", p, j), "has %d values but x has %d labels", len(s.Values), len(a.X))
		}
		if a.Chart == "stacked_bar" || a.Chart == "pie" {
			for _, v := range s.Values {
				if v < 0 {
					add(fmt.Sprintf("%s.series[%d].values", p, j), "a %s takes values of 0 or more", a.Chart)
					break
				}
			}
		}
	}
	switch a.Chart {
	case "pie":
		if len(a.Series) != 1 {
			add(p+".series", "a pie takes exactly one series")
			return
		}
		vals := a.Series[0].Values
		if len(vals) > MaxPieSlices {
			add(p+".series[0].values", "give 1 to %d slices", MaxPieSlices)
		}
		sum := 0.0
		for _, v := range vals {
			sum += v
		}
		if sum <= 0 {
			add(p+".series[0].values", "a pie needs a total above 0")
		}
		if len(a.X) != len(vals) {
			add(p+".x", "name each slice: give one label in x per value")
		}
	case "scatter":
		if len(a.X) == 0 {
			add(p+".x", "a scatter needs the numeric x coordinates in x")
		}
		for j, x := range a.X {
			if _, ok := answerNumber(x); !ok {
				add(fmt.Sprintf("%s.x[%d]", p, j), "a scatter's x coordinates are numbers")
				break
			}
		}
	}
}

// checkUploadRef checks the form of an "upload:<sha256>" reference; Submit
// then checks that the calling agent owns it.
func checkUploadRef(add func(string, string, ...any), path, ref string) {
	if _, ok := ParseUploadRef(ref); !ok {
		add(path, "use the ref inbox.upload returned: \"upload:<sha256>\" (64 lowercase hex)")
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

// ScopeValidator optionally validates call parameters against the actual tool schema.
type ScopeValidator interface {
	ValidateScope(context.Context, string, ToolRequest) []Problem
}

func isPlaceholder(v string) bool {
	x := strings.ToLower(strings.TrimSpace(v))
	if strings.HasPrefix(x, "<") && strings.HasSuffix(x, ">") {
		return true
	}
	switch x {
	case "todo", "tbd", "n/a", "na", "placeholder", "test", "testing", "...", "reason", "purpose", "safe", "none":
		return true
	}
	return false
}
func validateCallContext(add func(string, string, ...any), p string, t *ToolRequest) {
	checkText(add, p+".target", t.Target, MaxFact, "Name the exact resource or bounded target.")
	checkText(add, p+".expected_effects", t.ExpectedEffects, MaxFact, "Describe the expected effects.")
	if t.Operation != "read" && t.Operation != "write" {
		add(p+".operation", "use read or write")
	}
	if t.Operation != "read" {
		checkText(add, p+".affected_scope", t.AffectedScope, MaxFact, "Name what changes.")
		checkText(add, p+".material_risks", t.MaterialRisks, MaxFact, "Describe concrete risks.")
		checkText(add, p+".undo", t.Undo, MaxFact, "Give a concrete undo path or state the action cannot be undone.")
	}
}
func validateCallIDs(add func(string, string, ...any), tools []ToolRequest) {
	ids := map[string]int{}
	validID := regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_.:-]{0,79}$`)
	for i, t := range tools {
		if !validID.MatchString(t.CallID) {
			add(fmt.Sprintf("tools[%d].call_id", i), "give a stable ID with 1-80 letters, digits, dots, colons, underscores or hyphens")
		}
		if _, ok := ids[t.CallID]; ok {
			add(fmt.Sprintf("tools[%d].call_id", i), "duplicate call ID; repeated tools need distinct call IDs")
		}
		ids[t.CallID] = i
	}
	states := map[string]int{}
	var visit func(string) bool
	visit = func(id string) bool {
		if states[id] == 1 {
			return true
		}
		if states[id] == 2 {
			return false
		}
		states[id] = 1
		if i, ok := ids[id]; ok {
			for _, dep := range tools[i].After {
				if _, ok := ids[dep]; ok && visit(dep) {
					return true
				}
			}
		}
		states[id] = 2
		return false
	}
	for _, t := range tools {
		if visit(t.CallID) {
			add("tools.after", "dependency cycle; use an acyclic graph of call IDs")
			break
		}
	}
}
