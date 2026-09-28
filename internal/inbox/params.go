package inbox

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"sort"
	"strings"
)

// Constraint limits one argument of a granted call.
//
// Agents write constraints as JSON objects with exactly one operator:
//
//	{"eq": "prod"}                      exact value (preferred)
//	{"in": ["api", "worker"]}           one of these values
//	{"prefix": "release/"}              a string starting with this
//	{"gte": 1, "lte": 6}                a number in this range (either bound optional)
//	{"limit": "merge commit of PR #218", "pattern": "^[0-9a-f]{7,40}$"}
//	                                    a value not known yet; pattern (optional)
//	                                    is enforced when the call is made
//	{"any": true}                       any value (always flagged)
//
// A bare value (string, number, bool, array, or an object without an
// operator key) is shorthand for {"eq": value}.
type Constraint struct {
	Op      string   // eq | in | prefix | range | limit | any
	Eq      any      // eq
	In      []any    // in
	Prefix  string   // prefix
	Gte     *float64 // range
	Lte     *float64 // range
	Limit   string   // limit: description shown to the owner
	Pattern string   // limit: optional regex the actual value must match

	re *regexp.Regexp
}

var operatorKeys = map[string]bool{"eq": true, "in": true, "prefix": true, "gte": true, "lte": true, "limit": true, "pattern": true, "any": true}

// ParseConstraint turns the JSON form (already decoded into Go values)
// into a Constraint.
func ParseConstraint(v any) (Constraint, error) {
	m, isObj := v.(map[string]any)
	if !isObj || !hasOperator(m) {
		return Constraint{Op: "eq", Eq: normalise(v)}, nil
	}
	for k := range m {
		if !operatorKeys[k] {
			return Constraint{}, fmt.Errorf("unknown key %q (use one of eq, in, prefix, gte/lte, limit, any)", k)
		}
	}
	ops := 0
	c := Constraint{}
	if x, ok := m["eq"]; ok {
		ops++
		c.Op, c.Eq = "eq", normalise(x)
	}
	if x, ok := m["in"]; ok {
		ops++
		arr, ok := x.([]any)
		if !ok || len(arr) == 0 {
			return Constraint{}, errors.New(`"in" must be a non-empty array`)
		}
		c.Op = "in"
		for _, e := range arr {
			c.In = append(c.In, normalise(e))
		}
	}
	if x, ok := m["prefix"]; ok {
		ops++
		s, ok := x.(string)
		if !ok || s == "" {
			return Constraint{}, errors.New(`"prefix" must be a non-empty string`)
		}
		c.Op, c.Prefix = "prefix", s
	}
	_, hasGte := m["gte"]
	_, hasLte := m["lte"]
	if hasGte || hasLte {
		ops++
		c.Op = "range"
		if hasGte {
			f, ok := toFloat(m["gte"])
			if !ok {
				return Constraint{}, errors.New(`"gte" must be a number`)
			}
			c.Gte = &f
		}
		if hasLte {
			f, ok := toFloat(m["lte"])
			if !ok {
				return Constraint{}, errors.New(`"lte" must be a number`)
			}
			c.Lte = &f
		}
		if c.Gte != nil && c.Lte != nil && *c.Gte > *c.Lte {
			return Constraint{}, errors.New(`"gte" is greater than "lte"`)
		}
	}
	if x, ok := m["limit"]; ok {
		ops++
		s, ok := x.(string)
		if !ok || strings.TrimSpace(s) == "" {
			return Constraint{}, errors.New(`"limit" must describe the value in words, e.g. "merge commit of PR #218"`)
		}
		c.Op, c.Limit = "limit", strings.TrimSpace(s)
		if p, ok := m["pattern"]; ok {
			ps, ok := p.(string)
			if !ok {
				return Constraint{}, errors.New(`"pattern" must be a string`)
			}
			re, err := regexp.Compile(ps)
			if err != nil {
				return Constraint{}, fmt.Errorf(`"pattern" is not a valid regular expression: %v`, err)
			}
			c.Pattern, c.re = ps, re
		}
	} else if _, ok := m["pattern"]; ok {
		return Constraint{}, errors.New(`"pattern" only goes with "limit"`)
	}
	if x, ok := m["any"]; ok {
		ops++
		if b, ok := x.(bool); !ok || !b {
			return Constraint{}, errors.New(`"any" must be true`)
		}
		c.Op = "any"
	}
	if ops != 1 {
		return Constraint{}, errors.New("use exactly one operator per parameter")
	}
	return c, nil
}

func hasOperator(m map[string]any) bool {
	for k := range m {
		if operatorKeys[k] {
			return true
		}
	}
	return false
}

// MarshalJSON writes the canonical object form.
func (c Constraint) MarshalJSON() ([]byte, error) {
	m := map[string]any{}
	switch c.Op {
	case "eq":
		m["eq"] = c.Eq
	case "in":
		m["in"] = c.In
	case "prefix":
		m["prefix"] = c.Prefix
	case "range":
		if c.Gte != nil {
			m["gte"] = *c.Gte
		}
		if c.Lte != nil {
			m["lte"] = *c.Lte
		}
	case "limit":
		m["limit"] = c.Limit
		if c.Pattern != "" {
			m["pattern"] = c.Pattern
		}
	case "any":
		m["any"] = true
	default:
		return nil, fmt.Errorf("constraint has no operator")
	}
	return json.Marshal(m)
}

// UnmarshalJSON accepts the same forms as ParseConstraint.
func (c *Constraint) UnmarshalJSON(b []byte) error {
	var v any
	dec := json.NewDecoder(bytes.NewReader(b))
	dec.UseNumber()
	if err := dec.Decode(&v); err != nil {
		return err
	}
	parsed, err := ParseConstraint(v)
	if err != nil {
		return err
	}
	*c = parsed
	return nil
}

// Describe is a short human form, used in coaching text and the dashboard.
func (c Constraint) Describe() string {
	switch c.Op {
	case "eq":
		return compactJSON(c.Eq)
	case "in":
		return "one of " + compactJSON(c.In)
	case "prefix":
		return "starts with " + c.Prefix
	case "range":
		switch {
		case c.Gte != nil && c.Lte != nil:
			return fmt.Sprintf("between %g and %g", *c.Gte, *c.Lte)
		case c.Gte != nil:
			return fmt.Sprintf("at least %g", *c.Gte)
		default:
			return fmt.Sprintf("at most %g", *c.Lte)
		}
	case "limit":
		return "not known yet: " + c.Limit
	case "any":
		return "any value"
	}
	return ""
}

// Matches reports whether an actual argument satisfies the constraint.
// present is false when the call omitted the argument.
func (c Constraint) Matches(actual any, present bool) bool {
	if c.Op == "any" {
		return true
	}
	if !present {
		return false
	}
	actual = normalise(actual)
	switch c.Op {
	case "eq":
		return equalJSON(c.Eq, actual)
	case "in":
		for _, e := range c.In {
			if equalJSON(e, actual) {
				return true
			}
		}
		return false
	case "prefix":
		s, ok := actual.(string)
		return ok && strings.HasPrefix(s, c.Prefix)
	case "range":
		f, ok := toFloat(actual)
		if !ok {
			return false
		}
		if c.Gte != nil && f < *c.Gte {
			return false
		}
		if c.Lte != nil && f > *c.Lte {
			return false
		}
		return true
	case "limit":
		if c.re == nil && c.Pattern != "" {
			c.re = regexp.MustCompile(c.Pattern)
		}
		if c.re == nil {
			return true
		}
		s, ok := actual.(string)
		if !ok {
			s = compactJSON(actual)
		}
		return c.re.MatchString(s)
	}
	return false
}

// MatchArgs checks a call's arguments against a parameter set. Every
// constrained parameter must match, and the call may not pass arguments
// that were never asked for. It returns a human reason on mismatch.
func MatchArgs(params map[string]Constraint, args map[string]any) (bool, string) {
	for _, k := range sortedKeys(args) {
		if _, ok := params[k]; !ok {
			return false, fmt.Sprintf("argument %q wasn't in the request, so the grant doesn't cover it", k)
		}
	}
	keys := make([]string, 0, len(params))
	for k := range params {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		c := params[k]
		v, present := args[k]
		if !c.Matches(v, present) {
			if !present {
				return false, fmt.Sprintf("argument %q is missing; the grant requires %s", k, c.Describe())
			}
			return false, fmt.Sprintf("argument %q is %s; the grant allows %s", k, compactJSON(normalise(v)), c.Describe())
		}
	}
	return true, ""
}

// ParamsFromArgs converts a call's actual arguments into exact constraints.
// Used to pre-fill the coaching draft.
func ParamsFromArgs(args map[string]any) map[string]Constraint {
	out := make(map[string]Constraint, len(args))
	for k, v := range args {
		out[k] = Constraint{Op: "eq", Eq: normalise(v)}
	}
	return out
}

// normalise round-trips a value through JSON so ints, float64s and
// json.Numbers compare equal, and maps/slices have consistent types.
func normalise(v any) any {
	b, err := json.Marshal(v)
	if err != nil {
		return v
	}
	var out any
	dec := json.NewDecoder(bytes.NewReader(b))
	dec.UseNumber()
	if err := dec.Decode(&out); err != nil {
		return v
	}
	return canonNumbers(out)
}

// canonNumbers turns json.Number into float64 so 1 and 1.0 compare equal.
func canonNumbers(v any) any {
	switch t := v.(type) {
	case json.Number:
		f, err := t.Float64()
		if err != nil {
			return t.String()
		}
		return f
	case []any:
		for i := range t {
			t[i] = canonNumbers(t[i])
		}
		return t
	case map[string]any:
		for k := range t {
			t[k] = canonNumbers(t[k])
		}
		return t
	}
	return v
}

func equalJSON(a, b any) bool {
	ab, err1 := json.Marshal(canonNumbers(normalise(a)))
	bb, err2 := json.Marshal(canonNumbers(normalise(b)))
	return err1 == nil && err2 == nil && bytes.Equal(ab, bb)
}

func toFloat(v any) (float64, bool) {
	switch t := v.(type) {
	case float64:
		return t, true
	case float32:
		return float64(t), true
	case int:
		return float64(t), true
	case int64:
		return float64(t), true
	case json.Number:
		f, err := t.Float64()
		return f, err == nil
	}
	return 0, false
}

func compactJSON(v any) string {
	b, err := json.Marshal(v)
	if err != nil {
		return fmt.Sprint(v)
	}
	return string(b)
}

func sortedKeys(m map[string]any) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
