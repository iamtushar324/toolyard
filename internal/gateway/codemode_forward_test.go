package gateway_test

import (
	"strings"
	"testing"

	"github.com/tusharbhardwaj/toolyard/internal/codemode"
	"github.com/tusharbhardwaj/toolyard/internal/gateway"
	"github.com/tusharbhardwaj/toolyard/internal/metrics"
	"github.com/tusharbhardwaj/toolyard/internal/upstreams"
)

// TestCodeModeForwardsIdentityOnNestedCalls: a script's call to an
// identity-forwarding upstream runs as the script's caller. The key rides on
// the nested tools/call and nothing else; a caller without a key is refused
// before the upstream is contacted, and the script stops with the refusal.
func TestCodeModeForwardsIdentityOnNestedCalls(t *testing.T) {
	up := newRecordingUpstream(t)
	f := newForwardFixture(t, keys())
	f.gw.RegisterBuiltins()
	f.add(t, "bk", up, &upstreams.IdentityForwarding{Header: identityHeader}, nil)

	// No _reason on the outer call, as a Bifrost client sends it.
	res, err := f.gw.RouteCall(callerCtx(alice), "test", gateway.CodeModeExecuteToolCode, map[string]any{
		"code": "who = bk.get_whoami()\nresult = {\"ran_as\": who}",
	})
	if err != nil {
		t.Fatal(err)
	}
	if res.IsError || !strings.Contains(text(res), "Return value: {\n  \"ran_as\": \"vk-alice-secret\"\n}") {
		t.Fatalf("alice's script (isError=%v):\n%s", res.IsError, text(res))
	}
	if calls := up.assertOnlyToolCallsCarryKey(t); len(calls) != 1 || calls[0] != "vk-alice-secret" {
		t.Fatalf("tools/call headers = %v", calls)
	}
	m, ok := f.met.last("bk.get_whoami")
	if !ok || m.Outcome != metrics.OutcomeOK || m.Via != codemode.Via || m.AgentID != alice {
		t.Fatalf("nested call metric = %+v", m)
	}

	// The stub for the forwarding upstream shows the tool, not the header.
	stub, err := f.gw.RouteCall(callerCtx(alice), "test", gateway.CodeModeReadToolFile, map[string]any{"fileName": "servers/bk/get_whoami.pyi"})
	if err != nil || stub.IsError || !strings.Contains(text(stub), "def get_whoami() -> dict:  # echo the identity header") {
		t.Fatalf("stub (err %v):\n%s", err, text(stub))
	}

	res, err = f.gw.RouteCall(callerCtx(nokey), "test", gateway.CodeModeExecuteToolCode, map[string]any{
		"code": "result = bk.get_whoami()",
	})
	if err != nil {
		t.Fatal(err)
	}
	if !res.IsError || !strings.Contains(text(res), "tool call failed for bk.get_whoami: bk records who made each change, and your toolyard user has no Beknown key yet.") {
		t.Fatalf("nokey's script (isError=%v):\n%s", res.IsError, text(res))
	}
	if up.count("tools/call") != 1 {
		t.Fatalf("upstream saw the refused call: %+v", up.requests())
	}
	if m, ok := f.met.last("bk.get_whoami"); !ok || m.ErrorClass != "identity" || m.Via != codemode.Via {
		t.Fatalf("refused call metric = %+v", m)
	}
}
