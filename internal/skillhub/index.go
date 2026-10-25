package skillhub

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"sync"
	"time"
)

// index is one node's copy of every source's active version: skills and
// file lists, never file bytes. Each srcIndex is immutable once built; a
// reload swaps it.
type index struct {
	mu     sync.RWMutex
	byID   map[string]*srcIndex
	bySlug map[string]*srcIndex
	// deleted remembers the sources this node saw deleted (IDs are never
	// reused), so a reload that read the source before its delete cannot
	// bring it back.
	deleted map[string]bool

	lmu       sync.Mutex
	nextL     int
	listeners map[int]func(slug string)
}

func newIndex() *index {
	return &index{byID: map[string]*srcIndex{}, bySlug: map[string]*srcIndex{}, deleted: map[string]bool{},
		listeners: map[int]func(string){}}
}

// srcIndex is one source and its active version.
type srcIndex struct {
	src       Source // without secrets
	versionID string // "" when nothing is active
	sha       string
	syncedAt  time.Time
	members   map[string]bool
	skills    []*skillEntry // by path
	byPath    map[string]*skillEntry
	files     []*fileEntry // by path
	byFile    map[string]*fileEntry
	warnings  []Warning
	// skillFiles and skillOmitted are each skill's files (nested skills'
	// included) and left-out files, by skill path.
	skillFiles   map[string][]*fileEntry
	skillOmitted map[string][]Warning
}

// skillEntry is one skill of an active version.
type skillEntry struct {
	path        string
	name        string
	description string
	frontmatter json.RawMessage
	skillMD     string // digest
	runs        Runs   // where it runs, after the override and defaults
	override    Runs
}

// fileEntry is one file of an active version.
type fileEntry struct {
	path  string // relative to the source folder
	skill string // innermost skill folder
	sha   string
	size  int64
	media string
}

func (si *srcIndex) repoLabel() string { return RepoLabel(si.src.Owner, si.src.Repo, si.sha) }

// visibleTo reports whether the viewer sees the source.
func (si *srcIndex) visibleTo(v Viewer) bool {
	if si.versionID == "" {
		return false
	}
	if v.Admin || si.src.Visibility != VisibleMembers {
		return true
	}
	return v.UserID != "" && si.members[v.UserID]
}

// showRuns reports whether a listing shows a skill that runs r.
func showRuns(v Viewer, includeHost bool, r Runs) bool {
	return r != RunsHost || v.OnHost || includeHost
}

// snapshot returns the sources, by slug.
func (ix *index) snapshot() []*srcIndex {
	ix.mu.RLock()
	defer ix.mu.RUnlock()
	out := make([]*srcIndex, 0, len(ix.bySlug))
	for _, si := range ix.bySlug {
		out = append(out, si)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].src.Slug < out[j].src.Slug })
	return out
}

func (ix *index) get(id string) *srcIndex {
	ix.mu.RLock()
	defer ix.mu.RUnlock()
	return ix.byID[id]
}

func (ix *index) slug(slug string) *srcIndex {
	ix.mu.RLock()
	defer ix.mu.RUnlock()
	return ix.bySlug[slug]
}

// put swaps in a source (nil removes id) and reports the slugs whose
// served content changed. Reloads may finish in any order, so put keeps
// the newer of two snapshots of a source (updated_at moves forward with
// every change), never brings back a deleted source, and gives a slug to
// the newest source that holds it (a slug freed by a delete may be taken
// by a new source before this node hears of the delete).
func (ix *index) put(id string, si *srcIndex) []string {
	ix.mu.Lock()
	old := ix.byID[id]
	switch {
	case si == nil:
		ix.deleted[id] = true
	case ix.deleted[id]:
		ix.mu.Unlock()
		return nil
	case old != nil && si.src.UpdatedAt.Before(old.src.UpdatedAt):
		ix.mu.Unlock()
		return nil
	}
	if old != nil {
		if ix.bySlug[old.src.Slug] == old {
			delete(ix.bySlug, old.src.Slug)
		}
		delete(ix.byID, id)
	}
	if si != nil {
		ix.byID[id] = si
		if cur := ix.bySlug[si.src.Slug]; cur == nil || cur.src.ID == id || !si.src.CreatedAt.Before(cur.src.CreatedAt) {
			ix.bySlug[si.src.Slug] = si
		}
	}
	ix.mu.Unlock()
	var changed []string
	switch {
	case old != nil && si == nil:
		changed = append(changed, old.src.Slug)
	case old == nil && si != nil:
		if si.versionID != "" {
			changed = append(changed, si.src.Slug)
		}
	case old != nil && si != nil:
		if old.src.Slug != si.src.Slug {
			changed = append(changed, old.src.Slug)
		}
		if old.versionID != si.versionID || !old.src.UpdatedAt.Equal(si.src.UpdatedAt) || old.src.Slug != si.src.Slug {
			changed = append(changed, si.src.Slug)
		}
	}
	return changed
}

// ids returns the indexed source IDs.
func (ix *index) ids() []string {
	ix.mu.RLock()
	defer ix.mu.RUnlock()
	out := make([]string, 0, len(ix.byID))
	for id := range ix.byID {
		out = append(out, id)
	}
	return out
}

// Subscribe calls fn with a source slug after what the source serves
// changed (its active version, visibility or runs, or the source was
// deleted), on this or another node. fn runs on the goroutine that
// reloaded the source, possibly several at once: keep it short and safe
// for concurrent calls.
func (s *Service) Subscribe(fn func(slug string)) (cancel func()) {
	ix := s.idx
	ix.lmu.Lock()
	ix.nextL++
	id := ix.nextL
	ix.listeners[id] = fn
	ix.lmu.Unlock()
	var once sync.Once
	return func() {
		once.Do(func() {
			ix.lmu.Lock()
			delete(ix.listeners, id)
			ix.lmu.Unlock()
		})
	}
}

func (ix *index) notify(slugs []string) {
	if len(slugs) == 0 {
		return
	}
	ix.lmu.Lock()
	fns := make([]func(string), 0, len(ix.listeners))
	for _, fn := range ix.listeners {
		fns = append(fns, fn)
	}
	ix.lmu.Unlock()
	for _, slug := range slugs {
		for _, fn := range fns {
			fn(slug)
		}
	}
}

// reloadSource reads one source and its active version into the index
// (or drops it when it was deleted). An unchanged source costs one small
// query.
func (s *Service) reloadSource(ctx context.Context, id string) error {
	row, err := s.getSourceRow(ctx, s.db, id)
	if err != nil {
		if isNotFound(err) {
			s.idx.notify(s.idx.put(id, nil))
			return nil
		}
		return err
	}
	if cur := s.idx.get(id); cur != nil && cur.versionID == row.ActiveVersionID && cur.src.UpdatedAt.Equal(row.UpdatedAt) {
		return nil
	}
	si, err := s.buildSrcIndex(ctx, row)
	if err != nil {
		return err
	}
	s.idx.notify(s.idx.put(id, si))
	return nil
}

// refreshAll compares the index with the database: changed sources are
// reloaded, deleted ones dropped. It heals missed cluster messages.
func (s *Service) refreshAll(ctx context.Context) error {
	rows, err := s.db.QueryContext(ctx, `SELECT id FROM skill_sources`)
	if err != nil {
		return fmt.Errorf("skillhub: refresh: %w", err)
	}
	present := map[string]bool{}
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			return fmt.Errorf("skillhub: refresh: %w", err)
		}
		present[id] = true
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return fmt.Errorf("skillhub: refresh: %w", err)
	}
	for _, id := range s.idx.ids() {
		if !present[id] {
			s.idx.notify(s.idx.put(id, nil))
		}
	}
	var firstErr error
	for id := range present {
		if err := s.reloadSource(ctx, id); err != nil && firstErr == nil {
			firstErr = err
		}
	}
	return firstErr
}

// buildSrcIndex reads a source's active version.
func (s *Service) buildSrcIndex(ctx context.Context, row *sourceRow) (*srcIndex, error) {
	si := &srcIndex{
		src: row.Source, members: map[string]bool{},
		byPath: map[string]*skillEntry{}, byFile: map[string]*fileEntry{},
		skillFiles: map[string][]*fileEntry{}, skillOmitted: map[string][]Warning{},
	}
	for _, m := range row.Members {
		si.members[m] = true
	}
	if row.ActiveVersionID == "" {
		return si, nil
	}
	v, err := s.getVersionRow(ctx, s.db, row.ID, row.ActiveVersionID)
	if err != nil {
		return nil, err
	}
	si.versionID, si.sha, si.syncedAt, si.warnings = v.ID, v.SHA, v.CreatedAt, v.Warnings
	entries, err := s.versionEntries(ctx, v.ID)
	if err != nil {
		return nil, err
	}
	files, err := s.versionFiles(ctx, v.ID)
	if err != nil {
		return nil, err
	}
	overrides, err := s.runsOverrides(ctx, row.ID)
	if err != nil {
		return nil, err
	}
	for _, e := range entries {
		se := &skillEntry{
			path: e.path, name: e.name, description: e.description,
			frontmatter: json.RawMessage(e.frontmatter), skillMD: e.skillMD, override: overrides[e.path],
		}
		se.runs = effectiveRuns(se.override, Runs(e.runs), row.DefaultRuns)
		si.skills = append(si.skills, se)
		si.byPath[se.path] = se
	}
	for _, f := range files {
		fe := &fileEntry{path: f.path, skill: f.skill, sha: f.sha, size: f.size, media: f.media}
		si.files = append(si.files, fe)
		si.byFile[fe.path] = fe
	}
	// Byte order, whatever the database's collation: listings and their
	// cursors compare paths as Go strings.
	sort.Slice(si.skills, func(i, j int) bool { return si.skills[i].path < si.skills[j].path })
	sort.Slice(si.files, func(i, j int) bool { return si.files[i].path < si.files[j].path })
	for _, f := range si.files {
		for _, sp := range enclosingSkills(f.path, si.byPath) {
			si.skillFiles[sp] = append(si.skillFiles[sp], f)
		}
	}
	for _, w := range si.warnings {
		if w.Severity != "skip" || w.Path == "" {
			continue
		}
		if rel, ok := underFolder(w.Path, row.Path); ok && rel != "" {
			for _, sp := range enclosingSkills(rel, si.byPath) {
				si.skillOmitted[sp] = append(si.skillOmitted[sp], w)
			}
		}
	}
	return si, nil
}

// effectiveRuns applies the order of precedence: an admin override, then
// the frontmatter, then the source default.
func effectiveRuns(override, fm, def Runs) Runs {
	switch {
	case override == RunsHost || override == RunsAnywhere:
		return override
	case fm == RunsHost || fm == RunsAnywhere:
		return fm
	case def == RunsHost:
		return RunsHost
	}
	return RunsAnywhere
}

// reloadQueue holds the sources the cluster asked this node to reload.
type reloadQueue struct {
	mu   sync.Mutex
	ids  map[string]bool
	all  bool
	wake chan struct{}
}

func (q *reloadQueue) mark(id string) {
	q.mu.Lock()
	q.ids[id] = true
	q.mu.Unlock()
	q.poke()
}

func (q *reloadQueue) markAll() {
	q.mu.Lock()
	q.all = true
	q.mu.Unlock()
	q.poke()
}

func (q *reloadQueue) poke() {
	select {
	case q.wake <- struct{}{}:
	default:
	}
}

func (q *reloadQueue) take() (ids []string, all bool) {
	q.mu.Lock()
	defer q.mu.Unlock()
	for id := range q.ids {
		ids = append(ids, id)
	}
	q.ids = map[string]bool{}
	all, q.all = q.all, false
	return ids, all
}

// runReloads serves the reload queue, and compares the whole index with
// the database every refreshEvery, until ctx ends.
func (s *Service) runReloads(ctx context.Context) {
	t := time.NewTicker(refreshEvery)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			s.reloads.markAll()
			continue
		case <-s.reloads.wake:
		}
		ids, all := s.reloads.take()
		rctx, cancel := context.WithTimeout(ctx, time.Minute)
		if all {
			if err := s.refreshAll(rctx); err != nil && ctx.Err() == nil {
				s.log.Warn("skillhub: refreshing the catalog failed; serving the cached copy", "err", err.Error())
			}
		} else {
			for _, id := range ids {
				if err := s.reloadSource(rctx, id); err != nil && ctx.Err() == nil {
					s.log.Warn("skillhub: reloading a source failed; serving the cached copy", "source", id, "err", err.Error())
				}
			}
		}
		cancel()
	}
}

func isNotFound(err error) bool { return errors.Is(err, ErrNotFound) }
