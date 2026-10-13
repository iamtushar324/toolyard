package inbox

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"sort"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/google/uuid"

	"github.com/tusharbhardwaj/toolyard/internal/actor"
	"github.com/tusharbhardwaj/toolyard/internal/store"
)

// Tunables.
const (
	RequestTTL         = 24 * time.Hour
	UpdateTTL          = 7 * 24 * time.Hour
	MaxPendingPerAgent = 16
	DryRunsPerHour     = 20
	dryRunWindow       = 2 * time.Hour
	judgeTimeout       = 25 * time.Second
	MaxWait            = 5 * time.Minute
)

// Errors returned to callers.
var (
	ErrExpired         = errors.New("this request has expired")
	ErrConflict        = errors.New("this request or submission has changed")
	ErrNotFound        = errors.New("not found")
	ErrNotPending      = errors.New("this request has already been decided")
	ErrRequiredRefused = errors.New("a required tool was refused; send the request back to the agent instead")
	ErrNothingAllowed  = errors.New("allow at least one tool, or deny the request")
	ErrBadDecision     = errors.New("that action doesn't apply to this kind of request")
	ErrWiden           = errors.New("you can narrow a request but not widen it")
	ErrTooManyPending  = fmt.Errorf("you already have %d requests waiting for your owner; cancel or wait for some first", MaxPendingPerAgent)
	ErrDryRunRateLimit = fmt.Errorf("dry-run limit reached (%d per hour)", DryRunsPerHour)
	ErrSessionNotYours = errors.New("that session doesn't belong to you")
	ErrRelatedNotYours = errors.New("request_id doesn't match one of your requests")
)

// Fetcher copies a linked file into toolyard's own store.
type Fetcher interface {
	Snapshot(ctx context.Context, rawURL, kind string) (sha, contentType string, size int64, err error)
}

// DecisionCallbacks writes subscriptions and decision events inside the Inbox transaction.
type DecisionCallbacks interface {
	RegisterTx(context.Context, *sql.Tx, *Request, string) error
	DecisionTx(context.Context, *sql.Tx, *Request) error
}

// Options configure a Service.
type Options struct {
	LegacyDecided func(context.Context, string)

	Callbacks            DecisionCallbacks
	MaxPendingTTLSeconds int
	DB                   *store.DB
	Catalog              Catalog
	// Judge, when non-nil and JudgeEnabled returns true, reviews requests.
	Judge        Judge
	JudgeEnabled func() bool
	// Fetcher, when non-nil and SnapshotEnabled returns true, copies
	// linked media when a request is sent.
	Fetcher         Fetcher
	SnapshotEnabled func() bool
	// AllowPrivateMedia accepts media links on private networks. The
	// Fetcher must be configured to match (Snapshotter.AllowPrivateNetworks).
	AllowPrivateMedia bool
	// Publish sends a realtime event to the dashboard.
	Publish func(eventType string, data any)
	// Notify delivers a push notification to the owner's devices. The
	// attention loop (Tick) decides when; see attention.go.
	Notify func(ctx context.Context, p Push)
	// Attention returns the owner's notification settings.
	Attention func() AttentionConfig
	// Voice records voice notes server-side when VoiceEnabled returns
	// true; Blobs stores them. Otherwise the browser speaks the script.
	Voice        Voice
	VoiceEnabled func() bool
	Blobs        BlobStore
	// Passkeys, when set, gates high-risk approvals on a passkey once the
	// owner has registered one.
	Passkeys PasskeyGate
	// AgentName resolves an agent ID to a display name.
	AgentName func(ctx context.Context, agentID string) string
	Now       func() time.Time
}

// Service is the inbox.
type Service struct {
	db           *store.DB
	opts         Options
	signer       *grantSigner
	now          func() time.Time
	bg           sync.WaitGroup
	submitMu     sync.Mutex
	watchMu      sync.Mutex
	watch        map[string]chan struct{}
	attnMu       sync.Mutex // one attention round (Tick / dispatch) at a time
	voiceMu      sync.Mutex
	voiceFlights map[string]*voiceFlight
}

// New creates the service and loads (or creates) the grant signing key.
func New(ctx context.Context, opts Options) (*Service, error) {
	signer, err := loadGrantSigner(ctx, opts.DB)
	if err != nil {
		return nil, fmt.Errorf("grant signing key: %w", err)
	}
	now := opts.Now
	if now == nil {
		now = time.Now
	}
	service := &Service{db: opts.DB, opts: opts, signer: signer, now: now, watch: map[string]chan struct{}{}}
	if _, err := service.RecoverExecutions(ctx); err != nil {
		return nil, fmt.Errorf("execution recovery: %w", err)
	}
	return service, nil
}

// SetCatalog wires the gateway after construction (the gateway and the
// inbox depend on each other).
func (s *Service) SetCatalog(c Catalog) { s.opts.Catalog = c }

// Flush waits for background checks to finish. Used by tests.
func (s *Service) Flush() { s.bg.Wait() }

// Close waits up to timeout for background checks, so shutdown isn't held
// hostage by a slow download. Unfinished checks are redone at the next
// start by RecheckUnchecked.
func (s *Service) Close(timeout time.Duration) {
	done := make(chan struct{})
	go func() { s.bg.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(timeout):
		log.Printf("inbox: shutdown with background checks still running; they'll be redone at next start")
	}
}

// RecheckUnchecked restarts the background check for pending requests
// whose check never finished (e.g. the gateway stopped mid-check).
func (s *Service) RecheckUnchecked(ctx context.Context) int {
	open, err := s.List(ctx, ListFilter{Open: true, Limit: 500})
	if err != nil {
		return 0
	}
	n := 0
	for _, r := range open {
		if r.Checked {
			continue
		}
		n++
		s.bg.Add(1)
		go func(id string) {
			defer s.bg.Done()
			s.backgroundCheck(context.Background(), id)
		}(r.ID)
	}
	return n
}

func (s *Service) publish(t string, data any) {
	if s.opts.Publish != nil {
		s.opts.Publish(t, data)
	}
}

func (s *Service) agentName(ctx context.Context, id string) string {
	if s.opts.AgentName != nil {
		if n := s.opts.AgentName(ctx, id); n != "" {
			return n
		}
	}
	return "agent " + shortID(id)
}

func shortID(id string) string {
	if len(id) > 8 {
		return id[:8]
	}
	return id
}

// ---- submission --------------------------------------------------------------

// SubmitResult is what the agent gets back from inbox.request/ask/post.
type SubmitResult struct {
	OK        bool       `json:"ok"`
	DryRun    bool       `json:"dry_run,omitempty"`
	RequestID string     `json:"request_id,omitempty"`
	Status    string     `json:"status,omitempty"`
	Problems  []Problem  `json:"problems"`
	Warnings  []Problem  `json:"warnings,omitempty"`
	Flags     []ToolFlag `json:"flags"`
	Preview   *Preview   `json:"preview,omitempty"`
	ExpiresAt int64      `json:"expires_at,omitempty"`
}

// ToolFlag is a flag with the tool it's on ("" for the whole request).
type ToolFlag struct {
	Tool string `json:"tool,omitempty"`
	Flag
}

// Preview is how the request will look on the owner's phone.
type Preview struct {
	CardTitle    string `json:"card_title"`
	AudioSeconds int    `json:"audio_seconds"`
	Tools        int    `json:"tools,omitempty"`
	Attachments  int    `json:"attachments"`
}

// Submit validates a submission and, unless it's a dry run, stores it.
// Validation problems are not errors: they come back in the result with
// OK=false so the agent can fix them.
func (s *Service) Submit(ctx context.Context, agentID string, sub *Submission) (*SubmitResult, error) {
	if sub.Kind == "" {
		sub.Kind = KindAccess
	}
	s.submitMu.Lock()
	defer s.submitMu.Unlock()
	fingerprint := hashJSON(sub)
	if sub.ClientRequestID != "" && !sub.DryRun {
		var doc string
		err := s.db.QueryRowContext(ctx, `SELECT doc FROM inbox_requests WHERE agent_id=? AND json_extract(doc,'$.client_request_id')=?`, agentID, sub.ClientRequestID).Scan(&doc)
		if err == nil {
			var prev Request
			if err = json.Unmarshal([]byte(doc), &prev); err != nil {
				return nil, err
			}
			if prev.SubmissionFingerprint != fingerprint {
				return nil, ErrConflict
			}
			return &SubmitResult{OK: true, RequestID: prev.ID, Status: prev.Status, ExpiresAt: prev.ExpiresAt}, nil
		}
		if !errors.Is(err, sql.ErrNoRows) {
			return nil, err
		}
	}
	r, probs, warns := Validate(ctx, s.opts.Catalog, agentID, sub, ValidateOptions{AllowPrivateMedia: s.opts.AllowPrivateMedia})
	r.AgentID = agentID
	if s.opts.MaxPendingTTLSeconds > 0 && r.PendingTTLSeconds > s.opts.MaxPendingTTLSeconds {
		probs = append(probs, Problem{"pending_ttl_seconds", "exceeds the administrator deadline limit"})
	}
	r.SubmissionFingerprint = fingerprint
	for i := range r.Tools {
		r.Tools[i].Flags = RuleFlags(r.Tools[i])
	}
	res := &SubmitResult{DryRun: sub.DryRun, Problems: probs, Warnings: warns}
	if res.Problems == nil {
		res.Problems = []Problem{}
	}

	if r.SessionID != "" {
		if sess, err := s.GetSession(ctx, r.SessionID); err != nil || sess.AgentID != agentID {
			res.Problems = append(res.Problems, Problem{"session_id", ErrSessionNotYours.Error()})
		}
	}
	if r.RelatedID != "" {
		if rel, err := s.Get(ctx, r.RelatedID); err != nil || rel.AgentID != agentID {
			res.Problems = append(res.Problems, Problem{"request_id", ErrRelatedNotYours.Error()})
		}
	}

	if sub.DryRun {
		if n, err := s.countDryRuns(ctx, agentID, time.Hour); err == nil && n >= DryRunsPerHour {
			return nil, ErrDryRunRateLimit
		}
		s.checkMedia(ctx, r, &res.Problems, false)
		if s.judgeOn() {
			jctx, cancel := context.WithTimeout(ctx, judgeTimeout)
			rv, err := s.opts.Judge.Review(jctx, r)
			cancel()
			if err != nil {
				res.Warnings = append(res.Warnings, Problem{"", "the judge model couldn't review this draft: " + err.Error()})
			} else {
				applyReview(r, rv)
			}
		}
		res.Flags = toolFlags(r)
		res.Preview = preview(r)
		res.OK = len(res.Problems) == 0
		labels := flagLabels(r)
		lb, _ := json.Marshal(labels)
		_, _ = s.db.ExecContext(ctx, `INSERT INTO inbox_dry_runs(agent_id, title, flags, created_at) VALUES (?,?,?,?)`,
			agentID, r.Title, string(lb), s.now().UnixMilli())
		return res, nil
	}

	res.Flags = toolFlags(r)
	res.Preview = preview(r)
	if len(res.Problems) > 0 {
		return res, nil
	}
	if r.Kind != KindUpdate {
		n, err := s.countPending(ctx, agentID)
		if err != nil {
			return nil, err
		}
		if n >= MaxPendingPerAgent {
			return nil, ErrTooManyPending
		}
	}

	if w := s.capUrgency(ctx, r); w != nil {
		res.Warnings = append(res.Warnings, *w)
	}
	now := s.now()
	r.ID = "rq_" + strings.ReplaceAll(uuid.NewString(), "-", "")[:16]
	r.Status = StatusPending
	r.CreatedAt = now.UnixMilli()
	r.UpdatedAt = r.CreatedAt
	r.ExpiresAt = now.Add(RequestTTL).UnixMilli()
	if r.Kind == KindAccess {
		r.ExpiresAt = now.Add(time.Duration(r.PendingTTLSeconds) * time.Second).UnixMilli()
	}
	if r.Kind == KindUpdate {
		r.ExpiresAt = now.Add(UpdateTTL).UnixMilli()
	}
	r.DryRunCount, _ = s.countDryRunsTitled(ctx, agentID, r.Title)
	r.addActivity(r.CreatedAt, "Sent by "+s.agentName(ctx, agentID))
	if r.DryRunCount > 0 {
		r.addActivity(r.CreatedAt, fmt.Sprintf("%d dry run%s before sending", r.DryRunCount, plural(r.DryRunCount)))
	}
	if r.Downgraded != "" {
		r.addActivity(r.CreatedAt, "Urgency lowered from now to soon: "+r.Downgraded)
	}
	if err := s.insert(ctx, r); err != nil {
		return nil, err
	}
	s.heartbeat(ctx, r.SessionID)
	res.OK, res.RequestID, res.Status, res.ExpiresAt = true, r.ID, r.Status, r.ExpiresAt

	s.publish("inbox", s.cardView(ctx, r))
	s.enqueueArrival(ctx, r)
	if r.Urgency == UrgencyNow {
		// Don't wait for the next tick for the one urgency that means it,
		// and don't hold the agent's call while the push goes out.
		s.bg.Add(1)
		go func() {
			defer s.bg.Done()
			s.dispatchPushes(context.Background())
		}()
	}
	s.bg.Add(1)
	go func(id string) {
		defer s.bg.Done()
		s.backgroundCheck(context.Background(), id)
	}(r.ID)
	return res, nil
}

// backgroundCheck copies linked media and runs the judge, then marks the
// request checked.
func (s *Service) backgroundCheck(ctx context.Context, id string) {
	r, err := s.Get(ctx, id)
	if err != nil {
		return
	}
	var probs []Problem
	s.checkMedia(ctx, r, &probs, true)
	var rv *Review
	var judgeErr error
	if s.judgeOn() {
		jctx, cancel := context.WithTimeout(ctx, judgeTimeout)
		rv, judgeErr = s.opts.Judge.Review(jctx, r)
		cancel()
	}
	dryLabels, _ := s.dryRunLabels(ctx, r.AgentID, r.Title)
	err = s.mutate(ctx, id, func(cur *Request) error {
		cur.Attachments = r.Attachments
		applyReview(cur, rv)
		cur.Checked = true
		final := map[string]bool{}
		for _, l := range flagLabels(cur) {
			final[l] = true
		}
		for _, l := range dryLabels {
			if !final[l] {
				cur.DroppedFlags = append(cur.DroppedFlags, l)
			}
		}
		n := len(cur.AllFlags())
		switch {
		case judgeErr != nil:
			cur.addActivity(s.now().UnixMilli(), fmt.Sprintf("Toolyard checked it by rule (%d flag%s); the judge model failed: %v", n, plural(n), judgeErr))
		case n == 0:
			cur.addActivity(s.now().UnixMilli(), "Toolyard checked it in the background. Nothing to flag.")
		default:
			cur.addActivity(s.now().UnixMilli(), fmt.Sprintf("Toolyard checked it in the background and added %d flag%s", n, plural(n)))
		}
		if len(cur.DroppedFlags) > 0 {
			cur.addActivity(s.now().UnixMilli(), "Flags seen in dry runs but gone now: "+strings.Join(cur.DroppedFlags, ", "))
		}
		return nil
	})
	if err != nil {
		log.Printf("inbox: background check %s: %v", id, err)
		return
	}
	if cur, err := s.Get(ctx, id); err == nil {
		s.publish("inbox", s.cardView(ctx, cur))
	}
}

func (s *Service) judgeOn() bool {
	return s.opts.Judge != nil && s.opts.JudgeEnabled != nil && s.opts.JudgeEnabled()
}

// checkMedia fetches (store=true) or probes linked media. Failures become
// problems on dry runs and fetch_error on stored requests.
func (s *Service) checkMedia(ctx context.Context, r *Request, probs *[]Problem, stored bool) {
	if s.opts.Fetcher == nil || s.opts.SnapshotEnabled == nil || !s.opts.SnapshotEnabled() {
		return
	}
	var wg sync.WaitGroup
	var mu sync.Mutex
	for i := range r.Attachments {
		a := &r.Attachments[i]
		if a.Type != "image" && a.Type != "video" && a.Type != "file" {
			continue
		}
		wg.Add(1)
		go func(i int, a *Attachment) {
			defer wg.Done()
			sha, ct, size, err := s.opts.Fetcher.Snapshot(ctx, a.URL, a.Type)
			var perr error
			if err == nil && a.Type == "video" && a.PosterURL != "" {
				psha, _, _, e := s.opts.Fetcher.Snapshot(ctx, a.PosterURL, "image")
				if e == nil {
					a.PosterBlob = psha
				} else {
					perr = e
				}
			}
			mu.Lock()
			defer mu.Unlock()
			if err != nil {
				a.FetchError = err.Error()
				*probs = append(*probs, Problem{fmt.Sprintf("attachments[%d].url", i), err.Error()})
				return
			}
			a.Blob, a.ContentType, a.Size = sha, ct, size
			if perr != nil {
				*probs = append(*probs, Problem{fmt.Sprintf("attachments[%d].poster_url", i), perr.Error()})
			}
		}(i, a)
	}
	wg.Wait()
	sort.Slice(*probs, func(i, j int) bool { return (*probs)[i].Path < (*probs)[j].Path })
}

func toolFlags(r *Request) []ToolFlag {
	out := []ToolFlag{}
	for _, f := range r.Flags {
		out = append(out, ToolFlag{Flag: f})
	}
	for _, t := range r.Tools {
		for _, f := range t.Flags {
			out = append(out, ToolFlag{Tool: t.Tool, Flag: f})
		}
	}
	return out
}

func flagLabels(r *Request) []string {
	seen := map[string]bool{}
	var out []string
	for _, f := range r.AllFlags() {
		if !seen[f.Label] {
			seen[f.Label] = true
			out = append(out, f.Label)
		}
	}
	sortStrings(out)
	return out
}

func preview(r *Request) *Preview {
	return &Preview{
		CardTitle:    r.Title,
		AudioSeconds: int(float64(len(strings.Fields(r.Audio.Script)))/2.6 + 0.5),
		Tools:        len(r.Tools),
		Attachments:  len(r.Attachments),
	}
}

// ---- owner decisions -----------------------------------------------------------

// Decision is what the owner chose.
type CallVerdict struct {
	Verdict string `json:"verdict"`
	Reason  string `json:"reason,omitempty"`
}

type Decision struct {
	Verdicts        map[string]CallVerdict `json:"verdicts,omitempty"`
	Response        *Response              `json:"response,omitempty"`
	RequestRevision int                    `json:"request_revision,omitempty"`
	SubmissionID    string                 `json:"submission_id,omitempty"`

	Action        string `json:"action"` // approve | deny | return | answer | snooze | read
	Allow         []bool `json:"allow,omitempty"`
	Note          string `json:"note,omitempty"`
	Option        *int   `json:"option,omitempty"`
	SnoozeMinutes int    `json:"snooze_minutes,omitempty"`
	// Params narrows allowed tools' parameters, by tool index: each
	// constraint must be within what the agent asked for.
	Params map[int]map[string]Constraint `json:"params,omitempty"`
	// TTLSeconds shortens how long the grants last (never lengthens).
	TTLSeconds int `json:"ttl_seconds,omitempty"`
	// Passkey is a WebAuthn assertion, required to allow high-risk tools
	// once the owner has registered a passkey (see passkey.go).
	Passkey *PasskeyAssertion `json:"passkey,omitempty"`
	// By is the display label for DecidedBy (a username). Decider says
	// who decided and how; when it is empty By stands in as the name.
	By      string        `json:"-"`
	Decider actor.Decider `json:"-"`
}

// decider is the structured actor of the decision: Decider when set, else
// By as a dashboard user's name.
func (d Decision) decider() actor.Decider {
	if !d.Decider.IsZero() {
		return d.Decider
	}
	return actor.Decider{Name: d.By, Via: actor.ViaDashboard}
}

// deciderLabel is how activity lines name the person: their name, else
// their email, else "You" (the single-owner wording).
func deciderLabel(d actor.Decider) string {
	switch {
	case d.Name != "":
		return d.Name
	case d.Email != "":
		return d.Email
	}
	return "You"
}

// deciderSuffix says how the decision was made when that matters.
func deciderSuffix(d actor.Decider) string {
	switch d.Via {
	case actor.ViaPushToken:
		return " from a notification"
	case actor.ViaPasskey:
		return " with a passkey"
	}
	return ""
}

// Decide applies an owner decision.
func (s *Service) Decide(ctx context.Context, id string, d Decision) (*Request, error) {
	now := s.now().UnixMilli()
	if d.Action == "submit" {
		d.Action = "approve"
	}
	if d.Verdicts != nil {
		pre, err := s.Get(ctx, id)
		if err != nil {
			return nil, err
		}
		d.Allow = make([]bool, len(pre.Tools))
		for i, t := range pre.Tools {
			d.Allow[i] = d.Verdicts[t.CallID].Verdict == VerdictAccepted
		}
	}
	// Apply the shared callback contract before passkey verification can
	// change credential counters, or any decision transaction starts.
	if utf8.RuneCountInString(d.Note) > 10000 {
		// Old answers/denials could contain larger notes. A matching retry
		// reads the committed result without rewriting that history.
		if d.SubmissionID != "" {
			r, err := s.Get(ctx, id)
			if err != nil {
				return nil, err
			}
			if r.Status != StatusPending {
				switch {
				case r.Kind == KindAccess && r.DecisionSubmissionID == d.SubmissionID:
					if r.DecisionFingerprint != decisionFingerprint(d) {
						return nil, ErrConflict
					}
					r.Replayed = true
					return r, nil
				case d.Action == "answer" && (r.Kind == KindQuestion || r.Kind == KindBlocker) && r.AnswerSubmissionID == d.SubmissionID:
					if r.AnswerFingerprint != answerFingerprint(d) {
						return nil, ErrConflict
					}
					r.Replayed = true
					return r, nil
				}
			}
		}
		return nil, fmt.Errorf("note: max 10000 characters")
	}
	dec := d.decider()
	// Passkey check happens before the transaction (verification writes
	// the credential's sign count) and is re-checked inside it, in case
	// the judge flagged a tool in between.
	passkeyOn, verified := false, false
	if d.Action == "approve" && s.opts.Passkeys != nil {
		var err error
		if gate, ok := s.opts.Passkeys.(interface {
			CheckEnabledFor(context.Context, string) (bool, error)
		}); ok && dec.UserID != "" {
			passkeyOn, err = gate.CheckEnabledFor(ctx, dec.UserID)
		} else {
			passkeyOn, err = s.opts.Passkeys.CheckEnabled(ctx)
		}
		if err != nil {
			return nil, fmt.Errorf("%w: cannot check registered passkeys", ErrPasskeyFailed)
		}
	}
	if passkeyOn {
		if pre, err := s.Get(ctx, id); err == nil && pre.IsOpen() && NeedsPasskey(pre, d.Allow) {
			if d.Passkey == nil {
				return nil, ErrPasskeyRequired
			}
			var credID string
			var err error
			if gate, ok := s.opts.Passkeys.(interface {
				VerifyCredentialFor(context.Context, string, string, *PasskeyAssertion) (string, error)
			}); ok && dec.UserID != "" {
				credID, err = gate.VerifyCredentialFor(ctx, dec.UserID, DecisionDigest(id, d), d.Passkey)
			} else {
				credID, err = s.opts.Passkeys.VerifyCredential(ctx, DecisionDigest(id, d), d.Passkey)
			}
			if err != nil {
				return nil, fmt.Errorf("%w: %v", ErrPasskeyFailed, err)
			}
			verified = true
			// The passkey is the instrument that authorised this decision.
			dec.Via, dec.Ref = actor.ViaPasskey, credID
		}
	}
	who, how := deciderLabel(dec), deciderSuffix(dec)
	var out *Request
	err := s.mutateTx(ctx, id, func(tx *sql.Tx, r *Request) error {
		if r.Kind == KindAccess && d.SubmissionID != "" && r.DecisionSubmissionID == d.SubmissionID {
			if r.DecisionFingerprint != decisionFingerprint(d) {
				return ErrConflict
			}
			r.Replayed = true
			out = r
			return nil
		}
		if d.Action == "answer" && d.SubmissionID != "" && r.AnswerSubmissionID == d.SubmissionID {
			if r.AnswerFingerprint != answerFingerprint(d) {
				return ErrConflict
			}
			r.Replayed = true
			out = r
			return nil
		}
		if r.ExpiresAt > 0 && s.now().UnixMilli() >= r.ExpiresAt {
			return ErrExpired
		}
		if d.Action != "snooze" && r.Status != StatusPending {
			return ErrNotPending
		}
		if r.Kind == KindAccess && d.Action == "approve" {
			if d.RequestRevision != r.Revision || d.SubmissionID == "" || len(d.SubmissionID) > 128 {
				return fmt.Errorf("%w: a decision needs request_revision and submission_id", ErrConflict)
			}
			if err := validateVerdicts(r, d); err != nil {
				return err
			}
		}
		note := strings.TrimSpace(d.Note)
		if d.Action != "snooze" {
			// Before the switch: issueGrants stamps issued_by from it.
			r.setDecider(dec, d.By, now)
		}
		switch d.Action {
		case "approve":
			if r.Kind != KindAccess {
				return ErrBadDecision
			}
			if len(d.Allow) != len(r.Tools) {
				return fmt.Errorf("allow must have one entry per tool (%d)", len(r.Tools))
			}
			n := 0
			for i := range r.Tools {
				if d.Allow[i] {
					n++
				}
			}
			if passkeyOn && !verified && NeedsPasskey(r, d.Allow) {
				return ErrPasskeyRequired
			}
			if r.ExecutionMode == "legacy" && (len(d.Params) > 0 || d.TTLSeconds != 0) {
				return fmt.Errorf("legacy parameters are fixed; reject and request a new Inbox item to change scope")
			}
			var narrowed []string
			for i, tighter := range d.Params {
				if i < 0 || i >= len(r.Tools) || !d.Allow[i] || len(tighter) == 0 {
					continue
				}
				np, err := Narrow(r.Tools[i].Params, tighter)
				if err != nil {
					return fmt.Errorf("%w: %s: %v", ErrWiden, r.Tools[i].Tool, err)
				}
				r.Tools[i].Requested = r.Tools[i].Params
				r.Tools[i].Params = np
				narrowed = append(narrowed, r.Tools[i].Tool)
			}
			if d.TTLSeconds != 0 {
				if d.TTLSeconds < MinTTL || d.TTLSeconds > r.TTLSeconds {
					return fmt.Errorf("%w: ttl_seconds must be between %d and the requested %d", ErrWiden, MinTTL, r.TTLSeconds)
				}
				r.TTLSeconds = d.TTLSeconds
			}
			for i := range r.Tools {
				r.Tools[i].Reason = strings.TrimSpace(d.Verdicts[r.Tools[i].CallID].Reason)
				if d.Allow[i] {
					r.Tools[i].Decision = ToolAllowed
					r.Tools[i].Verdict = VerdictAccepted
				} else {
					r.Tools[i].Decision = ToolRefused
					r.Tools[i].Verdict = VerdictRejected
				}
			}
			if len(narrowed) > 0 {
				sort.Strings(narrowed)
				r.addActivity(now, "You narrowed "+strings.Join(narrowed, ", "))
			}
			ids, err := s.issueGrants(ctx, tx, r, now)
			if err != nil {
				return err
			}
			for i, gid := range ids {
				r.Tools[i].GrantID = gid
			}
			r.Status = StatusApproved
			if n == 0 {
				r.Status = StatusDenied
			} else if r.ExecutionMode != "legacy" {
				r.GrantsExpire = now + int64(r.TTLSeconds)*1000
			}
			r.addActivity(now, fmt.Sprintf("%s allowed %d of %d tool%s%s", who, n, len(r.Tools), plural(len(r.Tools)), how))
		case "deny":
			for i := range r.Tools {
				r.Tools[i].Decision = ToolRefused
				r.Tools[i].Verdict = VerdictRejected
			}
			r.Status = StatusDenied
			r.addActivity(now, who+" denied it"+how+withNote(note))
		case "return":
			if r.Kind != KindAccess {
				return ErrBadDecision
			}
			for i := range r.Tools {
				r.Tools[i].Decision = ToolRefused
				r.Tools[i].Verdict = VerdictRejected
			}
			r.Status = StatusReturned
			r.addActivity(now, who+" sent it back to replan"+how+withNote(note))
		case "answer":
			if r.Kind != KindQuestion && r.Kind != KindBlocker {
				return ErrBadDecision
			}
			if err := answerQuestion(r, d, now); err != nil {
				return err
			}
			r.Status = StatusAnswered
			r.addActivity(now, who+" answered"+how+": "+r.Answer)
		case "read":
			if r.Kind != KindUpdate {
				return ErrBadDecision
			}
			r.Status = StatusRead
		case "snooze":
			if r.Status != StatusPending {
				return ErrNotPending
			}
			mins := d.SnoozeMinutes
			if mins <= 0 || mins > 7*24*60 {
				return errors.New("snooze_minutes must be between 1 and 10080")
			}
			r.SnoozedUntil = now + int64(mins)*60000
			r.addActivity(now, fmt.Sprintf("Snoozed until %s", time.UnixMilli(r.SnoozedUntil).Format("Mon 15:04")))
		default:
			return fmt.Errorf("unknown action %q", d.Action)
		}
		if note != "" {
			r.OwnerNote = note
		}
		if r.Kind == KindAccess && d.Action != "snooze" {
			r.DecisionSubmissionID = d.SubmissionID
			r.DecisionFingerprint = decisionFingerprint(d)
		}
		out = r
		return nil
	})
	if err != nil {
		return nil, err
	}
	if out.Replayed {
		return out, nil
	}
	if out.ExecutionMode == "legacy" && d.Action != "snooze" && s.opts.LegacyDecided != nil {
		s.opts.LegacyDecided(ctx, id)
	}
	s.wake(id)
	if d.Action == "snooze" {
		s.enqueuePush(ctx, out, "snooze_end", out.ID, time.UnixMilli(out.SnoozedUntil))
	}
	s.publish("inbox", s.cardView(ctx, out))
	return out, nil
}

func withNote(n string) string {
	if n == "" {
		return ""
	}
	return " with a note"
}

// ---- agent side -----------------------------------------------------------------

// AgentView is what an agent sees about one of its requests.
type AgentView struct {
	Revision     int             `json:"revision"`
	Response     *AnswerResponse `json:"response,omitempty"`
	RequestID    string          `json:"request_id"`
	Kind         string          `json:"kind"`
	Status       string          `json:"status"`
	SnoozedUntil int64           `json:"snoozed_until,omitempty"`
	OwnerNote    string          `json:"owner_note,omitempty"`
	Answer       string          `json:"answer,omitempty"`
	Tools        []AgentToolView `json:"tools,omitempty"`
	ExpiresAt    int64           `json:"expires_at,omitempty"`
	GrantsExpire int64           `json:"grants_expire_at,omitempty"`
	Next         string          `json:"next"`
}

// AgentToolView is one tool's outcome, with the grant token the first time
// the agent sees it.
type AgentToolView struct {
	CallID    string     `json:"call_id"`
	Verdict   string     `json:"verdict,omitempty"`
	Reason    string     `json:"reason,omitempty"`
	Execution *Execution `json:"execution,omitempty"`
	Tool      string     `json:"tool"`
	Decision  string     `json:"decision"` // pending | allowed | refused
	// Narrowed is set when the owner tightened the parameters; Params is
	// then what the grant allows.
	Narrowed  bool                  `json:"narrowed,omitempty"`
	Params    map[string]Constraint `json:"params,omitempty"`
	GrantID   string                `json:"grant_id,omitempty"`
	Grant     string                `json:"grant,omitempty"`
	GrantNote string                `json:"grant_note,omitempty"`
}

// Status returns the agent's view of its requests. Grant tokens are handed
// over once: the first Status/Wait after approval includes them.
func (s *Service) Status(ctx context.Context, agentID string, ids []string) ([]AgentView, error) {
	out := make([]AgentView, 0, len(ids))
	for _, id := range ids {
		r, err := s.Get(ctx, id)
		if err != nil || r.AgentID != agentID {
			out = append(out, AgentView{RequestID: id, Status: "not_found", Next: "No request with this ID belongs to you."})
			continue
		}
		if r.Status == StatusAnswered && r.RetrievedAt == 0 {
			if err := s.mutate(ctx, id, func(current *Request) error {
				if current.RetrievedAt == 0 {
					current.RetrievedAt = s.now().UnixMilli()
					current.addActivity(current.RetrievedAt, "Agent retrieved the answer")
				}
				r = current
				return nil
			}); err != nil {
				return nil, err
			}
			s.publish("inbox", s.cardView(ctx, r))
		}
		v := AgentView{Revision: r.Revision, RequestID: r.ID, Kind: r.Kind, Status: r.Status, SnoozedUntil: r.SnoozedUntil, OwnerNote: r.OwnerNote,
			Answer: r.Answer, Response: r.Response, ExpiresAt: r.ExpiresAt, GrantsExpire: r.GrantsExpire}
		var toks map[string]string
		if r.Status == StatusApproved {
			toks, err = s.collectTokens(ctx, r.ID, agentID)
			if err != nil {
				return nil, err
			}
		}
		for _, t := range r.Tools {
			tv := AgentToolView{CallID: t.CallID, Verdict: t.Verdict, Reason: t.Reason, Tool: t.Tool, Decision: t.Decision, GrantID: t.GrantID}
			if tv.Decision == "" {
				tv.Decision = "pending"
			}
			if t.Requested != nil {
				tv.Narrowed, tv.Params = true, t.Params
			}
			if t.GrantID != "" {
				tv.Execution, _ = s.Execution(ctx, t.GrantID, agentID)
				if tok, ok := toks[t.GrantID]; ok {
					tv.Grant = tok
				} else {
					tv.GrantNote = "permission is used, expired or revoked; inspect its execution state"
				}
			}
			v.Tools = append(v.Tools, tv)
		}
		v.Next = nextStep(r, len(toks) > 0)
		out = append(out, v)
	}
	return out, nil
}

func nextStep(r *Request, freshTokens bool) string {
	switch r.Status {
	case StatusPending:
		if r.SnoozedUntil > 0 {
			return "Your owner snoozed this. Keep working on anything that doesn't depend on it."
		}
		return "Waiting for your owner. A registered callback announces the decision. Use inbox.status for recovery; inbox.wait is optional."
	case StatusApproved:
		if freshTokens {
			return "Call each allowed tool with its grant as `_grant`, exactly within the parameters you asked for (or `params`, where your owner narrowed them). Each grant works once. Keep the tokens out of logs. Follow owner_note if there is one."
		}
		return "Accepted calls have single-use grants. Read inbox.status to recover each still-valid unused grant."
	case StatusDenied:
		return "Denied. Read owner_note. Ask again only with new information."
	case StatusReturned:
		return "A required tool was refused. Replan, read owner_note, and say what changed in your next request."
	case StatusAnswered:
		return "Your owner answered; carry on with that option."
	case StatusCancelled:
		return "You cancelled this request."
	case StatusExpired:
		return "Nobody decided in time. If you still need it, send it again once and say it's a re-request."
	case StatusRead:
		return "Your owner read this update."
	}
	return ""
}

// Wait blocks until any (mode "any") or all (mode "all") of the requests
// leave pending, or the timeout passes. It then returns Status.
func (s *Service) Wait(ctx context.Context, agentID string, ids []string, mode string, timeout time.Duration) ([]AgentView, error) {
	if timeout <= 0 || timeout > MaxWait {
		timeout = MaxWait
	}
	deadline := time.NewTimer(timeout)
	defer deadline.Stop()
	for {
		done, total := 0, 0
		var chans []chan struct{}
		for _, id := range ids {
			r, err := s.Get(ctx, id)
			if err != nil || r.AgentID != agentID {
				continue
			}
			total++
			if r.Status != StatusPending {
				done++
			} else {
				chans = append(chans, s.watchCh(id))
			}
		}
		if total == 0 || (mode == "any" && done > 0) || done == total {
			return s.Status(ctx, agentID, ids)
		}
		changed := make(chan struct{}, 1)
		stop := make(chan struct{})
		for _, c := range chans {
			go func(c chan struct{}) {
				select {
				case <-c:
					select {
					case changed <- struct{}{}:
					default:
					}
				case <-stop:
				}
			}(c)
		}
		select {
		case <-changed:
			close(stop)
		case <-deadline.C:
			close(stop)
			return s.Status(ctx, agentID, ids)
		case <-ctx.Done():
			close(stop)
			return nil, ctx.Err()
		}
	}
}

func (s *Service) watchCh(id string) chan struct{} {
	s.watchMu.Lock()
	defer s.watchMu.Unlock()
	c, ok := s.watch[id]
	if !ok {
		c = make(chan struct{})
		s.watch[id] = c
	}
	return c
}

func (s *Service) wake(id string) {
	s.watchMu.Lock()
	c, ok := s.watch[id]
	delete(s.watch, id)
	s.watchMu.Unlock()
	if ok {
		close(c)
	}
}

// Cancel withdraws one of the agent's pending requests.
func (s *Service) Cancel(ctx context.Context, agentID, id string) (*Request, error) {
	var out *Request
	err := s.mutate(ctx, id, func(r *Request) error {
		if r.AgentID != agentID {
			return ErrNotFound
		}
		if r.Status != StatusPending {
			return ErrNotPending
		}
		r.Status = StatusCancelled
		r.addActivity(s.now().UnixMilli(), "Withdrawn by the agent")
		out = r
		return nil
	})
	if err != nil {
		return nil, err
	}
	s.wake(id)
	s.publish("inbox", s.cardView(ctx, out))
	return out, nil
}

// CheckCall is one call an agent plans to make.
type CheckCall struct {
	Tool string         `json:"tool"`
	Args map[string]any `json:"args,omitempty"`
}

// CheckResult says how a planned call would be treated.
type CheckResult struct {
	Tool    string `json:"tool"`
	Status  string `json:"status"` // open | granted | restricted | denied | unknown
	GrantID string `json:"grant_id,omitempty"`
	Note    string `json:"note"`
}

// Check reports, for each planned call, whether the agent can make it now.
func (s *Service) Check(ctx context.Context, agentID string, calls []CheckCall) ([]CheckResult, error) {
	active, err := s.ListGrants(ctx, GrantActive, agentID, 500)
	if err != nil {
		return nil, err
	}
	out := make([]CheckResult, 0, len(calls))
	for _, c := range calls {
		access := AccessUnknown
		if s.opts.Catalog != nil {
			access, _ = s.opts.Catalog.Access(ctx, agentID, c.Tool, c.Args)
		}
		res := CheckResult{Tool: c.Tool, Status: access}
		switch access {
		case AccessOpen:
			res.Note = "You can call this now."
		case AccessDenied:
			res.Note = "Your owner has blocked this tool for you. Don't ask for it."
		case AccessUnknown:
			res.Note = "Not a toolyard tool. Check the name with tools.search."
		case AccessRestricted:
			res.Note = "Include this in your inbox.request."
			for _, g := range active {
				if g.Tool != c.Tool || s.now().UnixMilli() >= g.ExpiresAt {
					continue
				}
				if c.Args == nil {
					res.Status, res.GrantID, res.Note = "granted", g.ID, "A live grant covers this tool; pass its token as _grant (check the parameters)."
					break
				}
				if ok, _ := MatchArgs(g.Params, c.Args); ok {
					res.Status, res.GrantID, res.Note = "granted", g.ID, "A live grant covers this call; pass its token as _grant."
					break
				}
			}
		}
		out = append(out, res)
	}
	return out, nil
}

// ---- reading -------------------------------------------------------------------

// ListFilter selects requests for the owner.
type ListFilter struct {
	Open     bool // pending only
	Closed   bool // decided, cancelled, expired, read
	AgentID  string
	AgentIDs []string // optional owner boundary, applied before LIMIT
	Limit    int
}

// List returns requests, newest first.
func (s *Service) List(ctx context.Context, f ListFilter) ([]Request, error) {
	if f.Limit <= 0 || f.Limit > 500 {
		f.Limit = 200
	}
	q := `SELECT doc FROM inbox_requests WHERE 1=1`
	var args []any
	if f.Open && !f.Closed {
		q += ` AND status = ?`
		args = append(args, StatusPending)
	} else if f.Closed && !f.Open {
		q += ` AND status != ?`
		args = append(args, StatusPending)
	}
	if f.AgentID != "" {
		q += ` AND agent_id = ?`
		args = append(args, f.AgentID)
	}
	if f.AgentIDs != nil {
		if len(f.AgentIDs) == 0 {
			return []Request{}, nil
		}
		marks := make([]string, len(f.AgentIDs))
		for i, id := range f.AgentIDs {
			marks[i] = "?"
			args = append(args, id)
		}
		q += ` AND agent_id IN (` + strings.Join(marks, ",") + `)`
	}
	q += ` ORDER BY created_at DESC LIMIT ?`
	args = append(args, f.Limit)
	rows, err := s.db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Request
	for rows.Next() {
		var doc string
		if err := rows.Scan(&doc); err != nil {
			return nil, err
		}
		var r Request
		if err := json.Unmarshal([]byte(doc), &r); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// Get loads one request.
func (s *Service) Get(ctx context.Context, id string) (*Request, error) {
	var doc string
	if err := s.db.QueryRowContext(ctx, `SELECT doc FROM inbox_requests WHERE id = ?`, id).Scan(&doc); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, ErrNotFound
		}
		return nil, err
	}
	var r Request
	if err := json.Unmarshal([]byte(doc), &r); err != nil {
		return nil, err
	}
	return &r, nil
}

// CardView is the compact shape pushed to the dashboard over SSE.
type CardView struct {
	ID        string `json:"id"`
	AgentID   string `json:"agent_id"`
	AgentName string `json:"agent_name"`
	Kind      string `json:"kind"`
	Status    string `json:"status"`
	Urgency   string `json:"urgency"`
	Title     string `json:"title"`
	Checked   bool   `json:"checked"`
	UpdatedAt int64  `json:"updated_at"`
}

func (s *Service) cardView(ctx context.Context, r *Request) CardView {
	return CardView{ID: r.ID, AgentID: r.AgentID, AgentName: s.agentName(ctx, r.AgentID), Kind: r.Kind, Status: r.Status,
		Urgency: r.Urgency, Title: r.Title, Checked: r.Checked, UpdatedAt: r.UpdatedAt}
}

// Summarize returns toolyard's own reading of a request, for "Summarize
// with toolyard". It uses the judge's summary when there is one, and a
// rule-based summary otherwise.
func (s *Service) Summarize(ctx context.Context, id string) (string, string, error) {
	r, err := s.Get(ctx, id)
	if err != nil {
		return "", "", err
	}
	if r.ToolyardSum != "" {
		return r.ToolyardSum, "judge", nil
	}
	if s.judgeOn() {
		jctx, cancel := context.WithTimeout(ctx, judgeTimeout)
		rv, err := s.opts.Judge.Review(jctx, r)
		cancel()
		if err == nil && rv != nil && rv.Summary != "" {
			_ = s.mutate(ctx, id, func(cur *Request) error {
				cur.ToolyardSum = rv.Summary
				for i := range cur.Tools {
					if i < len(rv.Tools) && cur.Tools[i].Explanation == "" {
						cur.Tools[i].Explanation = rv.Tools[i].Explanation
					}
				}
				return nil
			})
			return rv.Summary, "judge", nil
		}
	}
	return ruleSummary(r), "rules", nil
}

// Explain returns toolyard's reading of one tool call ("Ask toolyard").
func (s *Service) Explain(ctx context.Context, id string, idx int) (string, string, error) {
	r, err := s.Get(ctx, id)
	if err != nil {
		return "", "", err
	}
	if idx < 0 || idx >= len(r.Tools) {
		return "", "", ErrNotFound
	}
	if r.Tools[idx].Explanation != "" {
		return r.Tools[idx].Explanation, "judge", nil
	}
	if s.judgeOn() {
		if _, src, err := s.Summarize(ctx, id); err == nil && src == "judge" {
			if r2, err := s.Get(ctx, id); err == nil && r2.Tools[idx].Explanation != "" {
				return r2.Tools[idx].Explanation, "judge", nil
			}
		}
	}
	return ruleExplain(r.Tools[idx]), "rules", nil
}

func ruleSummary(r *Request) string {
	var b strings.Builder
	switch r.Kind {
	case KindAccess:
		req := 0
		for _, t := range r.Tools {
			if t.Required {
				req++
			}
		}
		fmt.Fprintf(&b, "%d tool%s, %d required. ", len(r.Tools), plural(len(r.Tools)), req)
	case KindQuestion, KindBlocker:
		fmt.Fprintf(&b, "A %s with %d options. ", r.Kind, len(r.Options))
	default:
		b.WriteString("An update. ")
	}
	fl := toolFlags(r)
	if len(fl) == 0 {
		b.WriteString("No rule raised a flag. ")
	} else {
		byLabel := map[string][]string{}
		var order []string
		for _, f := range fl {
			if _, ok := byLabel[f.Label]; !ok {
				order = append(order, f.Label)
			}
			if f.Tool != "" {
				byLabel[f.Label] = append(byLabel[f.Label], f.Tool)
			}
		}
		var parts []string
		for _, l := range order {
			if len(byLabel[l]) > 0 {
				parts = append(parts, fmt.Sprintf("%s (%s)", l, strings.Join(dedupe(byLabel[l]), ", ")))
			} else {
				parts = append(parts, l)
			}
		}
		b.WriteString("Flags: " + strings.Join(parts, "; ") + ". ")
	}
	if n := len(r.Attachments); n > 0 {
		var kinds []string
		for _, a := range r.Attachments {
			kinds = append(kinds, a.Type)
		}
		fmt.Fprintf(&b, "%d attachment%s (%s). ", n, plural(n), strings.Join(dedupe(kinds), ", "))
	}
	if r.DryRunCount > 0 {
		fmt.Fprintf(&b, "%d dry run%s before sending. ", r.DryRunCount, plural(r.DryRunCount))
	}
	b.WriteString("The judge model is off, so this is what toolyard's rules found, not a reading of the agent's intent.")
	return strings.TrimSpace(b.String())
}

func ruleExplain(t ToolRequest) string {
	var parts []string
	for _, k := range sortedConstraintKeys(t.Params) {
		parts = append(parts, k+" "+t.Params[k].Describe())
	}
	s := "Calls " + t.Tool
	if len(parts) > 0 {
		s += " with " + strings.Join(parts, ", ")
	} else {
		s += " with no arguments"
	}
	s += "."
	if len(t.Flags) > 0 {
		var fl []string
		for _, f := range t.Flags {
			fl = append(fl, f.Label)
		}
		s += " Flags: " + strings.Join(fl, ", ") + "."
	}
	return s
}

func dedupe(xs []string) []string {
	seen := map[string]bool{}
	var out []string
	for _, x := range xs {
		if !seen[x] {
			seen[x] = true
			out = append(out, x)
		}
	}
	return out
}

func plural(n int) string {
	if n == 1 {
		return ""
	}
	return "s"
}

func sortStrings(xs []string) { sort.Strings(xs) }

// ---- housekeeping ----------------------------------------------------------------

// Sweep expires stale requests and grants.
func (s *Service) Sweep(ctx context.Context) {
	now := s.now().UnixMilli()
	rows, err := s.db.QueryContext(ctx, `SELECT id FROM inbox_requests WHERE status = ? AND expires_at <= ?`, StatusPending, now)
	if err == nil {
		var ids []string
		for rows.Next() {
			var id string
			if rows.Scan(&id) == nil {
				ids = append(ids, id)
			}
		}
		rows.Close()
		for _, id := range ids {
			var out *Request
			if err := s.mutate(ctx, id, func(r *Request) error {
				if r.Status != StatusPending {
					return nil
				}
				r.Status = StatusExpired
				r.addActivity(now, "Expired without a decision")
				out = r
				return nil
			}); err == nil && out != nil {
				s.wake(id)
				s.publish("inbox", s.cardView(ctx, out))
			}
		}
	}
	if _, err := s.expireGrants(ctx); err != nil {
		log.Printf("inbox: expire grants: %v", err)
	}
	_, _ = s.db.ExecContext(ctx, `DELETE FROM inbox_dry_runs WHERE created_at < ?`, s.now().Add(-48*time.Hour).UnixMilli())
	_, _ = s.db.ExecContext(ctx, `DELETE FROM inbox_pushes WHERE sent_at IS NOT NULL AND sent_at < ?`, s.now().Add(-7*24*time.Hour).UnixMilli())
	_, _ = s.db.ExecContext(ctx, `DELETE FROM inbox_digests WHERE sent_at < ?`, s.now().Add(-30*24*time.Hour).UnixMilli())
}

// RunSweeper runs Sweep on an interval until ctx is done.
func (s *Service) RunSweeper(ctx context.Context, every time.Duration) {
	t := time.NewTicker(every)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			s.Sweep(ctx)
		}
	}
}

// ---- storage ---------------------------------------------------------------------

func (s *Service) insert(ctx context.Context, r *Request) error {
	doc, err := json.Marshal(r)
	if err != nil {
		return err
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	_, err = tx.ExecContext(ctx, `INSERT INTO inbox_requests(id, agent_id, session_id, kind, status, urgency, related_id, doc, created_at, updated_at, expires_at)
		VALUES (?,?,?,?,?,?,?,?,?,?,?)`,
		r.ID, r.AgentID, nullStr(r.SessionID), r.Kind, r.Status, r.Urgency, nullStr(r.RelatedID), string(doc), r.CreatedAt, r.UpdatedAt, r.ExpiresAt)
	if err != nil {
		return err
	}
	if s.opts.Callbacks != nil {
		if err = s.opts.Callbacks.RegisterTx(ctx, tx, r, r.CallbackRef); err != nil {
			return err
		}
	} else if r.CallbackRef != "" {
		return errors.New("callback_ref: callback delivery is not configured")
	}
	return tx.Commit()
}

func (s *Service) mutate(ctx context.Context, id string, fn func(*Request) error) error {
	return s.mutateTx(ctx, id, func(_ *sql.Tx, r *Request) error { return fn(r) })
}

// mutateTx loads a request inside a transaction, applies fn and writes it
// back. fn must not use s.db (the store has one connection).
func (s *Service) mutateTx(ctx context.Context, id string, fn func(*sql.Tx, *Request) error) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	var doc string
	if err := tx.QueryRowContext(ctx, `SELECT doc FROM inbox_requests WHERE id = ?`, id).Scan(&doc); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return ErrNotFound
		}
		return err
	}
	var r Request
	if err := json.Unmarshal([]byte(doc), &r); err != nil {
		return err
	}
	previousStatus := r.Status
	if err := fn(tx, &r); err != nil {
		return err
	}
	if previousStatus == StatusPending && r.Status != StatusPending && !r.Replayed {
		r.Revision++
		if r.ExecutionMode == "legacy" {
			if err := s.legacyDecisionTx(ctx, tx, &r); err != nil {
				return err
			}
		}
		if err := s.recordDecisionAuditTx(ctx, tx, &r); err != nil {
			return err
		}
		if s.opts.Callbacks != nil {
			if err := s.opts.Callbacks.DecisionTx(ctx, tx, &r); err != nil {
				return err
			}
		}
	}
	r.UpdatedAt = s.now().UnixMilli()
	nb, err := json.Marshal(&r)
	if err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `UPDATE inbox_requests SET doc = ?, status = ?, urgency = ?, updated_at = ? WHERE id = ?`,
		string(nb), r.Status, r.Urgency, r.UpdatedAt, id); err != nil {
		return err
	}
	return tx.Commit()
}

func (s *Service) countPending(ctx context.Context, agentID string) (int, error) {
	var n int
	err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM inbox_requests WHERE agent_id = ? AND status = ? AND kind != ?`,
		agentID, StatusPending, KindUpdate).Scan(&n)
	return n, err
}

func (s *Service) countDryRuns(ctx context.Context, agentID string, window time.Duration) (int, error) {
	var n int
	err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM inbox_dry_runs WHERE agent_id = ? AND created_at >= ?`,
		agentID, s.now().Add(-window).UnixMilli()).Scan(&n)
	return n, err
}

func (s *Service) countDryRunsTitled(ctx context.Context, agentID, title string) (int, error) {
	var n int
	err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM inbox_dry_runs WHERE agent_id = ? AND title = ? AND created_at >= ?`,
		agentID, title, s.now().Add(-dryRunWindow).UnixMilli()).Scan(&n)
	return n, err
}

func (s *Service) dryRunLabels(ctx context.Context, agentID, title string) ([]string, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT flags FROM inbox_dry_runs WHERE agent_id = ? AND title = ? AND created_at >= ?`,
		agentID, title, s.now().Add(-dryRunWindow).UnixMilli())
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	seen := map[string]bool{}
	var out []string
	for rows.Next() {
		var raw string
		if rows.Scan(&raw) != nil {
			continue
		}
		var labels []string
		_ = json.Unmarshal([]byte(raw), &labels)
		for _, l := range labels {
			if !seen[l] {
				seen[l] = true
				out = append(out, l)
			}
		}
	}
	sortStrings(out)
	return out, rows.Err()
}

func nullStr(s string) any {
	if s == "" {
		return nil
	}
	return s
}
