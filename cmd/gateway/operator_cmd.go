package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/tusharbhardwaj/toolyard/internal/audit"
	"github.com/tusharbhardwaj/toolyard/internal/identity"
	"github.com/tusharbhardwaj/toolyard/internal/store"
)

// runOperatorToken manages operator tokens straight from the data directory.
// It is the bootstrap path: whoever can read the data dir is already the
// operator, so no login is needed. The token is written only to --out.
//
//	toolyard operator-token create --data DIR --name NAME --out FILE [--user U] [--scopes read,write] [--ttl 720h]
//	toolyard operator-token list   --data DIR [--user U]
//	toolyard operator-token revoke --data DIR ID
func runOperatorToken(argv []string) error {
	if len(argv) == 0 {
		return errors.New("usage: toolyard operator-token create|list|revoke --data DIR ...")
	}
	sub, argv := argv[0], argv[1:]
	fs := flag.NewFlagSet("operator-token "+sub, flag.ExitOnError)
	dataDir := fs.String("data", defaultDataDir(), "toolyard data directory")
	user := fs.String("user", "", "owner: username, email or user id (default: oldest active admin)")
	name := fs.String("name", "", "token name, e.g. \"claude-code on hypnos\" (create)")
	scopes := fs.String("scopes", "read,write", "comma-separated scopes: read, write, owner (create)")
	ttl := fs.Duration("ttl", 0, "expiry, e.g. 720h; 0 never expires (create)")
	out := fs.String("out", "", "file to write the token to, mode 0600 (create; required)")
	if err := fs.Parse(argv); err != nil {
		return err
	}
	db, err := store.Open(filepath.Join(*dataDir, "toolyard.db"))
	if err != nil {
		return fmt.Errorf("open db: %w", err)
	}
	defer db.Close()
	ctx := context.Background()
	ids := identity.New(db)

	switch sub {
	case "create":
		if *out == "" {
			return errors.New("--out FILE is required; the token is never printed")
		}
		u, err := ids.FindUserForOperator(ctx, *user)
		if err != nil {
			return err
		}
		if u.Role != identity.RoleAdmin {
			return fmt.Errorf("user %s is not an admin", u.Username)
		}
		tok, t, err := ids.CreateOperatorToken(ctx, u.ID, *name, strings.Split(*scopes, ","), *ttl, "cli")
		if err != nil {
			return err
		}
		if err := writeSecretFile(*out, tok); err != nil {
			_ = ids.RevokeOperatorToken(ctx, "", t.ID)
			return err
		}
		_ = audit.New(db).Write(ctx, audit.Event{
			EventType: "operator_token.create", AgentID: "user:" + u.ID,
			ResultSummary: t.ID + " " + t.Name + " [" + strings.Join(t.Scopes, " ") + "] via local cli",
		})
		fmt.Printf("created %s %q for %s, scopes %s; token written to %s\n",
			t.ID, t.Name, u.Username, strings.Join(t.Scopes, " "), *out)
	case "list":
		owner := ""
		if *user != "" {
			u, err := ids.FindUserForOperator(ctx, *user)
			if err != nil {
				return err
			}
			owner = u.ID
		}
		toks, err := ids.ListOperatorTokens(ctx, owner)
		if err != nil {
			return err
		}
		tw := tabwriter.NewWriter(os.Stdout, 0, 2, 2, ' ', 0)
		fmt.Fprintln(tw, "ID\tNAME\tUSER\tSCOPES\tLAST USED\tSTATE")
		for _, t := range toks {
			state := "active"
			switch {
			case t.RevokedAt != 0:
				state = "revoked"
			case t.ExpiresAt != 0 && time.Now().UnixMilli() >= t.ExpiresAt:
				state = "expired"
			}
			fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\t%s\n", t.ID, t.Name, t.UserID,
				strings.Join(t.Scopes, " "), fmtMillis(t.LastUsedAt), state)
		}
		return tw.Flush()
	case "revoke":
		if fs.NArg() != 1 {
			return errors.New("usage: toolyard operator-token revoke --data DIR ID")
		}
		if err := ids.RevokeOperatorToken(ctx, "", fs.Arg(0)); err != nil {
			return err
		}
		_ = audit.New(db).Write(ctx, audit.Event{
			EventType: "operator_token.revoke", ResultSummary: fs.Arg(0) + " via local cli",
		})
		fmt.Println("revoked", fs.Arg(0))
	default:
		return fmt.Errorf("unknown operator-token command %q", sub)
	}
	return nil
}

// writeSecretFile creates path with mode 0600, refusing to overwrite.
func writeSecretFile(path, value string) error {
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return err
	}
	if _, err := f.WriteString(value + "\n"); err != nil {
		f.Close()
		return err
	}
	return f.Close()
}

func fmtMillis(ms int64) string {
	if ms == 0 {
		return "-"
	}
	return time.UnixMilli(ms).UTC().Format("2006-01-02 15:04Z")
}
