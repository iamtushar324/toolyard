package inbox

import (
	"context"
	"errors"
	"fmt"
	"log"
	"strings"
	"time"

	"github.com/google/uuid"
)

// System items are Inbox items that toolyard raises itself: a server-side
// feature (the skills hub's review of a new commit, an agent's skill
// source proposal) asks one person a question and acts on the answer.
//
// A system item is a schema version 3 question. Request.System names the
// feature's handler and the feature's own reference. When an owner
// decision on the item commits (an answer, or a deny), Decide calls the
// handler registered for that kind, on the node that took the decision,
// and adds the handler's outcome to the item's timeline. Agents cannot set
// Request.System: it is not part of a Submission, and Validate never
// copies it.
//
// Who owns a system item: an item about an agent (an agent's proposal)
// carries that agent's ID and belongs to the agent's owner like any item
// the agent sent; the agent also sees it in inbox.status. An item that
// toolyard raises on its own carries SystemAgentID(userID), a reserved
// per-person sender that no agent can hold; the dashboard shows it from
// SystemSenderName and lists it for that person only (the API adds the
// sender to the person's owned agents).
//
// The handler runs after the decision committed, never inside its
// transaction (a handler may write the database, which the SQLite store's
// single connection would refuse), and only for a decision: an item that
// expires, or that the agent or toolyard cancels, runs no handler. So a
// feature reconciles from its own records with a periodic sweep that reads
// its items' statuses: the skills hub runs skillhub.RunInboxSweeps under a
// leader lease, which also applies a decision recorded while no handler ran
// (the process stopped in between).
//
// Rollout: Request.System lives in the stored document. A replica that
// predates it drops the tag when it rewrites an item (a snooze, a
// reminder), and the decision then runs no handler; run this version on
// every replica before a feature creates system items.

const (
	// SystemSenderName is how the dashboard names toolyard as the sender
	// of a system item.
	SystemSenderName = "toolyard"
	// SystemItemTTL is how long a system item waits for a decision unless
	// the feature asks for another time.
	SystemItemTTL = 7 * 24 * time.Hour

	systemAgentPrefix = "toolyard:"
	// systemHandlerTimeout bounds one handler run; it is detached from
	// the deciding request, so a closed browser tab does not cut it short.
	systemHandlerTimeout = 2 * time.Minute
)

// ErrSystemItem reports a system item the feature built wrongly (missing
// owner, kind or question, or text the Inbox refuses).
var ErrSystemItem = errors.New("system item refused")

// SystemAgentID is the reserved sender of the system items toolyard raises
// on its own for one person. Agent IDs never contain a colon, so it cannot
// name an agent.
func SystemAgentID(userID string) string { return systemAgentPrefix + userID }

// SystemAgentUser returns the person a SystemAgentID belongs to.
func SystemAgentUser(agentID string) (string, bool) {
	uid, ok := strings.CutPrefix(agentID, systemAgentPrefix)
	return uid, ok && uid != ""
}

// SystemTag marks a system item: the handler kind ("skillhub.review") and
// the feature's own reference (for example a proposal ID).
type SystemTag struct {
	Kind string `json:"kind"`
	Ref  string `json:"ref"`
}

// SystemItem is a question a server-side feature puts in one person's
// Inbox. Set exactly one of UserID (toolyard asks this person) and AgentID
// (the item is about this agent and goes to its owner).
type SystemItem struct {
	Kind    string
	Ref     string
	UserID  string
	AgentID string

	Title     string
	Summary   string
	Message   string // Markdown
	Urgency   string // default soon
	Questions []QuestionSpec
	// Activity is the first line of the item's timeline; default "Sent
	// by toolyard".
	Activity string
	// TTL is how long the item waits for a decision; 0 means
	// SystemItemTTL.
	TTL time.Duration
}

// SystemHandler acts on an owner decision on a system item. r is the
// decided item (Status answered or denied; Answers, OwnerNote and the
// Decider* fields set). The returned note, or the error, is added to the
// item's timeline.
type SystemHandler func(ctx context.Context, r *Request) (note string, err error)

// HandleSystem registers the handler for one system item kind. Register
// every kind on every node at start, before the Inbox serves decisions:
// the node that takes a decision runs its handler.
func (s *Service) HandleSystem(kind string, h SystemHandler) {
	s.systemMu.Lock()
	defer s.systemMu.Unlock()
	if s.system == nil {
		s.system = map[string]SystemHandler{}
	}
	s.system[kind] = h
}

func (s *Service) systemHandler(kind string) SystemHandler {
	s.systemMu.RLock()
	defer s.systemMu.RUnlock()
	return s.system[kind]
}

// CreateSystemItem validates and stores a system item, and notifies its
// owner as for an agent's question. An item about an agent counts toward
// that agent's pending limit.
func (s *Service) CreateSystemItem(ctx context.Context, it SystemItem) (*Request, error) {
	it.Kind, it.Ref = strings.TrimSpace(it.Kind), strings.TrimSpace(it.Ref)
	switch {
	case it.Kind == "" || it.Ref == "":
		return nil, fmt.Errorf("%w: kind and ref are required", ErrSystemItem)
	case (it.UserID == "") == (it.AgentID == ""):
		return nil, fmt.Errorf("%w: set exactly one of user and agent", ErrSystemItem)
	case it.AgentID != "" && strings.Contains(it.AgentID, ":"):
		return nil, fmt.Errorf("%w: %q is not an agent ID", ErrSystemItem, it.AgentID)
	case len(it.Questions) == 0:
		return nil, fmt.Errorf("%w: give at least one question", ErrSystemItem)
	}
	if s.opts.RichEnabled != nil && !s.opts.RichEnabled() {
		return nil, fmt.Errorf("%w: schema version 3 items are turned off on this server", ErrSystemItem)
	}
	agentID := it.AgentID
	if agentID == "" {
		agentID = SystemAgentID(it.UserID)
	}
	if it.Urgency == "" {
		it.Urgency = UrgencySoon
	}
	sub := &Submission{
		Kind: KindQuestion, SchemaVersion: SchemaVersionRich,
		Title: it.Title, Summary: it.Summary, Message: it.Message, Urgency: it.Urgency,
		Questions: it.Questions,
	}
	r, probs, _ := Validate(ctx, nil, agentID, sub, ValidateOptions{AllowPrivateMedia: s.opts.AllowPrivateMedia})
	if len(probs) > 0 {
		parts := make([]string, 0, len(probs))
		for _, p := range probs {
			parts = append(parts, strings.TrimSpace(p.Path+": "+p.Message))
		}
		return nil, fmt.Errorf("%w: %s", ErrSystemItem, strings.Join(parts, "; "))
	}
	if it.AgentID != "" {
		n, err := s.countPending(ctx, it.AgentID)
		if err != nil {
			return nil, err
		}
		if n >= MaxPendingPerAgent {
			return nil, ErrTooManyPending
		}
	}
	ttl := it.TTL
	if ttl <= 0 {
		ttl = SystemItemTTL
	}
	now := s.now()
	r.AgentID = agentID
	r.System = &SystemTag{Kind: it.Kind, Ref: it.Ref}
	r.ID = "rq_" + strings.ReplaceAll(uuid.NewString(), "-", "")[:16]
	r.Status = StatusPending
	r.CreatedAt = now.UnixMilli()
	r.UpdatedAt = r.CreatedAt
	r.ExpiresAt = now.Add(ttl).UnixMilli()
	// toolyard wrote every word: there is nothing for the background check
	// (media copies, the judge) to look at.
	r.Checked = true
	first := strings.TrimSpace(it.Activity)
	if first == "" {
		first = "Sent by " + SystemSenderName
	}
	r.addActivity(r.CreatedAt, first)
	if err := s.insert(ctx, r); err != nil {
		return nil, err
	}
	s.publish("inbox", s.cardView(ctx, r))
	s.enqueueArrival(ctx, r)
	if r.Urgency == UrgencyNow {
		s.bg.Add(1)
		go func() {
			defer s.bg.Done()
			s.dispatchPushes(context.Background())
		}()
	}
	return r, nil
}

// SystemChoice is the option ID the owner chose for one single_choice
// question of a decided item, or "" when it was not answered (a denied
// item, a custom text answer).
func (r *Request) SystemChoice(questionID string) string {
	if r.Status != StatusAnswered {
		return ""
	}
	if v, ok := r.Answers[questionID].Value.(string); ok {
		return v
	}
	return ""
}

// afterSystemDecision runs the handler of a decided system item (answered
// or denied; never expired or cancelled) and returns the item as stored
// afterwards (with the handler's timeline line). Items without a tag, or
// without a registered handler, come back unchanged.
func (s *Service) afterSystemDecision(ctx context.Context, r *Request) *Request {
	if r == nil || r.System == nil || (r.Status != StatusAnswered && r.Status != StatusDenied) {
		return r
	}
	h := s.systemHandler(r.System.Kind)
	if h == nil {
		log.Printf("inbox: no handler for system item %s (kind %q); the decision is recorded only", r.ID, r.System.Kind)
		return r
	}
	hctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), systemHandlerTimeout)
	defer cancel()
	note, err := runSystemHandler(hctx, h, r)
	line := strings.TrimSpace(note)
	if err != nil {
		line = "toolyard could not apply this decision: " + limitText(err.Error(), 500)
	}
	if line == "" {
		return r
	}
	var out *Request
	if merr := s.mutate(hctx, r.ID, func(cur *Request) error {
		cur.addActivity(s.now().UnixMilli(), line)
		out = cur
		return nil
	}); merr != nil {
		log.Printf("inbox: system item %s: record the outcome: %v", r.ID, merr)
		return r
	}
	return out
}

// runSystemHandler calls h and turns a panic into an error, so a broken
// feature cannot take the deciding request down with it.
func runSystemHandler(ctx context.Context, h SystemHandler, r *Request) (note string, err error) {
	defer func() {
		if p := recover(); p != nil {
			err = fmt.Errorf("handler panicked: %v", p)
		}
	}()
	cp := *r
	return h(ctx, &cp)
}
