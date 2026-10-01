package main

import (
	"flag"
	"strings"
	"testing"
)

func TestRedactJSONHidesTokensAtAnyDepth(t *testing.T) {
	in := `{"agent_id":"ag_1","token":"ag_1.secret","nested":[{"token":"tyop_x.y","name":"n"}],"token_file":"f"}`
	out := string(redactJSON([]byte(in)))
	if strings.Contains(out, "ag_1.secret") || strings.Contains(out, "tyop_x.y") {
		t.Fatalf("token leaked: %s", out)
	}
	if !strings.Contains(out, `"agent_id":"ag_1"`) || !strings.Contains(out, `"name":"n"`) {
		t.Fatalf("other fields lost: %s", out)
	}
	if string(redactJSON([]byte("not json"))) != "not json" {
		t.Fatal("non-JSON should pass through")
	}
}

func TestSplitFlagsAllowsFlagsAfterArgs(t *testing.T) {
	fs := flag.NewFlagSet("t", flag.ContinueOnError)
	out := fs.String("out", "", "")
	pos, err := splitFlags(fs, []string{"create", "hermes", "--out", "/tmp/x"})
	if err != nil || strings.Join(pos, " ") != "create hermes" || *out != "/tmp/x" {
		t.Fatalf("pos=%v out=%q err=%v", pos, *out, err)
	}
}
