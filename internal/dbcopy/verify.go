package dbcopy

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"sort"
	"strings"

	"github.com/tusharbhardwaj/toolyard/internal/blobstore"
	"github.com/tusharbhardwaj/toolyard/internal/keyguard"
	"github.com/tusharbhardwaj/toolyard/internal/sealbox"
	"github.com/tusharbhardwaj/toolyard/internal/store"
)

// Verify compares the SQLite database src (and the blob folder srcBlobDir)
// with the Postgres database dst a Copy filled. It only reads both.
//
//   - Every table both have: the row count and a SHA-256 over its rows in
//     primary-key order, on canonical values (see appendCanonical), with
//     the same tables, columns and skipped rows as Copy.
//   - Every keyguard.SealedColumns entry: each sealed value is opened with
//     its key from keys on both sides, and the two sides must hold the same
//     multiset of (identifying columns, plaintext hash) pairs. A value that
//     does not open, on either side, is a failure.
//   - The signing keys (server_keys grant_sign, approval_sign, vapid), the
//     federation instance id and the key canaries, by name; each canary
//     whose key is in keys must open on the target.
//   - Every source blob, read back from the target blob store, must hash
//     to its name.
//
// Report.OK is false on any difference. The report holds names, counts,
// hashes and booleans only: no value and no plaintext is printed, logged
// or kept. An error means the comparison could not finish.
func Verify(ctx context.Context, src *sql.DB, srcBlobDir string, dst *store.DB, keys map[string][]byte) (*Report, error) {
	rep := newReport("verify")
	if src == nil || dst == nil {
		return rep, errors.New("db verify: no source or no target database")
	}
	if !dst.Dialect.IsPostgres() {
		return rep, errors.New("db verify: the target must be a Postgres database")
	}
	stx, err := src.BeginTx(ctx, nil)
	if err != nil {
		return rep, fmt.Errorf("db verify: read the source: %w", err)
	}
	defer func() { _ = stx.Rollback() }()
	// One snapshot of the target too.
	dtx, err := dst.BeginTx(ctx, &sql.TxOptions{Isolation: sql.LevelRepeatableRead, ReadOnly: true})
	if err != nil {
		return rep, fmt.Errorf("db verify: read the target: %w", err)
	}
	defer func() { _ = dtx.Rollback() }()

	p, err := buildPlan(ctx, stx, dtx)
	if err != nil {
		return rep, fmt.Errorf("db verify: %w", err)
	}
	p.fill(rep)

	for _, t := range p.Tables {
		sd, err := digestTable(ctx, stx, t)
		if err != nil {
			return rep, fmt.Errorf("db verify: source: %w", err)
		}
		td, err := digestTable(ctx, dtx, t)
		if err != nil {
			return rep, fmt.Errorf("db verify: target: %w", err)
		}
		tr := TableReport{
			Name:              t.Name,
			SourceRows:        sd.Rows,
			SkippedRows:       sd.Skipped,
			TargetRows:        td.Rows,
			TargetSkippedRows: td.Skipped,
			SourceSHA256:      sd.SHA256,
			TargetSHA256:      td.SHA256,
			Match:             sd.SHA256 == td.SHA256,
		}
		if !tr.Match {
			rep.problemf("table %s differs: %d source rows, %d target rows", t.Name, sd.Rows, td.Rows)
		}
		rep.Tables = append(rep.Tables, tr)
	}

	if err := verifySealed(ctx, stx, dtx, keys, rep); err != nil {
		return rep, err
	}
	if err := verifyNamedRows(ctx, stx, dtx, keys, rep); err != nil {
		return rep, err
	}
	if err := verifyBlobs(ctx, stx, srcBlobDir, blobstore.NewPostgres(dst.DB), &rep.Blobs); err != nil {
		return rep, err
	}
	if n := len(rep.Blobs.TargetMissing); n > 0 {
		rep.problemf("%d source blobs are missing in the target", n)
	}
	if n := len(rep.Blobs.TargetMismatched); n > 0 {
		rep.problemf("%d target blobs do not hash to their name", n)
	}
	return rep, nil
}

// sealedStats counts one side's values of one sealed column.
type sealedStats struct {
	values   int
	unopened int
}

func verifySealed(ctx context.Context, src, dst store.Querier, keys map[string][]byte, rep *Report) error {
	ciphers := map[string]*sealbox.Cipher{}
	for _, sc := range keyguard.SealedColumns() {
		c, seen := ciphers[sc.Key]
		if !seen {
			if k := keys[sc.Key]; k != nil {
				var err error
				if c, err = sealbox.NewCipher(k); err != nil {
					return fmt.Errorf("db verify: key %s: %w", sc.Key, err)
				}
			}
			ciphers[sc.Key] = c
		}
		col := sealedColumnName(sc.Query)
		sset, sst, err := openSealedColumn(ctx, src, sc, c)
		if err != nil {
			return fmt.Errorf("db verify: source %s.%s: %w", sc.Table, col, err)
		}
		tset, tst, err := openSealedColumn(ctx, dst, sc, c)
		if err != nil {
			return fmt.Errorf("db verify: target %s.%s: %w", sc.Table, col, err)
		}
		sr := SealedReport{
			Key: sc.Key, Table: sc.Table, Column: col, KeyLoaded: c != nil,
			SourceValues: sst.values, TargetValues: tst.values,
			SourceUnopened: sst.unopened, TargetUnopened: tst.unopened,
		}
		sr.Match = sst.unopened == 0 && tst.unopened == 0 && sameMultiset(sset, tset)
		if !sr.Match {
			switch {
			case c == nil:
				rep.problemf("%s.%s: %d source and %d target sealed values, but %s was not loaded", sc.Table, col, sst.values, tst.values, sc.Key)
			case sst.unopened+tst.unopened > 0:
				rep.problemf("%s.%s: %d of %d source values and %d of %d target values do not open with %s",
					sc.Table, col, sst.unopened, sst.values, tst.unopened, tst.values, sc.Key)
			default:
				rep.problemf("%s.%s: the opened values differ (%d source, %d target values)", sc.Table, col, sst.values, tst.values)
			}
		}
		rep.Sealed = append(rep.Sealed, sr)
	}
	return nil
}

// openSealedColumn opens every value of one sealed column with c (nil: no
// key, so every value counts as unopened). Each opened value becomes one
// element: SHA-256 of its length-prefixed AAD parts and the SHA-256 of its
// plaintext. The plaintext is wiped at once and never leaves this function.
func openSealedColumn(ctx context.Context, q store.Querier, sc keyguard.SealedColumn, c *sealbox.Cipher) (map[[sha256.Size]byte]int, sealedStats, error) {
	var st sealedStats
	rows, err := q.QueryContext(ctx, sc.Query)
	if err != nil {
		return nil, st, err
	}
	defer rows.Close()
	cols, err := rows.Columns()
	if err != nil {
		return nil, st, err
	}
	if len(cols) == 0 {
		return nil, st, errors.New("the query selects no column")
	}
	set := map[[sha256.Size]byte]int{}
	vals := make([]sql.NullString, len(cols))
	ptrs := make([]any, len(cols))
	for i := range vals {
		ptrs[i] = &vals[i]
	}
	for rows.Next() {
		if err := rows.Scan(ptrs...); err != nil {
			return nil, st, err
		}
		parts := make([]string, len(cols)-1)
		for i := range parts {
			parts[i] = vals[i].String
		}
		sealed := vals[len(cols)-1].String
		if sc.JSON {
			var s string
			if json.Unmarshal([]byte(sealed), &s) != nil {
				st.values++
				st.unopened++ // not a stored string: cannot be a sealed value
				continue
			}
			sealed = s
		}
		if sealed == "" {
			continue // nothing sealed
		}
		st.values++
		if c == nil {
			st.unopened++
			continue
		}
		pt, err := c.Open(sealed, sc.AAD(parts))
		if err != nil {
			st.unopened++
			continue
		}
		ptSum := sha256.Sum256(pt)
		clear(pt)
		h := sha256.New()
		for _, part := range parts {
			writeField(h, part)
		}
		_, _ = h.Write(ptSum[:])
		var e [sha256.Size]byte
		copy(e[:], h.Sum(nil))
		set[e]++
	}
	return set, st, rows.Err()
}

func sameMultiset(a, b map[[sha256.Size]byte]int) bool {
	if len(a) != len(b) {
		return false
	}
	for k, n := range a {
		if b[k] != n {
			return false
		}
	}
	return true
}

// sealedColumnName is the last column a SealedColumn query selects: the
// sealed value's column.
func sealedColumnName(q string) string {
	up := strings.ToUpper(q)
	sel := strings.Index(up, "SELECT ")
	from := strings.Index(up, " FROM ")
	if sel < 0 || from < sel {
		return "?"
	}
	list := q[sel+len("SELECT ") : from]
	if i := strings.LastIndex(list, ","); i >= 0 {
		list = list[i+1:]
	}
	return strings.TrimSpace(list)
}

// namedRow is a row checked by name, byte for byte.
type namedRow struct {
	name   string
	query  string
	args   []any
	cols   []column
	canary string // the key whose canary this is; "" otherwise
	blob   int    // index in cols of the canary's check_blob
}

func namedRows() []namedRow {
	serverKey := []column{
		{Name: "id", Kind: kindText}, {Name: "purpose", Kind: kindText},
		{Name: "private_key", Kind: kindBytes}, {Name: "public_key", Kind: kindBytes},
		{Name: "created_at", Kind: kindInt, Bits: 64},
	}
	canary := []column{
		{Name: "name", Kind: kindText}, {Name: "kid", Kind: kindText},
		{Name: "check_blob", Kind: kindBytes}, {Name: "created_at", Kind: kindInt, Bits: 64},
	}
	var out []namedRow
	// grant_sign signs Inbox grants, approval_sign approval tokens, vapid
	// every push subscription: a changed key breaks what they signed.
	for _, purpose := range []string{"grant_sign", "approval_sign", "vapid"} {
		out = append(out, namedRow{
			name:  "server_keys " + purpose,
			query: `SELECT id, purpose, private_key, public_key, created_at FROM server_keys WHERE purpose = ?`,
			args:  []any{purpose}, cols: serverKey,
		})
	}
	out = append(out, namedRow{
		name:  "federation_instance",
		query: `SELECT id FROM federation_instance`,
		cols:  []column{{Name: "id", Kind: kindText}},
	})
	for _, k := range []string{keyguard.IdentityKey, keyguard.OAuthKey, keyguard.SecretsKey, keyguard.SessionKey} {
		out = append(out, namedRow{
			name:  "key_canaries " + k,
			query: `SELECT name, kid, check_blob, created_at FROM key_canaries WHERE name = ?`,
			args:  []any{k}, cols: canary, canary: k, blob: 2,
		})
	}
	return out
}

// namedSide is one side's rows of a named row: how many, their hash, and
// the last check_blob read (canaries).
type namedSide struct {
	rows int
	sum  string
	blob []byte
}

func readNamed(ctx context.Context, q store.Querier, nr namedRow) (namedSide, error) {
	var s namedSide
	rows, err := q.QueryContext(ctx, nr.query, nr.args...)
	if err != nil {
		return s, err
	}
	defer rows.Close()
	vals, ptrs := scanTargets(len(nr.cols))
	var sums [][sha256.Size]byte
	for rows.Next() {
		if err := rows.Scan(ptrs...); err != nil {
			return s, err
		}
		var buf []byte
		for i, c := range nr.cols {
			if buf, err = appendCanonical(buf, vals[i], c); err != nil {
				return s, err
			}
		}
		sums = append(sums, sha256.Sum256(buf))
		if nr.canary != "" {
			switch b := vals[nr.blob].(type) {
			case []byte:
				s.blob = b
			case string:
				s.blob = []byte(b)
			}
		}
	}
	if err := rows.Err(); err != nil {
		return s, err
	}
	sort.Slice(sums, func(a, b int) bool { return string(sums[a][:]) < string(sums[b][:]) })
	h := sha256.New()
	_, _ = io.WriteString(h, nr.name)
	for i := range sums {
		_, _ = h.Write(sums[i][:])
	}
	s.rows = len(sums)
	s.sum = fmt.Sprintf("%x", h.Sum(nil))
	return s, nil
}

func verifyNamedRows(ctx context.Context, src, dst store.Querier, keys map[string][]byte, rep *Report) error {
	for _, nr := range namedRows() {
		s, err := readNamed(ctx, src, nr)
		if err != nil {
			return fmt.Errorf("db verify: source %s: %w", nr.name, err)
		}
		t, err := readNamed(ctx, dst, nr)
		if err != nil {
			return fmt.Errorf("db verify: target %s: %w", nr.name, err)
		}
		r := NamedRowReport{
			Name:          nr.name,
			SourcePresent: s.rows > 0,
			TargetPresent: t.rows > 0,
			Match:         s.rows == t.rows && s.sum == t.sum,
		}
		if !r.Match {
			rep.problemf("%s differs: %d source rows, %d target rows", nr.name, s.rows, t.rows)
		}
		if key := keys[nr.canary]; nr.canary != "" && key != nil && t.rows > 0 {
			opens := keyguard.CanaryMatches(nr.canary, key, t.blob)
			r.KeyMatches = &opens
			if !opens {
				rep.problemf("%s: the target's canary does not open with the %s given, so it is not this installation's key", nr.name, nr.canary)
			}
		}
		rep.NamedRows = append(rep.NamedRows, r)
	}
	return nil
}
