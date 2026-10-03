package upstreams

import (
	"context"
	"testing"
)

func TestGitHubRequiresPerUserFixedOriginWithoutSharedCredentials(t *testing.T) {
	valid := Server{Name: "github", Transport: "github", URL: "https://api.github.com", AuthMode: AuthPerUser}
	if err := validate(valid); err != nil {
		t.Fatal(err)
	}
	for _, mutate := range []func(*Server){func(s *Server) { s.AuthMode = AuthShared }, func(s *Server) { s.URL = "https://example.com" }, func(s *Server) { s.Headers = map[string]string{"Authorization": "Bearer shared"} }, func(s *Server) { s.Env = map[string]string{"TOKEN": "secret"} }} {
		s := valid
		mutate(&s)
		if validate(s) == nil {
			t.Fatalf("unsafe config accepted: %v", s)
		}
	}
	svc, _, gw := newServiceFixture(t)
	svc.SetPerUserAuth(bearerFake{bearers: map[string]string{}})
	if _, err := svc.Add(context.Background(), valid); err != nil {
		t.Fatal(err)
	}
	if gw.UpstreamToolCount("github") != 8 || !gw.IsPerUser("github") {
		t.Fatal("native tool catalog missing before first sign-in")
	}
}
