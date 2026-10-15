package gateway

import (
	"bytes"
	"encoding/json"
	"testing"
)

// asInt accepts every numeric shape a decoded call carries, including the
// json.Number that a UseNumber decode (PreserveToolCallNumbers) leaves in
// the arguments; before, such a limit or since_minutes was ignored.
func TestAsInt(t *testing.T) {
	cases := []struct {
		in     any
		want   int
		wantOK bool
	}{
		{json.Number("5"), 5, true},
		{json.Number("-3"), -3, true},
		{json.Number("2.9"), 2, true},
		{json.Number("1e3"), 1000, true},
		{json.Number("abc"), 0, false},
		{5, 5, true},
		{int32(7), 7, true},
		{int64(9), 9, true},
		{float64(4.7), 4, true},
		{"5", 0, false},
		{nil, 0, false},
	}
	for _, c := range cases {
		got, ok := asInt(c.in)
		if got != c.want || ok != c.wantOK {
			t.Errorf("asInt(%#v) = %d, %v; want %d, %v", c.in, got, ok, c.want, c.wantOK)
		}
	}
}

// Arguments decoded the way PreserveToolCallNumbers decodes them give the
// integer back through asInt and the float through numericArgument.
func TestNumbersFromUseNumberDecode(t *testing.T) {
	var args map[string]any
	dec := json.NewDecoder(bytes.NewReader([]byte(`{"limit":5,"since_minutes":30,"timeout_seconds":1.5}`)))
	dec.UseNumber()
	if err := dec.Decode(&args); err != nil {
		t.Fatal(err)
	}
	if n, ok := asInt(args["limit"]); !ok || n != 5 {
		t.Errorf("limit = %d, %v; want 5, true", n, ok)
	}
	if n, ok := asInt(args["since_minutes"]); !ok || n != 30 {
		t.Errorf("since_minutes = %d, %v; want 30, true", n, ok)
	}
	if f, ok := numericArgument(args["timeout_seconds"]); !ok || f != 1.5 {
		t.Errorf("timeout_seconds = %v, %v; want 1.5, true", f, ok)
	}
}
