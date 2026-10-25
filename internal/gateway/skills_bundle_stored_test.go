package gateway

import (
	"context"
	"strings"
	"testing"
)

// skills.bundle is refused inside code mode and async jobs, before a
// ticket exists: their results are stored with secrets redacted, so the
// link would come back dead and still use one of the caller's unused-link
// slots. Called directly it works.
func TestSkillsBundleRefusedInCodeModeAndJobs(t *testing.T) {
	f := newSkillsFixture(t)
	for name, ctx := range map[string]context.Context{
		"code mode": withinCodeMode(f.member),
		"async job": jobContext(f.member),
	} {
		res := f.call(t, ctx, SkillsBundleTool, map[string]any{"skills": []any{"deploy"}})
		code, _ := structured(t, res)["error"].(string)
		if !res.IsError || code != SkillsErrInvalid || !strings.Contains(textOf(res), "ty skills pull") {
			t.Fatalf("%s: %s", name, textOf(res))
		}
	}
	var n int
	if err := f.db.QueryRow(`SELECT count(*) FROM skill_bundle_tickets`).Scan(&n); err != nil || n != 0 {
		t.Fatalf("refused bundles minted %d tickets (%v)", n, err)
	}
	out := f.ok(t, f.member, SkillsBundleTool, map[string]any{"skills": []any{"deploy"}})
	if url, _ := out["url"].(string); !strings.Contains(url, SkillBundlePathPrefix) {
		t.Fatalf("direct call: %v", out)
	}
}
