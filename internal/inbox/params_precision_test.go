package inbox

import (
	"encoding/json"
	"strings"
	"testing"
)

func exactConstraint(t *testing.T, raw string) Constraint {
	t.Helper()
	var c Constraint
	if err := json.Unmarshal([]byte(raw), &c); err != nil {
		t.Fatal(err)
	}
	return c
}

func TestConstraintExactNumericPrecision(t *testing.T) {
	c := exactConstraint(t, `{"eq":9007199254740992}`)
	if c.Matches(json.Number("9007199254740993"), true) {
		t.Fatal("rounded adjacent integer authorized")
	}
	if !c.Matches(json.Number("9007199254740992.0"), true) {
		t.Fatal("equivalent decimal rejected")
	}
	nested := exactConstraint(t, `{"eq":{"values":[1,9007199254740992,{"fraction":0.10000000000000000000001}]}}`)
	good := map[string]any{"values": []any{json.Number("1.0"), json.Number("9007199254740992e0"), map[string]any{"fraction": json.Number("1.0000000000000000000001e-1")}}}
	if !nested.Matches(good, true) {
		t.Fatal("equivalent nested numeric values rejected")
	}
	good["values"].([]any)[1] = json.Number("9007199254740993")
	if nested.Matches(good, true) {
		t.Fatal("nested adjacent integer authorized")
	}
	strings := exactConstraint(t, `{"eq":"1"}`)
	if strings.Matches(json.Number("1"), true) {
		t.Fatal("numeric string and numeric value conflated")
	}
}

func TestConstraintSetWithinNarrowNumericPrecision(t *testing.T) {
	set := exactConstraint(t, `{"in":[9007199254740992,1]}`)
	good := exactConstraint(t, `{"eq":1.0}`)
	bad := exactConstraint(t, `{"eq":9007199254740993}`)
	if !good.Within(set) || bad.Within(set) {
		t.Fatal("set narrowing lost numeric precision")
	}
	if set.Matches(json.Number("9007199254740993"), true) {
		t.Fatal("set authorized adjacent integer")
	}
	if _, err := Narrow(map[string]Constraint{"id": set}, map[string]Constraint{"id": bad}); err == nil {
		t.Fatal("narrowing widened set by rounded integer")
	}
	if _, err := Narrow(map[string]Constraint{"id": set}, map[string]Constraint{"id": good}); err != nil {
		t.Fatal(err)
	}
}

func TestConstraintRangeNumericPrecision(t *testing.T) {
	c := exactConstraint(t, `{"gte":9007199254740992,"lte":9007199254740992}`)
	if c.Matches(json.Number("9007199254740993"), true) || c.Matches(json.Number("9007199254740991"), true) {
		t.Fatal("range authorized adjacent integer")
	}
	if !c.Matches(json.Number("9007199254740992.0"), true) {
		t.Fatal("range rejected equivalent decimal")
	}
	wider := exactConstraint(t, `{"gte":9007199254740992,"lte":9007199254740993}`)
	if wider.Within(c) {
		t.Fatal("range comparison rounded wider upper bound")
	}
	if _, err := Narrow(map[string]Constraint{"id": c}, map[string]Constraint{"id": wider}); err == nil {
		t.Fatal("range narrowing widened permission")
	}
	b, err := json.Marshal(c)
	if err != nil {
		t.Fatal(err)
	}
	recovered := exactConstraint(t, string(b))
	if recovered.Matches(json.Number("9007199254740993"), true) {
		t.Fatal("round trip lost precise bound")
	}
	var invalid Constraint
	if err = json.Unmarshal([]byte(`{"gte":9007199254740993,"lte":9007199254740992}`), &invalid); err == nil {
		t.Fatal("inverted range allowed after rounding")
	}
	decimal := exactConstraint(t, `{"gte":0.10000000000000000000001,"lte":0.10000000000000000000002}`)
	if decimal.Matches(json.Number("0.1"), true) {
		t.Fatal("tiny fractional difference rounded")
	}
}

func TestDecodeSubmissionPreservesJSONNumbers(t *testing.T) {
	s, err := DecodeSubmission(map[string]any{"tools": []any{map[string]any{"call_id": "big", "params": map[string]any{"id": map[string]any{"eq": json.Number("9007199254740993")}}}}})
	if err != nil {
		t.Fatal(err)
	}
	c, err := ParseConstraint(s.Tools[0].Params["id"])
	if err != nil {
		t.Fatal(err)
	}
	if !c.Matches(json.Number("9007199254740993"), true) || c.Matches(json.Number("9007199254740992"), true) {
		t.Fatal("submission decoding rounded exact numeric scope")
	}
}

func TestBrowserPermissionPrecision(t *testing.T) {
	for _, raw := range []string{`9007199254740993`, `1.00000000000000001`, `{"eq":{"id":9007199254740993}}`} {
		var v any
		d := json.NewDecoder(strings.NewReader(raw))
		d.UseNumber()
		if e := d.Decode(&v); e != nil {
			t.Fatal(e)
		}
		if browserNumbersSafe(v) {
			t.Fatal("unsafe browser scope accepted", raw)
		}
	}
	for _, raw := range []string{`10`, `0.05`, `{"eq":{"id":"9007199254740993"}}`} {
		var v any
		d := json.NewDecoder(strings.NewReader(raw))
		d.UseNumber()
		d.Decode(&v)
		if !browserNumbersSafe(v) {
			t.Fatal("safe scope rejected", raw)
		}
	}
}
