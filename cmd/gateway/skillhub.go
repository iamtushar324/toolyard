package main

// skillhub (slice A): the -skills flag of the skills hub (TEC-1782,
// docs/skills-hub.md).

import (
	"flag"
	"log/slog"
	"strings"
)

// deprecatedSkillsFlags belonged to the removed local skills folder. They
// still parse, so service units that pass them keep starting, and are
// ignored.
var deprecatedSkillsFlags = []string{"skills-dir", "skills-sync-interval", "claude-skills-dir"}

// parseSkillsMode reads -skills: on (the default; auto means on) or off.
// An unknown value is reported and read as on, as the old auto mode did.
func parseSkillsMode(v string, lg *slog.Logger) bool {
	switch strings.ToLower(strings.TrimSpace(v)) {
	case "", "on", "auto", "true", "1", "yes":
		return true
	case "off", "false", "0", "no":
		return false
	}
	if lg != nil {
		lg.Warn("unknown -skills value; the skills hub stays on (use on or off)", "value", v)
	}
	return true
}

// setFlagNames returns which of names were set, on the command line or
// from the environment (loadFlagEnv sets flags through fs.Set).
func setFlagNames(fs *flag.FlagSet, names ...string) []string {
	want := make(map[string]bool, len(names))
	for _, n := range names {
		want[n] = true
	}
	var out []string
	fs.Visit(func(f *flag.Flag) {
		if want[f.Name] {
			out = append(out, f.Name)
		}
	})
	return out
}
