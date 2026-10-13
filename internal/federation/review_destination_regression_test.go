package federation

import "testing"

func TestReviewPrivateReceiverRequiresLiteralCallbackPath(t *testing.T) {
	s, _, key := federationFixture(t)
	ctx := t.Context()
	p := principal(t, s, key, "stage", "review-member")
	c, err := s.Connect(ctx, p, 0)
	if err != nil {
		t.Fatal(err)
	}
	origin := "https://stage.example.test"
	if !s.PrivateReceiverAllowed(ctx, c.AgentID, "stage", origin+"/api/session-webhooks/session_123") {
		t.Fatal("literal authorized callback was rejected")
	}
	for _, path := range []string{"/api/session-webhooks/../admin", "/api/session-webhooks/session/../../admin", "/api/session-webhooks/%2e%2e%2fadmin", "/api/session-webhooks/%73ession_123", "/api/session-webhooks/session_123?target=admin", "/api/session-webhooks/session_123#admin"} {
		if s.PrivateReceiverAllowed(ctx, c.AgentID, "stage", origin+path) {
			t.Fatalf("nonliteral callback allowed: %s", path)
		}
	}
	if s.PrivateReceiverAllowed(ctx, c.AgentID, "stable", "https://stable.example.test/api/session-webhooks/session_123") {
		t.Fatal("agent used another environment's private destination")
	}
}
