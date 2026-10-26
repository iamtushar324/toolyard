// Package builtin holds the Agent Skills that ship inside toolyard: they
// teach any agent connected to a toolyard gateway how toolyard works and
// how to use it well (permissions and the Inbox, code mode, async jobs,
// callbacks, the skills hub, GitHub writes, connecting clients).
//
// The skills live in skills/<name>/ in the Agent Skills layout
// (https://agentskills.io/specification) and are compiled into the binary.
// The skills hub serves them as the read-only source "toolyard"
// (internal/skillhub/builtin.go): the same validation, versions, search,
// grep, listings and bundles as a GitHub source, with no GitHub call. A new
// toolyard binary whose skills differ makes a new version of that source.
package builtin

import (
	"crypto/sha256"
	"embed"
	"encoding/hex"
	"fmt"
	"io/fs"
	"sort"
	"strings"
)

// root is the folder in Skills that holds one folder per skill.
const root = "skills"

//go:embed skills
var skills embed.FS

// Files returns every embedded file by its path relative to the skills
// folder ("toolyard-basics/SKILL.md", "toolyard-basics/references/x.md").
func Files() (map[string][]byte, error) {
	out := map[string][]byte{}
	err := fs.WalkDir(skills, root, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			return nil
		}
		b, err := skills.ReadFile(p)
		if err != nil {
			return err
		}
		out[strings.TrimPrefix(p, root+"/")] = b
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("builtin skills: %w", err)
	}
	return out, nil
}

// Digest identifies a tree of files: the SHA-256, as 64 lowercase hex
// characters, of every path and the SHA-256 of its content, in path order.
// It is the "commit" of the built-in source's version, so a binary with
// the same skills makes no new version.
func Digest(files map[string][]byte) string {
	paths := make([]string, 0, len(files))
	for p := range files {
		paths = append(paths, p)
	}
	sort.Strings(paths)
	h := sha256.New()
	for _, p := range paths {
		sum := sha256.Sum256(files[p])
		fmt.Fprintf(h, "%s\x00%s\n", p, hex.EncodeToString(sum[:]))
	}
	return hex.EncodeToString(h.Sum(nil))
}
