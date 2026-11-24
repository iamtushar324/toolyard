package jobs

import "context"

// listFilteredMaxScan bounds how many rows one ListFiltered call reads, so
// a caller who may see few of many jobs cannot make one request scan the
// whole table. Past it the page may be short; its cursor still moves on.
const listFilteredMaxScan = 2000

// ListCursor is where the next page of a ListFiltered listing starts (the
// Filter's BeforeCreated and BeforeID).
type ListCursor struct {
	CreatedAt int64
	ID        string
}

// ListFiltered is List for a caller who may not see every row the filter
// matches: it returns up to f.Limit jobs, newest first, for which keep is
// true, and the cursor of the next page (nil when no row is left).
//
// Filtering happens after the read (what keep decides, a caller's live
// access scope, is not in the table), so it reads batches by keyset from
// f's own cursor and keeps going until the page is full or the rows run
// out. The page is full only when one more kept row exists, so a cursor is
// never handed out for an empty next page. The next page starts after the
// last returned row: rows skipped before it are not read again, and rows
// skipped after it are read again and skipped again on the next page, so
// no kept row is lost or repeated. When listFilteredMaxScan rows were read
// without filling the page, the page is returned short and the cursor
// points after the last row read.
func (s *Service) ListFiltered(ctx context.Context, f Filter, keep func(*Job) bool) ([]*Job, *ListCursor, error) {
	want := f.Limit
	if want <= 0 || want > 500 {
		want = 50
	}
	batch := want + 1
	if batch < 50 {
		batch = 50
	}
	if batch > 500 {
		batch = 500
	}
	out := []*Job{}
	var last *Job
	scanned := 0
	for {
		page := f
		// Never read past listFilteredMaxScan: the last batch asks only
		// for the rows still allowed (at least one, checked below).
		page.Limit = batch
		if left := listFilteredMaxScan - scanned; page.Limit > left {
			page.Limit = left
		}
		if last != nil {
			page.BeforeCreated, page.BeforeID = last.CreatedAt, last.ID
		}
		rows, err := s.List(ctx, page)
		if err != nil {
			return nil, nil, err
		}
		for _, j := range rows {
			last = j
			scanned++
			if keep != nil && !keep(j) {
				continue
			}
			out = append(out, j)
			if len(out) > want {
				out = out[:want]
				end := out[want-1]
				return out, &ListCursor{CreatedAt: end.CreatedAt, ID: end.ID}, nil
			}
		}
		if len(rows) < page.Limit {
			return out, nil, nil
		}
		if scanned >= listFilteredMaxScan {
			return out, &ListCursor{CreatedAt: last.CreatedAt, ID: last.ID}, nil
		}
	}
}
