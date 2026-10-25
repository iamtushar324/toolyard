package codemode

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/mark3labs/mcp-go/mcp"
)

// blockingCaller is the JavaScript fake whose demo.read blocks until the
// call's context ends, as an upstream call does when toolyard stops.
type blockingCaller struct{ *jsFake }

func (c blockingCaller) RouteCall(ctx context.Context, via, target string, args map[string]any) (*mcp.CallToolResult, error) {
	if target == "demo.read" {
		<-ctx.Done()
		return nil, ctx.Err()
	}
	return c.jsFake.RouteCall(ctx, via, target, args)
}

// cancelAfter is a context cancelled after d, as a drain or a client that
// went away cancels a run.
func cancelAfter(t *testing.T, d time.Duration) context.Context {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	timer := time.AfterFunc(d, cancel)
	t.Cleanup(func() { timer.Stop(); cancel() })
	return ctx
}

// TestCancelledCallerInterruptsTheRun: a run whose caller's context is
// cancelled (a drain, a shutdown, a client that went away) is interrupted,
// not timed out: outcome_unknown once a tool call was sent, failed with
// nothing run before. The script's own clock stays a timeout.
func TestCancelledCallerInterruptsTheRun(t *testing.T) {
	bothWays(t, blockingCaller{newJSFake()}, Limits{ScriptTimeout: 30 * time.Second}, func(t *testing.T, rt *Runtime) {
		start := time.Now()
		rr, err := rt.Run(cancelAfter(t, 500*time.Millisecond), "a = demo.facts()\nb = demo.read(q=\"x\")\nresult = b\n", RunOptions{})
		if err != nil {
			t.Fatal(err)
		}
		if rr.Status != StatusOutcomeUnknown || rr.ErrorKind != ErrorInterrupted || !strings.Contains(rr.ErrorMessage, "after it sent 2 tool call(s)") {
			t.Fatalf("after two calls: %+v", rr)
		}
		if took := time.Since(start); took > 15*time.Second {
			t.Fatalf("the interrupted run took %s", took)
		}

		rr, err = rt.Run(cancelAfter(t, 500*time.Millisecond), "let i = 0;\nwhile (true) { i++; }", RunOptions{Language: LanguageJavaScript})
		if err != nil {
			t.Fatal(err)
		}
		if rr.Status != StatusFailed || rr.ErrorKind != ErrorInterrupted || !strings.Contains(rr.ErrorMessage, "nothing ran") {
			t.Fatalf("before any call: %+v", rr)
		}
	})

	// The script's own clock is a timeout, not an interruption.
	rt := inProcess(blockingCaller{newJSFake()}, Limits{ScriptTimeout: 500 * time.Millisecond})
	rr, err := rt.Run(context.Background(), "let i = 0;\nwhile (true) { i++; }", RunOptions{Language: LanguageJavaScript})
	if err != nil {
		t.Fatal(err)
	}
	if rr.Status != StatusFailed || rr.ErrorKind != ErrorTimeout {
		t.Fatalf("own clock: %+v", rr)
	}
}

// TestExecuteToolCodeReportsAnInterruption: executeToolCode's result says
// the same, so a caller (an async job) can tell an interrupted run from a
// failed one.
func TestExecuteToolCodeReportsAnInterruption(t *testing.T) {
	rt := inProcess(blockingCaller{newJSFake()}, Limits{ScriptTimeout: 30 * time.Second})
	out := rt.Execute(cancelAfter(t, 500*time.Millisecond), "a = demo.facts()\nb = demo.read(q=\"x\")\nresult = b\n", ExecOptions{})
	if !out.Failed || out.Status != StatusOutcomeUnknown || out.ErrorKind != ErrorInterrupted || !strings.Contains(out.Text, "toolyard stopped the script before it finished") {
		t.Fatalf("interrupted after a call: %+v", out)
	}
	out = rt.Execute(cancelAfter(t, 500*time.Millisecond), "let i = 0;\nwhile (true) { i++; }", ExecOptions{Language: LanguageJavaScript})
	if !out.Failed || out.Status != StatusFailed || out.ErrorKind != ErrorInterrupted || !strings.Contains(out.Text, "nothing ran") {
		t.Fatalf("interrupted before a call: %+v", out)
	}
	out = rt.Execute(context.Background(), "result = demo.facts()\n", ExecOptions{})
	if out.Failed || out.Status != StatusSucceeded || out.ErrorKind != "" {
		t.Fatalf("a plain run: %+v", out)
	}
}
