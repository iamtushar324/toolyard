package api

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
	"strings"

	"github.com/tusharbhardwaj/toolyard/internal/audit"
	"github.com/tusharbhardwaj/toolyard/internal/events"
)

// EventsAPI is the subset of *internal/events.Service the api layer needs.
// Declared as an interface so api doesn't hard-depend on the concrete service
// for the (nil-checked) wiring; in practice main passes *events.Service.
type EventsAPI interface {
	VerifySourceToken(ctx context.Context, token string) (*events.Source, error)
	Ingest(ctx context.Context, src *events.Source, in events.IngestInput) (*events.Event, bool, error)
	Query(ctx context.Context, f events.Filter) ([]events.Event, error)
	Get(ctx context.Context, id string) (*events.Event, error)
	Ack(ctx context.Context, ids []string, actor string) (int, error)
	UnackedCount(ctx context.Context) (int, error)
	ListSources(ctx context.Context) ([]events.Source, error)
	GetSource(ctx context.Context, id string) (*events.Source, error)
	CreateSource(ctx context.Context, in events.CreateSourceInput) (*events.Source, string, error)
	UpdateSource(ctx context.Context, id string, in events.UpdateSourceInput) (*events.Source, error)
	DeleteSource(ctx context.Context, id string) error
	RotateSourceToken(ctx context.Context, id string) (string, error)
}

// eventsRoutes registers the Events Hub HTTP surface.
func (s *Server) eventsRoutes(mux *http.ServeMux) {
	mux.HandleFunc("/v1/ingest", s.eventsIngest)
	mux.HandleFunc("/v1/ingest/", s.eventsIngestPathToken)
	mux.HandleFunc("/v1/events", s.eventsList)
	mux.HandleFunc("/v1/events/ack", s.eventsAckBatch)
	mux.HandleFunc("/v1/events/", s.eventsItem)
	mux.HandleFunc("/v1/event-sources", s.eventSourcesCollection)
	mux.HandleFunc("/v1/event-sources/", s.eventSourcesItem)
}

// ---- webhook ingest (bearer source-token) ----------------------------------

func (s *Server) eventsIngest(w http.ResponseWriter, r *http.Request) {
	tok := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
	s.doIngest(w, r, strings.TrimSpace(tok))
}

// eventsIngestPathToken supports POST /v1/ingest/{token}. Documented as
// log-leaky (token in URL); bearer is preferred. Useful for webhook senders
// that can't set custom headers.
func (s *Server) eventsIngestPathToken(w http.ResponseWriter, r *http.Request) {
	tok := strings.TrimPrefix(r.URL.Path, "/v1/ingest/")
	s.doIngest(w, r, tok)
}

func (s *Server) doIngest(w http.ResponseWriter, r *http.Request, token string) {
	if r.Method != http.MethodPost {
		writeError(w, http.StatusMethodNotAllowed, "POST only")
		return
	}
	if s.events == nil {
		writeError(w, http.StatusServiceUnavailable, "events hub is disabled")
		return
	}
	if token == "" {
		writeError(w, http.StatusUnauthorized, "source token required")
		return
	}
	src, err := s.events.VerifySourceToken(r.Context(), token)
	if err != nil {
		// Never log the token.
		writeError(w, http.StatusUnauthorized, "invalid source token")
		return
	}
	var in events.IngestInput
	if err := decode(r, &in); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	ev, deduped, err := s.events.Ingest(r.Context(), src, in)
	if err != nil {
		switch {
		case errors.Is(err, events.ErrPayloadTooBig):
			writeError(w, http.StatusRequestEntityTooLarge, err.Error())
		default:
			writeError(w, http.StatusBadRequest, err.Error())
		}
		return
	}
	if deduped {
		writeJSON(w, http.StatusOK, map[string]any{"deduped": true})
		return
	}
	writeJSON(w, http.StatusCreated, map[string]any{"event_id": ev.ID, "summary": ev.Summary})
}

// ---- session-auth list / ack / get -----------------------------------------

func (s *Server) eventsList(w http.ResponseWriter, r *http.Request) {
	if _, err := s.requireUser(r); err != nil {
		writeError(w, http.StatusUnauthorized, "unauthorized")
		return
	}
	if s.events == nil {
		writeJSON(w, http.StatusOK, map[string]any{"events": []any{}, "unacked": 0})
		return
	}
	q := r.URL.Query()
	limit, _ := strconv.Atoi(q.Get("limit"))
	f := events.Filter{
		SourceID:    q.Get("source_id"),
		Type:        q.Get("type"),
		Q:           q.Get("q"),
		Since:       qInt(r, "since"),
		Before:      qInt(r, "before"),
		UnackedOnly: q.Get("unacked") == "1",
		Limit:       limit,
	}
	evs, err := s.events.Query(r.Context(), f)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	unacked, _ := s.events.UnackedCount(r.Context())
	var nextBefore int64
	if len(evs) > 0 {
		nextBefore = evs[len(evs)-1].ReceivedAt
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"events":      evs,
		"unacked":     unacked,
		"next_before": nextBefore,
	})
}

func (s *Server) eventsItem(w http.ResponseWriter, r *http.Request) {
	if _, err := s.requireUser(r); err != nil {
		writeError(w, http.StatusUnauthorized, "unauthorized")
		return
	}
	if s.events == nil {
		writeError(w, http.StatusServiceUnavailable, "events hub is disabled")
		return
	}
	tail := strings.TrimPrefix(r.URL.Path, "/v1/events/")
	// /v1/events/stream is owned by the SSE handler; defensively 404 here.
	if tail == "" || tail == "stream" {
		http.NotFound(w, r)
		return
	}
	if strings.HasSuffix(tail, "/ack") {
		id := strings.TrimSuffix(tail, "/ack")
		s.eventAckOne(w, r, id)
		return
	}
	if r.Method != http.MethodGet {
		writeError(w, http.StatusMethodNotAllowed, "GET")
		return
	}
	ev, err := s.events.Get(r.Context(), tail)
	if err != nil {
		writeError(w, http.StatusNotFound, "event not found")
		return
	}
	writeJSON(w, http.StatusOK, ev)
}

func (s *Server) eventAckOne(w http.ResponseWriter, r *http.Request, id string) {
	if r.Method != http.MethodPost {
		writeError(w, http.StatusMethodNotAllowed, "POST")
		return
	}
	uid, _ := s.requireUser(r)
	n, err := s.events.Ack(r.Context(), []string{id}, "user:"+uid)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"acked": n})
}

func (s *Server) eventsAckBatch(w http.ResponseWriter, r *http.Request) {
	uid, err := s.requireUser(r)
	if err != nil {
		writeError(w, http.StatusUnauthorized, "unauthorized")
		return
	}
	if s.events == nil {
		writeError(w, http.StatusServiceUnavailable, "events hub is disabled")
		return
	}
	if r.Method != http.MethodPost {
		writeError(w, http.StatusMethodNotAllowed, "POST only")
		return
	}
	var body struct {
		IDs []string `json:"ids"`
	}
	if err := decode(r, &body); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	n, err := s.events.Ack(r.Context(), body.IDs, "user:"+uid)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"acked": n})
}

// ---- session-auth source management ----------------------------------------

func (s *Server) eventSourcesCollection(w http.ResponseWriter, r *http.Request) {
	if _, err := s.requireUser(r); err != nil {
		writeError(w, http.StatusUnauthorized, "unauthorized")
		return
	}
	if s.events == nil {
		if r.Method == http.MethodGet {
			writeJSON(w, http.StatusOK, []any{})
			return
		}
		writeError(w, http.StatusServiceUnavailable, "events hub is disabled")
		return
	}
	switch r.Method {
	case http.MethodGet:
		srcs, err := s.events.ListSources(r.Context())
		if err != nil {
			writeError(w, http.StatusInternalServerError, err.Error())
			return
		}
		writeJSON(w, http.StatusOK, srcs)
	case http.MethodPost:
		var body struct {
			Name        string               `json:"name"`
			Kind        string               `json:"kind"`
			PollerCfg   *events.PollerConfig `json:"poller_config"`
			Notify      bool                 `json:"notify"`
			NotifyTypes []string             `json:"notify_types"`
		}
		if err := decode(r, &body); err != nil {
			writeError(w, http.StatusBadRequest, err.Error())
			return
		}
		src, token, err := s.events.CreateSource(r.Context(), events.CreateSourceInput{
			Name: body.Name, Kind: body.Kind, PollerCfg: body.PollerCfg,
			Notify: body.Notify, NotifyTypes: body.NotifyTypes,
		})
		if err != nil {
			status := http.StatusBadRequest
			if errors.Is(err, events.ErrExists) {
				status = http.StatusConflict
			}
			writeError(w, status, err.Error())
			return
		}
		s.auditEvents(r, "event_source.create", src.Name)
		resp := map[string]any{"source": src}
		if token != "" {
			resp["token"] = token
			resp["curl_example"] = curlExample(s.security.PublicURL, token)
		}
		writeJSON(w, http.StatusOK, resp)
	default:
		writeError(w, http.StatusMethodNotAllowed, "GET or POST")
	}
}

func (s *Server) eventSourcesItem(w http.ResponseWriter, r *http.Request) {
	if _, err := s.requireUser(r); err != nil {
		writeError(w, http.StatusUnauthorized, "unauthorized")
		return
	}
	if s.events == nil {
		writeError(w, http.StatusServiceUnavailable, "events hub is disabled")
		return
	}
	tail := strings.TrimPrefix(r.URL.Path, "/v1/event-sources/")
	parts := strings.SplitN(tail, "/", 2)
	id := parts[0]
	sub := ""
	if len(parts) > 1 {
		sub = parts[1]
	}
	if id == "" {
		http.NotFound(w, r)
		return
	}
	if sub == "rotate-token" {
		if r.Method != http.MethodPost {
			writeError(w, http.StatusMethodNotAllowed, "POST")
			return
		}
		token, err := s.events.RotateSourceToken(r.Context(), id)
		if err != nil {
			writeError(w, http.StatusBadRequest, err.Error())
			return
		}
		s.auditEvents(r, "event_source.rotate", id)
		writeJSON(w, http.StatusOK, map[string]any{
			"token":        token,
			"curl_example": curlExample(s.security.PublicURL, token),
		})
		return
	}
	switch r.Method {
	case http.MethodPatch:
		var body struct {
			Enabled     *bool                `json:"enabled"`
			Notify      *bool                `json:"notify"`
			NotifyTypes *[]string            `json:"notify_types"`
			PollerCfg   *events.PollerConfig `json:"poller_config"`
		}
		if err := decode(r, &body); err != nil {
			writeError(w, http.StatusBadRequest, err.Error())
			return
		}
		src, err := s.events.UpdateSource(r.Context(), id, events.UpdateSourceInput{
			Enabled: body.Enabled, Notify: body.Notify,
			NotifyTypes: body.NotifyTypes, PollerCfg: body.PollerCfg,
		})
		if err != nil {
			status := http.StatusBadRequest
			if errors.Is(err, events.ErrNotFound) {
				status = http.StatusNotFound
			}
			writeError(w, status, err.Error())
			return
		}
		writeJSON(w, http.StatusOK, src)
	case http.MethodDelete:
		if err := s.events.DeleteSource(r.Context(), id); err != nil {
			status := http.StatusInternalServerError
			if errors.Is(err, events.ErrNotFound) {
				status = http.StatusNotFound
			}
			writeError(w, status, err.Error())
			return
		}
		s.auditEvents(r, "event_source.delete", id)
		writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
	default:
		writeError(w, http.StatusMethodNotAllowed, "PATCH or DELETE")
	}
}

func (s *Server) auditEvents(r *http.Request, eventType, summary string) {
	if s.audit == nil {
		return
	}
	_ = s.audit.Write(r.Context(), audit.Event{EventType: eventType, ResultSummary: summary})
}

func curlExample(publicURL, token string) string {
	base := strings.TrimRight(publicURL, "/")
	if base == "" {
		base = "http://localhost:8787"
	}
	body, _ := json.Marshal(map[string]any{
		"type":    "deploy",
		"title":   "build #123 finished",
		"summary": "Build #123 finished successfully on main",
	})
	return "curl -X POST " + base + "/v1/ingest -H 'Authorization: Bearer " + token +
		"' -H 'Content-Type: application/json' -d '" + string(body) + "'"
}
