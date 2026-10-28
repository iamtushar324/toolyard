package gateway

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
)

type exactToolArguments struct {
	Name string
	Args map[string]any
}
type exactToolArgumentsKey struct{}

// PreserveToolCallNumbers keeps the original JSON numbers across the MCP
// library's generic float64 decoder. Authentication still uses the same request.
// A malformed envelope is left to the MCP server's normal validation.
func PreserveToolCallNumbers(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			next.ServeHTTP(w, r)
			return
		}
		raw, err := io.ReadAll(r.Body)
		if err != nil {
			http.Error(w, "invalid MCP body", 400)
			return
		}
		r.Body = io.NopCloser(bytes.NewReader(raw))
		// The bytes ride along so a later layer (MCPTasks) reads them
		// without buffering the body again.
		r = r.WithContext(context.WithValue(r.Context(), rawMCPBodyKey{}, raw))
		var envelope struct {
			Method string `json:"method"`
			Params struct {
				Name      string         `json:"name"`
				Arguments map[string]any `json:"arguments"`
			} `json:"params"`
		}
		decoder := json.NewDecoder(bytes.NewReader(raw))
		decoder.UseNumber()
		if decoder.Decode(&envelope) == nil && envelope.Method == "tools/call" && envelope.Params.Name != "" {
			r = r.WithContext(context.WithValue(r.Context(), exactToolArgumentsKey{}, exactToolArguments{Name: envelope.Params.Name, Args: envelope.Params.Arguments}))
		}
		next.ServeHTTP(w, r)
	})
}

// rawMCPBodyKey carries the /mcp POST body PreserveToolCallNumbers read.
type rawMCPBodyKey struct{}

// rawMCPBody is the body PreserveToolCallNumbers read, if it ran.
func rawMCPBody(ctx context.Context) ([]byte, bool) {
	raw, ok := ctx.Value(rawMCPBodyKey{}).([]byte)
	return raw, ok
}

// exactArguments is the call's arguments with their original JSON numbers
// when PreserveToolCallNumbers read them for this tool, else fallback. The
// name on the wire may be the tool's published name (tool_names.go) while
// name is the canonical one the handler runs as: both mean the same call.
func (g *Gateway) exactArguments(ctx context.Context, name string, fallback map[string]any) map[string]any {
	exact, ok := ctx.Value(exactToolArgumentsKey{}).(exactToolArguments)
	if ok && (exact.Name == name || g.resolveToolName(exact.Name) == name) {
		return exact.Args
	}
	return fallback
}

func numericArgument(v any) (float64, bool) {
	switch n := v.(type) {
	case float64:
		return n, true
	case json.Number:
		f, e := n.Float64()
		return f, e == nil
	case int:
		return float64(n), true
	}
	return 0, false
}

// stringArg is args[key] when it is a string, else "".
func stringArg(args map[string]any, key string) string {
	v, _ := args[key].(string)
	return v
}

// asInt reads an integer argument from any of the numeric types a decoded
// call can carry, including the json.Number that PreserveToolCallNumbers
// (UseNumber) leaves in the arguments. A json.Number that is not an
// integer ("2.5") is truncated like a float64.
func asInt(v any) (int, bool) {
	switch x := v.(type) {
	case int:
		return x, true
	case int32:
		return int(x), true
	case int64:
		return int(x), true
	case float64:
		return int(x), true
	case json.Number:
		if n, err := x.Int64(); err == nil {
			return int(n), true
		}
		if f, err := x.Float64(); err == nil {
			return int(f), true
		}
	}
	return 0, false
}
