package previewassertion

import (
	"context"
	"encoding/json"
	"errors"
	"math"
	"testing"
)

func TestPreviewFixedCatalogContainsExactlySixClosedOperations(t *testing.T) {
	want := map[string]string{"bks_preview.create": "preview_create", "bks_preview.inspect": "preview_inspect", "bks_preview.heartbeat": "preview_heartbeat", "bks_preview.release": "preview_release", "bks_preview.snapshots": "preview_snapshots", "bks_preview.results": "preview_results"}
	for _, d := range Descriptors() {
		if want[d.Name] != d.Operation || d.Schema["additionalProperties"] != false {
			t.Fatal("catalog escaped fixed mapping")
		}
		delete(want, d.Name)
		properties := d.Schema["properties"].(map[string]any)
		if len(properties) != len(arguments[d.Operation]) {
			t.Fatal("schema fields disagree")
		}
		for k := range properties {
			if !arguments[d.Operation][k] {
				t.Fatal("caller identity field exposed")
			}
		}
	}
	if len(want) != 0 || len(Operations()) != 6 {
		t.Fatal("catalog incomplete")
	}
}
func TestPreviewArgumentValidationRejectsWrongTypesBoundsAndMissingFields(t *testing.T) {
	c, _ := NewClient(signer(t, binding(agentA, sessionA)))
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
