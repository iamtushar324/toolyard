package settings

import "testing"

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
