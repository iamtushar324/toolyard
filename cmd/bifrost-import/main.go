// Command bifrost-import copies MCP servers from a Bifrost config store into
// toolyard.
//
// It runs where Bifrost's database and encryption key live: inside the
// Bifrost container, which already has BIFROST_ENCRYPTION_KEY and any env.*
// values its servers refer to. By default it only prints a plan (server
// names, hosts, header names), never a header value or a full URL.
//
// With -apply it signs in to toolyard, stores every header value as a
// toolyard secret and adds each server with secret:// references to them, so
// no credential is printed or written to disk on the way. Servers toolyard
// can't run the same way (local programs, per-person sign-in, private
// addresses) are skipped with the reason.
//
//	docker cp bifrost-import "$CTR":/tmp/bifrost-import
//	docker exec "$CTR" /tmp/bifrost-import                       # plan only
//	docker exec -i "$CTR" /tmp/bifrost-import -apply -password-stdin <<<"$PW"
package main

import (
	"bufio"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	_ "github.com/mattn/go-sqlite3"
)

func main() {
	if err := run(os.Args[1:], os.Stdin, os.Stdout); err != nil {
		fmt.Fprintln(os.Stderr, "bifrost-import:", err)
		os.Exit(1)
	}
}

func run(args []string, stdin io.Reader, stdout io.Writer) error {
	fs := flag.NewFlagSet("bifrost-import", flag.ContinueOnError)
	dbPath := fs.String("db", "/app/data/config.db", "Bifrost config store (SQLite), opened read-only")
	base := fs.String("toolyard", "https://toolyard.dev.beknown.live", "toolyard base URL")
	user := fs.String("user", "tushar", "toolyard local username")
	passwordStdin := fs.Bool("password-stdin", false, "read the toolyard password from the first line of stdin (else $TOOLYARD_PASSWORD)")
	apply := fs.Bool("apply", false, "create the secrets and servers in toolyard (default: print the plan only)")
	only := fs.String("only", "", "comma-separated Bifrost server names to import (default: all)")
	urlMapPath := fs.String("url-map", "", `JSON file {"ServerName": "https://public/url"} for servers Bifrost reaches on a private address`)
	prefix := fs.String("secret-prefix", "BIFROST", "prefix of the toolyard secret names")
	createDisabled := fs.Bool("create-disabled", false, "create every server disabled")
	allowURLCreds := fs.Bool("allow-url-credentials", false, "import servers whose URL seems to carry a credential")
	if err := fs.Parse(args); err != nil {
		return err
	}

	urlMap := map[string]string{}
	if *urlMapPath != "" {
		b, err := os.ReadFile(*urlMapPath)
		if err != nil {
			return err
		}
		if err := json.Unmarshal(b, &urlMap); err != nil {
			return fmt.Errorf("-url-map: %w", err)
		}
	}

	password := os.Getenv("TOOLYARD_PASSWORD")
	if *passwordStdin {
		line, err := bufio.NewReader(stdin).ReadString('\n')
		if err != nil && !errors.Is(err, io.EOF) {
			return fmt.Errorf("read password: %w", err)
		}
		password = strings.TrimRight(line, "\r\n")
	}
	if *apply && password == "" {
		return errors.New("-apply needs the toolyard password: -password-stdin or $TOOLYARD_PASSWORD")
	}

	clients, err := readBifrost(*dbPath, os.Getenv("BIFROST_ENCRYPTION_KEY"))
	if err != nil {
		return err
	}
	if *only != "" {
		want := map[string]bool{}
		for _, n := range strings.Split(*only, ",") {
			want[strings.TrimSpace(n)] = true
		}
		var kept []bifrostClient
		for _, c := range clients {
			if want[c.Name] {
				kept = append(kept, c)
				delete(want, c.Name)
			}
		}
		for n := range want {
			fmt.Fprintf(stdout, "note: %s is not in Bifrost\n", n)
		}
		clients = kept
	}

	// With a password, even the plan checks toolyard for names it already has.
	var api *toolyardAPI
	opt := planOptions{URLMap: urlMap, SecretPrefix: *prefix, CreateDisabled: *createDisabled, AllowURLCredentials: *allowURLCreds}
	var existingSecrets map[string]bool
	if password != "" {
		api = newToolyardAPI(*base)
		if err := api.login(*user, password); err != nil {
			return fmt.Errorf("toolyard sign-in: %w", err)
		}
		if opt.Existing, err = api.serverNames(); err != nil {
			return err
		}
		if existingSecrets, err = api.secretNames(); err != nil {
			return err
		}
	}
	plan := buildPlan(clients, opt)
	printPlan(stdout, plan, opt.Existing != nil)
	if !*apply {
		fmt.Fprintln(stdout, "\nPlan only. Rerun with -apply to create these in toolyard.")
		return nil
	}
	return applyPlan(stdout, api, plan, existingSecrets)
}

func printPlan(w io.Writer, plan []planItem, checkedToolyard bool) {
	imports := 0
	for _, p := range plan {
		if p.Skip == "" {
			imports++
		}
	}
	fmt.Fprintf(w, "Bifrost servers: %d. To import: %d. Skipped: %d.\n", len(plan), imports, len(plan)-imports)
	if !checkedToolyard {
		fmt.Fprintln(w, "(toolyard not checked: names it already has are skipped when applying)")
	}
	for _, p := range plan {
		if p.Skip != "" {
			continue
		}
		fmt.Fprintf(w, "\nIMPORT %s  %s  enabled=%v\n", p.Name, p.Where, p.Server.Enabled)
		for _, s := range p.Secrets {
			fmt.Fprintf(w, "  header %s -> secret://%s\n", s.Header, s.Name)
		}
		for _, n := range p.Notes {
			fmt.Fprintf(w, "  note: %s\n", n)
		}
	}
	for _, p := range plan {
		if p.Skip == "" {
			continue
		}
		where := ""
		if p.Where != "" {
			where = "  " + p.Where
		}
		fmt.Fprintf(w, "\nSKIP %s%s\n  why: %s\n", p.Name, where, p.Skip)
	}
}

// applyPlan creates each server's secrets, then the server. A server whose
// secret names are taken is left alone, and a failed server create removes
// the secrets made for it.
func applyPlan(w io.Writer, api *toolyardAPI, plan []planItem, existingSecrets map[string]bool) error {
	fmt.Fprintln(w, "\nApplying:")
	stamp := time.Now().UTC().Format("2006-01-02")
	var failed int
	for _, p := range plan {
		if p.Skip != "" {
			continue
		}
		var taken []string
		for _, s := range p.Secrets {
			if existingSecrets[s.Name] {
				taken = append(taken, s.Name)
			}
		}
		if len(taken) > 0 {
			fmt.Fprintf(w, "  %s: not created, secret already exists: %s\n", p.Name, strings.Join(taken, ", "))
			failed++
			continue
		}
		var made []string
		var err error
		for _, s := range p.Secrets {
			desc := fmt.Sprintf("Header %s of %s, copied from Bifrost on %s", s.Header, p.Name, stamp)
			if err = api.createSecret(s.Name, s.Value, desc); err != nil {
				break
			}
			made = append(made, s.Name)
		}
		var res serverResult
		var warning string
		if err == nil {
			res, warning, err = api.createServer(p.Server)
		}
		if err != nil {
			for _, n := range made {
				if derr := api.deleteSecret(n); derr != nil {
					fmt.Fprintf(w, "  %s: could not remove secret %s after the failure: %v\n", p.Name, n, derr)
				}
			}
			fmt.Fprintf(w, "  %s: FAILED: %v\n", p.Name, err)
			failed++
			continue
		}
		switch {
		case warning != "":
			fmt.Fprintf(w, "  %s: saved, first connect failed: %s\n", p.Name, warning)
		case !p.Server.Enabled:
			fmt.Fprintf(w, "  %s: saved, disabled\n", p.Name)
		default:
			fmt.Fprintf(w, "  %s: connected, %d tools\n", p.Name, res.ToolCount)
		}
	}
	if failed > 0 {
		return fmt.Errorf("%d server(s) not created", failed)
	}
	return nil
}
