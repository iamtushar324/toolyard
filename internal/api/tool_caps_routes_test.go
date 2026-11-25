package api

import (
	"encoding/json"
	"net/http"
	"testing"

	"github.com/tusharbhardwaj/toolyard/internal/gateway"
	"github.com/tusharbhardwaj/toolyard/internal/toolcaps"
)

// TestToolCapsRoutesAdminOnly: an admin lists, sets and removes caps (each
// change audited as tool_cap.change); a member gets admin_only on every
// route and changes nothing; an unknown tool and an empty cap are 400.
func TestToolCapsRoutesAdminOnly(t *testing.T) {
	e := newAccessTestServer(t)
	e.gw.SetToolCaps(toolcaps.New(e.db))
	admin := e.cookieFor(t, e.admin.ID)
	m := e.member(t, "user_m", "m@beknown.work")
	member := e.cookieFor(t, m.ID)

	expectStatus(t, "member list", e.do(t, member, http.MethodGet, "/v1/tool-caps", ""), http.StatusForbidden, "admin_only")
	expectStatus(t, "member set", e.do(t, member, http.MethodPost, "/v1/tool-caps", `{"tool":"access.whoami","per_caller_daily":3}`), http.StatusForbidden, "admin_only")

	expectStatus(t, "unknown tool", e.do(t, admin, http.MethodPost, "/v1/tool-caps", `{"tool":"nope.nothing","per_caller_daily":3}`), http.StatusBadRequest, "")
	expectStatus(t, "no limit", e.do(t, admin, http.MethodPost, "/v1/tool-caps", `{"tool":"access.whoami"}`), http.StatusBadRequest, "")

	rec := e.do(t, admin, http.MethodPost, "/v1/tool-caps", `{"tool":"access.whoami","per_caller_daily":3,"total_daily":10,"note":"test"}`)
	expectStatus(t, "admin set", rec, http.StatusOK, "")
	var saved struct {
		gateway.ToolCap
		UsedToday []any `json:"used_today"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &saved); err != nil {
		t.Fatal(err)
	}
	if saved.Tool != "access.whoami" || saved.Upstream == "" || saved.PerCallerDaily != 3 || saved.TotalDaily != 10 || saved.UpdatedBy != e.admin.ID {
		t.Fatalf("saved = %+v", saved)
	}

	rec = e.do(t, admin, http.MethodGet, "/v1/tool-caps", "")
	expectStatus(t, "admin list", rec, http.StatusOK, "")
	var list struct {
		Day     string `json:"day"`
		ResetAt string `json:"reset_at"`
		Caps    []struct {
			Tool           string `json:"tool"`
			UsedTodayTotal int64  `json:"used_today_total"`
		} `json:"caps"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &list); err != nil {
		t.Fatal(err)
	}
	if list.Day == "" || list.ResetAt == "" || len(list.Caps) != 1 || list.Caps[0].Tool != "access.whoami" {
		t.Fatalf("list = %+v", list)
	}

	expectStatus(t, "member delete", e.do(t, member, http.MethodDelete, "/v1/tool-caps/access.whoami", ""), http.StatusForbidden, "admin_only")
	expectStatus(t, "admin delete", e.do(t, admin, http.MethodDelete, "/v1/tool-caps/access.whoami", ""), http.StatusOK, "")
	expectStatus(t, "delete again", e.do(t, admin, http.MethodDelete, "/v1/tool-caps/access.whoami", ""), http.StatusNotFound, "")
	if n := e.auditRows(t, gateway.EventToolCapChange, ""); n != 2 {
		t.Fatalf("tool_cap.change rows = %d, want 2 (set, clear)", n)
	}
}
