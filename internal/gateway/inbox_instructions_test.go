package gateway

import (
	"strings"
	"testing"
	"time"
)

func TestInboxAgentInstructionsDescribeDecisionAndRecovery(t *testing.T) {
	text := buildInstructions(nil, time.Second)
	for _, want := range []string{"call_id", "task.objective", "material_risks", "callback_ref", "inbox.wait remains optional", "same still-valid unused token", "outcome_unknown", "Rejected calls receive no grant", "required is a planning hint"} {
		if !strings.Contains(text, want) {
			t.Errorf("missing %q", want)
		}
	}
	for _, stale := range []string{"AUTO-EXECUTE ON APPROVE", "auto-approval rule", "invoke them in parallel", "token is shown once"} {
		if strings.Contains(text, stale) {
			t.Errorf("obsolete instruction %q", stale)
		}
	}
}
