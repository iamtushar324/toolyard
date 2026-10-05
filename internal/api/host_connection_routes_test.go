package api

import (
	"bytes"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"
	"github.com/google/uuid"
	"github.com/tusharbhardwaj/toolyard/internal/federation"
	"github.com/tusharbhardwaj/toolyard/internal/sealbox"
)

func TestHostRoutesExplicitConsentAndMemberManagement(t *testing.T) {
	f := newInboxAPIFixture(t)
	cipher, _ := sealbox.NewCipher(make([]byte, 32))
	fed, e := federation.New(t.Context(), f.db, f.srv.identity, cipher)
	if e != nil {
		t.Fatal(e)
	}
	f.srv.federation = fed
	f.srv.security.PublicURL = "https://toolyard.example"
	handler := f.srv.SecurityHeaders(f.srv.EnforceOriginOnMutations(f.srv.HardenAPI(f.srv.RoleGuard(f.mux))))
	pub, key, _ := ed25519.GenerateKey(rand.Reader)
	request := uuid.NewString()
	proof := func(action string) string {
		now := time.Now()
		c := federation.HostClaims{RegisteredClaims: jwt.RegisteredClaims{Issuer: "mac-api", Subject: "local-user", Audience: jwt.ClaimStrings{fed.InstanceID}, ID: uuid.NewString(), IssuedAt: jwt.NewNumericDate(now), ExpiresAt: jwt.NewNumericDate(now.Add(time.Minute))}, Action: action, RequestID: request, PublicKey: base64.StdEncoding.EncodeToString(pub), HostName: "My Mac", Platform: "darwin"}
		raw, e := jwt.NewWithClaims(jwt.SigningMethodEdDSA, c).SignedString(key)
		if e != nil {
			t.Fatal(e)
		}
		return raw
	}
	call := func(action string) *httptest.ResponseRecorder {
		raw, _ := json.Marshal(map[string]string{"proof": proof(action)})
		req := httptest.NewRequest("POST", "/v1/connections/host/"+action, bytes.NewReader(raw))
		req.Header.Set("Content-Type", "application/json")
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, req)
		return rec
	}
	begin := call("begin")
	if begin.Code != 200 {
		t.Fatal("begin", begin.Code, begin.Body.String())
	}
	var b map[string]string
	json.Unmarshal(begin.Body.Bytes(), &b)
	target, _ := url.Parse(b["authorization_url"])
	if target.Path != "/connections/authorize" {
		t.Fatal("wrong destination")
	}
	pageReq := httptest.NewRequest("GET", target.RequestURI(), nil)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, pageReq)
	if rec.Code != 401 || !strings.Contains(rec.Body.String(), "/login?host_request=") {
		t.Fatal("anonymous consent", rec.Code)
	}
	pageReq = httptest.NewRequest("GET", target.RequestURI(), nil)
	pageReq.AddCookie(f.cookie)
	rec = httptest.NewRecorder()
	handler.ServeHTTP(rec, pageReq)
	if rec.Code != 200 {
		t.Fatal("consent page", rec.Code, rec.Body.String())
	}
	if strings.Contains(rec.Body.String(), "ag_") {
		t.Fatal("credential in browser")
	}
	var nonceCookie *http.Cookie
	for _, c := range rec.Result().Cookies() {
		if strings.Contains(c.Name, "ty_host_") {
			nonceCookie = c
		}
	}
	matches := regexp.MustCompile(`name="nonce" value="([^"]+)"`).FindStringSubmatch(rec.Body.String())
	if len(matches) != 2 || nonceCookie == nil {
		t.Fatal("nonce missing")
	}
	before := call("poll")
	if before.Code != 200 || !strings.Contains(before.Body.String(), `"status":"pending"`) {
		t.Fatal("GET granted permission", before.Code)
	}
	submit := func(nonce, origin string) *httptest.ResponseRecorder {
		form := url.Values{"nonce": {nonce}, "verdict": {"accept"}}
		req := httptest.NewRequest("POST", target.RequestURI(), strings.NewReader(form.Encode()))
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		req.Header.Set("Origin", origin)
		req.Header.Set("Sec-Fetch-Site", "same-origin")
		req.AddCookie(f.cookie)
		req.AddCookie(nonceCookie)
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, req)
		return rec
	}
	if bad := submit("wrong", "https://toolyard.example"); bad.Code != 403 {
		t.Fatal("bad nonce", bad.Code)
	}
	if bad := submit(matches[1], "https://evil.example"); bad.Code != 403 {
		t.Fatal("cross origin", bad.Code)
	}
	if good := submit(matches[1], "https://toolyard.example"); good.Code != 200 || !strings.Contains(good.Body.String(), "approved") {
		t.Fatal("submit", good.Code, good.Body.String())
	}
	approved := call("poll")
	if approved.Code != 200 {
		t.Fatal("claim", approved.Code, approved.Body.String())
	}
	var cred struct {
		federation.Credential
		Status           string `json:"status"`
		RequestExpiresAt string `json:"request_expires_at"`
	}
	if e = json.Unmarshal(approved.Body.Bytes(), &cred); e != nil || cred.Token == "" || cred.Status != "approved" {
		t.Fatal("credential result", e)
	}
	requestDeadline, parseErr := time.Parse(time.RFC3339, cred.RequestExpiresAt)
	if parseErr != nil {
		t.Fatal("missing request deadline", parseErr)
	}
	leaseDeadline, parseErr := time.Parse(time.RFC3339, cred.ExpiresAt)
	if parseErr != nil || leaseDeadline.Sub(requestDeadline) < 29*24*time.Hour {
		t.Fatal("request deadline collided with credential lease", cred.RequestExpiresAt, cred.ExpiresAt, parseErr)
	}
	if cred.RequestExpiresAt != b["expires_at"] {
		t.Fatal("request deadline changed after approval")
	}
	owner, _ := f.srv.identity.VerifyAgentToken(t.Context(), f.token)
	f.db.Exec(`UPDATE users SET role='member' WHERE id=?`, owner.Owner)
	req := httptest.NewRequest("GET", "/v1/connections/hosts", nil)
	req.AddCookie(f.cookie)
	rec = httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != 200 || !strings.Contains(rec.Body.String(), cred.AgentID) {
		t.Fatal("member own hosts", rec.Code, rec.Body.String())
	}
	req = httptest.NewRequest("GET", "/v1/connections/hosts?all=true", nil)
	req.AddCookie(f.cookie)
	rec = httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != 403 {
		t.Fatal("member all hosts", rec.Code)
	}
	req = httptest.NewRequest("POST", "/v1/connections/hosts/"+cred.AgentID+"/revoke", nil)
	req.AddCookie(f.cookie)
	req.Header.Set("Origin", "https://toolyard.example")
	rec = httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != 403 {
		t.Fatal("management csrf", rec.Code)
	}
	req.Header.Set("X-Requested-With", "toolyard")
	rec = httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != 200 {
		t.Fatal("member revoke", rec.Code, rec.Body.String())
	}
	if _, e = fed.VerifyAgent(t.Context(), cred.Token); e == nil {
		t.Fatal("revoked host still authenticates")
	}
}
