package upstreams

import "testing"

// The fingerprint follows every setting that changes the connection and
// nothing else: a row read back (empty lists as none, the default auth
// mode spelt out) matches what was saved, so a resync reconnects nothing.
func TestConfigFingerprint(t *testing.T) {
	base := Server{Name: "linear", Transport: "http", URL: "https://mcp.example.com/mcp"}
	same := base
	same.Args, same.Env, same.Headers = []string{}, map[string]string{}, map[string]string{}
	same.AuthMode = AuthShared
	same.LastStatus, same.LastError, same.ToolCount, same.UpdatedAt = "ok", "", 12, 99
	if configFingerprint(base) != configFingerprint(same) {
		t.Fatal("status, timestamps or empty lists changed the fingerprint")
	}
	for name, edit := range map[string]func(*Server){
		"url":       func(s *Server) { s.URL = "https://other.example.com/mcp" },
		"headers":   func(s *Server) { s.Headers = map[string]string{"X-Key": "secret://K"} },
		"env":       func(s *Server) { s.Env = map[string]string{"A": "1"} },
		"auth mode": func(s *Server) { s.AuthMode = AuthPerUser },
		"identity":  func(s *Server) { s.Identity = &IdentityForwarding{Header: "x-bk-key"} },
		"transport": func(s *Server) { s.Transport = "stdio"; s.Command = "npx" },
	} {
		changed := base
		edit(&changed)
		if configFingerprint(changed) == configFingerprint(base) {
			t.Errorf("changing the %s left the fingerprint as it was", name)
		}
	}
}
