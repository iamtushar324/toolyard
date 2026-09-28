package inbox

import (
	"context"
	"fmt"
	"regexp"
	"strings"
)

// Rule flags are predictable: the same request always gets the same flags,
// and an agent can read in the protocol what raises each one. They run
// before (and independently of) the judge model.

// Flag labels. The dashboard and the protocol use these exact strings.
const (
	FlagMismatch     = "Doesn't match the request"
	FlagIrreversible = "Can't be undone"
	FlagDeletes      = "Deletes data"
	FlagMoney        = "Spends money"
	FlagProduction   = "Production"
	FlagCustomers    = "Customers affected"
	FlagUnknownValue = "Value not known yet"
	FlagSecrets      = "Touches secrets"
	FlagUnrestricted = "Any value allowed"
)

var (
	prodValue   = regexp.MustCompile(`(?i)^(prod|production|prd|live)([-_.].*)?$`)
	envKeys     = map[string]bool{"env": true, "environment": true, "stage": true, "target": true, "cluster": true, "namespace": true, "context": true, "tier": true, "project": true}
	deleteWords = []string{"delete", "drop", "truncate", "destroy", "purge", "wipe", "remove", "erase", "rm"}
	sendWords   = []string{"send", "email", "mail", "sms", "publish", "broadcast", "tweet", "post_to_customers", "notify_customers"}
	moneyWords  = []string{"pay", "payment", "charge", "refund", "payout", "transfer", "purchase", "buy", "invoice_create", "create_invoice", "subscribe", "billing_charge"}
	secretWords = []string{"secret", "secrets", "credential", "credentials", "password", "passwd", "token", "api_key", "apikey", "private_key", "ssh_key", "rotate_key"}
	flagWords   = []string{"flag", "flags", "rollout", "feature", "features"}
	custWords   = []string{"email", "mail", "sms", "notify", "broadcast", "customer", "user_message"}
)

// words splits a tool name like "github.merge_pull_request" into
// lower-case tokens, plus the joined form of neighbours so multi-word
// markers ("api_key") match.
func words(name string) []string {
	low := strings.ToLower(name)
	parts := strings.FieldsFunc(low, func(r rune) bool { return r == '.' || r == '_' || r == '-' || r == '/' || r == ' ' })
	out := append([]string(nil), parts...)
	for i := 0; i+1 < len(parts); i++ {
		out = append(out, parts[i]+"_"+parts[i+1])
	}
	return out
}

func hasWord(ws []string, list []string) bool {
	for _, w := range ws {
		for _, l := range list {
			if w == l {
				return true
			}
		}
	}
	return false
}

// RuleFlags computes the rule flags for one tool request.
func RuleFlags(t ToolRequest) []Flag {
	var out []Flag
	add := func(level, label, why string) {
		for _, f := range out {
			if f.Label == label {
				return
			}
		}
		out = append(out, Flag{Level: level, Label: label, Why: why, Source: "rule"})
	}
	ws := words(t.Tool)

	if hasWord(ws, deleteWords) {
		add(LevelRed, FlagDeletes, fmt.Sprintf("%s removes or deletes things.", t.Tool))
	}
	if hasWord(ws, moneyWords) {
		add(LevelRed, FlagMoney, fmt.Sprintf("%s looks like it moves or charges money.", t.Tool))
	}
	if hasWord(ws, sendWords) {
		add(LevelRed, FlagIrreversible, fmt.Sprintf("%s sends or publishes something, which can't be recalled.", t.Tool))
	}
	if hasWord(ws, moneyWords) {
		add(LevelRed, FlagIrreversible, fmt.Sprintf("Payments made by %s can't simply be undone.", t.Tool))
	}

	prod := ""
	for k, c := range t.Params {
		if !envKeys[strings.ToLower(k)] {
			continue
		}
		for _, v := range constraintStrings(c) {
			if prodValue.MatchString(v) {
				prod = fmt.Sprintf("%s = %s", k, v)
			}
		}
	}
	if prod == "" && (hasWord(ws, []string{"prod", "production"})) {
		prod = "the tool name"
	}
	if prod != "" {
		add(LevelAmber, FlagProduction, fmt.Sprintf("Targets production (%s).", prod))
	}

	if hasWord(ws, custWords) {
		add(LevelAmber, FlagCustomers, fmt.Sprintf("%s reaches people outside the team.", t.Tool))
	}
	if hasWord(ws, flagWords) && prod != "" && enablesSomething(t.Params) {
		add(LevelAmber, FlagCustomers, "Turns something on in production, so customers may notice.")
	}

	if hasWord(ws, secretWords) {
		add(LevelAmber, FlagSecrets, fmt.Sprintf("%s reads or changes credentials.", t.Tool))
	} else {
		for _, k := range sortedConstraintKeys(t.Params) {
			if hasWord(words(k), secretWords) {
				add(LevelAmber, FlagSecrets, fmt.Sprintf("The %q parameter looks like a credential.", k))
				break
			}
			for _, v := range constraintStrings(t.Params[k]) {
				if hasWord(words(v), secretWords) {
					add(LevelAmber, FlagSecrets, fmt.Sprintf("%s = %s names a credential.", k, v))
					break
				}
			}
		}
	}

	for _, k := range sortedConstraintKeys(t.Params) {
		c := t.Params[k]
		switch c.Op {
		case "limit":
			why := fmt.Sprintf("%q isn't known yet (%s).", k, c.Limit)
			if c.Pattern != "" {
				why += " Toolyard checks it against the pattern when the call is made."
			} else {
				why += " Toolyard records the actual value when the call is made but can't check it in advance."
			}
			add(LevelAmber, FlagUnknownValue, why)
		case "any":
			add(LevelAmber, FlagUnrestricted, fmt.Sprintf("%q can be anything.", k))
		}
	}
	return out
}

func enablesSomething(params map[string]Constraint) bool {
	for k, c := range params {
		lk := strings.ToLower(k)
		if c.Op != "eq" {
			continue
		}
		switch v := c.Eq.(type) {
		case bool:
			if v && (strings.Contains(lk, "enable") || lk == "on" || lk == "value" || lk == "state") {
				return true
			}
		case float64:
			if v > 0 && (strings.Contains(lk, "percent") || strings.Contains(lk, "rollout")) {
				return true
			}
		case string:
			if strings.EqualFold(v, "on") || strings.EqualFold(v, "true") || strings.EqualFold(v, "enabled") {
				return true
			}
		}
	}
	return false
}

func constraintStrings(c Constraint) []string {
	var out []string
	switch c.Op {
	case "eq":
		if s, ok := c.Eq.(string); ok {
			out = append(out, s)
		}
	case "in":
		for _, e := range c.In {
			if s, ok := e.(string); ok {
				out = append(out, s)
			}
		}
	case "prefix":
		out = append(out, c.Prefix)
	}
	return out
}

func sortedConstraintKeys(m map[string]Constraint) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sortStrings(out)
	return out
}

// Judge is an optional second opinion (a language model) that compares the
// agent's own description with each call it asks for. It only ever adds
// FlagMismatch flags and plain-language explanations; it can't approve or
// deny anything.
type Judge interface {
	Review(ctx context.Context, r *Request) (*Review, error)
}

// Review is a Judge's output.
type Review struct {
	// Summary is toolyard's own reading of the whole request, shown only
	// when the owner asks for it.
	Summary string
	// Tools is indexed like Request.Tools.
	Tools []ToolReview
	// RequestFlags apply to the request as a whole (questions, blockers).
	RequestFlags []Flag
}

// ToolReview is the judge's view of one tool call.
type ToolReview struct {
	Explanation string
	Mismatch    string // non-empty: why this call contradicts the request
}

// applyReview merges a judge review into the request.
func applyReview(r *Request, rv *Review) {
	if rv == nil {
		return
	}
	if rv.Summary != "" {
		r.ToolyardSum = rv.Summary
	}
	for i := range r.Tools {
		if i >= len(rv.Tools) {
			break
		}
		tr := rv.Tools[i]
		if tr.Explanation != "" {
			r.Tools[i].Explanation = tr.Explanation
		}
		if tr.Mismatch != "" {
			r.Tools[i].Flags = append([]Flag{{Level: LevelRed, Label: FlagMismatch, Why: tr.Mismatch, Source: "judge"}}, r.Tools[i].Flags...)
		}
	}
	for _, f := range rv.RequestFlags {
		f.Source = "judge"
		r.Flags = append(r.Flags, f)
	}
}
