package settings

import (
	"context"
	"path/filepath"
	"strings"
	"testing"

	"github.com/tusharbhardwaj/toolyard/internal/store"
)

func TestNormaliseInboxAttention(t *testing.T) {
	out := normalise(map[string]any{
		InboxNowPerHour:  float64(500),
		InboxQuietHours:  "22:00-07:00",
		InboxDigestTimes: "09:30, 25:00, 18:30,nonsense",
		InboxTimezone:    "Europe/Berlin",
		InboxQuietAllow:  " deploy.*, pagerduty.page ",
	})
	if out[InboxNowPerHour] != 60 || out[InboxQuietHours] != "22:00-07:00" || out[InboxDigestTimes] != "09:30,18:30" ||
		out[InboxTimezone] != "Europe/Berlin" || out[InboxQuietAllow] != "deploy.*, pagerduty.page" {
		t.Fatalf("normalised: %v", out)
	}
	bad := normalise(map[string]any{InboxNowPerHour: float64(0), InboxQuietHours: "22:00-22:00", InboxTimezone: "Mars/Olympus"})
	if bad[InboxNowPerHour] != 1 {
		t.Errorf("now budget below 1 not clamped: %v", bad[InboxNowPerHour])
	}
	if _, ok := bad[InboxQuietHours]; ok {
		t.Errorf("an empty quiet range was kept: %v", bad[InboxQuietHours])
	}
	if _, ok := bad[InboxTimezone]; ok {
		t.Errorf("an unknown time zone was kept: %v", bad[InboxTimezone])
	}
	if off := normalise(map[string]any{InboxQuietHours: "", InboxDigestTimes: ""}); off[InboxQuietHours] != "" || off[InboxDigestTimes] != "" {
		t.Errorf("turning quiet hours and digests off: %v", off)
	}
}

// A retired key's stored row stays as it is, but no settings response
// carries it and Patch refuses it.
func TestRetiredKeysStayHidden(t *testing.T) {
	ctx := context.Background()
	db, err := store.Open(filepath.Join(t.TempDir(), "settings.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if _, err := db.Exec(`INSERT INTO system_settings(key, value, updated_at) VALUES('clickhouse_password', '"stored-value"', 0)`); err != nil {
		t.Fatal(err)
	}
	s, err := New(ctx, db)
	if err != nil {
		t.Fatal(err)
	}
	all, err := s.All(ctx)
	if err != nil {
		t.Fatal(err)
	}
	for k := range all {
		if strings.HasPrefix(k, "clickhouse_password") {
			t.Errorf("All returned the retired key %q", k)
		}
	}
	if err := s.Patch(ctx, map[string]any{"clickhouse_password": "new"}); err == nil {
		t.Error("Patch accepted a retired key")
	}
	if v := s.GetString("clickhouse_password", ""); v != "stored-value" {
		t.Errorf("stored row changed: %q", v)
	}
}
