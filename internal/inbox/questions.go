package inbox

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/url"
	"strings"
	"unicode/utf8"
)

// Question is the version 2 question contract. Text is always a valid alternative.
type Question struct {
	Type          string   `json:"type"`
	MinSelections int      `json:"min_selections,omitempty"`
	MaxSelections int      `json:"max_selections,omitempty"`
	Options       []Option `json:"options,omitempty"`
}
type TaskContext struct {
	Title string `json:"title"`
	URL   string `json:"url,omitempty"`
}
type Response struct {
	SelectedOptionIDs []string `json:"selected_option_ids,omitempty"`
	Text              string   `json:"text"`
}
type AnswerResponse struct {
	Response
	SelectedLabels []string `json:"selected_labels,omitempty"`
	SubmittedAt    int64    `json:"submitted_at"`
	UserID         string   `json:"user_id,omitempty"`
}

func hashJSON(v any) string {
	b, _ := json.Marshal(v)
	h := sha256.Sum256(b)
	return hex.EncodeToString(h[:])
}
func limitText(s string, n int) string {
	r := []rune(s)
	if len(r) > n {
		r = r[:n]
	}
	return string(r)
}

func normalizeQuestion(s *Submission) *Submission {
	c := *s
	if c.SchemaVersion == 2 && (c.Kind == KindQuestion || c.Kind == KindBlocker) {
		if c.Title == "" {
			c.Title = limitText(c.Prompt, MaxTitle)
		}
		if c.Summary == "" {
			c.Summary = limitText(c.Prompt, MaxSummary)
		}
		if c.Message == "" {
			c.Message = c.Prompt
			if c.Context != "" {
				c.Message += "\n\n" + c.Context
			}
		}
		if c.Urgency == "" {
			c.Urgency = UrgencySoon
		}
		if c.Question != nil {
			c.Options = append([]Option(nil), c.Question.Options...)
		}
	}
	return &c
}

func validateQuestion(r *Request, s *Submission, add func(string, string, ...any)) {
	if s.SchemaVersion != 0 && s.SchemaVersion != 1 && s.SchemaVersion != 2 {
		add("schema_version", "use 1 or 2")
	}
	if len(s.ClientRequestID) > 128 {
		add("client_request_id", "max 128 bytes")
	}
	if s.SchemaVersion == 2 {
		checkText(add, "prompt", s.Prompt, 1000, "")
		if s.Question == nil {
			add("question", "required for schema_version 2")
			return
		}
		q := *s.Question
		q.Options = append([]Option(nil), s.Question.Options...)
		r.Question = &q
		if q.Type != "free_text" && q.Type != "single_choice" && q.Type != "multiple_choice" {
			add("question.type", "use free_text, single_choice or multiple_choice")
		}
		if q.Type == "free_text" && (len(q.Options) != 0 || q.MinSelections != 0 || q.MaxSelections != 0) {
			add("question", "free_text cannot have choices or selection limits")
		}
		if q.Type != "free_text" && (len(q.Options) < 2 || len(q.Options) > 12) {
			add("question.options", "give 2 to 12 options")
		}
		if q.Type == "single_choice" {
			if q.MaxSelections > 1 || q.MinSelections > 1 {
				add("question", "single_choice permits at most one selection")
			}
			q.MaxSelections = 1
		}
		if q.MaxSelections == 0 && q.Type == "multiple_choice" {
			q.MaxSelections = len(q.Options)
		}
		if q.MinSelections < 0 || q.MaxSelections < 0 || q.MinSelections > q.MaxSelections || q.MaxSelections > len(q.Options) {
			add("question", "invalid selection limits")
		}
		*r.Question = q
		r.Options = q.Options
	} else {
		if len(r.Options) < MinOptions || len(r.Options) > MaxOptions {
			add("options", "give %d to %d options", MinOptions, MaxOptions)
		}
		r.Options = append([]Option(nil), r.Options...)
		for i := range r.Options {
			r.Options[i].ID = fmt.Sprintf("option_%d", i+1)
		}
		r.Question = &Question{Type: "single_choice", MaxSelections: 1, Options: r.Options}
	}
	ids, labels := map[string]bool{}, map[string]bool{}
	for i := range r.Options {
		o := &r.Options[i]
		p := fmt.Sprintf("question.options[%d]", i)
		if o.ID == "" || len(o.ID) > 80 || strings.TrimSpace(o.ID) != o.ID || ids[o.ID] {
			add(p+".id", "must be a unique nonempty ID of at most 80 bytes")
		}
		ids[o.ID] = true
		o.Label = strings.TrimSpace(o.Label)
		checkText(add, p+".label", o.Label, MaxOptionLabel, "")
		label := strings.ToLower(o.Label)
		if labels[label] {
			add(p+".label", "duplicate label")
		}
		labels[label] = true
		if utf8.RuneCountInString(o.Detail) > MaxOptionDetail {
			add(p+".detail", "max %d characters", MaxOptionDetail)
		}
	}
	r.Question.Options = r.Options
	if r.Task != nil {
		if utf8.RuneCountInString(r.Task.Title) > 200 {
			add("task.title", "max 200 characters")
		}
		if r.Task.URL != "" {
			u, e := url.Parse(r.Task.URL)
			if e != nil || u.Scheme != "https" || u.Host == "" || u.User != nil {
				add("task.url", "use a public HTTPS URL without credentials")
			}
		}
	}
}

func answerQuestion(r *Request, d Decision, now int64) error {
	q := r.Question
	if q == nil {
		q = &Question{Type: "single_choice", MaxSelections: 1, Options: append([]Option(nil), r.Options...)}
		for i := range q.Options {
			q.Options[i].ID = fmt.Sprintf("option_%d", i+1)
		}
	}
	a := d.Response
	if a == nil {
		if d.Option == nil || *d.Option < 0 || *d.Option >= len(q.Options) {
			return fmt.Errorf("write an answer or choose an option")
		}
		a = &Response{SelectedOptionIDs: []string{q.Options[*d.Option].ID}, Text: d.Note}
	}
	if d.Response != nil && (d.RequestRevision != r.Revision || d.SubmissionID == "" || len(d.SubmissionID) > 128) {
		return fmt.Errorf("%w: reload the request; an answer needs its revision and a submission ID", ErrConflict)
	}
	if utf8.RuneCountInString(a.Text) > 10000 {
		return fmt.Errorf("answer text exceeds 10000 characters")
	}
	if len(a.SelectedOptionIDs) == 0 && strings.TrimSpace(a.Text) == "" {
		return fmt.Errorf("write an answer or choose an option")
	}
	seen := map[string]bool{}
	labels := []string{}
	for _, id := range a.SelectedOptionIDs {
		if seen[id] {
			return fmt.Errorf("duplicate option ID")
		}
		seen[id] = true
		found := false
		for _, o := range q.Options {
			if o.ID == id {
				found = true
				labels = append(labels, o.Label)
				if o.Exclusive && len(a.SelectedOptionIDs) > 1 {
					return fmt.Errorf("%s must be selected alone", o.Label)
				}
			}
		}
		if !found {
			return fmt.Errorf("unknown option ID")
		}
	}
	n := len(a.SelectedOptionIDs)
	if q.Type == "free_text" && n > 0 || q.Type == "single_choice" && n > 1 || q.MaxSelections > 0 && n > q.MaxSelections {
		return fmt.Errorf("too many selections")
	}
	if n > 0 && n < q.MinSelections {
		return fmt.Errorf("select at least %d options or write a custom answer", q.MinSelections)
	}
	r.Response = &AnswerResponse{Response: *a, SelectedLabels: labels, SubmittedAt: now, UserID: d.decider().UserID}
	r.Answer = strings.Join(labels, ", ")
	if a.Text != "" {
		if r.Answer != "" {
			r.Answer += "\n\n"
		}
		r.Answer += a.Text
	}
	r.AnswerSubmissionID = d.SubmissionID
	r.AnswerFingerprint = answerFingerprint(d)
	return nil
}
func answerFingerprint(d Decision) string {
	return hashJSON([]any{d.Response, d.Option, d.RequestRevision, d.decider(), d.Note})
}
