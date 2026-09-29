package api

import (
	"context"
	"net/http"
	"testing"

	"github.com/tusharbhardwaj/toolyard/internal/push"
)

// PushSubscription.toJSON() always includes expirationTime: null in
// Safari, and a number where the push service sets one. Both must be
// accepted, or "Enable push" fails on iPhone.
func TestPushSubscribeAcceptsBrowserPayload(t *testing.T) {
	e := newAccessTestServer(t)
	ps, err := push.New(context.Background(), e.db, "ops@example.com")
	if err != nil {
		t.Fatal(err)
	}
	e.srv.push = ps
	admin := e.cookieFor(t, e.admin.ID)

	for name, body := range map[string]string{
		"safari null": `{"endpoint":"https://web.push.apple.com/QGx1","keys":{"p256dh":"BNx","auth":"aZ"},"expirationTime":null}`,
		"number":      `{"endpoint":"https://fcm.googleapis.com/fcm/send/x1","keys":{"p256dh":"BNy","auth":"bZ"},"expirationTime":1790781142651}`,
	} {
		rec := e.do(t, admin, http.MethodPost, "/v1/push/subscribe", body)
		if rec.Code != http.StatusOK {
			t.Errorf("%s: %d %s", name, rec.Code, rec.Body.String())
		}
	}
	subs, err := ps.ListForUser(context.Background(), e.admin.ID)
	if err != nil || len(subs) != 2 {
		t.Fatalf("subs = %d, %v", len(subs), err)
	}

	// Other unknown fields are still refused.
	rec := e.do(t, admin, http.MethodPost, "/v1/push/subscribe", `{"endpoint":"https://x.example/1","keys":{"p256dh":"a","auth":"b"},"extra":1}`)
	if rec.Code != http.StatusBadRequest {
		t.Errorf("unknown field: %d, want 400", rec.Code)
	}
}
