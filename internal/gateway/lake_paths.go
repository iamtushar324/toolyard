package gateway

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// ingestAllowedRoots returns the absolute, slash-suffixed roots that
// lake.ingest will read from. Override at runtime with the env var
// TOOLYARD_LAKE_INGEST_ROOTS (colon-separated). The default is the
// toolyard inbox under ~/.toolyard/inbox/. Anything else is rejected;
// the agent gets a clear error so it can move the file or ask for a
// path expansion.
func ingestAllowedRoots() []string {
	if v := os.Getenv("TOOLYARD_LAKE_INGEST_ROOTS"); v != "" {
		out := []string{}
		for _, p := range strings.Split(v, ":") {
			abs, err := absUserPath(p)
			if err != nil {
				continue
			}
			out = append(out, ensureTrailingSlash(abs))
		}
		if len(out) > 0 {
			return out
		}
	}
	home, _ := os.UserHomeDir()
	return []string{
		ensureTrailingSlash(filepath.Join(home, ".toolyard", "inbox")),
	}
}

func absUserPath(p string) (string, error) {
	if strings.HasPrefix(p, "~/") || p == "~" {
		home, err := os.UserHomeDir()
		if err != nil {
			return "", err
		}
		p = filepath.Join(home, strings.TrimPrefix(p, "~"))
	}
	abs, err := filepath.Abs(p)
	if err != nil {
		return "", fmt.Errorf("resolve %q: %w", p, err)
	}
	return abs, nil
}

func ensureTrailingSlash(p string) string {
	if strings.HasSuffix(p, string(os.PathSeparator)) {
		return p
	}
	return p + string(os.PathSeparator)
}
