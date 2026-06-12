package oauth

import (
	"testing"
	"time"
)

// TestBackoffSatisfiedAttemptBased verifies that after a failed attempt the
// next retry is gated on the attempt time, not just the last success. A
// failing IdP advances lastAttempt every tick, so backoff must space retries
// out from the most recent attempt.
func TestBackoffSatisfiedAttemptBased(t *testing.T) {
	base := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	// One consecutive failure -> 1m backoff (see refresherBackoff).
	const failures = 1
	want := refresherBackoff(failures) // 1m
	if want != time.Minute {
		t.Fatalf("test assumes 1m backoff for 1 failure, got %v", want)
	}

	lastSuccess := base // last successful refresh
	// We just attempted (and failed) at base+5s — far more recent than the
	// last success, which itself is old enough that a success-only gate would
	// already permit a retry.
	lastAttempt := base.Add(5 * time.Second)

	// 30s after the failed attempt (a single tick later): NOT enough time has
	// elapsed since the attempt, so a retry must be blocked even though the
	// last *success* is older than the backoff window.
	now := lastAttempt.Add(30 * time.Second)
	if backoffSatisfied(now, lastSuccess, lastAttempt, failures) {
		t.Errorf("retry allowed only 30s after failed attempt; want blocked until %v elapsed", want)
	}

	// Once the full backoff has elapsed since the attempt, retry is allowed.
	now = lastAttempt.Add(want)
	if !backoffSatisfied(now, lastSuccess, lastAttempt, failures) {
		t.Errorf("retry blocked after full backoff elapsed since attempt")
	}
}

// TestBackoffSatisfiedSuccessPath verifies the gate still respects the last
// success time when no later attempt has been recorded, and that zero backoff
// (no failures) always permits a refresh.
func TestBackoffSatisfiedSuccessPath(t *testing.T) {
	base := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)

	// No failures -> no backoff -> always satisfied.
	if !backoffSatisfied(base, base, time.Time{}, 0) {
		t.Errorf("zero-failure backoff should always be satisfied")
	}

	// With failures but no recorded attempt, the gate falls back to lastSuccess.
	want := refresherBackoff(2) // 5m
	if backoffSatisfied(base.Add(want-time.Second), base, time.Time{}, 2) {
		t.Errorf("retry allowed before backoff elapsed since last success")
	}
	if !backoffSatisfied(base.Add(want), base, time.Time{}, 2) {
		t.Errorf("retry blocked after backoff elapsed since last success")
	}
}

// TestRecordAndReadAttempt exercises the in-memory attempt tracking.
func TestRecordAndReadAttempt(t *testing.T) {
	s := &Service{lastAttempt: map[string]time.Time{}}

	if got := s.lastAttemptAt("up"); !got.IsZero() {
		t.Errorf("unrecorded upstream should return zero time, got %v", got)
	}

	when := time.Date(2026, 6, 10, 12, 0, 0, 0, time.UTC)
	s.recordAttempt("up", when)
	if got := s.lastAttemptAt("up"); !got.Equal(when) {
		t.Errorf("lastAttemptAt = %v, want %v", got, when)
	}
}
