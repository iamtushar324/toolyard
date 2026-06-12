package gateway

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"
)

func TestUpstreamBackoffSchedule(t *testing.T) {
	cases := []struct {
		failures int
		want     time.Duration
	}{
		{0, 0},
		{1, 10 * time.Second},
		{2, 30 * time.Second},
		{3, 2 * time.Minute},
		{4, 10 * time.Minute},
		{5, 30 * time.Minute},
		{6, 30 * time.Minute},
		{100, 30 * time.Minute},
	}
	for _, c := range cases {
		if got := upstreamBackoff(c.failures); got != c.want {
			t.Errorf("upstreamBackoff(%d) = %s, want %s", c.failures, got, c.want)
		}
	}
}

// TestResumeFailFastAfterDialFailure verifies the circuit breaker: the first
// resume against a bogus command fails (and arms the breaker); the second
// resume returns immediately with a backoff error WITHOUT attempting to dial.
func TestResumeFailFastAfterDialFailure(t *testing.T) {
	u := &upstream{cfg: UpstreamConfig{
		Name:      "bogus",
		Transport: "stdio",
		Command:   "/nonexistent/toolyard-bogus-cmd-xyz",
	}}

	ctx := context.Background()
	if err := u.resume(ctx); err == nil {
		t.Fatal("first resume against bogus command: want error, got nil")
	}
	u.mu.Lock()
	failures := u.consecutiveFailures
	nextRetry := u.nextRetryAt
	u.mu.Unlock()
	if failures != 1 {
		t.Fatalf("consecutiveFailures = %d, want 1", failures)
	}
	if nextRetry.IsZero() || time.Until(nextRetry) <= 0 {
		t.Fatalf("nextRetryAt not armed: %v", nextRetry)
	}

	// Second resume must fail fast (no dial attempt) — measure it returns
	// near-instantly and reports the backoff window.
	start := time.Now()
	err := u.resume(ctx)
	elapsed := time.Since(start)
	if err == nil {
		t.Fatal("second resume during backoff: want error, got nil")
	}
	if !strings.Contains(err.Error(), "in backoff") {
		t.Errorf("second resume error = %q, want it to mention backoff", err)
	}
	if elapsed > 100*time.Millisecond {
		t.Errorf("second resume took %s; expected fail-fast (no dial)", elapsed)
	}
	// The breaker must not count a fail-fast as another dial failure.
	u.mu.Lock()
	failures = u.consecutiveFailures
	u.mu.Unlock()
	if failures != 1 {
		t.Errorf("consecutiveFailures after fail-fast = %d, want 1", failures)
	}
}

// blockingCloser blocks in Close until release is closed.
type blockingCloser struct{ release chan struct{} }

func (b blockingCloser) Close() error { <-b.release; return nil }

// TestCloseWithTimeoutAbandonsHungCloser verifies a wedged child can't block
// the caller: closeWithTimeout returns within the timeout even though Close
// never completes.
func TestCloseWithTimeoutAbandonsHungCloser(t *testing.T) {
	bc := blockingCloser{release: make(chan struct{})}
	defer close(bc.release) // let the abandoned goroutine exit at test end

	done := make(chan struct{})
	go func() {
		closeWithTimeout("hung", bc, 50*time.Millisecond)
		close(done)
	}()

	select {
	case <-done:
		// returned despite the closer still blocking — correct.
	case <-time.After(2 * time.Second):
		t.Fatal("closeWithTimeout blocked on a hung closer; timeout not enforced")
	}
}

// TestNewUpstreamEnvFuncErrorFailsDial verifies a secret-resolution failure
// (EnvFunc returning an error) aborts the stdio dial before any subprocess is
// started, surfacing the resolution error.
func TestNewUpstreamEnvFuncErrorFailsDial(t *testing.T) {
	cfg := UpstreamConfig{
		Name:      "needs-secret",
		Transport: "stdio",
		Command:   "echo",
		EnvFunc: func(ctx context.Context) (map[string]string, error) {
			return nil, errors.New("secret \"API_KEY\": not found")
		},
	}
	_, err := newUpstream(context.Background(), cfg)
	if err == nil {
		t.Fatal("expected dial to fail when EnvFunc errors")
	}
	if !strings.Contains(err.Error(), "API_KEY") {
		t.Fatalf("error should name the missing secret, got: %v", err)
	}
}
