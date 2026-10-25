package inbox

import (
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"regexp"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"
)

// Schema version 3 ("rich items"): one item carries up to MaxTools tool
// calls and MaxQuestions questions of every type, and one decision answers
// them all. Versions 1 and 2 keep their contracts (questions.go).
const (
	SchemaVersionRich = 3
	MaxQuestions      = 20
	MaxQuestionPrompt = 1000
	MaxQuestionHelp   = 2000
	MaxChoiceOptions  = 12
	MaxAnswerText     = 10000
	MaxScalePoints    = 10
	MaxScaleLabel     = 80
	MaxUnit           = 24
	MaxCallbackRefs   = 5
	MaxInlineImages   = 24
)

// Question types. free_text, single_choice and multiple_choice are also the
// version 2 types.
const (
	QuestionFreeText       = "free_text"
	QuestionSingleChoice   = "single_choice"
	QuestionMultipleChoice = "multiple_choice"
	QuestionYesNo          = "yes_no"
	QuestionNumber         = "number"
	QuestionDate           = "date"
	QuestionDateTime       = "datetime"
	QuestionScale          = "scale"
	QuestionRanking        = "ranking"
)

// QuestionTypes lists the schema version 3 question types.
var QuestionTypes = []string{QuestionFreeText, QuestionSingleChoice, QuestionMultipleChoice, QuestionYesNo,
	QuestionNumber, QuestionDate, QuestionDateTime, QuestionScale, QuestionRanking}

// ErrBadAnswer wraps every refused answer: a missing required answer, an
// unknown option, a number out of range, an incomplete ranking.
var ErrBadAnswer = errors.New("answers are incomplete or invalid")

// stableID is the shape of question IDs (and of call IDs, validateCallIDs).
var stableID = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_.:-]{0,79}$`)

// QuestionSpec is one question of a schema version 3 item. Which fields
// apply depends on Type; validateQuestionShape refuses the others.
//
// The answer's value (Answer.Value) is typed by the question:
//
//	free_text        string
//	single_choice    option ID (string)
//	multiple_choice  option IDs ([]string)
//	yes_no           true or false
//	number           number within min..max, a multiple of step from min
//	date             "YYYY-MM-DD"
//	datetime         RFC 3339, e.g. "2026-10-25T14:30:00+05:30"
//	scale            whole number from 1 to scale
//	ranking          every option ID, most preferred first ([]string)
//
// Default pre-fills the answer form; Recommended is shown as a suggestion
// and is never selected by itself. Both have the answer's type.
type QuestionSpec struct {
	ID          string `json:"id"`
	Type        string `json:"type"`
	Prompt      string `json:"prompt"`
	Help        string `json:"help,omitempty"`
	Required    *bool  `json:"required,omitempty"` // default true
	Default     any    `json:"default,omitempty"`
	Recommended any    `json:"recommended,omitempty"`

	// single_choice, multiple_choice, ranking
	Options       []Option `json:"options,omitempty"`
	MinSelections int      `json:"min_selections,omitempty"`
	MaxSelections int      `json:"max_selections,omitempty"`
	// AllowOther accepts a custom text answer instead of a selection
	// (single_choice, multiple_choice).
	AllowOther bool `json:"allow_other,omitempty"`

	// free_text
	MinLength int  `json:"min_length,omitempty"`
	MaxLength int  `json:"max_length,omitempty"`
	Multiline bool `json:"multiline,omitempty"`

	// number
	Min  *float64 `json:"min,omitempty"`
	Max  *float64 `json:"max,omitempty"`
	Step *float64 `json:"step,omitempty"`
	Unit string   `json:"unit,omitempty"`

	// scale: 1 to Scale (default 5, at most 10), with end labels
	Scale    int    `json:"scale,omitempty"`
	MinLabel string `json:"min_label,omitempty"`
	MaxLabel string `json:"max_label,omitempty"`
}

// Answer is the owner's answer to one question, keyed by question ID in
// Decision.Answers and Request.Answers. Value is typed by the question
// (see QuestionSpec); Text is a custom answer (allow_other) or a comment.
// Labels are set by toolyard: the chosen options' labels (in ranking
// order), or Yes / No.
type Answer struct {
	Value  any      `json:"value,omitempty"`
	Text   string   `json:"text,omitempty"`
	Labels []string `json:"labels,omitempty"`
}

// InlineImage is an upload that a Markdown image in the message or in a
// markdown attachment cites ("![alt](upload:<sha256>)"). Toolyard resolves
// it when the item is sent; the dashboard shows only these images inline.
type InlineImage struct {
	Ref         string `json:"ref"`
	Blob        string `json:"blob"`
	ContentType string `json:"content_type,omitempty"`
}

func (q *QuestionSpec) required() bool { return q.Required == nil || *q.Required }

func (q *QuestionSpec) choice() bool {
	return q.Type == QuestionSingleChoice || q.Type == QuestionMultipleChoice || q.Type == QuestionRanking
}

func (q *QuestionSpec) option(id string) (Option, bool) {
	for _, o := range q.Options {
		if o.ID == id {
			return o, true
		}
	}
	return Option{}, false
}

func knownQuestionType(t string) bool {
	for _, k := range QuestionTypes {
		if k == t {
			return true
		}
	}
	return false
}

// legacyQuestions returns the questions of a schema version 3 submission:
// questions[], or else the single version 2 question (or the version 1
// options of a question) mapped to questions[0] with ID "answer". As in
// version 2, the mapped choice question accepts a custom text answer.
func legacyQuestions(s *Submission) []QuestionSpec {
	if len(s.Questions) > 0 {
		return s.Questions
	}
	prompt := strings.TrimSpace(s.Prompt)
	if prompt == "" {
		prompt = strings.TrimSpace(s.Title)
	}
	if s.Question != nil {
		q := QuestionSpec{ID: "answer", Type: s.Question.Type, Prompt: prompt, Options: append([]Option(nil), s.Question.Options...),
			MinSelections: s.Question.MinSelections, MaxSelections: s.Question.MaxSelections}
		q.AllowOther = q.Type == QuestionSingleChoice || q.Type == QuestionMultipleChoice
		return []QuestionSpec{q}
	}
	if len(s.Options) > 0 && (s.Kind == KindQuestion || s.Kind == KindBlocker) {
		opts := append([]Option(nil), s.Options...)
		for i := range opts {
			if opts[i].ID == "" {
				opts[i].ID = fmt.Sprintf("option_%d", i+1)
			}
		}
		return []QuestionSpec{{ID: "answer", Type: QuestionSingleChoice, Prompt: prompt, Options: opts, AllowOther: true}}
	}
	return nil
}

// validateQuestionSpecs checks the questions of a version 3 item and
// returns them normalized (required set, selection and length limits
// filled in, default and recommended in their stored form).
func validateQuestionSpecs(add func(string, string, ...any), in []QuestionSpec) []QuestionSpec {
	if len(in) > MaxQuestions {
		add("questions", "%d questions; max %d. Ask only what blocks the work, or split it over more items.", len(in), MaxQuestions)
	}
	seen := map[string]bool{}
	out := make([]QuestionSpec, 0, len(in))
	for i, q := range in {
		p := fmt.Sprintf("questions[%d]", i)
		q.Options = append([]Option(nil), q.Options...)
		switch {
		case !stableID.MatchString(q.ID):
			add(p+".id", "give a stable ID with 1-80 letters, digits, dots, colons, underscores or hyphens; answers are keyed by it")
		case seen[q.ID]:
			add(p+".id", "duplicate question ID")
		}
		seen[q.ID] = true
		q.Prompt = strings.TrimSpace(q.Prompt)
		checkBoundedText(add, p+".prompt", q.Prompt, MaxQuestionPrompt, "Ask the question in plain words.")
		q.Help = strings.TrimSpace(q.Help)
		if utf8.RuneCountInString(q.Help) > MaxQuestionHelp {
			add(p+".help", "max %d characters", MaxQuestionHelp)
		}
		req := q.required()
		q.Required = &req
		if !knownQuestionType(q.Type) {
			add(p+".type", "use one of %s", strings.Join(QuestionTypes, ", "))
			out = append(out, q)
			continue
		}
		validateQuestionShape(add, p, &q)
		for _, f := range []struct {
			name string
			v    *any
		}{{"default", &q.Default}, {"recommended", &q.Recommended}} {
			if *f.v == nil {
				continue
			}
			v, _, err := normalizeValue(&q, *f.v)
			if err != nil {
				add(p+"."+f.name, "%v", err)
				continue
			}
			*f.v = v
		}
		out = append(out, q)
	}
	return out
}

// validateQuestionShape checks the fields that depend on the question type
// and fills in the defaults of the limits.
func validateQuestionShape(add func(string, string, ...any), p string, q *QuestionSpec) {
	if q.choice() {
		if len(q.Options) < MinOptions || len(q.Options) > MaxChoiceOptions {
			add(p+".options", "give %d to %d options", MinOptions, MaxChoiceOptions)
		}
		validateOptionList(add, p+".options", q.Options)
	} else if len(q.Options) > 0 {
		add(p+".options", "only single_choice, multiple_choice and ranking questions take options")
	}
	switch q.Type {
	case QuestionSingleChoice:
		if q.MinSelections > 1 || q.MaxSelections > 1 {
			add(p, "single_choice permits at most one selection")
		}
		q.MinSelections, q.MaxSelections = 0, 1
	case QuestionMultipleChoice:
		if q.MaxSelections == 0 {
			q.MaxSelections = len(q.Options)
		}
		if q.MinSelections < 0 || q.MaxSelections < 0 || q.MinSelections > q.MaxSelections || q.MaxSelections > len(q.Options) {
			add(p, "invalid selection limits: use 0 ≤ min_selections ≤ max_selections ≤ the number of options")
		}
	default:
		if q.MinSelections != 0 || q.MaxSelections != 0 {
			add(p, "min_selections and max_selections apply to multiple_choice only")
		}
	}
	if q.AllowOther && q.Type != QuestionSingleChoice && q.Type != QuestionMultipleChoice {
		add(p+".allow_other", "only single_choice and multiple_choice questions take a custom answer")
	}
	if q.Type == QuestionRanking {
		for j, o := range q.Options {
			if o.Exclusive {
				add(fmt.Sprintf("%s.options[%d].exclusive", p, j), "a ranking orders every option; exclusive doesn't apply")
			}
		}
	}
	if q.Type == QuestionFreeText {
		if q.MaxLength == 0 {
			q.MaxLength = MaxAnswerText
		}
		if q.MinLength < 0 || q.MaxLength < 0 || q.MaxLength > MaxAnswerText || q.MinLength > q.MaxLength {
			add(p, "use 0 ≤ min_length ≤ max_length ≤ %d", MaxAnswerText)
		}
	} else if q.MinLength != 0 || q.MaxLength != 0 || q.Multiline {
		add(p, "min_length, max_length and multiline apply to free_text only")
	}
	if q.Type == QuestionNumber {
		if q.Min != nil && q.Max != nil && *q.Min > *q.Max {
			add(p+".min", "min is above max")
		}
		if q.Step != nil && !(*q.Step > 0) {
			add(p+".step", "use a positive step")
		}
		if utf8.RuneCountInString(q.Unit) > MaxUnit {
			add(p+".unit", "max %d characters", MaxUnit)
		}
	} else if q.Min != nil || q.Max != nil || q.Step != nil || q.Unit != "" {
		add(p, "min, max, step and unit apply to number only")
	}
	if q.Type == QuestionScale {
		if q.Scale == 0 {
			q.Scale = 5
		}
		if q.Scale < 2 || q.Scale > MaxScalePoints {
			add(p+".scale", "use 2 to %d points", MaxScalePoints)
		}
		if utf8.RuneCountInString(q.MinLabel) > MaxScaleLabel || utf8.RuneCountInString(q.MaxLabel) > MaxScaleLabel {
			add(p, "end labels: max %d characters each", MaxScaleLabel)
		}
	} else if q.Scale != 0 || q.MinLabel != "" || q.MaxLabel != "" {
		add(p, "scale, min_label and max_label apply to scale only")
	}
}

// validateOptionList checks the options of one version 3 question: unique
// stable IDs, unique bounded labels.
func validateOptionList(add func(string, string, ...any), p string, opts []Option) {
	ids, labels := map[string]bool{}, map[string]bool{}
	for i := range opts {
		o := &opts[i]
		op := fmt.Sprintf("%s[%d]", p, i)
		if o.ID == "" || len(o.ID) > 80 || strings.TrimSpace(o.ID) != o.ID || ids[o.ID] {
			add(op+".id", "must be a unique nonempty ID of at most 80 bytes")
		}
		ids[o.ID] = true
		o.Label = strings.TrimSpace(o.Label)
		checkBoundedText(add, op+".label", o.Label, MaxOptionLabel, "")
		if l := strings.ToLower(o.Label); labels[l] {
			add(op+".label", "duplicate label")
		} else {
			labels[l] = true
		}
		if utf8.RuneCountInString(o.Detail) > MaxOptionDetail {
			add(op+".detail", "max %d characters", MaxOptionDetail)
		}
	}
}

// answerNumber reads a JSON number in any of the forms a decoder yields.
func answerNumber(v any) (float64, bool) {
	var f float64
	switch n := v.(type) {
	case float64:
		f = n
	case float32:
		f = float64(n)
	case int:
		f = float64(n)
	case int64:
		f = float64(n)
	case json.Number:
		x, err := n.Float64()
		if err != nil {
			return 0, false
		}
		f = x
	default:
		return 0, false
	}
	if math.IsNaN(f) || math.IsInf(f, 0) {
		return 0, false
	}
	return f, true
}

// answerStrings reads a JSON array of strings.
func answerStrings(v any) ([]string, bool) {
	switch a := v.(type) {
	case []string:
		return append([]string(nil), a...), true
	case []any:
		out := make([]string, 0, len(a))
		for _, x := range a {
			s, ok := x.(string)
			if !ok {
				return nil, false
			}
			out = append(out, s)
		}
		return out, true
	}
	return nil, false
}

func formatNumber(f float64) string { return strconv.FormatFloat(f, 'f', -1, 64) }

// normalizeValue checks one answer value against q and returns it in its
// stored form (a string, a bool, a float64 or a []string), with the option
// labels of a choice or ranking (Yes / No for yes_no). An empty value (nil,
// "", an empty list) is no answer: (nil, nil, nil).
func normalizeValue(q *QuestionSpec, v any) (any, []string, error) {
	if v == nil {
		return nil, nil, nil
	}
	switch q.Type {
	case QuestionFreeText:
		s, ok := v.(string)
		if !ok {
			return nil, nil, errors.New("give the answer as a string")
		}
		if s == "" {
			return nil, nil, nil
		}
		maxLen := q.MaxLength
		if maxLen == 0 {
			maxLen = MaxAnswerText
		}
		n := utf8.RuneCountInString(s)
		if n > maxLen {
			return nil, nil, fmt.Errorf("%d characters; max %d", n, maxLen)
		}
		if n < q.MinLength {
			return nil, nil, fmt.Errorf("write at least %d characters", q.MinLength)
		}
		return s, nil, nil
	case QuestionSingleChoice:
		s, ok := v.(string)
		if !ok {
			return nil, nil, errors.New("give the ID of one option")
		}
		if s == "" {
			return nil, nil, nil
		}
		o, ok := q.option(s)
		if !ok {
			return nil, nil, fmt.Errorf("unknown option ID %q", s)
		}
		return s, []string{o.Label}, nil
	case QuestionMultipleChoice:
		ids, ok := answerStrings(v)
		if !ok {
			return nil, nil, errors.New("give a list of option IDs")
		}
		if len(ids) == 0 {
			return nil, nil, nil
		}
		seen := map[string]bool{}
		labels := make([]string, 0, len(ids))
		for _, id := range ids {
			if seen[id] {
				return nil, nil, fmt.Errorf("duplicate option ID %q", id)
			}
			seen[id] = true
			o, ok := q.option(id)
			if !ok {
				return nil, nil, fmt.Errorf("unknown option ID %q", id)
			}
			if o.Exclusive && len(ids) > 1 {
				return nil, nil, fmt.Errorf("%s must be selected alone", o.Label)
			}
			labels = append(labels, o.Label)
		}
		if q.MaxSelections > 0 && len(ids) > q.MaxSelections {
			return nil, nil, fmt.Errorf("select at most %d options", q.MaxSelections)
		}
		if len(ids) < q.MinSelections {
			return nil, nil, fmt.Errorf("select at least %d options", q.MinSelections)
		}
		return ids, labels, nil
	case QuestionYesNo:
		b, ok := v.(bool)
		if !ok {
			return nil, nil, errors.New("answer true (yes) or false (no)")
		}
		if b {
			return true, []string{"Yes"}, nil
		}
		return false, []string{"No"}, nil
	case QuestionNumber:
		f, ok := answerNumber(v)
		if !ok {
			return nil, nil, errors.New("give a number")
		}
		if q.Min != nil && f < *q.Min {
			return nil, nil, fmt.Errorf("%s is below the minimum %s", formatNumber(f), formatNumber(*q.Min))
		}
		if q.Max != nil && f > *q.Max {
			return nil, nil, fmt.Errorf("%s is above the maximum %s", formatNumber(f), formatNumber(*q.Max))
		}
		if q.Step != nil && *q.Step > 0 {
			base := 0.0
			if q.Min != nil {
				base = *q.Min
			}
			k := (f - base) / *q.Step
			if math.Abs(k-math.Round(k)) > 1e-9*math.Max(1, math.Abs(k)) {
				return nil, nil, fmt.Errorf("%s isn't a step of %s from %s", formatNumber(f), formatNumber(*q.Step), formatNumber(base))
			}
		}
		return f, nil, nil
	case QuestionDate:
		s, ok := v.(string)
		if !ok {
			return nil, nil, errors.New("give a date as YYYY-MM-DD")
		}
		if s == "" {
			return nil, nil, nil
		}
		if _, err := time.Parse("2006-01-02", s); err != nil {
			return nil, nil, errors.New("give a date as YYYY-MM-DD")
		}
		return s, nil, nil
	case QuestionDateTime:
		s, ok := v.(string)
		if !ok {
			return nil, nil, errors.New("give a date and time in RFC 3339, e.g. 2026-10-25T14:30:00+05:30")
		}
		if s == "" {
			return nil, nil, nil
		}
		if _, err := time.Parse(time.RFC3339, s); err != nil {
			return nil, nil, errors.New("give a date and time in RFC 3339, e.g. 2026-10-25T14:30:00+05:30")
		}
		return s, nil, nil
	case QuestionScale:
		f, ok := answerNumber(v)
		if !ok || f != math.Trunc(f) || f < 1 || f > float64(q.Scale) {
			return nil, nil, fmt.Errorf("give a whole number from 1 to %d", q.Scale)
		}
		return f, nil, nil
	case QuestionRanking:
		ids, ok := answerStrings(v)
		if !ok {
			return nil, nil, errors.New("give every option ID, most preferred first")
		}
		if len(ids) == 0 {
			return nil, nil, nil
		}
		if len(ids) != len(q.Options) {
			return nil, nil, fmt.Errorf("rank all %d options, not %d", len(q.Options), len(ids))
		}
		seen := map[string]bool{}
		labels := make([]string, 0, len(ids))
		for _, id := range ids {
			if seen[id] {
				return nil, nil, fmt.Errorf("duplicate option ID %q", id)
			}
			seen[id] = true
			o, ok := q.option(id)
			if !ok {
				return nil, nil, fmt.Errorf("unknown option ID %q", id)
			}
			labels = append(labels, o.Label)
		}
		return ids, labels, nil
	}
	return nil, nil, fmt.Errorf("unknown question type %q", q.Type)
}

// validateAnswers checks a decision's answers against r.Questions and
// returns them normalized. Every required question needs an answer, a
// choice question with allow_other also takes a custom text answer, and
// unknown question IDs are refused. Unanswered optional questions are left
// out.
func validateAnswers(r *Request, in map[string]Answer) (map[string]Answer, error) {
	known := map[string]bool{}
	for _, q := range r.Questions {
		known[q.ID] = true
	}
	for id := range in {
		if !known[id] {
			return nil, fmt.Errorf("%w: answers.%s: unknown question ID", ErrBadAnswer, id)
		}
	}
	out := map[string]Answer{}
	for i := range r.Questions {
		q := &r.Questions[i]
		a := in[q.ID]
		if n := utf8.RuneCountInString(a.Text); n > MaxAnswerText {
			return nil, fmt.Errorf("%w: answers.%s.text: %d characters; max %d", ErrBadAnswer, q.ID, n, MaxAnswerText)
		}
		value := a.Value
		if q.Type == QuestionFreeText && emptyValue(value) && strings.TrimSpace(a.Text) != "" {
			value, a.Text = a.Text, ""
		}
		v, labels, err := normalizeValue(q, value)
		if err != nil {
			return nil, fmt.Errorf("%w: answers.%s: %v", ErrBadAnswer, q.ID, err)
		}
		if s, ok := v.(string); ok && q.Type == QuestionFreeText && strings.TrimSpace(s) == "" {
			v = nil
		}
		custom := q.AllowOther && strings.TrimSpace(a.Text) != ""
		if v == nil && !custom {
			if q.required() {
				return nil, fmt.Errorf("%w: answers.%s: required; answer %q", ErrBadAnswer, q.ID, limitText(q.Prompt, 80))
			}
			if strings.TrimSpace(a.Text) == "" {
				continue
			}
		}
		out[q.ID] = Answer{Value: v, Text: a.Text, Labels: labels}
	}
	return out, nil
}

func emptyValue(v any) bool {
	switch x := v.(type) {
	case nil:
		return true
	case string:
		return x == ""
	case []any:
		return len(x) == 0
	case []string:
		return len(x) == 0
	}
	return false
}

// answerQuestions records the owner's answers to a version 3 question
// item: one complete answer set, with the request revision and a stable
// submission ID like an access decision.
func answerQuestions(r *Request, d Decision) error {
	if d.RequestRevision != r.Revision || d.SubmissionID == "" || len(d.SubmissionID) > 128 {
		return fmt.Errorf("%w: reload the request; answers need its revision and a submission ID", ErrConflict)
	}
	answers, err := validateAnswers(r, d.Answers)
	if err != nil {
		return err
	}
	r.Answers = answers
	r.Answer = answerSummary(r.Questions, answers)
	r.AnswerSubmissionID = d.SubmissionID
	r.AnswerFingerprint = answerFingerprint(d)
	return nil
}

// answerSummary is the plain-text form of the answers (the legacy answer
// string and the activity line): one "prompt: answer" line per answered
// question, in question order.
func answerSummary(qs []QuestionSpec, answers map[string]Answer) string {
	var lines []string
	for _, q := range qs {
		a, ok := answers[q.ID]
		if !ok {
			continue
		}
		lines = append(lines, limitText(q.Prompt, 120)+": "+answerText(q, a))
	}
	return strings.Join(lines, "\n")
}

func answerText(q QuestionSpec, a Answer) string {
	var parts []string
	switch v := a.Value.(type) {
	case nil:
	case float64:
		s := formatNumber(v)
		if q.Type == QuestionScale {
			s += " of " + strconv.Itoa(q.Scale)
		} else if q.Unit != "" {
			s += " " + q.Unit
		}
		parts = append(parts, s)
	case string:
		if len(a.Labels) == 0 {
			parts = append(parts, limitText(v, 200))
		}
	}
	if len(a.Labels) > 0 {
		sep := ", "
		if q.Type == QuestionRanking {
			sep = " > "
		}
		parts = append(parts, strings.Join(a.Labels, sep))
	}
	if a.Text != "" {
		parts = append(parts, limitText(a.Text, 200))
	}
	return strings.Join(parts, " — ")
}

// mdImage finds Markdown images: ![alt](src "title").
var mdImage = regexp.MustCompile(`!\[[^\]\n]*\]\(\s*<?([^)\s>]+)>?(?:\s+"[^"\n]*")?\s*\)`)

// markdownImages returns the image sources of a Markdown text: the upload
// refs ("upload:<sha256>") and the other sources, which the dashboard
// shows as plain links.
func markdownImages(text string) (uploads, others []string) {
	for _, m := range mdImage.FindAllStringSubmatch(text, -1) {
		if _, ok := ParseUploadRef(m[1]); ok {
			uploads = append(uploads, m[1])
		} else {
			others = append(others, m[1])
		}
	}
	return uploads, others
}

// inlineImageSources lists, per path (message, attachments[i].body), the
// upload refs that Markdown images there cite.
func (r *Request) inlineImageSources() []struct{ Path, Ref string } {
	var out []struct{ Path, Ref string }
	addText := func(path, text string) {
		ups, _ := markdownImages(text)
		for _, ref := range ups {
			out = append(out, struct{ Path, Ref string }{path, ref})
		}
	}
	addText("message", r.Message)
	for i, a := range r.Attachments {
		if a.Type == "markdown" {
			addText(fmt.Sprintf("attachments[%d].body", i), a.Body)
		}
	}
	return out
}
