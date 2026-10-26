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
//     source) is cancelled, so nobody approves an outdated version;
//   - an item decided on a replica that predates system items (it dropped
//     the tag, so no handler ran) fails its row, so limits free up and a
//     new question can be asked; toolyard never acts on such an answer;
//   - an approval whose process stopped before it finished (the row stays
//     approved) is resumed once it is approvedStaleAfter old, under a
//     compare-and-swap claim so two replicas never both act.
//
// Every step is a conditional update of the row, so a second runner, or the
// Inbox handler racing the sweep, applies nothing twice.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
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
	// approvedStaleAfter is how long an approved review or source proposal
	// may stay unfinished before the sweep resumes it: longer than the
	// Inbox handler may run (inbox systemHandlerTimeout, 2 minutes).
	approvedStaleAfter = 5 * time.Minute
	// lostTagError is a row whose Inbox item was decided without its system
	// tag (a replica that predates system items rewrote it).
	lostTagError = "the Inbox item was decided on a toolyard replica that predates system items, so toolyard did not act on the answer; ask again"
)

// lostSystemTag reports an item that is not (or no longer) the system item
// of kind: an older replica rewrote it without the tag.
func lostSystemTag(item *inbox.Request, kind string) bool {
	return item.System == nil || item.System.Kind != kind
}

// closeProposalRows moves pending rows to status to with reason, and
// withdraws their Inbox items (sent by toolyard for row.UserID).
func closeProposalRows(ctx context.Context, st proposalStore, ib *inbox.Service, rows []proposalRow, to, reason string) error {
	var unread error
	for _, row := range rows {
		moved, err := st.move(ctx, row.ID, []string{proposalPending}, to, proposalUpdate{Error: reason})
		if err != nil {
			return err
		}
		inboxID := row.InboxID
		if moved && inboxID == "" {
			// Read before its item was recorded: the item may be recorded
			// now (an asker records it only while the row is pending). A
			// failed read is reported once the other rows are closed.
			cur, err := st.get(ctx, row.ID)
			if err != nil {
				if unread == nil {
					unread = fmt.Errorf("skillhub: read the Inbox item of closed row %s: %w", row.ID, err)
				}
				continue
			}
			inboxID = cur.InboxID
		}
		if !moved || inboxID == "" {
			continue
		}
		if _, err := ib.CancelBySystem(ctx, inbox.SystemAgentID(row.UserID), inboxID, reason); err != nil &&
			!errors.Is(err, inbox.ErrNotPending) && !errors.Is(err, inbox.ErrNotFound) {
			return err
		}
	}
	return unread
}

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
	// Contributor sources' approval questions (review_approval.go).
	if err := ri.sweepApprovals(ctx); err != nil && first == nil {
		first = err
	}
	stale, err := ri.store.staleApproved(ctx, proposalKindReview, ri.o.Now().Add(-approvedStaleAfter).UnixMilli(), inboxSweepBatch)
	if err != nil {
		if first == nil {
			first = err
		}
		return first
	}
	for i := range stale {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if err := ri.resume(ctx, &stale[i]); err != nil && first == nil {
			first = err
		}
	}
	return first
}

// resume finishes an approved review whose process stopped: the version
// goes live if it still waits for review, the row is done if it is live
// already, and else (another version waits or is live) the row is
// cancelled as superseded. Only the caller that wins the claim acts.
func (ri *ReviewInbox) resume(ctx context.Context, row *proposalRow) error {
	ok, err := ri.store.claim(ctx, row.ID, proposalApproved, row.UpdatedAt)
	if err != nil || !ok {
		return err
	}
	decider := row.DecidedBy
	if decider == "" {
		decider = row.UserID
	}
	var in reviewInput
	_ = json.Unmarshal([]byte(row.Input), &in)
	src, err := ri.o.Admin.GetSource(ctx, row.SourceID)
	switch {
	case errors.Is(err, ErrNotFound):
		_, err = ri.store.move(ctx, row.ID, []string{proposalApproved}, proposalCancelled, proposalUpdate{Error: "the source was deleted"})
		return err
	case err != nil:
		return err // claimed: the next sweep after approvedStaleAfter tries again
	case src.ActiveVersionID == row.VersionID:
		_, err = ri.store.move(ctx, row.ID, []string{proposalApproved}, proposalDone, proposalUpdate{})
		return err
	case src.PendingVersionID == row.VersionID:
		_, _ = ri.applyApproved(ctx, row, decider, reviewShort(in.SHA), "inbox, resumed") // records its outcome on the row
		return nil
	}
	_, err = ri.store.move(ctx, row.ID, []string{proposalApproved}, proposalCancelled,
		proposalUpdate{Error: "superseded: another version waits for review or is live now"})
	return err
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
		if lostSystemTag(item, ReviewItemKind) {
			// Never act on it; a failed row lets the next check ask again.
			_, err = ri.store.move(ctx, row.ID, []string{proposalPending}, proposalFailed, proposalUpdate{Error: lostTagError})
			return err
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
