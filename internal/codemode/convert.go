package codemode

import (
	"bytes"
	"encoding/json"
	"errors"
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
// back to JSON for tool arguments and the "Return value" line.
//
// The Starlark-to-Go direction is defensive where Bifrost's is not: a list
// or dict that contains itself, or a value nested absurdly deep, is an
// error rather than a stack overflow, and a value JSON cannot carry (an
// integer past 64 bits, bytes) is an error rather than a surprise string.

// maxDepth is how deep a script value may nest before conversion refuses.
const maxDepth = 64

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

// toGo converts a script value to a JSON-ready Go value, or explains why
// it cannot be sent.
func toGo(v starlark.Value) (any, error) {
	c := converter{seen: map[any]bool{}}
	return c.value(v)
}

// converter walks one value, tracking the path of containers it is inside
// (so a container reached twice on one path is a cycle, while the same
// list appearing in two places is fine) and how deep it is.
type converter struct {
	depth int
	seen  map[any]bool
}

func (c *converter) enter(id any) error {
	if c.seen[id] {
		return errors.New("value contains a cycle (a list or dict that contains itself) and cannot be sent as JSON")
	}
	if c.depth >= maxDepth {
		return fmt.Errorf("value nests deeper than %d levels and cannot be sent as JSON", maxDepth)
	}
	c.seen[id] = true
	c.depth++
	return nil
}

func (c *converter) leave(id any) {
	delete(c.seen, id)
	c.depth--
}

func (c *converter) value(v starlark.Value) (any, error) {
	switch val := v.(type) {
	case starlark.NoneType:
		return nil, nil
	case starlark.Bool:
		return bool(val), nil
	case starlark.Int:
		if i, ok := val.Int64(); ok {
			return i, nil
		}
		if u, ok := val.Uint64(); ok {
			return u, nil
		}
		return nil, fmt.Errorf("integer %s is too large to send as JSON (64-bit limit); send it as a string", val.String())
	case starlark.Float:
		return float64(val), nil
	case starlark.String:
		return string(val), nil
	case starlark.Bytes:
		return nil, errors.New("bytes values cannot be sent as JSON; convert them to a string first")
	case *starlark.List:
		if err := c.enter(val); err != nil {
			return nil, err
		}
		defer c.leave(val)
		out := make([]any, val.Len())
		for i := 0; i < val.Len(); i++ {
			item, err := c.value(val.Index(i))
			if err != nil {
				return nil, err
			}
			out[i] = item
		}
		return out, nil
	case starlark.Tuple:
		if c.depth >= maxDepth {
			return nil, fmt.Errorf("value nests deeper than %d levels and cannot be sent as JSON", maxDepth)
		}
		c.depth++
		defer func() { c.depth-- }()
		out := make([]any, len(val))
		for i, item := range val {
			converted, err := c.value(item)
			if err != nil {
				return nil, err
			}
			out[i] = converted
		}
		return out, nil
	case *starlark.Set:
		if err := c.enter(val); err != nil {
			return nil, err
		}
		defer c.leave(val)
		out := make([]any, 0, val.Len())
		for item := range val.Elements() {
			converted, err := c.value(item)
			if err != nil {
				return nil, err
			}
			out = append(out, converted)
		}
		return out, nil
	case *starlark.Dict:
		if err := c.enter(val); err != nil {
			return nil, err
		}
		defer c.leave(val)
		out := make(map[string]any, val.Len())
		for _, item := range val.Items() {
			key := item[0].String()
			if k, ok := item[0].(starlark.String); ok {
				key = string(k)
			}
			converted, err := c.value(item[1])
			if err != nil {
				return nil, err
			}
			out[key] = converted
		}
		return out, nil
	case *starlarkstruct.Struct:
		if err := c.enter(val); err != nil {
			return nil, err
		}
		defer c.leave(val)
		out := map[string]any{}
		for _, name := range val.AttrNames() {
			attr, err := val.Attr(name)
			if err != nil {
				continue
			}
			converted, err := c.value(attr)
			if err != nil {
				return nil, err
			}
			out[name] = converted
		}
		return out, nil
	}
	return v.String(), nil
}

// decodeResult turns a tool's text into a script value: JSON when it
// parses, the raw text otherwise. JSON is built into Starlark values
// straight from the token stream, with no intermediate Go tree, so a
// result near the size cap decodes within the worker's memory. Object keys
// keep the document's order.
func decodeResult(text string) starlark.Value {
	dec := json.NewDecoder(strings.NewReader(text))
	dec.UseNumber()
	v, err := decodeValue(dec)
	if err != nil {
		return starlark.String(text)
	}
	// Trailing content means the text only started like JSON.
	if _, err := dec.Token(); err != io.EOF {
		return starlark.String(text)
	}
	return v
}

// decodeValue builds one JSON value from the decoder's next tokens.
func decodeValue(dec *json.Decoder) (starlark.Value, error) {
	tok, err := dec.Token()
	if err != nil {
		return nil, err
	}
	return buildValue(dec, tok)
}

// buildValue builds the value that starts with tok.
func buildValue(dec *json.Decoder, tok json.Token) (starlark.Value, error) {
	switch t := tok.(type) {
	case json.Delim:
		switch t {
		case '{':
			d := starlark.NewDict(0)
			for {
				keyTok, err := dec.Token()
				if err != nil {
					return nil, err
				}
				if delim, ok := keyTok.(json.Delim); ok && delim == '}' {
					return d, nil
				}
				key, ok := keyTok.(string)
				if !ok {
					return nil, fmt.Errorf("object key %v is not a string", keyTok)
				}
				val, err := decodeValue(dec)
				if err != nil {
					return nil, err
				}
				if err := d.SetKey(starlark.String(key), val); err != nil {
					return nil, err
				}
			}
		case '[':
			items := []starlark.Value{}
			for {
				itemTok, err := dec.Token()
				if err != nil {
					return nil, err
				}
				if delim, ok := itemTok.(json.Delim); ok && delim == ']' {
					return starlark.NewList(items), nil
				}
				val, err := buildValue(dec, itemTok)
				if err != nil {
					return nil, err
				}
				items = append(items, val)
			}
		}
		return nil, fmt.Errorf("unexpected %v", t)
	case string:
		return starlark.String(t), nil
	case json.Number:
		return numberToStarlark(t), nil
	case bool:
		return starlark.Bool(t), nil
	case nil:
		return starlark.None, nil
	}
	return nil, fmt.Errorf("unexpected token %T", tok)
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
