package inbox

import (
	"context"
	"crypto/ed25519"
	"database/sql"
	"encoding/base64"
	"errors"
	"fmt"
	"log"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/tusharbhardwaj/toolyard/internal/actor"
)

// Attention decides when the owner's phone buzzes (inbox-spec.md §6).
//
//   - now:    pushed at once, one notification per request. Each agent gets
//     NowPerHour of them; beyond that the request is lowered to soon and
//     marked "downgraded".
//   - soon:   held for GroupWindow, then pushed as one notification per
//     session ("3 requests from Ship billing v2").
//   - digest: never pushed on its own; collected into the next digest.
//   - fyi:    never pushed.
//
// During quiet hours everything is held until they end, except `now`
// access requests whose tools are all on the quiet-hours allow list. A
// request whose agent is blocked on the owner gets one reminder. A snoozed
// request is pushed again when the snooze ends.
//
// Pushes carry no agent-written text unless the owner turns on details
// (the payload transits third-party push services). Notification actions
// never approve: allowing tools happens on the review page, after reading.

// Attention defaults.
const (
	DefaultNowPerHour = 3
	GroupWindow       = 90 * time.Second
	MinReminderAfter  = 15 * time.Minute
	MaxReminderAfter  = 4 * time.Hour
	digestGrace       = time.Hour
	tapTokenPrefix    = "tyt_"
)

// DefaultDigestTimes are the owner's digest slots unless changed.
var DefaultDigestTimes = []string{"09:30", "13:30", "18:30"}

// AttentionConfig is the owner's notification settings.
type AttentionConfig struct {
	NowPerHour  int
	QuietHours  string   // "22:00-07:00", or "" for none
	QuietAllow  []string // tools (exact, or "prefix.*") that may break quiet hours with urgency now
	DigestTimes []string // "HH:MM" in Location
	Location    *time.Location
	Details     bool // include titles and summaries in pushes
}

// Push is one notification for the owner's devices.
type Push struct {
	Title     string       `json:"title"`
	Body      string       `json:"body"`
	Tag       string       `json:"tag"`
	URL       string       `json:"url"`
	RequestID string       `json:"request_id,omitempty"`
	Actions   []PushAction `json:"actions,omitempty"`
	// TapToken lets the service worker act on a notification without a
	// session cookie. It names one request and the actions offered.
	// It is the unbound (legacy) form; the sender should prefer
	// TapTokenFor, which also names the person the notification went to,
	// so a tap is attributed to them.
	TapToken string `json:"inbox_token,omitempty"`
	// TapTokenFor mints the token for one recipient (a user id). nil
	// when the notification offers no actions.
	TapTokenFor func(userID string) string `json:"-"`
	Reason      string                     `json:"reason"` // new | grouped | reminder | snooze_end | digest
}

// PushAction is one notification button.
type PushAction struct {
	Action string `json:"action"`
	Title  string `json:"title"`
}

// Tap actions a notification can carry.
const (
	TapDeny   = "deny"
	TapSnooze = "snooze"
	TapOption = "opt" // opt0, opt1: answer a question
)

func (s *Service) attention() AttentionConfig {
	var c AttentionConfig
	if s.opts.Attention != nil {
		c = s.opts.Attention()
	}
	if c.NowPerHour <= 0 {
		c.NowPerHour = DefaultNowPerHour
	}
	if c.DigestTimes == nil {
		c.DigestTimes = DefaultDigestTimes
	}
	if c.Location == nil {
		c.Location = time.Local
	}
	return c
}

// ParseClock parses "HH:MM" into minutes after midnight.
func ParseClock(v string) (int, bool) {
	v = strings.TrimSpace(v)
	h, m, ok := strings.Cut(v, ":")
	if !ok || len(h) == 0 || len(h) > 2 || len(m) != 2 {
		return 0, false
	}
	hh, err1 := strconv.Atoi(h)
	mm, err2 := strconv.Atoi(m)
	if err1 != nil || err2 != nil || hh < 0 || hh > 23 || mm < 0 || mm > 59 {
		return 0, false
	}
	return hh*60 + mm, true
}

// ParseQuietHours parses "HH:MM-HH:MM". Empty means none.
func ParseQuietHours(v string) (start, end int, ok bool) {
	v = strings.TrimSpace(v)
	if v == "" {
		return 0, 0, false
	}
	a, b, found := strings.Cut(v, "-")
	if !found {
		return 0, 0, false
	}
	start, ok1 := ParseClock(a)
	end, ok2 := ParseClock(b)
	if !ok1 || !ok2 || start == end {
		return 0, 0, false
	}
	return start, end, true
}

func atClock(t time.Time, mins int) time.Time {
	return time.Date(t.Year(), t.Month(), t.Day(), mins/60, mins%60, 0, 0, t.Location())
}

// quietWindow reports whether t falls in quiet hours and, if so, when they end.
func quietWindow(c AttentionConfig, t time.Time) (bool, time.Time) {
	start, end, ok := ParseQuietHours(c.QuietHours)
	if !ok {
		return false, time.Time{}
	}
	t = t.In(c.Location)
	m := t.Hour()*60 + t.Minute()
	var in bool
	if start < end {
		in = m >= start && m < end
	} else {
		in = m >= start || m < end
	}
	if !in {
		return false, time.Time{}
	}
	e := atClock(t, end)
	if !e.After(t) {
		e = atClock(t.AddDate(0, 0, 1), end)
	}
	return true, e
}

func toolAllowedInQuiet(allow []string, tool string) bool {
	for _, a := range allow {
		a = strings.TrimSpace(a)
		switch {
		case a == "":
		case a == "*" || a == tool:
			return true
		case strings.HasSuffix(a, "*") && strings.HasPrefix(tool, strings.TrimSuffix(a, "*")):
			return true
		}
	}
	return false
}

// breaksQuiet reports whether a request may interrupt quiet hours.
func breaksQuiet(c AttentionConfig, r *Request, urgency string) bool {
	if urgency != UrgencyNow {
		return false
	}
	if r.Kind != KindAccess || len(r.Tools) == 0 {
		return toolAllowedInQuiet(c.QuietAllow, "*")
	}
	for _, t := range r.Tools {
		if !toolAllowedInQuiet(c.QuietAllow, t.Tool) {
			return false
		}
	}
	return true
}

// HighRisk reports whether allowing this request needs the extra
// confirmation step: any tool flagged red or Production.
func HighRisk(r *Request) bool {
	for _, t := range r.Tools {
		if toolHighRisk(t) {
			return true
		}
	}
	return false
}

func toolHighRisk(t ToolRequest) bool {
	for _, f := range t.Flags {
		if f.Level == LevelRed || f.Label == FlagProduction {
			return true
		}
	}
	return false
}

// ---- arrival ---------------------------------------------------------------------

// capUrgency applies the per-agent `now` budget before a request is stored.
// It returns a warning for the agent when the urgency was lowered.
func (s *Service) capUrgency(ctx context.Context, r *Request) *Problem {
	if r.Urgency != UrgencyNow || r.Kind == KindUpdate {
		return nil
	}
	c := s.attention()
	since := s.now().Add(-time.Hour).UnixMilli()
	var n int
	if err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM inbox_requests WHERE agent_id = ? AND created_at >= ?
		AND (urgency = 'now' OR json_extract(doc, '$.requested_urgency') = 'now')`, r.AgentID, since).Scan(&n); err != nil {
		return nil
	}
	if n < c.NowPerHour {
		return nil
	}
	r.RequestedUrgency = UrgencyNow
	r.Urgency = UrgencySoon
	r.Downgraded = fmt.Sprintf("more than %d urgent requests from this agent in an hour", c.NowPerHour)
	return &Problem{"urgency", fmt.Sprintf("lowered to soon: you've sent %d `now` requests in the last hour (limit %d). Keep `now` for production being down or money or data being lost.", n, c.NowPerHour)}
}

func groupKey(r *Request) string {
	if r.SessionID != "" {
		return r.SessionID
	}
	return "agent:" + r.AgentID
}

func (s *Service) enqueuePush(ctx context.Context, r *Request, reason, group string, due time.Time) {
	if _, err := s.db.ExecContext(ctx, `INSERT INTO inbox_pushes(request_id, agent_id, group_key, reason, urgency, due_at) VALUES (?,?,?,?,?,?)`,
		r.ID, r.AgentID, group, reason, r.Urgency, due.UnixMilli()); err != nil {
		log.Printf("inbox: queue push for %s: %v", r.ID, err)
	}
}

// enqueueArrival queues the push for a newly stored request.
func (s *Service) enqueueArrival(ctx context.Context, r *Request) {
	if r.Kind == KindUpdate {
		return
	}
	now := s.now()
	switch r.Urgency {
	case UrgencyNow:
		s.enqueuePush(ctx, r, "new", r.ID, now)
	case UrgencySoon:
		s.enqueuePush(ctx, r, "new", groupKey(r), now.Add(GroupWindow))
	}
}

// ---- dispatch --------------------------------------------------------------------

type pushRow struct {
	id                                int64
	requestID, group, reason, urgency string
}

// Tick runs one round of the attention loop: due pushes, reminders and
// the digest. RunAttention calls it on an interval; tests call it directly.
func (s *Service) Tick(ctx context.Context) {
	s.attnMu.Lock()
	defer s.attnMu.Unlock()
	s.scanReminders(ctx)
	s.dispatchLocked(ctx)
	s.runDigest(ctx)
}

// RunAttention runs Tick on an interval until ctx is done.
func (s *Service) RunAttention(ctx context.Context, every time.Duration) {
	t := time.NewTicker(every)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			s.Tick(ctx)
		}
	}
}

func (s *Service) markPush(ctx context.Context, id int64, outcome string) {
	_, _ = s.db.ExecContext(ctx, `UPDATE inbox_pushes SET sent_at = ?, outcome = ? WHERE id = ?`, s.now().UnixMilli(), outcome, id)
}

// dispatchPushes sends due notifications. One dispatcher runs at a time,
// so a row is never sent twice.
func (s *Service) dispatchPushes(ctx context.Context) {
	s.attnMu.Lock()
	defer s.attnMu.Unlock()
	s.dispatchLocked(ctx)
}

func (s *Service) dispatchLocked(ctx context.Context) {
	now := s.now()
	rows, err := s.db.QueryContext(ctx, `SELECT id, request_id, group_key, reason, urgency FROM inbox_pushes
		WHERE sent_at IS NULL AND due_at <= ? ORDER BY due_at, id`, now.UnixMilli())
	if err != nil {
		return
	}
	var due []pushRow
	for rows.Next() {
		var p pushRow
		if rows.Scan(&p.id, &p.requestID, &p.group, &p.reason, &p.urgency) == nil {
			due = append(due, p)
		}
	}
	rows.Close()
	if len(due) == 0 {
		return
	}
	c := s.attention()
	quiet, quietEnd := quietWindow(c, now)

	type item struct {
		row pushRow
		r   *Request
	}
	groups := map[string][]item{}
	var order []string
	for _, p := range due {
		r, err := s.Get(ctx, p.requestID)
		if err != nil || !r.IsOpen() {
			s.markPush(ctx, p.id, "skipped")
			continue
		}
		if r.SnoozedUntil > now.UnixMilli() {
			// Snoozed: the snooze_end row will bring it back.
			s.markPush(ctx, p.id, "skipped")
			continue
		}
		if p.reason == "new" && r.RemindedAt > 0 {
			s.markPush(ctx, p.id, "skipped")
			continue
		}
		if quiet && !breaksQuiet(c, r, p.urgency) {
			_, _ = s.db.ExecContext(ctx, `UPDATE inbox_pushes SET due_at = ? WHERE id = ?`, quietEnd.UnixMilli(), p.id)
			continue
		}
		key := "one:" + p.requestID
		if p.reason == "new" && p.urgency == UrgencySoon {
			key = p.group
		}
		if _, seen := groups[key]; !seen {
			order = append(order, key)
		}
		// Never two rows for the same request in one notification.
		dup := false
		for _, it := range groups[key] {
			if it.r.ID == r.ID {
				dup = true
			}
		}
		if dup {
			s.markPush(ctx, p.id, "grouped")
			continue
		}
		groups[key] = append(groups[key], item{p, r})
	}
	for _, key := range order {
		items := groups[key]
		if len(items) == 1 {
			s.send(ctx, s.itemPush(ctx, c, items[0].r, items[0].row.reason))
			s.markPush(ctx, items[0].row.id, "sent")
			continue
		}
		reqs := make([]*Request, len(items))
		for i, it := range items {
			reqs[i] = it.r
		}
		s.send(ctx, s.groupPush(ctx, c, key, reqs))
		for _, it := range items {
			s.markPush(ctx, it.row.id, "grouped")
		}
	}
}

func (s *Service) send(ctx context.Context, p Push) {
	if s.opts.Notify != nil {
		s.opts.Notify(ctx, p)
	}
}

var kindPhrase = map[string]string{
	KindAccess:   "is asking for access",
	KindQuestion: "has a question",
	KindBlocker:  "is stuck",
	KindUpdate:   "sent an update",
}

// itemPush builds the notification for one request.
func (s *Service) itemPush(ctx context.Context, c AttentionConfig, r *Request, reason string) Push {
	who := s.agentName(ctx, r.AgentID)
	if who == "" {
		who = "An agent"
	}
	p := Push{Tag: r.ID, URL: "/#inbox/" + r.ID, RequestID: r.ID, Reason: reason}
	if c.Details {
		p.Title = r.Title
		p.Body = who + ": " + r.Summary
		if r.Kind == KindAccess {
			p.Body += "\n" + ScopeLine(r)
		}
	} else {
		p.Title = who + " " + kindPhrase[r.Kind]
		p.Body = "Tap to review."
		if r.Kind == KindAccess {
			p.Body = fmt.Sprintf("%d tool%s. Tap to review.", len(r.Tools), plural(len(r.Tools)))
		}
	}
	switch reason {
	case "reminder":
		p.Title = "Still waiting: " + p.Title
	case "snooze_end":
		p.Title = "Back from snooze: " + p.Title
	}
	if r.Kind == KindAccess && HighRisk(r) {
		p.Body += "\nHigh risk: open it on your phone to review."
	}

	actions := []string{}
	switch r.Kind {
	case KindAccess:
		p.Actions = []PushAction{{TapDeny, "Deny"}, {TapSnooze, "Snooze 1h"}}
		actions = []string{TapDeny, TapSnooze}
	case KindQuestion, KindBlocker:
		if r.SchemaVersion < 2 && c.Details && len(r.Options) > 0 && len(r.Options) <= 2 {
			for i, o := range r.Options {
				a := TapOption + strconv.Itoa(i)
				p.Actions = append(p.Actions, PushAction{a, truncate(o.Label, 24)})
				actions = append(actions, a)
			}
		} else {
			p.Actions = []PushAction{{TapSnooze, "Snooze 1h"}}
			actions = []string{TapSnooze}
		}
	}
	if len(actions) > 0 {
		exp := time.UnixMilli(r.ExpiresAt)
		id := r.ID
		p.TapToken = s.tapToken(id, actions, exp)
		p.TapTokenFor = func(userID string) string { return s.tapTokenFor(id, actions, exp, userID) }
	}
	return p
}

func (s *Service) groupPush(ctx context.Context, c AttentionConfig, key string, rs []*Request) Push {
	from := s.agentName(ctx, rs[0].AgentID)
	if c.Details && rs[0].SessionID != "" {
		if ss, err := s.GetSession(ctx, rs[0].SessionID); err == nil {
			from = ss.Title
		}
	}
	if from == "" {
		from = "an agent"
	}
	p := Push{Title: fmt.Sprintf("%d requests from %s", len(rs), from), Tag: key, URL: "/#inbox", Reason: "grouped"}
	p.Body = countPhrase(rs) + ". Tap to review."
	if c.Details {
		var titles []string
		for _, r := range rs {
			titles = append(titles, "• "+r.Title)
		}
		p.Body = strings.Join(titles, "\n")
	}
	return p
}

// countPhrase describes a set of requests: "2 access requests and 1 question".
func countPhrase(rs []*Request) string {
	n := map[string]int{}
	for _, r := range rs {
		n[r.Kind]++
	}
	names := []struct{ kind, one, many string }{
		{KindAccess, "access request", "access requests"},
		{KindQuestion, "question", "questions"},
		{KindBlocker, "blocker", "blockers"},
		{KindUpdate, "update", "updates"},
	}
	var parts []string
	for _, nm := range names {
		if k := n[nm.kind]; k > 0 {
			w := nm.many
			if k == 1 {
				w = nm.one
			}
			parts = append(parts, fmt.Sprintf("%d %s", k, w))
		}
	}
	switch len(parts) {
	case 0:
		return "nothing"
	case 1:
		return parts[0]
	}
	return strings.Join(parts[:len(parts)-1], ", ") + " and " + parts[len(parts)-1]
}

// ScopeLine is the one-line scope shown in a push and on the watch:
// "deploy.run env=prod · +2 tools · 30m".
func ScopeLine(r *Request) string {
	if len(r.Tools) == 0 {
		return ""
	}
	t := r.Tools[0]
	keys := make([]string, 0, len(t.Params))
	for k := range t.Params {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var ps []string
	for _, k := range keys {
		if len(ps) == 2 {
			break
		}
		d := t.Params[k].Describe()
		if u, err := strconv.Unquote(d); err == nil {
			d = u
		}
		ps = append(ps, k+"="+truncate(d, 24))
	}
	line := t.Tool
	if len(ps) > 0 {
		line += " " + strings.Join(ps, " ")
	}
	if len(r.Tools) > 1 {
		line += fmt.Sprintf(" · +%d tool%s", len(r.Tools)-1, plural(len(r.Tools)-1))
	}
	if r.TTLSeconds > 0 {
		line += " · " + shortDuration(time.Duration(r.TTLSeconds)*time.Second)
	}
	return line
}

func shortDuration(d time.Duration) string {
	switch {
	case d >= time.Hour && d%time.Hour == 0:
		return fmt.Sprintf("%dh", int(d/time.Hour))
	case d >= time.Minute:
		return fmt.Sprintf("%dm", int(d/time.Minute))
	}
	return fmt.Sprintf("%ds", int(d/time.Second))
}

func truncate(s string, n int) string {
	rs := []rune(s)
	if len(rs) <= n {
		return s
	}
	return string(rs[:n-1]) + "…"
}

// ---- reminders ---------------------------------------------------------------------

// reminderAfter is max(15 min, the owner's median decision time), capped.
func (s *Service) reminderAfter(ctx context.Context) time.Duration {
	since := s.now().Add(-30 * 24 * time.Hour).UnixMilli()
	rows, err := s.db.QueryContext(ctx, `SELECT updated_at - created_at FROM inbox_requests
		WHERE created_at >= ? AND status IN ('approved','denied','returned','answered') ORDER BY 1 LIMIT 1000`, since)
	if err != nil {
		return MinReminderAfter
	}
	var ds []int64
	for rows.Next() {
		var d int64
		if rows.Scan(&d) == nil {
			ds = append(ds, d)
		}
	}
	rows.Close()
	after := MinReminderAfter
	if len(ds) >= 5 {
		if p50 := time.Duration(ds[len(ds)/2]) * time.Millisecond; p50 > after {
			after = p50
		}
	}
	if after > MaxReminderAfter {
		after = MaxReminderAfter
	}
	return after
}

// scanReminders queues the one reminder a blocked request gets.
func (s *Service) scanReminders(ctx context.Context) {
	open, err := s.List(ctx, ListFilter{Open: true, Limit: 500})
	if err != nil || len(open) == 0 {
		return
	}
	now := s.now()
	var after time.Duration
	for i := range open {
		r := &open[i]
		if r.Kind == KindUpdate || r.RemindedAt > 0 || r.SnoozedUntil > now.UnixMilli() {
			continue
		}
		if r.Urgency == UrgencyFYI {
			continue
		}
		blocked := r.Kind == KindBlocker
		if !blocked && r.SessionID != "" {
			if ss, err := s.GetSession(ctx, r.SessionID); err == nil && ss.Status == SessionBlockedOnOwner {
				blocked = true
			}
		}
		if !blocked {
			continue
		}
		if after == 0 {
			after = s.reminderAfter(ctx)
		}
		if now.Sub(time.UnixMilli(r.CreatedAt)) < after {
			continue
		}
		var out *Request
		if err := s.mutate(ctx, r.ID, func(x *Request) error {
			if x.RemindedAt > 0 || !x.IsOpen() {
				return errSkip
			}
			x.RemindedAt = now.UnixMilli()
			x.addActivity(x.RemindedAt, "Reminded you: the agent is blocked on this")
			out = x
			return nil
		}); err != nil || out == nil {
			continue
		}
		s.enqueuePush(ctx, out, "reminder", out.ID, now)
		s.publish("inbox", s.cardView(ctx, out))
	}
}

var errSkip = errors.New("skip")

// ---- digest ----------------------------------------------------------------------

// digestSlot returns the latest digest time at or before t.
func digestSlot(c AttentionConfig, t time.Time) (time.Time, bool) {
	t = t.In(c.Location)
	var best time.Time
	for _, day := range []time.Time{t, t.AddDate(0, 0, -1)} {
		for _, v := range c.DigestTimes {
			m, ok := ParseClock(v)
			if !ok {
				continue
			}
			at := atClock(day, m)
			if !at.After(t) && at.After(best) {
				best = at
			}
		}
	}
	return best, !best.IsZero()
}

func (s *Service) runDigest(ctx context.Context) {
	c := s.attention()
	now := s.now()
	slot, ok := digestSlot(c, now)
	if !ok || now.Sub(slot) > digestGrace {
		return
	}
	if quiet, _ := quietWindow(c, now); quiet {
		return
	}
	key := slot.Format("2006-01-02T15:04")
	var exists int
	if err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM inbox_digests WHERE slot = ?`, key).Scan(&exists); err != nil || exists > 0 {
		return
	}
	open, err := s.List(ctx, ListFilter{Open: true, Limit: 500})
	if err != nil {
		return
	}
	var items []*Request
	agents := map[string]bool{}
	for i := range open {
		r := &open[i]
		if r.Urgency != UrgencyDigest || r.DigestedAt > 0 || r.SnoozedUntil > now.UnixMilli() {
			continue
		}
		items = append(items, r)
		agents[r.AgentID] = true
	}
	res, err := s.db.ExecContext(ctx, `INSERT OR IGNORE INTO inbox_digests(slot, sent_at, items) VALUES (?,?,?)`, key, now.UnixMilli(), len(items))
	if err != nil {
		return
	}
	if n, _ := res.RowsAffected(); n == 0 || len(items) == 0 {
		return
	}
	for _, r := range items {
		_ = s.mutate(ctx, r.ID, func(x *Request) error { x.DigestedAt = now.UnixMilli(); return nil })
	}
	p := Push{Title: "Your toolyard digest", Tag: "digest", URL: "/#inbox/updates", Reason: "digest"}
	p.Body = fmt.Sprintf("%s from %d agent%s.", capitalize(countPhrase(items)), len(agents), plural(len(agents)))
	if c.Details {
		for i, r := range items {
			if i == 4 {
				p.Body += fmt.Sprintf("\n…and %d more", len(items)-4)
				break
			}
			p.Body += "\n• " + r.Title
		}
	}
	s.send(ctx, p)
}

func capitalize(s string) string {
	if s == "" {
		return s
	}
	return strings.ToUpper(s[:1]) + s[1:]
}

// ---- notification taps -----------------------------------------------------------

// ErrTapToken is returned for a bad, expired or mismatched tap token.
var ErrTapToken = errors.New("this notification has expired; open the request to decide")

// Tap tokens come in two formats. v1 (three fields) named the request,
// the actions and the expiry; v2 adds the recipient's user id, so the tap
// is attributed to the person the notification went to. v1 tokens already
// on phones stay valid until they expire.
func tapMessage(id, actions string, exp int64) []byte {
	return []byte("toolyard-inbox-tap-v1\n" + id + "\n" + actions + "\n" + strconv.FormatInt(exp, 10))
}

func tapMessageFor(id, actions string, exp int64, userID string) []byte {
	return []byte("toolyard-inbox-tap-v2\n" + id + "\n" + actions + "\n" + strconv.FormatInt(exp, 10) + "\n" + userID)
}

// tapToken signs "this request, these actions, until then" (v1).
func (s *Service) tapToken(id string, actions []string, exp time.Time) string {
	acts := strings.Join(actions, ",")
	e := exp.UnixMilli()
	sig := ed25519.Sign(s.signer.priv, tapMessage(id, acts, e))
	body := id + "|" + acts + "|" + strconv.FormatInt(e, 10)
	return tapTokenPrefix + base64.RawURLEncoding.EncodeToString([]byte(body)) + "." + base64.RawURLEncoding.EncodeToString(sig)
}

// tapTokenFor signs "this request, these actions, until then, for this
// person" (v2). A userID containing '|' is refused (empty token) rather
// than producing an ambiguous body.
func (s *Service) tapTokenFor(id string, actions []string, exp time.Time, userID string) string {
	userID = strings.TrimSpace(userID)
	if userID == "" || strings.ContainsAny(userID, "|\n") {
		return ""
	}
	acts := strings.Join(actions, ",")
	e := exp.UnixMilli()
	sig := ed25519.Sign(s.signer.priv, tapMessageFor(id, acts, e, userID))
	body := id + "|" + acts + "|" + strconv.FormatInt(e, 10) + "|" + userID
	return tapTokenPrefix + base64.RawURLEncoding.EncodeToString([]byte(body)) + "." + base64.RawURLEncoding.EncodeToString(sig)
}

// verifyTap returns the request id the token names and, for a v2 token,
// the user it was issued to.
func (s *Service) verifyTap(tok, action string) (id, userID string, err error) {
	tok = strings.TrimSpace(tok)
	if !strings.HasPrefix(tok, tapTokenPrefix) {
		return "", "", ErrTapToken
	}
	b, sigs, ok := strings.Cut(strings.TrimPrefix(tok, tapTokenPrefix), ".")
	if !ok {
		return "", "", ErrTapToken
	}
	body, err1 := base64.RawURLEncoding.DecodeString(b)
	sig, err2 := base64.RawURLEncoding.DecodeString(sigs)
	if err1 != nil || err2 != nil || len(sig) != ed25519.SignatureSize {
		return "", "", ErrTapToken
	}
	parts := strings.Split(string(body), "|")
	var msg []byte
	switch len(parts) {
	case 3:
		exp, perr := strconv.ParseInt(parts[2], 10, 64)
		if perr != nil {
			return "", "", ErrTapToken
		}
		msg = tapMessage(parts[0], parts[1], exp)
	case 4:
		exp, perr := strconv.ParseInt(parts[2], 10, 64)
		if perr != nil || parts[3] == "" {
			return "", "", ErrTapToken
		}
		msg = tapMessageFor(parts[0], parts[1], exp, parts[3])
		userID = parts[3]
	default:
		return "", "", ErrTapToken
	}
	if !ed25519.Verify(s.signer.pub, msg, sig) {
		return "", "", ErrTapToken
	}
	exp, _ := strconv.ParseInt(parts[2], 10, 64)
	if s.now().UnixMilli() >= exp {
		return "", "", ErrTapToken
	}
	for _, a := range strings.Split(parts[1], ",") {
		if a == action {
			return parts[0], userID, nil
		}
	}
	return "", "", ErrTapToken
}

// DecideByTap applies a notification action. Only the actions the
// notification offered are accepted, and none of them allow tools. The
// decision is recorded as made through the push token, by the person the
// token was issued to when it is a bound (v2) token.
func (s *Service) DecideByTap(ctx context.Context, token, action string) (*Request, error) {
	id, userID, err := s.verifyTap(token, action)
	if err != nil {
		return nil, err
	}
	d := Decision{By: "notification", Decider: actor.Decider{UserID: userID, Via: actor.ViaPushToken}}
	switch {
	case action == TapDeny:
		d.Action = "deny"
	case action == TapSnooze:
		d.Action, d.SnoozeMinutes = "snooze", 60
	case strings.HasPrefix(action, TapOption):
		n, err := strconv.Atoi(strings.TrimPrefix(action, TapOption))
		if err != nil {
			return nil, ErrTapToken
		}
		d.Action, d.Option = "answer", &n
	default:
		return nil, ErrTapToken
	}
	return s.Decide(ctx, id, d)
}

// PendingPushes counts queued, unsent notifications (for the settings card).
func (s *Service) PendingPushes(ctx context.Context) (int, error) {
	var n int
	err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM inbox_pushes WHERE sent_at IS NULL`).Scan(&n)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, nil
	}
	return n, err
}

// Info is what the owner's settings screen shows about the inbox.
type Info struct {
	JudgeAvailable bool   `json:"judge_available"`
	VoiceAvailable bool   `json:"voice_available"`
	PendingPushes  int    `json:"pending_pushes"`
	QuietNow       bool   `json:"quiet_now"`
	QuietUntil     int64  `json:"quiet_until,omitempty"`
	NextDigest     int64  `json:"next_digest,omitempty"`
	Timezone       string `json:"timezone"`
}

// Info reports the inbox's current attention state.
func (s *Service) Info(ctx context.Context) Info {
	c := s.attention()
	now := s.now()
	in := Info{JudgeAvailable: s.opts.Judge != nil, VoiceAvailable: s.voiceAvailable(), Timezone: c.Location.String()}
	in.PendingPushes, _ = s.PendingPushes(ctx)
	if q, end := quietWindow(c, now); q {
		in.QuietNow, in.QuietUntil = true, end.UnixMilli()
	}
	if next, ok := nextDigest(c, now); ok {
		in.NextDigest = next.UnixMilli()
	}
	return in
}

// nextDigest is the first digest slot after t.
func nextDigest(c AttentionConfig, t time.Time) (time.Time, bool) {
	t = t.In(c.Location)
	var best time.Time
	for _, day := range []time.Time{t, t.AddDate(0, 0, 1)} {
		for _, v := range c.DigestTimes {
			m, ok := ParseClock(v)
			if !ok {
				continue
			}
			at := atClock(day, m)
			if at.After(t) && (best.IsZero() || at.Before(best)) {
				best = at
			}
		}
	}
	return best, !best.IsZero()
}
