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
func exactArguments(ctx context.Context, name string, fallback map[string]any) map[string]any {
	if exact, ok := ctx.Value(exactToolArgumentsKey{}).(exactToolArguments); ok && exact.Name == name {
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
