package skillhub

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/tusharbhardwaj/toolyard/internal/store"
)

// sourceRow is a source as stored, with what is never returned.
type sourceRow struct {
	Source
	tokenSealed   string
	webhookSealed string
	headSHA       string
	headETag      string
	failures      int
}

// sourceColumns are the columns scanSource reads, in order.
const sourceColumns = `s.id, s.slug, s.kind, s.owner, s.repo, s.branch, s.path, s.exclude_json, s.mode, s.pinned_sha,
	s.auth, s.token_sealed, s.poll_seconds, s.enabled, s.visibility, s.members_json, s.default_runs,
	s.limits_json, s.webhook_secret_sealed, s.created_by, s.created_at, s.updated_at,
	s.active_version_id, s.pending_version_id, s.head_sha, s.head_etag, s.last_checked_at,
	s.next_check_at, s.last_error, s.failures,
	COALESCE(v.sha, ''), COALESCE(v.skill_count, 0), COALESCE(v.warning_count, 0)`

const sourceFrom = ` FROM skill_sources s LEFT JOIN skill_versions v ON v.id = s.active_version_id`

type rowScanner interface {
	Scan(dest ...any) error
}

func scanSource(sc rowScanner) (*sourceRow, error) {
	var (
		r                                  sourceRow
		exclude, members, limits           string
		mode, auth, vis, runs              string
		poll, enabled, created, updated    int64
		lastChecked, nextCheck             sql.NullInt64
		failures, skillCount, warningCount int64
	)
	err := sc.Scan(&r.ID, &r.Slug, &r.Kind, &r.Owner, &r.Repo, &r.Branch, &r.Path, &exclude, &mode, &r.PinnedSHA,
		&auth, &r.tokenSealed, &poll, &enabled, &vis, &members, &runs,
		&limits, &r.webhookSealed, &r.CreatedBy, &created, &updated,
		&r.ActiveVersionID, &r.PendingVersionID, &r.headSHA, &r.headETag, &lastChecked,
		&nextCheck, &r.LastError, &failures,
		&r.ActiveSHA, &skillCount, &warningCount)
	if err != nil {
		return nil, err
	}
	r.Mode, r.Auth, r.Visibility, r.DefaultRuns = UpdateMode(mode), AuthMode(auth), Visibility(vis), Runs(runs)
	r.PollSeconds, r.Enabled = int(poll), enabled != 0
	r.CreatedAt, r.UpdatedAt = time.UnixMilli(created).UTC(), time.UnixMilli(updated).UTC()
	if lastChecked.Valid {
		r.LastCheckedAt = msPtr(&lastChecked.Int64)
	}
	if nextCheck.Valid {
		r.NextCheckAt = msPtr(&nextCheck.Int64)
	}
	r.failures, r.SkillCount, r.WarningCount = int(failures), int(skillCount), int(warningCount)
	_ = json.Unmarshal([]byte(exclude), &r.Exclude)
	_ = json.Unmarshal([]byte(members), &r.Members)
	_ = json.Unmarshal([]byte(limits), &r.Limits)
	r.HasToken = r.tokenSealed != ""
	r.HasWebhookSecret = r.webhookSealed != ""
	r.WebhookURLPath = "/v1/skills/sources/" + r.ID + "/webhook"
	return &r, nil
}

// getSourceRow reads one source; ErrNotFound when there is none.
func (s *Service) getSourceRow(ctx context.Context, q store.Querier, id string) (*sourceRow, error) {
	r, err := scanSource(q.QueryRowContext(ctx, `SELECT `+sourceColumns+sourceFrom+` WHERE s.id = ?`, id))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, fmt.Errorf("%w: source %s", ErrNotFound, id)
	}
	if err != nil {
		return nil, fmt.Errorf("skillhub: read source %s: %w", id, err)
	}
	return r, nil
}

// listSourceRows reads every source, by slug.
func (s *Service) listSourceRows(ctx context.Context) ([]*sourceRow, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT `+sourceColumns+sourceFrom+` ORDER BY s.slug`)
	if err != nil {
		return nil, fmt.Errorf("skillhub: list sources: %w", err)
	}
	defer rows.Close()
	var out []*sourceRow
	for rows.Next() {
		r, err := scanSource(rows)
		if err != nil {
			return nil, fmt.Errorf("skillhub: list sources: %w", err)
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// configHash is a digest of the settings a version is built with; an
// incremental update starts only from a version with the same one.
func (r *sourceRow) configHash() string {
	b, _ := json.Marshal(struct {
		Path    string   `json:"p"`
		Exclude []string `json:"e"`
		Limits  [4]int64 `json:"l"`
	}{Path: r.Path, Exclude: nonNil(r.Exclude), Limits: limitsKey(r.Limits.effective())})
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:8])
}

func limitsKey(l effLimits) [4]int64 {
	return [4]int64{l.fileBytes, l.totalBytes, int64(l.skills), int64(l.files)}
}

// rootName is the name a skill at the source folder itself must have.
func (r *sourceRow) rootName() string {
	if r.Path != "" {
		segs := splitPath(r.Path)
		return segs[len(segs)-1]
	}
	return r.Repo
}

// insertSource stores a new source.
func (s *Service) insertSource(ctx context.Context, r *sourceRow) error {
	exclude, _ := json.Marshal(nonNil(r.Exclude))
	members, _ := json.Marshal(nonNil(r.Members))
	limits, _ := json.Marshal(r.Limits)
	now := s.now().UnixMilli()
	_, err := s.db.ExecContext(ctx, `INSERT INTO skill_sources (id, slug, kind, owner, repo, branch, path, exclude_json,
		mode, pinned_sha, auth, token_sealed, poll_seconds, enabled, visibility, members_json, default_runs,
		limits_json, created_by, created_at, updated_at, next_check_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		r.ID, r.Slug, r.Kind, r.Owner, r.Repo, r.Branch, r.Path, string(exclude),
		string(r.Mode), r.PinnedSHA, string(r.Auth), r.tokenSealed, int64(r.PollSeconds), boolInt(r.Enabled),
		string(r.Visibility), string(members), string(r.DefaultRuns),
		string(limits), r.CreatedBy, now, now, now)
	return err
}

// updateSourceSettings stores a changed source's settings. due makes its
// next check due now and forgets the branch head it saw.
func (s *Service) updateSourceSettings(ctx context.Context, r *sourceRow, due bool) error {
	exclude, _ := json.Marshal(nonNil(r.Exclude))
	members, _ := json.Marshal(nonNil(r.Members))
	limits, _ := json.Marshal(r.Limits)
	// updated_at always moves, so every node's index sees the change.
	now := max(s.now().UnixMilli(), r.UpdatedAt.UnixMilli()+1)
	q := `UPDATE skill_sources SET branch = ?, path = ?, exclude_json = ?, mode = ?, pinned_sha = ?, auth = ?,
		token_sealed = ?, poll_seconds = ?, enabled = ?, visibility = ?, members_json = ?, default_runs = ?,
		limits_json = ?, updated_at = ?`
	args := []any{r.Branch, r.Path, string(exclude), string(r.Mode), r.PinnedSHA, string(r.Auth),
		r.tokenSealed, int64(r.PollSeconds), boolInt(r.Enabled), string(r.Visibility), string(members), string(r.DefaultRuns),
		string(limits), now}
	if due {
		q += `, next_check_at = ?, head_sha = '', head_etag = '', failures = 0`
		args = append(args, now)
	}
	q += ` WHERE id = ?`
	args = append(args, r.ID)
	res, err := s.db.ExecContext(ctx, q, args...)
	if err != nil {
		return fmt.Errorf("skillhub: update source %s: %w", r.ID, err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return fmt.Errorf("%w: source %s", ErrNotFound, r.ID)
	}
	return nil
}

func nonNil(xs []string) []string {
	if xs == nil {
		return []string{}
	}
	return xs
}

// versionColumns are the columns scanVersion reads.
const versionColumns = `id, source_id, sha, status, config, created_at, activated_at, decided_by, note,
	skill_count, file_count, total_bytes, warnings_json, diff_json, error`

// versionRow is a version as stored.
type versionRow struct {
	Version
	config string
}

func scanVersion(sc rowScanner) (*versionRow, error) {
	var (
		v                         versionRow
		status, warnings, diff    string
		created                   int64
		activated                 sql.NullInt64
		skills, files, totalBytes int64
	)
	if err := sc.Scan(&v.ID, &v.SourceID, &v.SHA, &status, &v.config, &created, &activated, &v.DecidedBy, &v.Note,
		&skills, &files, &totalBytes, &warnings, &diff, &v.Error); err != nil {
		return nil, err
	}
	v.Status = VersionStatus(status)
	v.CreatedAt = time.UnixMilli(created).UTC()
	if activated.Valid {
		v.ActivatedAt = msPtr(&activated.Int64)
	}
	v.SkillCount, v.FileCount, v.TotalBytes = int(skills), int(files), totalBytes
	_ = json.Unmarshal([]byte(warnings), &v.Warnings)
	if diff != "" {
		var d Diff
		if json.Unmarshal([]byte(diff), &d) == nil {
			v.Diff = &d
		}
	}
	return &v, nil
}

// getVersionRow reads one version of a source.
func (s *Service) getVersionRow(ctx context.Context, q store.Querier, sourceID, versionID string) (*versionRow, error) {
	v, err := scanVersion(q.QueryRowContext(ctx, `SELECT `+versionColumns+` FROM skill_versions WHERE id = ? AND source_id = ?`, versionID, sourceID))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, fmt.Errorf("%w: version %s", ErrNotFound, versionID)
	}
	if err != nil {
		return nil, fmt.Errorf("skillhub: read version %s: %w", versionID, err)
	}
	return v, nil
}

// findVersion returns the newest version of a source at sha built with
// config, or nil.
func (s *Service) findVersion(ctx context.Context, sourceID, sha, config string) (*versionRow, error) {
	v, err := scanVersion(s.db.QueryRowContext(ctx, `SELECT `+versionColumns+` FROM skill_versions
		WHERE source_id = ? AND sha = ? AND config = ? ORDER BY created_at DESC, id DESC LIMIT 1`, sourceID, sha, config))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("skillhub: find version: %w", err)
	}
	return v, nil
}

// listVersions reads a source's versions, newest first.
func (s *Service) listVersions(ctx context.Context, sourceID string, limit int) ([]Version, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT `+versionColumns+` FROM skill_versions WHERE source_id = ?
		ORDER BY created_at DESC, id DESC LIMIT ?`, sourceID, int64(limit))
	if err != nil {
		return nil, fmt.Errorf("skillhub: list versions: %w", err)
	}
	defer rows.Close()
	var out []Version
	for rows.Next() {
		v, err := scanVersion(rows)
		if err != nil {
			return nil, fmt.Errorf("skillhub: list versions: %w", err)
		}
		out = append(out, v.Version)
	}
	return out, rows.Err()
}

// storedEntry is one skill_entries row.
type storedEntry struct {
	path, name, description, frontmatter, runs, skillMD, filesDigest string
}

// versionEntries reads the skills of a version, by path.
func (s *Service) versionEntries(ctx context.Context, versionID string) ([]storedEntry, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT path, name, description, frontmatter_json, runs, skill_md_sha256, files_digest
		FROM skill_entries WHERE version_id = ? ORDER BY path`, versionID)
	if err != nil {
		return nil, fmt.Errorf("skillhub: read skills of %s: %w", versionID, err)
	}
	defer rows.Close()
	var out []storedEntry
	for rows.Next() {
		var e storedEntry
		if err := rows.Scan(&e.path, &e.name, &e.description, &e.frontmatter, &e.runs, &e.skillMD, &e.filesDigest); err != nil {
			return nil, fmt.Errorf("skillhub: read skills of %s: %w", versionID, err)
		}
		out = append(out, e)
	}
	return out, rows.Err()
}

// storedFile is one skill_files row.
type storedFile struct {
	path, skill, sha, media string
	size                    int64
}

// versionFiles reads the files of a version, by path.
func (s *Service) versionFiles(ctx context.Context, versionID string) ([]storedFile, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT path, skill_path, sha256, size, media_type
		FROM skill_files WHERE version_id = ? ORDER BY path`, versionID)
	if err != nil {
		return nil, fmt.Errorf("skillhub: read files of %s: %w", versionID, err)
	}
	defer rows.Close()
	var out []storedFile
	for rows.Next() {
		var f storedFile
		if err := rows.Scan(&f.path, &f.skill, &f.sha, &f.size, &f.media); err != nil {
			return nil, fmt.Errorf("skillhub: read files of %s: %w", versionID, err)
		}
		out = append(out, f)
	}
	return out, rows.Err()
}

// runsOverrides reads a source's admin overrides, by skill path.
func (s *Service) runsOverrides(ctx context.Context, sourceID string) (map[string]Runs, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT skill_path, runs FROM skill_runs_overrides WHERE source_id = ?`, sourceID)
	if err != nil {
		return nil, fmt.Errorf("skillhub: read runs overrides: %w", err)
	}
	defer rows.Close()
	out := map[string]Runs{}
	for rows.Next() {
		var p, r string
		if err := rows.Scan(&p, &r); err != nil {
			return nil, fmt.Errorf("skillhub: read runs overrides: %w", err)
		}
		out[p] = Runs(r)
	}
	return out, rows.Err()
}

// blobBatch is how many blobs one query reads or writes, and blobBatchBytes
// the most content one insert carries.
const (
	blobBatch      = 50
	blobBatchBytes = 8 << 20
	rowBatch       = 100
)

// loadBlobs reads blob contents by digest.
func (s *Service) loadBlobs(ctx context.Context, shas []string) (map[string][]byte, error) {
	out := make(map[string][]byte, len(shas))
	uniq := uniqueStrings(shas)
	for start := 0; start < len(uniq); start += blobBatch {
		batch := uniq[start:min(start+blobBatch, len(uniq))]
		args := make([]any, len(batch))
		for i, sha := range batch {
			args[i] = sha
		}
		rows, err := s.db.QueryContext(ctx, `SELECT sha256, content FROM skill_blobs WHERE sha256 IN (`+placeholders(len(batch))+`)`, args...)
		if err != nil {
			return nil, fmt.Errorf("skillhub: read file contents: %w", err)
		}
		for rows.Next() {
			var sha string
			var content []byte
			if err := rows.Scan(&sha, &content); err != nil {
				rows.Close()
				return nil, fmt.Errorf("skillhub: read file contents: %w", err)
			}
			out[sha] = content
		}
		err = rows.Err()
		rows.Close()
		if err != nil {
			return nil, fmt.Errorf("skillhub: read file contents: %w", err)
		}
	}
	return out, nil
}

// loadBlob reads one blob.
func (s *Service) loadBlob(ctx context.Context, sha string) ([]byte, error) {
	var content []byte
	err := s.db.QueryRowContext(ctx, `SELECT content FROM skill_blobs WHERE sha256 = ?`, sha).Scan(&content)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, fmt.Errorf("%w: file content %s", ErrNotFound, sha)
	}
	if err != nil {
		return nil, fmt.Errorf("skillhub: read file content: %w", err)
	}
	return content, nil
}

func uniqueStrings(xs []string) []string {
	seen := make(map[string]bool, len(xs))
	out := make([]string, 0, len(xs))
	for _, x := range xs {
		if !seen[x] {
			seen[x] = true
			out = append(out, x)
		}
	}
	return out
}

// newVersion describes a version to store.
type newVersion struct {
	src    *sourceRow
	sha    string
	config string
	b      *built
	full   bool
	diff   *Diff
}

// insertVersion stores a staged version in one transaction: the new
// blobs first, then the version, its skills and its files. It fails with
// ErrNotFound when the source was deleted meanwhile.
func (s *Service) insertVersion(ctx context.Context, nv newVersion) (*versionRow, error) {
	warnings, _ := json.Marshal(nonNilWarnings(nv.b.warnings))
	var diff []byte
	if nv.diff != nil {
		diff, _ = json.Marshal(nv.diff)
	}
	v := &versionRow{Version: Version{
		ID: newID("skv_"), SourceID: nv.src.ID, SHA: nv.sha, Status: VersionStaged, CreatedAt: s.now().UTC(),
		SkillCount: len(nv.b.skills), FileCount: len(nv.b.files), TotalBytes: nv.b.totalBytes,
		Warnings: nv.b.warnings, Diff: nv.diff,
	}, config: nv.config}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, fmt.Errorf("skillhub: store version: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	var id string
	err = tx.QueryRowContext(ctx, `SELECT id FROM skill_sources WHERE id = ?`+s.db.Dialect.ForUpdate(), nv.src.ID).Scan(&id)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, fmt.Errorf("%w: source %s was deleted", ErrNotFound, nv.src.ID)
	}
	if err != nil {
		return nil, fmt.Errorf("skillhub: store version: %w", err)
	}
	if err := s.storeBlobs(ctx, tx, nv.b.files); err != nil {
		return nil, err
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO skill_versions (id, source_id, sha, status, config, created_at,
		skill_count, file_count, total_bytes, warning_count, warnings_json, diff_json, full_sync)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		v.ID, v.SourceID, v.SHA, string(v.Status), v.config, v.CreatedAt.UnixMilli(),
		int64(v.SkillCount), int64(v.FileCount), v.TotalBytes, int64(len(nv.b.warnings)), string(warnings), string(diff), boolInt(nv.full)); err != nil {
		return nil, fmt.Errorf("skillhub: store version: %w", err)
	}
	if err := insertEntries(ctx, tx, v.ID, nv.b.skills); err != nil {
		return nil, err
	}
	if err := insertFiles(ctx, tx, v.ID, nv.b.files); err != nil {
		return nil, err
	}
	if err := tx.Commit(); err != nil {
		return nil, fmt.Errorf("skillhub: store version: %w", err)
	}
	return v, nil
}

func nonNilWarnings(w []Warning) []Warning {
	if w == nil {
		return []Warning{}
	}
	return w
}

// storeBlobs writes the contents a version adds: blobs already stored are
// only touched (so the purge leaves them alone), the rest inserted.
func (s *Service) storeBlobs(ctx context.Context, tx *sql.Tx, files []builtFile) error {
	var fresh []*candidate
	seen := map[string]bool{}
	for _, f := range files {
		if f.data == nil || seen[f.sha] {
			continue
		}
		seen[f.sha] = true
		fresh = append(fresh, f.candidate)
	}
	now := s.now().UnixMilli()
	for start := 0; start < len(fresh); start += blobBatch {
		batch := fresh[start:min(start+blobBatch, len(fresh))]
		args := make([]any, len(batch))
		for i, c := range batch {
			args[i] = c.sha
		}
		rows, err := tx.QueryContext(ctx, `SELECT sha256 FROM skill_blobs WHERE sha256 IN (`+placeholders(len(batch))+`)`, args...)
		if err != nil {
			return fmt.Errorf("skillhub: store file contents: %w", err)
		}
		have := map[string]bool{}
		for rows.Next() {
			var sha string
			if err := rows.Scan(&sha); err != nil {
				rows.Close()
				return fmt.Errorf("skillhub: store file contents: %w", err)
			}
			have[sha] = true
		}
		err = rows.Err()
		rows.Close()
		if err != nil {
			return fmt.Errorf("skillhub: store file contents: %w", err)
		}
		if len(have) > 0 {
			touch := []any{now}
			for sha := range have {
				touch = append(touch, sha)
			}
			if _, err := tx.ExecContext(ctx, `UPDATE skill_blobs SET touched_at = ? WHERE sha256 IN (`+placeholders(len(have))+`)`, touch...); err != nil {
				return fmt.Errorf("skillhub: store file contents: %w", err)
			}
		}
		var missing []*candidate
		for _, c := range batch {
			if !have[c.sha] {
				missing = append(missing, c)
			}
		}
		if err := insertBlobs(ctx, tx, missing, now); err != nil {
			return err
		}
	}
	return nil
}

// insertBlobs inserts blobs in statements of at most blobBatchBytes.
func insertBlobs(ctx context.Context, tx *sql.Tx, blobs []*candidate, now int64) error {
	for len(blobs) > 0 {
		n, size := 0, int64(0)
		for n < len(blobs) && (n == 0 || size+blobs[n].size <= blobBatchBytes) {
			size += blobs[n].size
			n++
		}
		batch := blobs[:n]
		blobs = blobs[n:]
		vals := make([]string, len(batch))
		args := make([]any, 0, 4*len(batch))
		for i, c := range batch {
			vals[i] = "(?, ?, ?, ?)"
			args = append(args, c.sha, c.size, c.data, now)
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO skill_blobs (sha256, size, content, touched_at) VALUES `+
			strings.Join(vals, ", ")+` ON CONFLICT (sha256) DO UPDATE SET touched_at = excluded.touched_at`, args...); err != nil {
			return fmt.Errorf("skillhub: store file contents: %w", err)
		}
	}
	return nil
}

// insertEntries writes a version's skills.
func insertEntries(ctx context.Context, tx *sql.Tx, versionID string, skills []*builtSkill) error {
	for start := 0; start < len(skills); start += rowBatch {
		batch := skills[start:min(start+rowBatch, len(skills))]
		vals := make([]string, len(batch))
		args := make([]any, 0, 8*len(batch))
		for i, sk := range batch {
			fm, err := json.Marshal(sk.meta.frontmatter)
			if err != nil {
				return fmt.Errorf("skillhub: %s: frontmatter: %w", sk.path, err)
			}
			vals[i] = "(?, ?, ?, ?, ?, ?, ?, ?)"
			args = append(args, versionID, sk.path, sk.meta.name, sk.meta.description, string(fm), string(sk.meta.runs), sk.skillMD.sha, sk.filesDigest)
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO skill_entries (version_id, path, name, description, frontmatter_json,
			runs, skill_md_sha256, files_digest) VALUES `+strings.Join(vals, ", "), args...); err != nil {
			return fmt.Errorf("skillhub: store skills: %w", err)
		}
	}
	return nil
}

// insertFiles writes a version's files.
func insertFiles(ctx context.Context, tx *sql.Tx, versionID string, files []builtFile) error {
	for start := 0; start < len(files); start += rowBatch {
		batch := files[start:min(start+rowBatch, len(files))]
		vals := make([]string, len(batch))
		args := make([]any, 0, 6*len(batch))
		for i, f := range batch {
			vals[i] = "(?, ?, ?, ?, ?, ?)"
			args = append(args, versionID, f.path, f.skill, f.sha, f.size, f.media)
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO skill_files (version_id, path, skill_path, sha256, size, media_type)
			VALUES `+strings.Join(vals, ", "), args...); err != nil {
			return fmt.Errorf("skillhub: store files: %w", err)
		}
	}
	return nil
}

// recordFailedVersion stores (or refreshes) the failed version of a
// commit, so the dashboard shows why it is not served.
func (s *Service) recordFailedVersion(ctx context.Context, src *sourceRow, sha, config string, cause error) {
	msg := cause.Error()
	now := s.now().UnixMilli()
	res, err := s.db.ExecContext(ctx, `UPDATE skill_versions SET error = ?, created_at = ?
		WHERE source_id = ? AND sha = ? AND config = ? AND status = ?`, msg, now, src.ID, sha, config, string(VersionFailed))
	if err == nil {
		if n, _ := res.RowsAffected(); n > 0 {
			return
		}
		_, err = s.db.ExecContext(ctx, `INSERT INTO skill_versions (id, source_id, sha, status, config, created_at, error)
			VALUES (?, ?, ?, ?, ?, ?, ?)`, newID("skv_"), src.ID, sha, string(VersionFailed), config, now, msg)
	}
	if err != nil {
		s.log.Warn("skillhub: recording a failed version failed", "source", src.ID, "err", err.Error())
	}
}

// activate makes a version active in one transaction: the old active one
// becomes retired, and the source names the new one. allowRejected lets a
// pinned source serve a commit an admin rejected before.
func (s *Service) activate(ctx context.Context, by, sourceID, versionID string, allowRejected bool) (bool, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return false, fmt.Errorf("skillhub: activate: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	var active, pending string
	err = tx.QueryRowContext(ctx, `SELECT active_version_id, pending_version_id FROM skill_sources WHERE id = ?`+s.db.Dialect.ForUpdate(),
		sourceID).Scan(&active, &pending)
	if errors.Is(err, sql.ErrNoRows) {
		return false, fmt.Errorf("%w: source %s", ErrNotFound, sourceID)
	}
	if err != nil {
		return false, fmt.Errorf("skillhub: activate: %w", err)
	}
	v, err := s.getVersionRow(ctx, tx, sourceID, versionID)
	if err != nil {
		return false, err
	}
	switch {
	case v.Status == VersionActive && active == versionID:
		return false, nil
	case v.Status == VersionStaged, v.Status == VersionRetired, v.Status == VersionActive:
	case v.Status == VersionRejected && allowRejected:
	default:
		return false, fmt.Errorf("%w: version %s is %s and cannot be made active", ErrConflict, versionID, v.Status)
	}
	now := s.now().UnixMilli()
	if active != "" && active != versionID {
		if _, err := tx.ExecContext(ctx, `UPDATE skill_versions SET status = ? WHERE id = ?`, string(VersionRetired), active); err != nil {
			return false, fmt.Errorf("skillhub: activate: %w", err)
		}
	}
	if _, err := tx.ExecContext(ctx, `UPDATE skill_versions SET status = ?, activated_at = ?, decided_by = ? WHERE id = ?`,
		string(VersionActive), now, by, versionID); err != nil {
		return false, fmt.Errorf("skillhub: activate: %w", err)
	}
	if pending == versionID {
		pending = ""
	}
	if _, err := tx.ExecContext(ctx, `UPDATE skill_sources SET active_version_id = ?, pending_version_id = ?, updated_at = ? WHERE id = ?`,
		versionID, pending, now, sourceID); err != nil {
		return false, fmt.Errorf("skillhub: activate: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return false, fmt.Errorf("skillhub: activate: %w", err)
	}
	return true, nil
}

// setPending makes a staged version the one waiting for review.
func (s *Service) setPending(ctx context.Context, sourceID, versionID string) error {
	_, err := s.db.ExecContext(ctx, `UPDATE skill_sources SET pending_version_id = ? WHERE id = ?`, versionID, sourceID)
	if err != nil {
		return fmt.Errorf("skillhub: stage version: %w", err)
	}
	return nil
}

// recordCheck stores the outcome of one check of a source: the branch head
// it saw and when to check next.
func (s *Service) recordCheck(ctx context.Context, src *sourceRow, out *syncOutcome, cause error) {
	now := s.now()
	poll := time.Duration(max(src.PollSeconds, MinPollSeconds)) * time.Second
	var err error
	if cause == nil {
		_, err = s.db.ExecContext(ctx, `UPDATE skill_sources SET last_checked_at = ?, next_check_at = ?, last_error = '',
			failures = 0, head_sha = ?, head_etag = ? WHERE id = ?`,
			now.UnixMilli(), now.Add(poll).UnixMilli(), out.headSHA, out.headETag, src.ID)
	} else {
		failures := src.failures + 1
		next := now.Add(backoff(poll, failures))
		if until, ok := retryAfter(cause); ok && until.After(next) {
			next = until
		}
		headSHA, headETag := src.headSHA, src.headETag
		if out != nil && out.headSHA != "" {
			headSHA, headETag = out.headSHA, out.headETag
		}
		_, err = s.db.ExecContext(ctx, `UPDATE skill_sources SET last_checked_at = ?, next_check_at = ?, last_error = ?,
			failures = ?, head_sha = ?, head_etag = ? WHERE id = ?`,
			now.UnixMilli(), next.UnixMilli(), cleanMessage(cause.Error()), int64(failures), headSHA, headETag, src.ID)
	}
	if err != nil {
		s.log.Warn("skillhub: recording a check failed", "source", src.ID, "err", err.Error())
	}
}

// backoff is the wait after n failed syncs in a row: the poll interval,
// doubled n times, at most maxBackoff.
func backoff(poll time.Duration, n int) time.Duration {
	d := poll
	for i := 0; i < n && d < maxBackoff; i++ {
		d *= 2
	}
	return min(d, maxBackoff)
}

// retain deletes all but the newest KeepVersions versions that are neither
// active nor pending, with their skills and files.
func (s *Service) retain(ctx context.Context, sourceID string) error {
	rows, err := s.db.QueryContext(ctx, `SELECT v.id FROM skill_versions v JOIN skill_sources s ON s.id = v.source_id
		WHERE v.source_id = ? AND v.id <> s.active_version_id AND v.id <> s.pending_version_id
		ORDER BY v.created_at DESC, v.id DESC`, sourceID)
	if err != nil {
		return fmt.Errorf("skillhub: retention: %w", err)
	}
	var old []any
	n := 0
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			return fmt.Errorf("skillhub: retention: %w", err)
		}
		if n++; n > KeepVersions {
			old = append(old, id)
		}
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return fmt.Errorf("skillhub: retention: %w", err)
	}
	for start := 0; start < len(old); start += rowBatch {
		batch := old[start:min(start+rowBatch, len(old))]
		in := placeholders(len(batch))
		tx, err := s.db.BeginTx(ctx, nil)
		if err != nil {
			return fmt.Errorf("skillhub: retention: %w", err)
		}
		for _, q := range []string{
			`DELETE FROM skill_files WHERE version_id IN (` + in + `)`,
			`DELETE FROM skill_entries WHERE version_id IN (` + in + `)`,
			`DELETE FROM skill_versions WHERE id IN (` + in + `)`,
		} {
			if _, err := tx.ExecContext(ctx, q, batch...); err != nil {
				_ = tx.Rollback()
				return fmt.Errorf("skillhub: retention: %w", err)
			}
		}
		if err := tx.Commit(); err != nil {
			return fmt.Errorf("skillhub: retention: %w", err)
		}
	}
	if len(old) > 0 {
		return s.purgeBlobs(ctx)
	}
	return nil
}

// purgeBlobs deletes the blobs no version references, except those a sync
// touched within blobGrace.
func (s *Service) purgeBlobs(ctx context.Context) error {
	cutoff := s.now().Add(-blobGrace).UnixMilli()
	_, err := s.db.ExecContext(ctx, `DELETE FROM skill_blobs WHERE touched_at < ?
		AND NOT EXISTS (SELECT 1 FROM skill_files f WHERE f.sha256 = skill_blobs.sha256)`, cutoff)
	if err != nil {
		return fmt.Errorf("skillhub: purge unused file contents: %w", err)
	}
	return nil
}
