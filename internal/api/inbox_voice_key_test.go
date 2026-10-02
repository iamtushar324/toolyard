package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"github.com/tusharbhardwaj/toolyard/internal/settings"
	"github.com/tusharbhardwaj/toolyard/internal/store"
)

func TestInboxVoiceKeyOwnerOnlyAndMasked(t *testing.T) {
	f := newInboxAPIFixture(t)
	db, err := store.Open(filepath.Join(t.TempDir(), "settings.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	f.srv.settings, err = settings.New(context.Background(), db)
	if err != nil {
		t.Fatal(err)
	}
	key := "test-elevenlabs-secret"
	code, out := f.owner(t, "POST", "/v1/inbox/voice-key", map[string]string{"api_key": key})
	if code != 200 || out["elevenlabs_api_key_present"] != true {
		t.Fatalf("save: %d %v", code, out)
	}
	code, out = f.owner(t, "GET", "/v1/settings", nil)
	body, _ := json.Marshal(out)
	if code != 200 || out["elevenlabs_api_key_present"] != true || strings.Contains(string(body), key) || out["elevenlabs_api_key"] != nil {
		t.Fatalf("key leaked: %d", code)
	}
	if f.srv.settings.GetString(settings.ElevenLabsAPIKey, "") != key {
		t.Fatal("key not stored")
	}
	if code, _ := f.owner(t, "PATCH", "/v1/settings", map[string]string{"elevenlabs_api_key": "bad"}); code != 400 {
		t.Fatalf("generic secret patch allowed: %d", code)
	}
	for _, bad := range []string{"", "a\nb", strings.Repeat("x", 1025)} {
		if code, _ := f.owner(t, "POST", "/v1/inbox/voice-key", map[string]string{"api_key": bad}); code != 400 {
			t.Fatalf("bad key accepted: %d", code)
		}
	}
	r := httptest.NewRequest("POST", "/v1/inbox/voice-key", strings.NewReader(`{"api_key":"bad"}`))
	r.Header.Set("Authorization", "Bearer "+f.token)
	w := httptest.NewRecorder()
	f.mux.ServeHTTP(w, r)
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("agent key allowed: %d", w.Code)
	}
}

func TestInboxVoiceKeyRejectsOperatorTokens(t *testing.T) {
	e := newOperatorEnv(t)
	for _, scope := range []string{"write", "owner"} {
		w := e.do(t, e.token(t, scope), "POST", "/v1/inbox/voice-key", `{"api_key":"bad"}`)
		if w.Code == 200 {
			t.Fatal("operator token accepted voice key")
		}
	}
}

func TestInboxVoiceKeyRejectsMember(t *testing.T) {
	f := newInboxAPIFixture(t)
	_, err := f.srv.identity.CreateUser(context.Background(), "member", "long-member-password")
	if err != nil {
		t.Fatal(err)
	}
	r := httptest.NewRequest("POST", "/v1/auth/login", strings.NewReader(`{"Username":"member","Password":"long-member-password"}`))
	w := httptest.NewRecorder()
	f.mux.ServeHTTP(w, r)
	cookies := w.Result().Cookies()
	if len(cookies) == 0 {
		t.Fatalf("member login failed: %d", w.Code)
	}
	r = httptest.NewRequest("POST", "/v1/inbox/voice-key", strings.NewReader(`{"api_key":"bad"}`))
	for _, cookie := range cookies {
		r.AddCookie(cookie)
	}
	w = httptest.NewRecorder()
	f.mux.ServeHTTP(w, r)
	if w.Code != http.StatusForbidden {
		t.Fatalf("member credential write: %d", w.Code)
	}
}
