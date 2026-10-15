package main

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/tusharbhardwaj/toolyard/internal/keyguard"
)

const keysUsageText = `usage:
  toolyard keys generate -name identity|oauth|secrets|session [-out FILE]
  toolyard keys generate -all -dir DIR

generate makes fresh master keys in the form toolyard reads: the base64 of
32 random bytes, as serve writes when it creates a key.

With -name it writes one key to stdout (one line), so it can be piped into a
secret store, for example
  toolyard keys generate -name identity | docker secret create toolyard_identity_key -
or, with -out, to a new file with mode 0600. With -all it writes all four
keys into the existing directory DIR as identity.key, oauth.key, secrets.key
and session.key, mode 0600. A file that already exists is never overwritten:
-all then writes nothing. The key id of each key (a fingerprint, not the key)
goes to stderr; keys-check and the serve logs show the same id.

serve -keys=require needs all four before its first start on a new database.
A key generated for a database already in use is a different key: never
replace an existing key file with a new one.
`

// runKeys is `toolyard keys generate` (TEC-1638). keys-check stays its own
// command.
func runKeys(argv []string) error {
	if len(argv) == 0 {
		fmt.Fprint(os.Stderr, keysUsageText)
		return errors.New("toolyard keys: name a subcommand: generate")
	}
	switch argv[0] {
	case "generate":
		return runKeysGenerate(argv[1:], os.Stdout, os.Stderr)
	case "help", "-h", "-help", "--help":
		fmt.Fprint(os.Stdout, keysUsageText)
		return nil
	}
	fmt.Fprint(os.Stderr, keysUsageText)
	return fmt.Errorf("toolyard keys: unknown subcommand %q (want generate)", argv[0])
}

// runKeysGenerate is `toolyard keys generate`. Only the key itself goes to
// stdout, and only without -out and -all; every other line goes to stderr.
func runKeysGenerate(argv []string, stdout, stderr io.Writer) error {
	fs := flag.NewFlagSet("keys generate", flag.ContinueOnError)
	fs.SetOutput(stderr)
	name := fs.String("name", "", "the key to generate: identity | oauth | secrets | session (or the file name, e.g. identity.key)")
	out := fs.String("out", "", "write the key to this new file, mode 0600, instead of stdout; an existing file is never overwritten")
	all := fs.Bool("all", false, "write all four keys into -dir as identity.key, oauth.key, secrets.key and session.key")
	dir := fs.String("dir", "", "with -all: the existing directory for the key files")
	if err := fs.Parse(argv); err != nil {
		return err
	}
	if fs.NArg() > 0 {
		return fmt.Errorf("toolyard keys generate: unexpected arguments %q", fs.Args())
	}

	if *all {
		if *name != "" || *out != "" {
			return errors.New("toolyard keys generate: -all writes every key into -dir; it takes neither -name nor -out")
		}
		if strings.TrimSpace(*dir) == "" {
			return errors.New("toolyard keys generate: -all needs -dir, the directory for the key files")
		}
		made, err := keyguard.WriteNewKeyFiles(*dir, keyguard.ServeKeys())
		if err != nil {
			return fmt.Errorf("toolyard keys generate: %w", err)
		}
		for _, g := range made {
			fmt.Fprintf(stderr, "wrote %s (key id %s)\n", g.Path, g.KeyID)
		}
		return nil
	}
	if *dir != "" {
		return errors.New("toolyard keys generate: -dir goes with -all; for one key use -out FILE")
	}
	if strings.TrimSpace(*name) == "" {
		return errors.New("toolyard keys generate: name the key with -name identity|oauth|secrets|session, or use -all -dir DIR")
	}
	spec, ok := keyguard.SpecByName(*name)
	if !ok {
		return fmt.Errorf("toolyard keys generate: -name must be identity, oauth, secrets or session, not %q", *name)
	}

	if *out != "" {
		g, err := keyguard.WriteNewKeyFile(*out, spec.Name)
		if err != nil {
			return fmt.Errorf("toolyard keys generate: %w", err)
		}
		fmt.Fprintf(stderr, "wrote %s to %s (key id %s)\n", g.Name, g.Path, g.KeyID)
		return nil
	}
	key, encoded, err := keyguard.GenerateKey()
	if err != nil {
		return fmt.Errorf("toolyard keys generate: %w", err)
	}
	// One line; the loader ignores the trailing newline.
	if _, err := fmt.Fprintf(stdout, "%s\n", encoded); err != nil {
		return fmt.Errorf("toolyard keys generate: write the key: %w", err)
	}
	fmt.Fprintf(stderr, "generated %s (key id %s)\n", spec.Name, keyguard.KeyID(spec.Name, key))
	return nil
}
