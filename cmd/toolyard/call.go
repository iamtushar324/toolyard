package main

import (
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"os"
	"strings"
	"time"
)

// runCall implements `toolyard call <tool> [--arg k=v]... [--json '{...}']
// [--reason "..."] [--approve-wait 30s]`.
//
// Result handling:
//   - normal call: prints structured_content (or content) as JSON.
//   - deferred call (status=pending_approval): if --approve-wait > 0, the
//     CLI polls /v1/agents/approvals/<id> until the executor writes the
//     result (or the timeout elapses) and prints the eventual outcome.
//     Without --approve-wait, the deferred envelope is printed verbatim
//     so the operator can inspect it.
type argList []string

func (a *argList) String() string     { return strings.Join(*a, ",") }
func (a *argList) Set(v string) error { *a = append(*a, v); return nil }

func runCall(argv []string) error {
	if len(argv) == 0 {
		return errors.New("usage: toolyard call <tool> [--arg k=v]... [--json '{...}'] [--reason ...] [--approve-wait 30s]")
	}
	tool := strings.TrimSpace(argv[0])
	if tool == "" || strings.HasPrefix(tool, "-") {
		return errors.New("usage: toolyard call <tool> [--arg k=v]... [--json '{...}'] [--reason ...] [--approve-wait 30s]")
	}

	fs := flag.NewFlagSet("call", flag.ExitOnError)
	var args argList
	fs.Var(&args, "arg", "argument as key=value (repeatable). Values are passed as strings; use --json for typed args.")
	jsonArgs := fs.String("json", "", "raw JSON object to use as the full arguments map. Mutually exclusive with --arg.")
	reason := fs.String("reason", "", "value for the required _reason field (shown to the human reviewer for write tools)")
	approveWait := fs.Duration("approve-wait", 0, "if the call needs approval, poll for this long (0 = print the deferred envelope and exit)")
	pretty := fs.Bool("pretty", true, "indent JSON output")
	_ = fs.Parse(argv[1:])

	arguments, err := buildArgs(args, *jsonArgs)
	if err != nil {
		return err
	}
	if *reason != "" {
		arguments["_reason"] = *reason
	}

	cfg, err := requireConfig()
	if err != nil {
		return err
	}
	client := newClient(cfg.Server, cfg.Token)

	var res map[string]any
	if err := client.doJSON("POST", "/v1/agents/tools/run",
		map[string]any{"tool": tool, "arguments": arguments}, &res); err != nil {
		return err
	}

	// Detect a deferred-approval response. The gateway wraps the
	// envelope into structured_content; status=="pending_approval" is the
	// signal. Without --approve-wait, we just print it.
	if approvalID := pendingApprovalID(res); approvalID != "" {
		if *approveWait <= 0 {
			return printJSON(res, *pretty)
		}
		final, err := waitForApproval(client, approvalID, *approveWait)
		if err != nil {
			return err
		}
		return printJSON(final, *pretty)
	}

	// Tool reported a structured error: surface as non-zero exit so
	// shell scripts can branch on it.
	if isErr, _ := res["is_error"].(bool); isErr {
		_ = printJSON(res, *pretty)
		os.Exit(2)
	}
	return printJSON(res, *pretty)
}

func buildArgs(kvs argList, raw string) (map[string]any, error) {
	if raw != "" && len(kvs) > 0 {
		return nil, errors.New("--json and --arg are mutually exclusive")
	}
	if raw != "" {
		var m map[string]any
		if err := json.Unmarshal([]byte(raw), &m); err != nil {
			return nil, fmt.Errorf("parse --json: %w", err)
		}
		return m, nil
	}
	m := map[string]any{}
	for _, kv := range kvs {
		eq := strings.IndexByte(kv, '=')
		if eq <= 0 {
			return nil, fmt.Errorf("--arg must be key=value, got %q", kv)
		}
		m[kv[:eq]] = kv[eq+1:]
	}
	return m, nil
}

// pendingApprovalID extracts the approval_id from a CallToolResult-shaped
// JSON if the call was deferred. Returns "" otherwise.
func pendingApprovalID(res map[string]any) string {
	sc, ok := res["structured_content"].(map[string]any)
	if !ok {
		return ""
	}
	status, _ := sc["status"].(string)
	if status != "pending_approval" {
		return ""
	}
	id, _ := sc["approval_id"].(string)
	return id
}

// waitForApproval polls /v1/agents/approvals/<id> at a gentle cadence
// until the approval terminates (allowed-with-result, denied, expired,
// cancelled) or the timeout elapses. Returns the final approval snapshot
// shaped to look like a tool result so the caller can print it uniformly.
func waitForApproval(client *httpClient, id string, timeout time.Duration) (map[string]any, error) {
	deadline := time.Now().Add(timeout)
	delay := time.Second
	const maxDelay = 5 * time.Second
	for {
		var req map[string]any
		if err := client.doJSON("GET", "/v1/agents/approvals/"+id, nil, &req); err != nil {
			return nil, err
		}
		status, _ := req["status"].(string)
		switch status {
		case "allowed":
			// Allowed but the executor may not have written the result
			// yet. Keep polling until result_executed_at is set.
			if _, ok := req["result_envelope"]; ok {
				return req, nil
			}
			if execAt, _ := req["result_executed_at"].(float64); execAt > 0 {
				return req, nil
			}
		case "denied", "expired", "cancelled":
			return req, nil
		}
		if time.Now().After(deadline) {
			return req, fmt.Errorf("timed out after %s waiting for approval %s (status=%s)",
				timeout, id, status)
		}
		time.Sleep(delay)
		if delay < maxDelay {
			delay = delay * 3 / 2
		}
	}
}

func printJSON(v any, pretty bool) error {
	enc := json.NewEncoder(os.Stdout)
	if pretty {
		enc.SetIndent("", "  ")
	}
	return enc.Encode(v)
}
