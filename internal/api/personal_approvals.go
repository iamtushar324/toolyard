package api

import (
	"github.com/tusharbhardwaj/toolyard/internal/approval"
	"github.com/tusharbhardwaj/toolyard/internal/audit"
	"github.com/tusharbhardwaj/toolyard/internal/identity"
	"github.com/tusharbhardwaj/toolyard/internal/realtime"
	"strings"
)

func approvalVisible(r *approval.Request, u *identity.User) bool {
	if owner := r.PersonalOwner(); owner != "" {
		return owner == u.ID
	}
	return u.Role == identity.RoleAdmin
}

func visibleApprovals(rows []approval.Request, u *identity.User) []approval.Request {
	out := make([]approval.Request, 0, len(rows))
	for _, r := range rows {
		if approvalVisible(&r, u) {
			out = append(out, r)
		}
	}
	return out
}

func personalGitHubTool(name string) bool {
	_, short, ok := strings.Cut(name, ".")
	if !ok {
		return false
	}
	switch short {
	case "get_pull_request", "list_pull_request_files", "list_pull_request_comments", "list_pull_request_reviews", "list_review_comments", "create_pull_request_comment", "submit_pull_request_review", "create_review_comment":
		return true
	}
	return false
}

func visibleAudit(rows []audit.Event, uid string) []audit.Event {
	out := make([]audit.Event, 0, len(rows))
	for _, row := range rows {
		if !personalGitHubTool(row.ToolName) || row.OwnerUserID == uid {
			out = append(out, row)
		}
	}
	return out
}

func personalEventVisible(e realtime.Event, u *identity.User) bool {
	switch data := e.Data.(type) {
	case *approval.Request:
		return approvalVisible(data, u)
	case audit.Event:
		return u.Role == identity.RoleAdmin && (!personalGitHubTool(data.ToolName) || data.OwnerUserID == u.ID)
	case map[string]any:
		if uid, ok := data["user_id"].(string); ok && uid != "" {
			return uid == u.ID
		}
	}
	return u.Role == identity.RoleAdmin
}
