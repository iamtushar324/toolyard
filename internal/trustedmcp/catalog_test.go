package trustedmcp

import (
	"context"
	"encoding/json"
	"errors"
	"math"
	"testing"
)

func TestTrustedMCPCatalogUsesReviewedShortAliasesWithoutNetwork(t *testing.T) {
	c := newTestClient(t, signer(t, binding(agentA, sessionA)))
	want := map[string]string{"create": "preview_create", "inspect": "preview_inspect", "heartbeat": "preview_heartbeat", "release": "preview_release", "snapshots": "preview_snapshots", "results": "preview_results"}
	for _, tool := range c.Catalog() {
		if want[tool.Name] != c.Operation(tool.Name) {
			t.Fatal("reviewed alias mapping changed")
		}
		delete(want, tool.Name)
		var schema map[string]any
		if json.Unmarshal(tool.RawInputSchema, &schema) != nil || schema["additionalProperties"] != false {
			t.Fatal("reviewed schema is not closed")
		}
		properties := schema["properties"].(map[string]any)
		for field := range properties {
			if field == "sid" || field == "session_id" || field == "endpoint" || field == "Authorization" {
				t.Fatal("caller authority field exposed")
			}
		}
	}
	if len(want) != 0 || c.Operation("unknown") != "" {
		t.Fatal("catalog escaped reviewed profile")
	}
}
func TestTrustedMCPArgumentValidationRejectsWrongTypesBoundsAndMissingFields(t *testing.T) {
	c := newTestClient(t, signer(t, binding(agentA, sessionA)))
	valid := func() map[string]any {
		return map[string]any{"source_ref": "pr:1039", "request_id": sessionA, "snapshot_id": "synthetic-v1", "pool": "large", "lifetime_seconds": float64(1200)}
	}
	if err := c.Preflight(context.Background(), agentA, "preview_create", valid()); err != nil {
		t.Fatal(err)
	}
	for _, mutation := range []func(map[string]any){func(a map[string]any) { delete(a, "request_id") }, func(a map[string]any) { a["source_ref"] = "" }, func(a map[string]any) { a["source_ref"] = 1 }, func(a map[string]any) { a["request_id"] = "not-uuid" }, func(a map[string]any) { a["snapshot_id"] = "s3://arbitrary" }, func(a map[string]any) { a["pool"] = "unbounded" }, func(a map[string]any) { a["lifetime_seconds"] = 299 }, func(a map[string]any) { a["lifetime_seconds"] = 21601 }, func(a map[string]any) { a["lifetime_seconds"] = 300.5 }, func(a map[string]any) { a["lifetime_seconds"] = math.NaN() }, func(a map[string]any) { a["lifetime_seconds"] = true }, func(a map[string]any) { a["sid"] = sessionB }} {
		a := valid()
		mutation(a)
		if err := c.Preflight(context.Background(), agentA, "preview_create", a); !errors.Is(err, ErrArguments) {
			t.Fatal("invalid create arguments accepted")
		}
	}
	for _, v := range []any{int(300), int64(21600), float64(1200), json.Number("600")} {
		a := valid()
		a["lifetime_seconds"] = v
		if err := c.Preflight(context.Background(), agentA, "preview_create", a); err != nil {
			t.Fatal("valid integer representation rejected")
		}
	}
	for _, operation := range []string{"preview_inspect", "preview_heartbeat", "preview_release", "preview_results"} {
		if err := c.Preflight(context.Background(), agentA, operation, nil); !errors.Is(err, ErrArguments) {
			t.Fatal("required environment omitted")
		}
		if err := c.Preflight(context.Background(), agentA, operation, map[string]any{"environment_id": sessionA}); err != nil {
			t.Fatal(err)
		}
	}
	if err := c.Preflight(context.Background(), agentA, "preview_release", map[string]any{"environment_id": sessionA, "outcome": "unknown"}); !errors.Is(err, ErrArguments) {
		t.Fatal("unknown release outcome accepted")
	}
	if err := c.Preflight(context.Background(), agentA, "arbitrary_exec", nil); !errors.Is(err, ErrArguments) {
		t.Fatal("unknown operation accepted")
	}
}
