package upstreams

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/mark3labs/mcp-go/mcp"
	"github.com/mark3labs/mcp-go/server"
	"github.com/mark3labs/mcp-go/server/servertest"
)

// TestDescriptionRoundTrip: Add stores the admin's description (0077),
// Get and List read it back, Update changes and clears it, and the
// gateway sees each value.
func TestDescriptionRoundTrip(t *testing.T) {
	ctx := context.Background()
	svc, _, gw := newServiceFixture(t)
	ts := newMCPTestServer(t)

	added, err := svc.Add(ctx, Server{Name: "bkt3_ds1", Transport: "http", URL: ts.URL,
		Description: "  bk t3 on the production server DS1  "})
	if err != nil {
		t.Fatalf("Add: %v", err)
	}
	if added.Description != "bk t3 on the production server DS1" {
		t.Fatalf("Add description = %q", added.Description)
	}
	got, err := svc.Get(ctx, "bkt3_ds1")
	if err != nil || got.Description != "bk t3 on the production server DS1" {
		t.Fatalf("Get = %+v, %v", got, err)
	}
	all, err := svc.List(ctx)
	if err != nil || len(all) != 1 || all[0].Description != "bk t3 on the production server DS1" {
		t.Fatalf("List = %+v, %v", all, err)
	}
	if d := gw.ServerDescription("bkt3_ds1"); d != "bk t3 on the production server DS1" {
		t.Fatalf("gateway description = %q", d)
	}

	upd, err := svc.Update(ctx, "bkt3_ds1", Patch{Description: strp("bk t3 on DS1, the production server")})
	if err != nil || upd.Description != "bk t3 on DS1, the production server" {
		t.Fatalf("Update = %+v, %v", upd, err)
	}
	if d := gw.ServerDescription("bkt3_ds1"); d != "bk t3 on DS1, the production server" {
		t.Fatalf("gateway description after update = %q", d)
	}
	// A description-only edit keeps the server connected.
	if n := gw.UpstreamToolCount("bkt3_ds1"); n != 1 {
		t.Fatalf("tool count after description edit = %d, want 1", n)
	}

	// An edit of another field keeps the description.
	upd, err = svc.Update(ctx, "bkt3_ds1", Patch{Enabled: boolp(true)})
	if err != nil || upd.Description != "bk t3 on DS1, the production server" {
		t.Fatalf("Update enabled = %+v, %v", upd, err)
	}

	upd, err = svc.Update(ctx, "bkt3_ds1", Patch{Description: strp("")})
	if err != nil || upd.Description != "" {
		t.Fatalf("clear = %+v, %v", upd, err)
	}
	if d := gw.ServerDescription("bkt3_ds1"); d != "" {
		t.Fatalf("gateway description after clear = %q, want none (the test server sends no instructions)", d)
	}
}

// TestDescriptionCapped: more than MaxDescription characters is refused at
// Add and at Update, and nothing changes.
func TestDescriptionCapped(t *testing.T) {
	ctx := context.Background()
	svc, _, _ := newServiceFixture(t)
	ts := newMCPTestServer(t)
	long := strings.Repeat("é", MaxDescription+1)

	if _, err := svc.Add(ctx, Server{Name: "long", Transport: "http", URL: ts.URL, Description: long}); !errors.Is(err, ErrInvalid) {
		t.Fatalf("Add long: err = %v, want ErrInvalid", err)
	}
	if _, err := svc.Add(ctx, Server{Name: "ok", Transport: "http", URL: ts.URL, Description: strings.Repeat("é", MaxDescription)}); err != nil {
		t.Fatalf("Add at the cap: %v", err)
	}
	if _, err := svc.Update(ctx, "ok", Patch{Description: strp(long)}); !errors.Is(err, ErrInvalid) {
		t.Fatalf("Update long: err = %v, want ErrInvalid", err)
	}
	if got, _ := svc.Get(ctx, "ok"); got.Description != strings.Repeat("é", MaxDescription) {
		t.Fatalf("description changed by a refused update")
	}
}

// TestDescriptionFallsBackToInstructions: with no admin description the
// gateway shows what the upstream said at initialize; the admin's text
// wins once set.
func TestDescriptionFallsBackToInstructions(t *testing.T) {
	ctx := context.Background()
	svc, _, gw := newServiceFixture(t)
	m := server.NewMCPServer("bkt3", "1.0.0", server.WithInstructions("bk t3 on the USA Mac mini.\n\nUse it for usm sessions."))
	m.AddTool(mcp.NewTool("t3_whoami"), func(context.Context, mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		return mcp.NewToolResultText("ok"), nil
	})
	ts := servertest.NewTestStreamableHTTPServer(m)
	t.Cleanup(ts.Close)

	if _, err := svc.Add(ctx, Server{Name: "bkt3_usm", Transport: "http", URL: ts.URL}); err != nil {
		t.Fatalf("Add: %v", err)
	}
	if d := gw.ServerDescription("bkt3_usm"); d != "bk t3 on the USA Mac mini. Use it for usm sessions." {
		t.Fatalf("fallback description = %q", d)
	}
	if _, err := svc.Update(ctx, "bkt3_usm", Patch{Description: strp("bk t3 on usm")}); err != nil {
		t.Fatal(err)
	}
	if d := gw.ServerDescription("bkt3_usm"); d != "bk t3 on usm" {
		t.Fatalf("admin description = %q", d)
	}
}
