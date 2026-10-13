package api

import (
	"context"
	"encoding/base64"
	"github.com/tusharbhardwaj/toolyard/internal/callbacks"
	"github.com/tusharbhardwaj/toolyard/internal/sealbox"
	"net"
	"net/http"
	"testing"
)

func TestAdminPrivateCallbackRegistration(t *testing.T) {
	f := newInboxAPIFixture(t)
	cipher, err := sealbox.NewCipher(make([]byte, 32))
	if err != nil {
		t.Fatal(err)
	}
	svc := callbacks.New(f.db, cipher)
	svc.Resolver = func(context.Context, string) ([]net.IPAddr, error) {
		return []net.IPAddr{{IP: net.ParseIP("10.0.0.5")}}, nil
	}
	f.srv.callbacks = svc
	body := map[string]any{"agent_id": f.agent, "client_receiver_id": "private-hook", "destination": "https://private.example/decision", "secret": "whsec_" + base64.StdEncoding.EncodeToString(make([]byte, 32))}
	if _, err = svc.Register(context.Background(), f.agent, "", "agent-attempt", body["destination"].(string), body["secret"].(string), false); err == nil {
		t.Fatal("unregistered private receiver accepted")
	}
	code, out := f.owner(t, http.MethodPost, "/v1/admin/callback-receivers", body)
	if code != 200 {
		t.Fatalf("admin registration: %d %+v", code, out)
	}
	receiver, err := svc.Get(context.Background(), f.agent, out["callback_ref"].(string))
	if err != nil || !receiver.PrivateAllowed {
		t.Fatalf("private trust missing: %+v %v", receiver, err)
	}
	body["destination"] = "https://changed.example/decision"
	if code, _ = f.owner(t, http.MethodPost, "/v1/admin/callback-receivers", body); code != 400 {
		t.Fatal("destination trust was reused", code)
	}
	body["agent_id"] = "unknown-agent"
	if code, _ = f.owner(t, http.MethodPost, "/v1/admin/callback-receivers", body); code != 404 {
		t.Fatal("unknown owner accepted", code)
	}
	if memberAllowed(http.MethodPost, "/v1/admin/callback-receivers") {
		t.Fatal("private receiver route is member writable")
	}
}
