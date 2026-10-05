package api

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"

	"github.com/tusharbhardwaj/toolyard/internal/callbacks"
	"github.com/tusharbhardwaj/toolyard/internal/federation"
	"github.com/tusharbhardwaj/toolyard/internal/sealbox"
)

func TestLocalConnectionRoutesAndPullOwnership(t *testing.T) {
	f := newInboxAPIFixture(t)
	ctx := t.Context()
	cipher, _ := sealbox.NewCipher(make([]byte, 32))
	fed, err := federation.New(ctx, f.db, f.srv.identity, cipher)
	if err != nil {
		t.Fatal(err)
	}
	f.srv.federation = fed
	f.srv.callbacks = callbacks.New(f.db, cipher)
	f.srv.security.PublicURL = "https://toolyard.example"
	handler := f.srv.SecurityHeaders(f.srv.EnforceOriginOnMutations(f.srv.HardenAPI(f.srv.RoleGuard(f.mux))))
	call := func(token, method, path string, body any) *httptest.ResponseRecorder {
		t.Helper()
		raw, _ := json.Marshal(body)
		req := httptest.NewRequest(method, path, bytes.NewReader(raw))
		req.Header.Set("Content-Type", "application/json")
		if token != "" {
			req.Header.Set("Authorization", "Bearer "+token)
		}
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, req)
		return rec
	}
	if got := call("", http.MethodPost, "/v1/connections/api-key", map[string]any{}); got.Code != 401 {
		t.Fatal("unauth", got.Code)
	}
	got := call(f.token, http.MethodPost, "/v1/connections/api-key", map[string]any{"environment_id": "mac", "local_user_id": "owner", "instance_id": fed.InstanceID, "expected_version": 0})
	if got.Code != 200 {
		t.Fatal("key exchange", got.Code, got.Body.String())
	}
	var credential federation.Credential
	if err = json.Unmarshal(got.Body.Bytes(), &credential); err != nil {
		t.Fatal(err)
	}
	got = call(credential.Token, http.MethodPost, "/v1/callbacks/receivers", map[string]any{"transport": "pull", "environment_id": "wrong", "client_receiver_id": "hook", "secret": "whsec_" + base64.StdEncoding.EncodeToString(make([]byte, 32))})
	if got.Code != 403 {
		t.Fatal("env mismatch", got.Code)
	}
	got = call(credential.Token, http.MethodPost, "/v1/callbacks/receivers", map[string]any{"transport": "pull", "environment_id": "mac", "destination": "https://any.example", "client_receiver_id": "hook", "secret": "whsec_" + base64.StdEncoding.EncodeToString(make([]byte, 32))})
	if got.Code != 403 {
		t.Fatal("pull URL accepted", got.Code)
	}
	got = call(credential.Token, http.MethodPost, "/v1/callbacks/receivers", map[string]any{"transport": "pull", "environment_id": "mac", "client_receiver_id": "hook", "secret": "whsec_" + base64.StdEncoding.EncodeToString(make([]byte, 32))})
	if got.Code != 200 {
		t.Fatal("pull register", got.Code, got.Body.String())
	}
	var receiver callbacks.Receiver
	json.Unmarshal(got.Body.Bytes(), &receiver)
	_, err = f.db.Exec(`INSERT INTO callback_outbox(id,request_id,decision_revision,receiver_id,receiver_revision,payload,next_attempt_at) VALUES('evt-api','req',1,?,1,'{}',0)`, receiver.ID)
	if err != nil {
		t.Fatal(err)
	}
	got = call(credential.Token, http.MethodGet, "/v1/callbacks/receivers/"+receiver.ID+"/events", nil)
	if got.Code != 200 {
		t.Fatal("pull", got.Code, got.Body.String())
	}
	var events struct {
		Events []callbacks.PullEvent `json:"events"`
	}
	json.Unmarshal(got.Body.Bytes(), &events)
	if len(events.Events) != 1 || events.Events[0].Body != "{}" {
		t.Fatal("event body")
	}
	got = call(f.token, http.MethodGet, "/v1/callbacks/receivers/"+receiver.ID+"/events", nil)
	if got.Code != 404 {
		t.Fatal("bootstrap stole derived events", got.Code)
	}
	got = call(credential.Token, http.MethodPost, "/v1/callbacks/receivers/"+receiver.ID+"/ack", map[string]string{"event_id": "evt-api"})
	if got.Code != 200 {
		t.Fatal("ack", got.Code)
	}
	got = call(credential.Token, http.MethodPost, "/v1/connections/handoff", map[string]string{})
	if got.Code != 200 {
		t.Fatal("handoff", got.Code)
	}
	var handoff map[string]string
	json.Unmarshal(got.Body.Bytes(), &handoff)
	target, err := url.Parse(handoff["url"])
	if err != nil || target.Host != "toolyard.example" || target.Path != "/connections/handoff" {
		t.Fatal("handoff destination")
	}
	got = call("", http.MethodGet, target.RequestURI(), nil)
	if got.Code != 303 || got.Header().Get("Location") != "/#inbox" || len(got.Result().Cookies()) == 0 {
		t.Fatal("handoff consume", got.Code)
	}
	got = call("", http.MethodGet, target.RequestURI(), nil)
	if got.Code != 401 {
		t.Fatal("handoff replay", got.Code)
	}
	got = call(credential.Token, http.MethodPost, "/v1/connections/revoke", map[string]string{})
	if got.Code != 200 {
		t.Fatal("revoke", got.Code)
	}
	got = call(credential.Token, http.MethodGet, "/v1/callbacks/receivers/"+receiver.ID+"/events", nil)
	if got.Code != 401 {
		t.Fatal("revoked pulled", got.Code)
	}
	for _, path := range []string{"/v1/connections/api-key", "/v1/connections/renew", "/v1/connections/handoff", "/v1/connections/revoke"} {
		if !operatorNeverPaths[path] || !exemptFromOriginCheck(path) || unauthRouteLimit(path) == nil {
			t.Fatal("route guards missing", path)
		}
	}
}
