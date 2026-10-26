package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/tusharbhardwaj/toolyard/internal/identity"
)

// runOwner shows the owner and is the host operator's break-glass for the
// owner role, straight from the data directory, like operator-token:
// whoever can read the data directory is already the operator. It works
// with the server stopped or running, and is how ownership moves when the
// owner is gone (lost their account, or left the organisation and was
// blocked by the Clerk sync).
//
//	toolyard owner show     --data DIR
//	toolyard owner transfer --data DIR --to USER [--unblock]
func runOwner(argv []string, out io.Writer) error {
	if len(argv) == 0 {
		return errors.New("usage: toolyard owner show|transfer --data DIR [--to USER] [--unblock]")
	}
	sub, argv := argv[0], argv[1:]
	fs := flag.NewFlagSet("owner "+sub, flag.ExitOnError)
	dataDir := fs.String("data", defaultDataDir(), "toolyard data directory")
	to := fs.String("to", "", "the new owner: username, email or user id (transfer; required)")
	unblock := fs.Bool("unblock", false, "re-activate the new owner if they are blocked (transfer)")
	credentialsDir := fs.String("credentials-dir", os.Getenv("TOOLYARD_CREDENTIALS_DIR"), "directory searched for toolyard.env and database-url after $CREDENTIALS_DIRECTORY, as for serve. Defaults to $TOOLYARD_CREDENTIALS_DIR.")
	if err := fs.Parse(argv); err != nil {
		return err
	}
	// The same database as serve, found the way operator-token finds it.
	rep, err := loadFlagEnv(fs, processEnv(), credentialsDir, "data", "credentials-dir")
	if err != nil {
		return err
	}
	for _, w := range rep.warnings {
		fmt.Fprintln(os.Stderr, "warning:", w)
	}
	target, err := resolveDatabase(*dataDir, *credentialsDir, os.Getenv)
	if err != nil {
		return err
	}
	ctx := context.Background()
	db, err := openDatabase(ctx, target, 2)
	if err != nil {
		return fmt.Errorf("open db (%s): %w", target.describe(), err)
	}
	defer db.Close()
	return ownerCommand(ctx, identity.New(db), sub, *to, *unblock, out)
}

// ownerCommand runs one owner subcommand against ids.
func ownerCommand(ctx context.Context, ids *identity.Service, sub, to string, unblock bool, out io.Writer) error {
	switch sub {
	case "show":
		u, err := ids.Owner(ctx)
		if errors.Is(err, identity.ErrNoUser) {
			fmt.Fprintln(out, "no owner yet: the first account set up, or the first sign-in, becomes the owner")
			return nil
		}
		if err != nil {
			return err
		}
		fmt.Fprintf(out, "owner %s (%s) email=%s role=%s status=%s\n", u.Username, u.ID, orDefault(u.Email, "-"), u.Role, u.Status)
		if u.Status != identity.StatusActive {
			fmt.Fprintf(out, "the owner is blocked (%s): move the role with: toolyard owner transfer --to <username|email>\n", orDefault(u.BlockedReason, "manual"))
		}
		return nil
	case "transfer":
		if strings.TrimSpace(to) == "" {
			return errors.New("--to USER is required (username, email or user id)")
		}
		u, err := ids.FindUserForOperator(ctx, to)
		if err != nil {
			return err
		}
		prev, err := ids.ForceTransferOwnership(ctx, u.ID, identity.ActorCLI, unblock)
		switch {
		case errors.Is(err, identity.ErrUserBlocked):
			return fmt.Errorf("user %s is blocked; pass --unblock to re-activate them as the new owner", u.Username)
		case errors.Is(err, identity.ErrAlreadyOwner):
			return fmt.Errorf("user %s is already the owner", u.Username)
		case err != nil:
			return err
		}
		from := "nobody"
		if prev != "" && prev != u.ID {
			if p, err := ids.GetUserByID(ctx, prev); err == nil {
				from = p.Username
			} else {
				from = prev
			}
		}
		if prev == u.ID {
			fmt.Fprintf(out, "%s stays the owner, now an active admin; audited as %s\n", u.Username, identity.ActorCLI)
			return nil
		}
		fmt.Fprintf(out, "owner %s -> %s; %s is an admin now", from, u.Username, u.Username)
		if prev != "" {
			fmt.Fprintf(out, ", and %s keeps their role and status", from)
		}
		fmt.Fprintf(out, "; audited as %s\n", identity.ActorCLI)
		return nil
	default:
		return fmt.Errorf("unknown owner command %q (show, transfer)", sub)
	}
}
