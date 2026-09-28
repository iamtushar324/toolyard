package main

import (
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// Inbox commands for agents without MCP. They call the same inbox.* tools
// through /v1/agents/tools/run, so the behaviour is identical.
//
//	toolyard guide [topic]
//	toolyard check <tool> [<tool>...]   |  --json '[{"tool":"x","args":{...}}]'
//	toolyard request --from request.json [--dry-run] [--kind access|question|blocker|update]
//	toolyard wait <request_id>... [--timeout 5m] [--mode any|all]
//	toolyard skills install toolyard-inbox [--dir ~/.claude/skills]

const cliReason = "toolyard CLI on behalf of this agent: "

func runInboxTool(tool string, args map[string]any, timeout time.Duration) (map[string]any, error) {
	cfg, err := requireConfig()
	if err != nil {
		return nil, err
	}
	client := newClient(cfg.Server, cfg.Token)
	if timeout > 0 {
		client.hc.Timeout = timeout + 30*time.Second
	}
	if _, ok := args["_reason"]; !ok {
		args["_reason"] = cliReason + tool
	}
	var res map[string]any
	if err := client.doJSON("POST", "/v1/agents/tools/run", map[string]any{"tool": tool, "arguments": args}, &res); err != nil {
		return nil, err
	}
	return res, nil
}

// printToolResult prints structured content when there is some, otherwise
// the text, and exits 2 when the tool reported an error.
func printToolResult(res map[string]any) error {
	if sc, ok := res["structured_content"]; ok && sc != nil {
		_ = printJSON(sc, true)
	} else if content, ok := res["content"].([]any); ok {
		for _, c := range content {
			if m, ok := c.(map[string]any); ok {
				if t, ok := m["text"].(string); ok {
					fmt.Println(t)
				}
			}
		}
	}
	if isErr, _ := res["is_error"].(bool); isErr {
		os.Exit(2)
	}
	return nil
}

func runGuide(argv []string) error {
	topic := ""
	if len(argv) > 0 {
		topic = argv[0]
	}
	cfg, err := requireConfig()
	if err != nil {
		return err
	}
	req, err := http.NewRequest("GET", strings.TrimRight(cfg.Server, "/")+"/v1/guide?topic="+url.QueryEscape(topic), nil)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+cfg.Token)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode >= 300 {
		return fmt.Errorf("guide: %s: %s", resp.Status, errMessage(body))
	}
	fmt.Println(string(body))
	return nil
}

func runCheck(argv []string) error {
	fs := flag.NewFlagSet("check", flag.ExitOnError)
	raw := fs.String("json", "", `calls as JSON: [{"tool":"deploy.run","args":{...}}]`)
	_ = fs.Parse(argv)
	var calls []any
	if *raw != "" {
		if err := json.Unmarshal([]byte(*raw), &calls); err != nil {
			return fmt.Errorf("parse --json: %w", err)
		}
	}
	for _, t := range fs.Args() {
		calls = append(calls, map[string]any{"tool": t})
	}
	if len(calls) == 0 {
		return errors.New("usage: toolyard check <tool>... | --json '[{\"tool\":...,\"args\":{...}}]'")
	}
	res, err := runInboxTool("inbox.check", map[string]any{"calls": calls}, 0)
	if err != nil {
		return err
	}
	return printToolResult(res)
}

func runRequest(argv []string) error {
	fs := flag.NewFlagSet("request", flag.ExitOnError)
	from := fs.String("from", "", "JSON file with the request (- for stdin)")
	dry := fs.Bool("dry-run", false, "check the request and show its flags without sending it")
	kind := fs.String("kind", "", "access (default), question, blocker or update; overrides the file's kind")
	_ = fs.Parse(argv)
	if *from == "" {
		return errors.New("usage: toolyard request --from request.json [--dry-run] [--kind access|question|blocker|update]")
	}
	var data []byte
	var err error
	if *from == "-" {
		data, err = io.ReadAll(os.Stdin)
	} else {
		data, err = os.ReadFile(*from)
	}
	if err != nil {
		return err
	}
	var args map[string]any
	if err := json.Unmarshal(data, &args); err != nil {
		return fmt.Errorf("parse %s: %w", *from, err)
	}
	k := *kind
	if k == "" {
		k, _ = args["kind"].(string)
	}
	tool := "inbox.request"
	switch k {
	case "", "access":
		delete(args, "kind")
	case "question", "blocker":
		tool = "inbox.ask"
		args["kind"] = k
	case "update":
		tool = "inbox.post"
		delete(args, "kind")
	default:
		return fmt.Errorf("unknown kind %q", k)
	}
	if *dry {
		if tool == "inbox.post" {
			return errors.New("updates don't have a dry run")
		}
		args["dry_run"] = true
	}
	res, err := runInboxTool(tool, args, 0)
	if err != nil {
		return err
	}
	return printToolResult(res)
}

func runWait(argv []string) error {
	fs := flag.NewFlagSet("wait", flag.ExitOnError)
	timeout := fs.Duration("timeout", 5*time.Minute, "how long to wait (max 5m)")
	mode := fs.String("mode", "any", "any: return on the first decision; all: wait for every request")
	_ = fs.Parse(argv)
	ids := fs.Args()
	if len(ids) == 0 {
		return errors.New("usage: toolyard wait <request_id>... [--timeout 5m] [--mode any|all]")
	}
	if *timeout > 5*time.Minute {
		*timeout = 5 * time.Minute
	}
	var list []any
	for _, id := range ids {
		list = append(list, id)
	}
	res, err := runInboxTool("inbox.wait", map[string]any{"ids": list, "mode": *mode, "timeout_seconds": int(timeout.Seconds())}, *timeout)
	if err != nil {
		return err
	}
	return printToolResult(res)
}

func runSkills(argv []string) error {
	if len(argv) < 2 || argv[0] != "install" || argv[1] != "toolyard-inbox" {
		return errors.New("usage: toolyard skills install toolyard-inbox [--dir ~/.claude/skills]")
	}
	fs := flag.NewFlagSet("skills", flag.ExitOnError)
	home, _ := os.UserHomeDir()
	dir := fs.String("dir", filepath.Join(home, ".claude", "skills"), "skills directory")
	_ = fs.Parse(argv[2:])
	cfg, err := requireConfig()
	if err != nil {
		return err
	}
	req, err := http.NewRequest("GET", strings.TrimRight(cfg.Server, "/")+"/v1/guide/skill", nil)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+cfg.Token)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode >= 300 {
		return fmt.Errorf("skill: %s: %s", resp.Status, errMessage(body))
	}
	target := filepath.Join(*dir, "toolyard-inbox")
	if err := os.MkdirAll(target, 0o755); err != nil {
		return err
	}
	path := filepath.Join(target, "SKILL.md")
	if err := os.WriteFile(path, body, 0o644); err != nil {
		return err
	}
	fmt.Println("installed", path)
	return nil
}
