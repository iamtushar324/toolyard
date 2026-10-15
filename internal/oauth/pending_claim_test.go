package oauth

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// raceN runs fn(0..n-1) in n goroutines released together and returns
// each call's error by index.
func raceN(n int, fn func(i int) error) []error {
	errs := make([]error, n)
	barrier := make(chan struct{})
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-barrier
			errs[i] = fn(i)
		}(i)
	}
	close(barrier)
	wg.Wait()
	return errs
}

// A callback state claimed several times at once is handed out exactly
// once; the rest find nothing, and the row is gone.
func TestClaimPendingOnce(t *testing.T) {
	f := newUserFixture(t)
	ctx := context.Background()
	_, state, err := f.svc.BeginCallback(ctx, "shared", "u_ada", ModeCallback, nil, false)
	if err != nil {
		t.Fatal(err)
	}
	const n = 6
	got := make([]*PendingRecord, n)
	errs := raceN(n, func(i int) error {
		p, err := f.svc.ClaimPending(ctx, state, ModeCallback)
		got[i] = p
		return err
	})
	won := 0
	for i, err := range errs {
		switch {
		case err == nil:
			won++
			if got[i].State != state || got[i].UpstreamName != "shared" || got[i].UserID != "u_ada" ||
				got[i].Mode != ModeCallback || got[i].CodeVerifier == "" {
				t.Errorf("claimed record = %+v", got[i])
			}
		case errors.Is(err, ErrPendingNotFound):
		default:
			t.Fatalf("claim %d: unexpected error %v", i, err)
		}
	}
	if won != 1 {
		t.Fatalf("%d claims won, want exactly 1", won)
	}
	if _, err := f.svc.LoadPending(ctx, state); !errors.Is(err, ErrPendingNotFound) {
		t.Errorf("row after the claim: %v, want ErrPendingNotFound", err)
	}
}

// A paste-mode flow is not claimed by the callback route: it reports the
// mismatch and leaves the row for the paste route.
func TestClaimPendingWrongModeLeavesRow(t *testing.T) {
	f := newUserFixture(t)
	ctx := context.Background()
	_, state, err := f.svc.BeginCallback(ctx, "shared", "u_ada", ModePaste, nil, false)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.svc.ClaimPending(ctx, state, ModeCallback); !errors.Is(err, ErrFlowMismatch) {
		t.Fatalf("claim of a paste flow: %v, want ErrFlowMismatch", err)
	}
	if p, err := f.svc.LoadPending(ctx, state); err != nil || p.Mode != ModePaste {
		t.Fatalf("paste flow after the refused claim: %+v, %v", p, err)
	}
	if _, err := f.svc.ClaimPending(ctx, "no-such-state", ModeCallback); !errors.Is(err, ErrPendingNotFound) {
		t.Fatalf("unknown state: %v, want ErrPendingNotFound", err)
	}
}

// An expired flow is consumed and reported as not found.
func TestClaimPendingExpired(t *testing.T) {
	f := newUserFixture(t)
	ctx := context.Background()
	_, state, err := f.svc.BeginCallback(ctx, "shared", "u_ada", ModeCallback, nil, false)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.db.Exec(`UPDATE oauth_pending SET expires_at = ? WHERE state = ?`,
		time.Now().Add(-time.Minute).UnixMilli(), state); err != nil {
		t.Fatal(err)
	}
	if _, err := f.svc.ClaimPending(ctx, state, ModeCallback); !errors.Is(err, ErrPendingNotFound) {
		t.Fatalf("expired claim: %v, want ErrPendingNotFound", err)
	}
	var n int
	if err := f.db.QueryRow(`SELECT count(*) FROM oauth_pending WHERE state = ?`, state).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 0 {
		t.Errorf("expired row still present")
	}
}

// Several device-flow polls that all get a token from the provider store
// it once: only the poll that removes the pending row wins.
func TestPollDeviceStoresOnce(t *testing.T) {
	f := newUserFixture(t)
	ctx := context.Background()
	var tokenCalls atomic.Int32
	mux := http.NewServeMux()
	mux.HandleFunc("/device", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"device_code": "dc-1", "user_code": "ABCD-EFGH", "verification_uri": "https://idp.example/device",
			"expires_in": 600, "interval": 1,
		})
	})
	mux.HandleFunc("/token", func(w http.ResponseWriter, r *http.Request) {
		tokenCalls.Add(1)
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"access_token": "at-device", "refresh_token": "rt-device", "token_type": "Bearer", "expires_in": 3600,
		})
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	if err := f.svc.PutClient(ctx, ClientRecord{
		UpstreamName: "shared", Issuer: srv.URL, AuthorizationEndpoint: srv.URL + "/authorize",
		TokenEndpoint: srv.URL + "/token", DeviceAuthorizationEndpoint: srv.URL + "/device",
		ClientID: "cid", RedirectURI: "https://toolyard.example/v1/mcp-oauth/callback",
		TokenEndpointAuthMethod: "none",
	}); err != nil {
		t.Fatal(err)
	}
	p, err := f.svc.BeginDevice(ctx, "shared", "u_ada", nil)
	if err != nil {
		t.Fatal(err)
	}
	const n = 4
	recs := make([]*TokenRecord, n)
	errs := raceN(n, func(i int) error {
		rec, err := f.svc.PollDevice(ctx, p.State)
		recs[i] = rec
		return err
	})
	won := 0
	for i, err := range errs {
		switch {
		case err == nil && recs[i] != nil:
			won++
		case errors.Is(err, ErrPendingNotFound):
		default:
			t.Fatalf("poll %d: rec=%v err=%v", i, recs[i], err)
		}
	}
	if won != 1 {
		t.Fatalf("%d polls stored a token, want exactly 1 (provider answered %d)", won, tokenCalls.Load())
	}
	tok, err := f.svc.GetToken(ctx, "shared")
	if err != nil || tok == nil || tok.AccessToken != "at-device" {
		t.Fatalf("stored token: %+v, %v", tok, err)
	}
	if _, err := f.svc.LoadPending(ctx, p.State); !errors.Is(err, ErrPendingNotFound) {
		t.Errorf("device flow still pending after the win: %v", err)
	}
}
