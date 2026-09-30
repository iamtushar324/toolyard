package codemode

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"math/big"
	"strings"

	"go.starlark.net/starlark"
	"go.starlark.net/starlarkstruct"
)

// Value conversions. Adapted from maximhq/bifrost core/mcp/codemode
// (starlark/utils.go, Apache-2.0): a tool result that is JSON text becomes
// Starlark dicts and lists, anything else a string; script values convert
// back to JSON for the "Return value" line.

// toStarlark converts a decoded JSON value (or a plain Go value) to Starlark.
func toStarlark(v any) starlark.Value {
	switch val := v.(type) {
	case nil:
		return starlark.None
	case bool:
		return starlark.Bool(val)
	case int:
		return starlark.MakeInt(val)
	case int64:
		return starlark.MakeInt64(val)
	case uint64:
		return starlark.MakeUint64(val)
	case float64:
		return starlark.Float(val)
	case json.Number:
		return numberToStarlark(val)
	case string:
		return starlark.String(val)
	case []any:
		items := make([]starlark.Value, len(val))
		for i, item := range val {
			items[i] = toStarlark(item)
		}
		return starlark.NewList(items)
	case map[string]any:
		d := starlark.NewDict(len(val))
		for k, item := range val {
			_ = d.SetKey(starlark.String(k), toStarlark(item))
		}
		return d
	}
	// Anything else goes through JSON so structs and typed maps still land
	// as dicts and lists.
	if b, err := json.Marshal(v); err == nil {
		var generic any
		if json.Unmarshal(b, &generic) == nil {
			return toStarlark(generic)
		}
	}
	return starlark.String(fmt.Sprintf("%v", v))
}

// numberToStarlark keeps JSON integers exact (ids, counts) and makes
// everything else a float. Bifrost decodes every number as float64; the
// exact form is strictly friendlier to scripts (range(n), "%d" % n, ids that
// survive round trips) and compares equal where a float would.
func numberToStarlark(n json.Number) starlark.Value {
	s := n.String()
	if !strings.ContainsAny(s, ".eE") {
		if i, ok := new(big.Int).SetString(s, 10); ok {
			return starlark.MakeBigInt(i)
		}
	}
	if f, err := n.Float64(); err == nil {
		return starlark.Float(f)
	}
	return starlark.String(s)
}

// fromStarlark converts a Starlark value to a JSON-ready Go value.
func fromStarlark(v starlark.Value) any {
	switch val := v.(type) {
	case starlark.NoneType:
		return nil
	case starlark.Bool:
		return bool(val)
	case starlark.Int:
		if i, ok := val.Int64(); ok {
			return i
		}
		if u, ok := val.Uint64(); ok {
			return u
		}
		return val.String()
	case starlark.Float:
		return float64(val)
	case starlark.String:
		return string(val)
	case *starlark.List:
		out := make([]any, val.Len())
		for i := 0; i < val.Len(); i++ {
			out[i] = fromStarlark(val.Index(i))
		}
		return out
	case starlark.Tuple:
		out := make([]any, len(val))
		for i, item := range val {
			out[i] = fromStarlark(item)
		}
		return out
	case *starlark.Set:
		out := make([]any, 0, val.Len())
		for item := range val.Elements() {
			out = append(out, fromStarlark(item))
		}
		return out
	case *starlark.Dict:
		out := make(map[string]any, val.Len())
		for _, item := range val.Items() {
			if k, ok := item[0].(starlark.String); ok {
				out[string(k)] = fromStarlark(item[1])
			} else {
				out[item[0].String()] = fromStarlark(item[1])
			}
		}
		return out
	case *starlarkstruct.Struct:
		out := map[string]any{}
		for _, name := range val.AttrNames() {
			if attr, err := val.Attr(name); err == nil {
				out[name] = fromStarlark(attr)
			}
		}
		return out
	}
	return v.String()
}

// decodeResult turns a tool's text into a script value: JSON when it parses,
// the raw text otherwise.
func decodeResult(text string) starlark.Value {
	dec := json.NewDecoder(strings.NewReader(text))
	dec.UseNumber()
	var v any
	if err := dec.Decode(&v); err != nil {
		return starlark.String(text)
	}
	// Trailing content means the text only started like JSON.
	if _, err := dec.Token(); err != io.EOF {
		return starlark.String(text)
	}
	return toStarlark(v)
}

// jsonText encodes v without HTML escaping, indented when indent is set.
func jsonText(v any, indent string) (string, error) {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if indent != "" {
		enc.SetIndent("", indent)
	}
	if err := enc.Encode(v); err != nil {
		return "", err
	}
	return strings.TrimSuffix(buf.String(), "\n"), nil
}
