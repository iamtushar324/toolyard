package inbox

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"math/big"
	"reflect"
	"regexp"
	"sort"
	"strconv"
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
	Op      string       // eq | in | prefix | range | limit | any
	Eq      any          // eq
	In      []any        // in
	Prefix  string       // prefix
	Gte     *json.Number // range
	Lte     *json.Number // range
	Limit   string       // limit: description shown to the owner
	Pattern string       // limit: optional regex the actual value must match

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
			f, ok := numberValue(m["gte"])
			if !ok {
				return Constraint{}, errors.New(`"gte" must be a number`)
			}
			c.Gte = &f
		}
		if hasLte {
			f, ok := numberValue(m["lte"])
			if !ok {
				return Constraint{}, errors.New(`"lte" must be a number`)
			}
			c.Lte = &f
		}
		if cmp, ok := compareBounds(c.Gte, c.Lte); ok && cmp > 0 {
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
			return fmt.Sprintf("between %s and %s", *c.Gte, *c.Lte)
		case c.Gte != nil:
			return fmt.Sprintf("at least %s", *c.Gte)
		default:
			return fmt.Sprintf("at most %s", *c.Lte)
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
		f, ok := numberValue(actual)
		if !ok {
			return false
		}
		if cmp, ok := compareBounds(&f, c.Gte); ok && cmp < 0 {
			return false
		}
		if cmp, ok := compareBounds(&f, c.Lte); ok && cmp > 0 {
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
	return out
}

// Numeric equality uses rational values, so 1 and 1.0 agree without rounding
// adjacent integers above 2^53. JSON containers compare recursively.
func equalJSON(a, b any) bool {
	return equalNormalised(normalise(a), normalise(b))
}

func equalNormalised(a, b any) bool {
	if an, ok := a.(json.Number); ok {
		bn, ok := b.(json.Number)
		if !ok {
			return false
		}
		cmp, ok := CompareNumbers(an, bn)
		return ok && cmp == 0
	}
	switch av := a.(type) {
	case []any:
		bv, ok := b.([]any)
		if !ok || len(av) != len(bv) {
			return false
		}
		for i := range av {
			if !equalNormalised(av[i], bv[i]) {
				return false
			}
		}
		return true
	case map[string]any:
		bv, ok := b.(map[string]any)
		if !ok || len(av) != len(bv) {
			return false
		}
		for key, value := range av {
			other, present := bv[key]
			if !present || !equalNormalised(value, other) {
				return false
			}
		}
		return true
	default:
		return reflect.DeepEqual(a, b)
	}
}

func numberValue(v any) (json.Number, bool) {
	// The JSON encoder rejects non-finite floats and invalid json.Number text.
	// UseNumber retains the precise decimal value of all supported Go numbers.
	n, ok := normalise(v).(json.Number)
	if !ok {
		return "", false
	}
	_, valid := new(big.Rat).SetString(n.String())
	return n, valid
}

// CompareNumbers compares JSON numeric values without binary float rounding.
// The bool is false when either operand is not a valid JSON number.
func CompareNumbers(a, b any) (int, bool) {
	an, aok := numberValue(a)
	bn, bok := numberValue(b)
	if !aok || !bok {
		return 0, false
	}
	ar, aok := new(big.Rat).SetString(an.String())
	br, bok := new(big.Rat).SetString(bn.String())
	if !aok || !bok {
		return 0, false
	}
	return ar.Cmp(br), true
}

func compareBounds(a, b *json.Number) (int, bool) {
	if a == nil || b == nil {
		return 0, false
	}
	return CompareNumbers(*a, *b)
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

// Within reports whether c allows nothing that req doesn't: every value c
// accepts, req accepts too. The owner can narrow a request's parameters
// with it but never widen them.
func (c Constraint) Within(req Constraint) bool {
	if req.Op == "any" {
		return true
	}
	if c.Op == "eq" {
		// A single value is within req exactly when req matches it.
		return req.Matches(c.Eq, true)
	}
	switch req.Op {
	case "in":
		if c.Op != "in" {
			return false
		}
		for _, v := range c.In {
			if !req.Matches(v, true) {
				return false
			}
		}
		return true
	case "prefix":
		return c.Op == "prefix" && strings.HasPrefix(c.Prefix, req.Prefix)
	case "range":
		if c.Op != "range" {
			return false
		}
		if cmp, ok := compareBounds(c.Gte, req.Gte); req.Gte != nil && (!ok || cmp < 0) {
			return false
		}
		if cmp, ok := compareBounds(c.Lte, req.Lte); req.Lte != nil && (!ok || cmp > 0) {
			return false
		}
		return true
	case "limit":
		return c.Op == "limit" && c.Pattern == req.Pattern
	}
	return false
}

// Narrow applies the owner's tighter constraints to a parameter set. Every
// key must already be in params and every constraint must be Within the
// original.
func Narrow(params map[string]Constraint, tighter map[string]Constraint) (map[string]Constraint, error) {
	out := make(map[string]Constraint, len(params))
	for k, v := range params {
		out[k] = v
	}
	for _, k := range sortedKeysC(tighter) {
		orig, ok := params[k]
		if !ok {
			return nil, fmt.Errorf("parameter %q wasn't in the request", k)
		}
		if !tighter[k].Within(orig) {
			return nil, fmt.Errorf("parameter %q: %s is wider than the request (%s)", k, tighter[k].Describe(), orig.Describe())
		}
		out[k] = tighter[k]
	}
	return out, nil
}

func sortedKeysC(m map[string]Constraint) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// The human dashboard uses JSON.parse. Reject numeric permission descriptions
// it cannot faithfully render rather than ask the human to approve rounded scope.
func browserNumbersSafe(value any) bool {
	switch v := normalise(value).(type) {
	case json.Number:
		f, err := strconv.ParseFloat(v.String(), 64)
		if err != nil || math.IsInf(f, 0) || math.Abs(f) > 9007199254740991 {
			return false
		}
		cmp, ok := CompareNumbers(v, json.Number(strconv.FormatFloat(f, 'g', -1, 64)))
		return ok && cmp == 0
	case []any:
		for _, child := range v {
			if !browserNumbersSafe(child) {
				return false
			}
		}
	case map[string]any:
		for _, child := range v {
			if !browserNumbersSafe(child) {
				return false
			}
		}
	}
	return true
}
