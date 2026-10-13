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

func TestInboxMCPMetadataDescribesCallbacksAndRecoverableGrants(t *testing.T) {
	entries := (&Gateway{}).inboxTools()
	found := make(map[string]bool)
	for _, entry := range entries {
		found[entry.tool.Name] = true
		switch entry.tool.Name {
		case "inbox.request":
			for _, want := range []string{"task.objective", "callback_ref", "continue unrelated work or end the turn", "inbox.status", "authoritative decisions", "inbox.wait is optional"} {
				if !strings.Contains(entry.tool.Description, want) {
					t.Errorf("inbox.request description is missing %q", want)
				}
			}
			requiredTask := false
			for _, field := range entry.tool.InputSchema.Required {
				if field == "task" {
					requiredTask = true
				}
			}
			if !requiredTask {
				t.Error("inbox.request schema must require task")
			}
			task := entry.tool.InputSchema.Properties["task"].(map[string]any)
			if fields := task["required"].([]string); len(fields) != 1 || fields[0] != "objective" {
				t.Error("inbox.request task must require objective")
			}
			tools := entry.tool.InputSchema.Properties["tools"].(map[string]any)
			items := tools["items"].(map[string]any)
			props := items["properties"].(map[string]any)
			summary := props["summary"].(map[string]any)["description"].(string)
			for _, want := range []string{"purpose", "relationship to task.objective"} {
				if !strings.Contains(summary, want) {
					t.Errorf("call summary description is missing %q", want)
				}
			}
		case "inbox.status":
			for _, want := range []string{"Status reads never consume grants", "same still-valid unused grant token", "lost response", "Expired, revoked, or consumed"} {
				if !strings.Contains(entry.tool.Description, want) {
					t.Errorf("inbox.status description is missing %q", want)
				}
			}
			if strings.Contains(entry.tool.Description, "shown once") {
				t.Error("inbox.status must not describe tokens as shown once")
			}
		case "inbox.wait":
			for _, want := range []string{"Optionally", "callback_ref", "continue unrelated work or end the turn", "inbox.status"} {
				if !strings.Contains(entry.tool.Description, want) {
					t.Errorf("inbox.wait description is missing %q", want)
				}
			}
		}
	}
	for _, name := range []string{"inbox.request", "inbox.status", "inbox.wait"} {
		if !found[name] {
			t.Errorf("missing MCP tool %q", name)
		}
	}
}
