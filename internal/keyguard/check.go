package keyguard

import (
	"context"
	"database/sql"
	"fmt"
	"path/filepath"
	"strings"
)

// Check statuses, one per key.
const (
	// StatusOK: the key would load (found and matching, found and to be
	// adopted, or missing on a new database and to be created).
	StatusOK = "ok"
	// StatusWarn: the key would load, with a side effect worth knowing
	// (a missing session key is recreated: everyone signs in again).
	StatusWarn = "warn"
	// StatusFail: the start would stop on this key.
	StatusFail = "FAIL"
)

// KeyReport is what Check found for one key. It never holds key material:
// KeyID is the non-secret fingerprint.
type KeyReport struct {
	Name   string
	Status string // StatusOK, StatusWarn or StatusFail
	Path   string // where the key was found; "" when it was not
	KeyID  string // fingerprint of the key found; "" when none was decoded
	Detail string // one line for an operator
}

// Check is a dry run of LoadAll that writes nothing: the same search,
// decode and canary rules, and the same test of a key without a canary
// against the data already sealed with it, reported per key instead of
// acted on. It never creates a key file and never writes a canary. cfg.DB
// may be nil when the database does not exist yet (a first start creates
// it, so it is new), and may be a read-only connection; a missing
// key_canaries table (a database from before canaries) means no canaries.
// The error matches ErrConfig when any key would stop the start, or when
// the database could not be read.
func Check(ctx context.Context, cfg Config, specs []Spec) ([]KeyReport, error) {
	mode, err := ParseMode(string(cfg.Mode))
	if err != nil {
		return nil, err
	}
	canaries := map[string]canary{}
	var sealed []string
	if cfg.DB != nil {
		if canaries, sealed, err = readState(ctx, cfg.DB); err != nil {
			return nil, configErrorf("cannot read the database: %v", err)
		}
	}
	fresh := len(canaries) == 0 && len(sealed) == 0
	dirs := cfg.SearchDirs()

	reports := make([]KeyReport, 0, len(specs))
	failed := 0
	for _, sp := range specs {
		// Notes from the search (a different copy further down) go into
		// this key's line, not the log.
		var notes []string
		quiet := cfg
		quiet.Logf = func(format string, args ...any) {
			notes = append(notes, strings.TrimPrefix(fmt.Sprintf(format, args...), "warning: keys: "))
		}
		r := checkOne(ctx, quiet, sp, mode, dirs, canaries, sealed, fresh)
		if len(notes) > 0 {
			r.Detail += "; note: " + strings.Join(notes, "; ")
		}
		if r.Status == StatusFail {
			failed++
		}
		reports = append(reports, r)
	}
	if failed > 0 {
		return reports, configErrorf("%d of %d keys would stop the start", failed, len(specs))
	}
	return reports, nil
}

func checkOne(ctx context.Context, cfg Config, sp Spec, mode Mode, dirs []string, canaries map[string]canary, sealed []string, fresh bool) KeyReport {
	r := KeyReport{Name: sp.Name}
	key, path, err := find(cfg, sp.Name, dirs)
	if err != nil {
		r.Status, r.Detail = StatusFail, err.Error()
		return r
	}
	if key != nil {
		r.Path, r.KeyID = path, KeyID(sp.Name, key)
		found := fmt.Sprintf("found at %s (key id %s)", path, r.KeyID)
		c, ok := canaries[sp.Name]
		switch {
		case ok && openCanary(sp.Name, key, c.blob):
			r.Status, r.Detail = StatusOK, found+"; matches its canary"
		case ok:
			r.Status, r.Detail = StatusFail, found+fmt.Sprintf("; does not match this database, which expects key id %s", c.kid)
		case cfg.DB == nil:
			r.Status, r.Detail = StatusOK, found+"; no canary yet: adopted on the next start"
		default:
			// As LoadAll: adopted only if it opens the data sealed with it.
			d, err := checkSealedData(ctx, cfg.DB, sp.Name, key)
			switch {
			case err != nil:
				r.Status, r.Detail = StatusFail, found+fmt.Sprintf("; cannot check it against the data: %v", err)
			case !d.ok():
				r.Status, r.Detail = StatusFail, found+fmt.Sprintf("; does not open existing data: none of the %d sealed values tried opened, starting with %s", d.tried, d.failed)
			case d.opened != "":
				r.Status, r.Detail = StatusOK, found+fmt.Sprintf("; no canary yet: opens existing data in %s, so it is adopted on the next start", d.opened)
			default:
				r.Status, r.Detail = StatusOK, found+"; no canary yet: adopted on the next start"
			}
		}
		return r
	}
	tried := triedPaths(sp.Name, dirs)
	create := filepath.Join(cfg.DataDir, sp.Name)
	c, hasCanary := canaries[sp.Name]
	switch {
	case mode == Require:
		r.Status, r.Detail = StatusFail, fmt.Sprintf("not found (tried %s); -keys=require never creates a key", tried)
	case strings.TrimSpace(cfg.DataDir) == "":
		r.Status, r.Detail = StatusFail, fmt.Sprintf("not found (tried %s), and no data directory to create it in", tried)
	case sp.Recreatable && !fresh:
		r.Status, r.Detail = StatusWarn, fmt.Sprintf("not found (tried %s); the next start creates a new one at %s and everyone signs in again", tried, create)
	case sp.Recreatable || fresh:
		r.Status, r.Detail = StatusOK, fmt.Sprintf("not found (tried %s); the first start creates it at %s (new database)", tried, create)
	case hasCanary:
		r.Status, r.Detail = StatusFail, fmt.Sprintf("not found (tried %s), but this database was set up with it (key id %s)", tried, c.kid)
	case len(canaries) > 0:
		r.Status, r.Detail = StatusFail, fmt.Sprintf("not found (tried %s), and this database is already in use (it has key canaries)", tried)
	default:
		r.Status, r.Detail = StatusFail, fmt.Sprintf("not found (tried %s), and this database already holds sealed data (%s)", tried, strings.Join(sealed, ", "))
	}
	return r
}

// readState reads the canaries (none when the table does not exist yet)
// and the tables that hold sealed rows.
func readState(ctx context.Context, db *sql.DB) (map[string]canary, []string, error) {
	canaries := map[string]canary{}
	ok, err := tableExists(ctx, db, "key_canaries")
	if err != nil {
		return nil, nil, err
	}
	if ok {
		if canaries, err = readCanaries(ctx, db); err != nil {
			return nil, nil, err
		}
	}
	sealed, err := sealedTables(ctx, db)
	if err != nil {
		return nil, nil, err
	}
	return canaries, sealed, nil
}
