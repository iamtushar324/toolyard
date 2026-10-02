package main

import (
	"strings"
	"testing"
)

func TestConnectAZP(t *testing.T) {
	const public = "https://toolyard.dev.beknown.live"
	got, err := connectAZP(" HTTPS://StageBKT3.dev.beknown.live/ ,https://bkt3.beknown.live,https://stagebkt3.dev.beknown.live", public, true)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Join(got, ",") != "https://stagebkt3.dev.beknown.live,https://bkt3.beknown.live" {
		t.Errorf("origins = %q", got)
	}

	// Empty is off, with or without Clerk.
	for _, raw := range []string{"", " ", ", ,"} {
		if got, err := connectAZP(raw, public, false); err != nil || got != nil {
			t.Errorf("connectAZP(%q) = %q, %v; want off", raw, got, err)
		}
	}

	bad := []struct{ raw, public string }{
		{"stagebkt3.dev.beknown.live", public},         // no scheme
		{"https://bkt3.example.com/app", public},       // path
		{"https://bkt3.example.com?x=1", public},       // query
		{"ftp://bkt3.example.com", public},             // scheme
		{"https://user@bkt3.example.com", public},      // credentials
		{"https://", public},                           // no host
		{"https://Toolyard.dev.beknown.live/", public}, // the dashboard itself
		{"http://stagebkt3.dev.beknown.live", public},  // plain http off loopback
		{"http://10.0.0.5:3000", public},               // private, not loopback
		{"http://localhost.evil.example", public},      // not localhost
	}
	for _, c := range bad {
		if got, err := connectAZP(c.raw, c.public, true); err == nil {
			t.Errorf("connectAZP(%q) = %q, want error", c.raw, got)
		}
	}
	// Plain http only on loopback, for local testing.
	got, err = connectAZP("http://localhost:3773,http://127.0.0.1:3773,http://[::1]:3773,HTTP://LocalHost:5173/", public, true)
	if err != nil {
		t.Fatalf("loopback http origins: %v", err)
	}
	if strings.Join(got, ",") != "http://localhost:3773,http://127.0.0.1:3773,http://[::1]:3773,http://localhost:5173" {
		t.Errorf("loopback origins = %q", got)
	}

	if _, err := connectAZP("https://bkt3.example.com", public, false); err == nil {
		t.Error("connect origins without Clerk accepted")
	}
}
