package gateway

import (
	"strings"
	"testing"
	"time"
)

func TestInboxAgentInstructionsDescribeDecisionAndRecovery(t *testing.T) {
	text := buildInstructions(nil, time.Second, false)
	for _, want := range []string{"_reason", "call_id", "task.objective", "material_risks", "callback_ref", "inbox.wait remains optional", "same still-valid unused token", "outcome_unknown", "Rejected calls receive no grant", "required is a planning hint", "_grant", "FALLBACK", "POST /v1/code/run", "TOOLYARD_TOKEN_FILE", "GET /v1/code/ty", "topic fallback"} {
		if !strings.Contains(text, want) {
			t.Errorf("missing %q", want)
		}
	}
	for _, stale := range []string{"AUTO-EXECUTE ON APPROVE", "auto-approval rule", "invoke them in parallel", "token is shown once",
		"run inbox.check for concrete planned calls", "tools.execute to discover tools outside your visible catalog"} {
		if strings.Contains(text, stale) {
			t.Errorf("obsolete instruction %q", stale)
		}
	}
}

// TestInstructionsTeachTheFiveToolFlow: the instructions name the five
// tools and the flow (find, read, run, ask once per round, read the
// decision), and still read true for an agent on the full tool list.
func TestInstructionsTeachTheFiveToolFlow(t *testing.T) {
	text := buildInstructions(nil, time.Second, false)
	for _, name := range SmallToolList {
		if !strings.Contains(text, name) {
			t.Errorf("the instructions do not name %s", name)
		}
	}
	for _, want := range []string{
		"by default you see five tools",
		"run: now, needs_approval or blocked",
		"inbox.request is the one way to get permission",
		"ONE inbox.request with every restricted call (run: needs_approval) and every question",
		"Start another round only for what the answers open",
		"tools.tryCall",
		"inbox.check stays available but is optional",
		"full tool list",
		"PERMISSIONS:",
	} {
		if !strings.Contains(text, want) {
			t.Errorf("missing %q", want)
		}
	}
	if skills := buildInstructions(nil, time.Second, false) + " " + skillsInstructions(false); !strings.Contains(skills, "SKILLS:") || !strings.Contains(skills, "skills.search") ||
		!strings.Contains(skills, "tools.search") || !strings.Contains(skills, "toolyard.skills_get") {
		t.Errorf("the skills paragraph does not point at tools.search and code mode: %q", skillsInstructions(false))
	}
	if guide := skillsGuideInstructions("toolyard/toolyard-basics", false); !strings.Contains(guide, `"toolyard/toolyard-basics"`) || !strings.Contains(guide, "toolyard.skills_get") {
		t.Errorf("the guide sentence: %q", guide)
	}
}

func TestInboxMCPMetadataDescribesCallbacksAndRecoverableGrants(t *testing.T) {
	entries := (&Gateway{}).inboxTools()
	found := make(map[string]bool)
	for _, entry := range entries {
		found[entry.tool.Name] = true
		switch entry.tool.Name {
		case "inbox.request":
			for _, want := range []string{"task.objective", "callback_ref", "continue unrelated work or end the turn", "inbox.status", "authoritative decisions", "inbox.wait is optional",
				"the one way an agent gets permission", "ONE request", "every question", "only for what the answers open"} {
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
			for _, want := range []string{"Status reads never consume grants", "same still-valid unused grant token", "lost response", "Expired, revoked, or consumed",
				"answers to your questions", "_grant", "one new inbox.request"} {
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
