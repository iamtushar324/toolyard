package skillhub

// The Inbox sweep: the skills hub's Inbox questions (reviews and source
// proposals) are settled by the Inbox handler the moment an owner decides.
// What that handler cannot see is settled here, on one node at a time:
//
//   - a decision recorded while no handler ran (a node stopped between the
//     decision and the handler, or a replica without the handler took it)
//     is applied now;
//   - an item that expired or was withdrawn closes its row (the Inbox runs
//     no handler for those);
//   - an open review whose version no longer waits for review (a newer
//     commit, a change of mode, branch, folder or pinned commit, a deleted
//     source) is cancelled, so nobody approves an outdated version.
//
// Every step is a conditional update of the row, so a second runner, or the
// Inbox handler racing the sweep, applies nothing twice.

import (
	"context"
	"errors"
	"time"

	"github.com/tusharbhardwaj/toolyard/internal/inbox"
	"github.com/tusharbhardwaj/toolyard/internal/logx"
)

const (
	// InboxSweepLoop names the leader loop that runs RunInboxSweeps in
	// cmd/gateway.
	InboxSweepLoop = "skills-inbox-sweep"
	// InboxSweepInterval is how often RunInboxSweeps sweeps.
	InboxSweepInterval = time.Minute
	// inboxSweepBatch bounds the rows one sweep of one kind reads.
	inboxSweepBatch = 200
	// inboxOrphanAfter is when a pending row whose Inbox item was never
	// created (the process stopped in between) is given up.
	inboxOrphanAfter = 10 * time.Minute
)

// InboxSweeper is a feature whose Inbox questions need a periodic sweep:
// *ReviewInbox and *SourceProposals.
type InboxSweeper interface {
	Sweep(ctx context.Context) error
}

// RunInboxSweeps runs every sweeper at once and then every interval, until
// ctx ends. Run it on one node at a time: cmd/gateway runs it under the
// cluster leader lease InboxSweepLoop. A failed sweep is logged and tried
// again on the next tick.
func RunInboxSweeps(ctx context.Context, every time.Duration, sweepers ...InboxSweeper) {
	if every <= 0 {
		every = InboxSweepInterval
	}
	log := logx.For("skillhub")
	t := time.NewTicker(every)
	defer t.Stop()
	for {
		for _, sw := range sweepers {
			if sw == nil {
				continue
			}
			if err := sw.Sweep(ctx); err != nil && ctx.Err() == nil {
				log.Warn("skillhub: inbox sweep failed", "err", err.Error())
			}
		}
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

// Sweep settles the open review rows: it applies an answered item's
// decision, closes the rows of withdrawn and expired items, and cancels an
// open item whose version no longer waits for review. One row's failure
// does not stop the others; the first error is returned.
func (ri *ReviewInbox) Sweep(ctx context.Context) error {
	if ri == nil {
		return nil
	}
	rows, err := ri.store.pendingRows(ctx, proposalKindReview, "", inboxSweepBatch)
	if err != nil {
		return err
	}
	var first error
	for i := range rows {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if err := ri.sweepRow(ctx, &rows[i]); err != nil && first == nil {
			first = err
		}
	}
	return first
}

func (ri *ReviewInbox) sweepRow(ctx context.Context, row *proposalRow) error {
	if row.InboxID == "" {
		if ri.o.Now().UnixMilli()-row.CreatedAt > inboxOrphanAfter.Milliseconds() {
			_, err := ri.store.move(ctx, row.ID, []string{proposalPending}, proposalFailed, proposalUpdate{Error: "the Inbox item was never created"})
			return err
		}
		return nil
	}
	item, err := ri.o.Inbox.Get(ctx, row.InboxID)
	if errors.Is(err, inbox.ErrNotFound) {
		_, err = ri.store.move(ctx, row.ID, []string{proposalPending}, proposalFailed, proposalUpdate{Error: "the Inbox item is gone"})
		return err
	}
	if err != nil {
		return err
	}
	switch item.Status {
	case inbox.StatusAnswered, inbox.StatusDenied:
		if item.System == nil || item.System.Kind != ReviewItemKind {
			return nil
		}
		// decide records its outcome on the row (done, rejected,
		// cancelled or failed); its error is that outcome, not a
		// failure of the sweep.
		_, _ = ri.decide(ctx, item)
		return nil
	case inbox.StatusCancelled:
		_, err = ri.store.move(ctx, row.ID, []string{proposalPending}, proposalCancelled, proposalUpdate{Error: "the Inbox item was withdrawn"})
		return err
	case inbox.StatusExpired:
		_, err = ri.store.move(ctx, row.ID, []string{proposalPending}, proposalCancelled,
			proposalUpdate{Error: "the Inbox item expired without a decision; the version still waits on the Skills page"})
		return err
	case inbox.StatusPending:
		if item.ExpiresAt > 0 && item.ExpiresAt <= ri.o.Now().UnixMilli() {
			// Past its deadline: it can no longer be decided, even before
			// the Inbox's own sweep marks it expired.
			_, err = ri.store.move(ctx, row.ID, []string{proposalPending}, proposalCancelled,
				proposalUpdate{Error: "the Inbox item expired without a decision; the version still waits on the Skills page"})
			return err
		}
		src, err := ri.o.Admin.GetSource(ctx, row.SourceID)
		switch {
		case errors.Is(err, ErrNotFound):
			return ri.settleRows(ctx, []proposalRow{*row}, "the source was deleted")
		case err != nil:
			return err
		case src.PendingVersionID != row.VersionID:
			return ri.settleRows(ctx, []proposalRow{*row}, "this version no longer waits for review")
		}
	}
	return nil
}
