package upstreams

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/tusharbhardwaj/toolyard/internal/jobs"
	"github.com/tusharbhardwaj/toolyard/internal/store/storetest"
)

func TestAsyncSettingIsStoredAndReadWithoutReconnecting(t *testing.T) {
	db := storetest.Open(t)
	ctx := context.Background()
	now := time.Now().UnixMilli()
	if _, err := db.ExecContext(ctx, `INSERT INTO upstream_servers(name, transport, url, args_json, env_json, headers_json, identity_json, auth_mode, enabled, created_at, updated_at)
        VALUES(?, 'http', 'http://127.0.0.1:1/mcp', '[]', '{}', '{}', NULL, 'shared', 1, ?, ?)`, "BkCore", now, now); err != nil {
		t.Fatal(err)
	}
	svc := New(db, nil)

	if got := svc.AsyncSetting("BkCore"); !got.IsZero() {
		t.Fatalf("new server is async: %+v", got)
	}
	set, err := svc.SetAsync(ctx, "BkCore", jobs.AsyncSetting{Mode: jobs.AsyncAfter, AfterMs: 3000,
		Tools: map[string]jobs.AsyncRule{"create_report": {Mode: jobs.AsyncAlways}}})
	if err != nil || set == nil || set.Mode != jobs.AsyncAfter {
		t.Fatalf("SetAsync = %+v %v", set, err)
	}
	if r := svc.AsyncSetting("BkCore").RuleFor("create_report"); r.Mode != jobs.AsyncAlways {
		t.Fatalf("tool rule = %+v", r)
	}
	if r := svc.AsyncSetting("BkCore").RuleFor("get_client"); r.Mode != jobs.AsyncAfter || r.AfterMs != 3000 {
		t.Fatalf("server rule = %+v", r)
	}
	// Another replica (a second service on the same database) reads it.
	other := New(db, nil)
	if r := other.AsyncSetting("BkCore").RuleFor("create_report"); r.Mode != jobs.AsyncAlways {
		t.Fatalf("another replica reads %+v", r)
	}
	list, err := svc.List(ctx)
	if err != nil || len(list) != 1 || list[0].Async == nil || list[0].Async.Mode != jobs.AsyncAfter {
		t.Fatalf("List = %+v %v", list, err)
	}
	srv, err := svc.Get(ctx, "BkCore")
	if err != nil || srv.Async == nil || srv.Async.Tools["create_report"].Mode != jobs.AsyncAlways {
		t.Fatalf("Get = %+v %v", srv, err)
	}

	if _, err := svc.SetAsync(ctx, "BkCore", jobs.AsyncSetting{Mode: "sometimes"}); !errors.Is(err, ErrInvalid) {
		t.Fatalf("invalid mode = %v", err)
	}
	if _, err := svc.SetAsync(ctx, "missing", jobs.AsyncSetting{Mode: jobs.AsyncAlways}); !errors.Is(err, ErrNotFound) {
		t.Fatalf("unknown server = %v", err)
	}
	// Off with no tool rules clears it.
	cleared, err := svc.SetAsync(ctx, "BkCore", jobs.AsyncSetting{Mode: jobs.AsyncOff})
	if err != nil || cleared != nil || !svc.AsyncSetting("BkCore").IsZero() {
		t.Fatalf("clear = %+v %v", cleared, err)
	}
	var raw *string
	if err := db.QueryRowContext(ctx, `SELECT async_json FROM upstream_servers WHERE name = ?`, "BkCore").Scan(&raw); err != nil || raw != nil {
		t.Fatalf("cleared column = %v %v", raw, err)
	}
}

// A change another replica saved applies here as soon as its cluster
// message arrives (refreshAsync), not only after the cache TTL.
func TestAsyncSettingFollowsAnotherReplicaAtOnce(t *testing.T) {
	db := storetest.Open(t)
	ctx := context.Background()
	now := time.Now().UnixMilli()
	if _, err := db.ExecContext(ctx, `INSERT INTO upstream_servers(name, transport, url, args_json, env_json, headers_json, identity_json, auth_mode, enabled, created_at, updated_at)
        VALUES(?, 'http', 'http://127.0.0.1:1/mcp', '[]', '{}', '{}', NULL, 'shared', 1, ?, ?)`, "BkCore", now, now); err != nil {
		t.Fatal(err)
	}
	here, there := New(db, nil), New(db, nil)
	if got := here.AsyncSetting("BkCore"); !got.IsZero() {
		t.Fatalf("new server is async: %+v", got)
	}
	if _, err := there.SetAsync(ctx, "BkCore", jobs.AsyncSetting{Mode: jobs.AsyncAlways}); err != nil {
		t.Fatal(err)
	}
	here.refreshAsync()
	if r := here.AsyncSetting("BkCore").RuleFor("get_client"); r.Mode != jobs.AsyncAlways {
		t.Fatalf("after the message, this replica reads %+v", r)
	}
}
