package skillhub

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
)

// One contributor's parallel creates stop at the cap; another contributor
// and an admin are not held back, and a decision frees a place.
func TestContributorPendingCapHoldsInParallel(t *testing.T) {
	gh := newFakeGitHub(t, "acme", "skills")
	gh.commit("main", baseTree())
	svc := newTestService(t, gh)
	ctx := context.Background()
	const tries = 8
	srcs := make([]*Source, tries)
	errs := make([]error, tries)
	start := make(chan struct{})
	var wg sync.WaitGroup
	for i := 0; i < tries; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-start
			srcs[i], errs[i] = svc.CreateSource(ctx, "usr_contrib", SourceInput{URL: "acme/skills", Slug: fmt.Sprintf("cap-%d", i),
				Path: "skills", Mode: ModeReview, PendingApproval: true})
		}(i)
	}
	close(start)
	wg.Wait()
	var created []*Source
	for i, err := range errs {
		switch {
		case err == nil:
			created = append(created, srcs[i])
		case errors.Is(err, ErrConflict) && strings.Contains(err.Error(), "waiting for an admin"):
		default:
			t.Errorf("create %d: %v", i, err)
		}
	}
	if len(created) != MaxPendingContributorSources {
		t.Fatalf("%d parallel creates passed, want %d", len(created), MaxPendingContributorSources)
	}
	if n, err := svc.pendingSourcesOf(ctx, "usr_contrib"); err != nil || n != MaxPendingContributorSources {
		t.Fatalf("waiting sources: %d %v", n, err)
	}
	if _, err := svc.CreateSource(ctx, "usr_other", SourceInput{URL: "acme/skills", Slug: "other", Path: "skills", Mode: ModeReview, PendingApproval: true}); err != nil {
		t.Fatalf("another contributor: %v", err)
	}
	if _, err := svc.CreateSource(ctx, "usr_admin", SourceInput{URL: "acme/skills", Slug: "admins", Path: "skills", Mode: ModeReview}); err != nil {
		t.Fatalf("an admin: %v", err)
	}
	if _, err := svc.DecideApproval(ctx, "usr_admin", created[0].ID, false, "no", ""); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.CreateSource(ctx, "usr_contrib", SourceInput{URL: "acme/skills", Slug: "after", Path: "skills", Mode: ModeReview, PendingApproval: true}); err != nil {
		t.Fatalf("create after a decision: %v", err)
	}
}
