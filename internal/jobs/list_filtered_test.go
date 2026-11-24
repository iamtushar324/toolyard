package jobs_test

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/tusharbhardwaj/toolyard/internal/jobs"
	"github.com/tusharbhardwaj/toolyard/internal/store/storetest"
)

// TestListFilteredFillsPagesPastHiddenRows: rows keep refuses are read
// past, so each page is full while kept rows remain, the cursor follows
// the last returned row, and no cursor is given when no kept row is left.
func TestListFilteredFillsPagesPastHiddenRows(t *testing.T) {
	db := storetest.Open(t)
	svc := newService(t, db, jobs.Options{})
	ctx := context.Background()
	now := time.Now().UnixMilli()
	// Newest first: j7 keep, j6 hide, j5 keep, j4 hide, j3 keep, j2 hide, j1 keep.
	for i := 1; i <= 7; i++ {
		tool := "alpha.keep"
		if i%2 == 0 {
			tool = "alpha.hide"
		}
		if _, err := db.ExecContext(ctx, `INSERT INTO tool_jobs(id, agent_id, owner_user_id, tool, upstream, params_hash, reason, via,
            status, owner_instance, lease_until, created_at, started_at, finished_at, updated_at)
            VALUES(?, 'ag_1', 'u_1', ?, 'alpha', 'h', 'test the filtered job listing', 'direct', 'succeeded', 'node', 0, ?, ?, ?, ?)`,
			fmt.Sprintf("job_%d", i), tool, int64(1000+i), now, now, now); err != nil {
			t.Fatal(err)
		}
	}
	keep := func(j *jobs.Job) bool { return j.Tool == "alpha.keep" }
	ids := func(list []*jobs.Job) []string {
		out := []string{}
		for _, j := range list {
			out = append(out, j.ID)
		}
		return out
	}

	page, next, err := svc.ListFiltered(ctx, jobs.Filter{AgentID: "ag_1", Limit: 2}, keep)
	if err != nil {
		t.Fatal(err)
	}
	if got := ids(page); len(got) != 2 || got[0] != "job_7" || got[1] != "job_5" || next == nil || next.ID != "job_5" {
		t.Fatalf("page 1 = %v, next %+v", got, next)
	}
	page, next, err = svc.ListFiltered(ctx, jobs.Filter{AgentID: "ag_1", Limit: 2, BeforeCreated: next.CreatedAt, BeforeID: next.ID}, keep)
	if err != nil {
		t.Fatal(err)
	}
	if got := ids(page); len(got) != 2 || got[0] != "job_3" || got[1] != "job_1" || next != nil {
		t.Fatalf("page 2 = %v, next %+v; want job_3, job_1 and no cursor", got, next)
	}
	// Exactly as many kept rows as the limit: a full page, no next page.
	page, next, err = svc.ListFiltered(ctx, jobs.Filter{AgentID: "ag_1", Limit: 4}, keep)
	if err != nil || len(page) != 4 || next != nil {
		t.Fatalf("limit 4 = %v, next %+v, %v", ids(page), next, err)
	}
	// Without keep it is List.
	if page, _, err := svc.ListFiltered(ctx, jobs.Filter{AgentID: "ag_1", Limit: 10}, nil); err != nil || len(page) != 7 {
		t.Fatalf("unfiltered = %v, %v", ids(page), err)
	}
}
