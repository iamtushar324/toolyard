package approval

import (
	"strings"

	"github.com/tusharbhardwaj/toolyard/internal/actor"
)

// DeciderFromLegacy maps the historical free-text decided_by value to an
// actor.Decider: "token" was a push tap, "telegram:<id>" a Telegram
// button, "rule:<id>" an auto-approval rule, "agent:<id>" the agent
// cancelling its own request, and anything else the id of the dashboard
// user who clicked. Values written by Decider.Legacy ("<via>:<ref>") map
// back to themselves.
func DeciderFromLegacy(decidedBy string) actor.Decider {
	switch decidedBy {
	case "":
		return actor.Decider{}
	case "token", actor.ViaPushToken:
		return actor.Decider{Via: actor.ViaPushToken}
	case actor.ViaExpiry:
		return actor.Decider{Via: actor.ViaExpiry}
	}
	if via, ref, ok := strings.Cut(decidedBy, ":"); ok {
		switch via + ":" {
		case actor.LegacyAutoRulePrefix:
			return actor.Decider{Via: actor.ViaAutoRule, Ref: ref}
		}
		switch via {
		case "telegram":
			return actor.Decider{Via: actor.ViaTelegram, Ref: ref}
		case "agent":
			return actor.Decider{Via: actor.ViaAgentCancel, Ref: ref}
		case actor.ViaDashboard, actor.ViaDashboardBatch, actor.ViaPushToken, actor.ViaPasskey,
			actor.ViaAutoRule, actor.ViaPolicy, actor.ViaInboxGrant, actor.ViaAgentCancel:
			return actor.Decider{Via: via, Ref: ref}
		}
	}
	return actor.Decider{UserID: decidedBy, Via: actor.ViaDashboard}
}

// Decider returns who decided the request and how. Rows written before
// the decider columns existed are mapped from decided_by; an expired row
// reports Via expiry.
func (r *Request) Decider() actor.Decider {
	if r.DecidedVia == "" {
		if r.Status == StatusExpired {
			return actor.Decider{Via: actor.ViaExpiry}
		}
		return DeciderFromLegacy(r.DecidedBy)
	}
	d := actor.Decider{Email: r.DeciderEmail, Name: r.DeciderName, Via: r.DecidedVia, Ref: r.DeciderRef}
	// decided_by holds the user id when a person decided, else via[:ref].
	if r.DecidedBy != "" && r.DecidedBy != d.Legacy() {
		d.UserID = r.DecidedBy
	}
	return d
}
