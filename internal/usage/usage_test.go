package usage

import (
	"context"
	"testing"

	"github.com/tusharbhardwaj/toolyard/internal/store/storetest"
)

// TestIncrementUpsertCounts checks the ON CONFLICT counter: the second and
// later calls for one (agent, tool) add one to the stored count. Postgres
// rejects the unqualified "count + 1" as ambiguous, so the statement names
// the table.
func TestIncrementUpsertCounts(t *testing.T) {
	s := New(storetest.Open(t))
	ctx := context.Background()
	for i := 0; i < 3; i++ {
		if err := s.Increment(ctx, "ag_1", "github.merge"); err != nil {
			t.Fatalf("Increment %d: %v", i, err)
		}
	}
	if err := s.Increment(ctx, "ag_1", "notes.read"); err != nil {
		t.Fatal(err)
	}
	// The shared stdio bucket uses agent_id ''.
	if err := s.Increment(ctx, "", "github.merge"); err != nil {
		t.Fatal(err)
	}
	if err := s.Increment(ctx, "ag_1", ""); err == nil {
		t.Fatal("empty tool name accepted")
	}

	total, err := s.TotalForAgent(ctx, "ag_1")
	if err != nil {
		t.Fatal(err)
	}
	if total != 4 {
		t.Fatalf("TotalForAgent = %d, want 4", total)
	}
	rows, err := s.All(ctx, "ag_1")
	if err != nil {
		t.Fatal(err)
	}
	counts := map[string]int64{}
	for _, r := range rows {
		counts[r.ToolName] = r.Count
	}
	if counts["github.merge"] != 3 || counts["notes.read"] != 1 || len(counts) != 2 {
		t.Fatalf("counts for ag_1 = %v", counts)
	}
	top, err := s.TopForAgent(ctx, "ag_1", 1)
	if err != nil {
		t.Fatal(err)
	}
	if len(top) != 1 || top[0] != "github.merge" {
		t.Fatalf("TopForAgent = %v", top)
	}
	per, err := s.AggregatePerTool(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if per["github.merge"] != 4 || per["notes.read"] != 1 {
		t.Fatalf("AggregatePerTool = %v", per)
	}
	overall, err := s.TopOverall(ctx, 2)
	if err != nil {
		t.Fatal(err)
	}
	if len(overall) != 2 || overall[0] != "github.merge" || overall[1] != "notes.read" {
		t.Fatalf("TopOverall = %v", overall)
	}
}
