package gateway

import (
	"context"
	"strings"
	"testing"

	"github.com/tusharbhardwaj/toolyard/internal/inbox"
)

func TestInboxIdempotentToolsAcrossGateway(t *testing.T) {
	for _, name := range []string{"inbox.request", "inbox.ask", "inbox.post"} {
		t.Run(name, func(t *testing.T) {
			f := newInboxFixture(t)
			args := requestArgs(deployArgs())
			args["idempotency_key"] = "manager-request"
			if name != "inbox.request" {
				delete(args, "facts")
				delete(args, "tools")
			}
			if name == "inbox.ask" {
				args["options"] = []any{map[string]any{"label": "Proceed"}, map[string]any{"label": "Stop"}}
			}
			first := f.call(t, f.agent, name, args)
			original := structured(t, first)
			if first.IsError || original["ok"] != true || original["request_id"] == nil {
				t.Fatalf("first request: %+v", original)
			}
			second := f.call(t, f.agent, name, args)
			retry := structured(t, second)
			if second.IsError || retry["request_id"] != original["request_id"] || retry["replayed"] != true {
				t.Fatalf("retry: %+v", retry)
			}
			if !strings.Contains(textOf(second), "Existing request") {
				t.Fatalf("retry text implies a new send: %s", textOf(second))
			}
			args["message"] = "I changed the scope."
			if res := f.call(t, f.agent, name, args); !res.IsError || !strings.Contains(textOf(res), "idempotency_key") {
				t.Fatalf("conflict did not reach caller: %+v", res)
			}
			if res := f.call(t, f.anonym, name, args); !res.IsError {
				t.Fatal("anonymous request accepted")
			}
		})
	}
}

func TestIdempotentSessionToolAcrossGateway(t *testing.T) {
	f := newInboxFixture(t)
	args := map[string]any{"title": "Manage the workspace", "repo": "repo", "branch": "main", "host": "server", "idempotency_key": "manager-session"}
	res := f.call(t, f.agent, "session.start", args)
	original := structured(t, res)
	id, ok := original["id"].(string)
	if res.IsError || !ok || id == "" {
		t.Fatalf("session start: %+v", original)
	}
	updated := f.call(t, f.agent, "session.update", map[string]any{"session_id": id, "status": inbox.SessionBlockedOnOwner, "note": "The scope needs a decision"})
	if updated.IsError {
		t.Fatalf("session update: %+v", updated)
	}
	retry := structured(t, f.call(t, f.agent, "session.start", args))
	if retry["id"] != id || retry["status"] != inbox.SessionBlockedOnOwner || retry["note"] != "The scope needs a decision" {
		t.Fatalf("retry reset the session: %+v", retry)
	}
	other := WithAgentID(context.Background(), "ag_2")
	if res := f.call(t, other, "session.update", map[string]any{"session_id": id, "status": inbox.SessionWorking}); !res.IsError {
		t.Fatal("another agent updated the session")
	}
	if res := structured(t, f.call(t, other, "session.start", args)); res["id"] == id {
		t.Fatal("key was shared across agents")
	}
	args["title"] = "Different work"
	if res := f.call(t, f.agent, "session.start", args); !res.IsError || !strings.Contains(textOf(res), "idempotency_key") {
		t.Fatalf("changed session accepted: %+v", res)
	}
}

func TestIdempotentSessionToolRejectsInvalidKeyTypes(t *testing.T) {
	f := newInboxFixture(t)
	for _, key := range []any{123, true, nil, map[string]any{"key": "job"}, []any{"job"}} {
		args := map[string]any{"title": "Manage the workspace", "idempotency_key": key}
		for range 2 {
			res := f.call(t, f.agent, "session.start", args)
			if !res.IsError || !strings.Contains(textOf(res), "idempotency_key must be a string") {
				t.Fatalf("invalid key accepted: %+v", res)
			}
		}
	}
	list, err := f.svc.ListSessions(context.Background())
	if err != nil || len(list) != 0 {
		t.Fatalf("invalid retries created sessions: %+v, %v", list, err)
	}
}
