package api

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"

	"github.com/google/uuid"

	"github.com/tusharbhardwaj/toolyard/internal/memwebhook"
)

// DefaultWebhookMaxBytes is the fallback cap on a memory-webhook ingest body
// when -webhook-max-bytes is unset. Large by design: n8n meeting/transcript
// payloads can be multi-MB. Enforced via http.MaxBytesReader (backpressure +
// a clean 413), independently of every other route's tight cap.
const DefaultWebhookMaxBytes int64 = 25 << 20 // 25 MiB

// MemoryWebhookIngestPath is the single bearer-authenticated ingest endpoint.
// It is referenced by the security middleware (CSRF/origin exemption, body cap)
// and by cmd/gateway when widening the universal body ceiling.
const MemoryWebhookIngestPath = "/v1/memory/webhooks/ingest"

// MemoryWebhookJobsPrefix is the bearer-authenticated job-status subtree:
// GET /v1/memory/webhooks/ingest/jobs/{job_id}. It is registered as a longer
// (more specific) prefix than the session-auth /v1/memory/webhooks/ subtree, so
// http.ServeMux routes job-status requests here and never to the management
// handler.
const MemoryWebhookJobsPrefix = "/v1/memory/webhooks/ingest/jobs/"

func (s *Server) memoryWebhookRoutes(mux *http.ServeMux) {
	mux.HandleFunc(MemoryWebhookIngestPath, s.memoryWebhookIngest)
	mux.HandleFunc(MemoryWebhookJobsPrefix, s.memoryWebhookIngestJob)
	mux.HandleFunc("/v1/memory/webhooks/wings", s.memoryWebhookWings)
	mux.HandleFunc("/v1/memory/webhooks", s.memoryWebhooksCollection)
	mux.HandleFunc("/v1/memory/webhooks/", s.memoryWebhooksItem)
	mux.HandleFunc("/v1/memory/metrics", s.memoryMetrics)
}

// ---- ingest (bearer webhook-token, large body) ------------------------------

// memoryWebhookIngest is the n8n target. Auth is the webhook's bearer token;
// the wing is the webhook's bound wing (never from the request). The body cap
// is webhookMaxBytes, applied by the security middleware; exceeding it surfaces
// as a clean 413.
//
// TEC-482: ingest is asynchronous. The request is validated and mapped
// synchronously (so bad payloads still get a fast 401/413/422), then a job is
// queued and the handler returns 202 with a job id; callers poll the status
// endpoint (memoryWebhookIngestJob) for the eventual MemPalace result. The slow
// embed/index/store never holds the HTTP request open.
func (s *Server) memoryWebhookIngest(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeError(w, http.StatusMethodNotAllowed, "POST only")
		return
	}
	if s.memWebhooks == nil {
		writeError(w, http.StatusServiceUnavailable, "memory webhooks are disabled")
		return
	}
	token := strings.TrimSpace(strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer "))
	if token == "" {
		s.memWebhooks.AuditAuthFailure(r.Context(), "missing bearer token from "+s.security.ClientIP(r))
		writeError(w, http.StatusUnauthorized, "webhook token required")
		return
	}
	wh, err := s.memWebhooks.VerifyToken(r.Context(), token)
	if err != nil {
		// Never echo or log the presented token.
		s.memWebhooks.AuditAuthFailure(r.Context(), "rejected token from "+s.security.ClientIP(r))
		writeError(w, http.StatusUnauthorized, "invalid webhook token")
		return
	}

	requestID := "req_" + uuid.NewString()
	source := strings.TrimSpace(r.Header.Get("X-Toolyard-Source"))

	body, err := io.ReadAll(r.Body)
	if err != nil {
		var mbe *http.MaxBytesError
		if errors.As(err, &mbe) {
			s.memWebhooks.RecordTooLarge(r.Context(), wh, requestID, source, mbe.Limit)
			writeJSON(w, http.StatusRequestEntityTooLarge, map[string]any{
				"error":      "payload too large",
				"request_id": requestID,
				"limit":      mbe.Limit,
			})
			return
		}
		writeError(w, http.StatusBadRequest, "failed to read request body")
		return
	}

	job, err := s.memWebhooks.Enqueue(r.Context(), wh, memwebhook.IngestRequest{
		RequestID:   requestID,
		Payload:     json.RawMessage(body),
		PayloadSize: int64(len(body)),
		Source:      source,
	})
	if err != nil {
		switch {
		case errors.Is(err, memwebhook.ErrSchemaViolation):
			writeJSON(w, http.StatusUnprocessableEntity, map[string]any{
				"error": err.Error(), "request_id": requestID, "status": "rejected",
			})
		case errors.Is(err, memwebhook.ErrMemUnavailable):
			writeJSON(w, http.StatusServiceUnavailable, map[string]any{
				"error": "mempalace unavailable", "request_id": requestID, "status": "unavailable",
			})
		default:
			writeError(w, http.StatusInternalServerError, "failed to queue ingest job")
		}
		return
	}
	// 202 Accepted: the job is queued; the caller polls status_url for the
	// eventual MemPalace result. job_id == request_id (== the ledger row id).
	writeJSON(w, http.StatusAccepted, map[string]any{
		"job_id":     job.ID,
		"status":     job.Status,
		"wing":       job.Wing,
		"bytes":      job.PayloadSize,
		"status_url": MemoryWebhookJobsPrefix + job.ID,
	})
}

// memoryWebhookIngestJob is the bearer-authenticated job-status endpoint:
// GET /v1/memory/webhooks/ingest/jobs/{job_id}. It authenticates with the same
// webhook token as ingest and only returns jobs that belong to that webhook —
// a job for another webhook (or a missing one) is a 404, so callers can neither
// read across webhooks nor probe for job existence. No token material is ever
// echoed, and the Job view carries no payload text.
func (s *Server) memoryWebhookIngestJob(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeError(w, http.StatusMethodNotAllowed, "GET only")
		return
	}
	if s.memWebhooks == nil {
		writeError(w, http.StatusServiceUnavailable, "memory webhooks are disabled")
		return
	}
	token := strings.TrimSpace(strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer "))
	if token == "" {
		s.memWebhooks.AuditAuthFailure(r.Context(), "missing bearer token from "+s.security.ClientIP(r))
		writeError(w, http.StatusUnauthorized, "webhook token required")
		return
	}
	wh, err := s.memWebhooks.VerifyToken(r.Context(), token)
	if err != nil {
		s.memWebhooks.AuditAuthFailure(r.Context(), "rejected token from "+s.security.ClientIP(r))
		writeError(w, http.StatusUnauthorized, "invalid webhook token")
		return
	}

	jobID := strings.TrimPrefix(r.URL.Path, MemoryWebhookJobsPrefix)
	if jobID == "" || strings.Contains(jobID, "/") {
		http.NotFound(w, r)
		return
	}
	job, err := s.memWebhooks.GetJob(r.Context(), jobID)
	if err != nil || job.WebhookID != wh.ID {
		// 404 for both not-found and cross-webhook access: no existence leak.
		writeError(w, http.StatusNotFound, "job not found")
		return
	}
	writeJSON(w, http.StatusOK, job)
}

// ---- session-auth management ------------------------------------------------

func (s *Server) memoryWebhooksCollection(w http.ResponseWriter, r *http.Request) {
	if _, err := s.requireUser(r); err != nil {
		writeError(w, http.StatusUnauthorized, "unauthorized")
		return
	}
	if s.memWebhooks == nil {
		if r.Method == http.MethodGet {
			writeJSON(w, http.StatusOK, []any{})
			return
		}
		writeError(w, http.StatusServiceUnavailable, "memory webhooks are disabled")
		return
	}
	switch r.Method {
	case http.MethodGet:
		list, err := s.memWebhooks.List(r.Context())
		if err != nil {
			writeError(w, http.StatusInternalServerError, err.Error())
			return
		}
		writeJSON(w, http.StatusOK, list)
	case http.MethodPost:
		var body struct {
			Name        string                  `json:"name"`
			Wing        string                  `json:"wing"`
			Source      string                  `json:"source"`
			Notes       string                  `json:"notes"`
			PayloadSpec *memwebhook.PayloadSpec `json:"payload_spec"`
		}
		if err := decode(r, &body); err != nil {
			writeError(w, http.StatusBadRequest, err.Error())
			return
		}
		wh, token, err := s.memWebhooks.Create(r.Context(), memwebhook.CreateInput{
			Name: body.Name, Wing: body.Wing, Source: body.Source,
			Notes: body.Notes, Spec: body.PayloadSpec,
		})
		if err != nil {
			writeError(w, createStatus(err), err.Error())
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{
			"webhook":      wh,
			"token":        token,
			"curl_example": memWebhookCurl(s.security.PublicURL, token),
		})
	default:
		writeError(w, http.StatusMethodNotAllowed, "GET or POST")
	}
}

func (s *Server) memoryWebhooksItem(w http.ResponseWriter, r *http.Request) {
	if _, err := s.requireUser(r); err != nil {
		writeError(w, http.StatusUnauthorized, "unauthorized")
		return
	}
	if s.memWebhooks == nil {
		writeError(w, http.StatusServiceUnavailable, "memory webhooks are disabled")
		return
	}
	tail := strings.TrimPrefix(r.URL.Path, "/v1/memory/webhooks/")
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
		token, err := s.memWebhooks.RotateToken(r.Context(), id)
		if err != nil {
			writeError(w, notFoundStatus(err), err.Error())
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{
			"token":        token,
			"curl_example": memWebhookCurl(s.security.PublicURL, token),
		})
		return
	}

	switch r.Method {
	case http.MethodGet:
		wh, err := s.memWebhooks.Get(r.Context(), id)
		if err != nil {
			writeError(w, notFoundStatus(err), err.Error())
			return
		}
		writeJSON(w, http.StatusOK, wh)
	case http.MethodPatch:
		var body struct {
			Enabled     *bool                   `json:"enabled"`
			Source      *string                 `json:"source"`
			Notes       *string                 `json:"notes"`
			PayloadSpec *memwebhook.PayloadSpec `json:"payload_spec"`
		}
		if err := decode(r, &body); err != nil {
			writeError(w, http.StatusBadRequest, err.Error())
			return
		}
		wh, err := s.memWebhooks.Update(r.Context(), id, memwebhook.UpdateInput{
			Enabled: body.Enabled, Source: body.Source, Notes: body.Notes, Spec: body.PayloadSpec,
		})
		if err != nil {
			status := http.StatusBadRequest
			if errors.Is(err, memwebhook.ErrNotFound) {
				status = http.StatusNotFound
			}
			writeError(w, status, err.Error())
			return
		}
		writeJSON(w, http.StatusOK, wh)
	case http.MethodDelete:
		if err := s.memWebhooks.Delete(r.Context(), id); err != nil {
			writeError(w, notFoundStatus(err), err.Error())
			return
		}
		writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
	default:
		writeError(w, http.StatusMethodNotAllowed, "GET, PATCH or DELETE")
	}
}

// memoryMetrics powers the MemPalace dashboard panel: MemPalace status +
// best-effort storage/index health + ledger-derived ingestion metrics.
func (s *Server) memoryMetrics(w http.ResponseWriter, r *http.Request) {
	if _, err := s.requireUser(r); err != nil {
		writeError(w, http.StatusUnauthorized, "unauthorized")
		return
	}
	resp := map[string]any{}
	if s.mempalace != nil {
		resp["mempalace"] = s.mempalace.Snapshot(r.Context())
		resp["wing_stats"] = s.mempalace.WingStats(r.Context())
	} else {
		resp["mempalace"] = map[string]any{"enabled": false}
	}
	if s.memWebhooks != nil {
		m, err := s.memWebhooks.Metrics(r.Context(), 50)
		if err != nil {
			writeError(w, http.StatusInternalServerError, err.Error())
			return
		}
		resp["ledger"] = m
	}
	writeJSON(w, http.StatusOK, resp)
}

// memoryWebhookWings returns wing-name suggestions for the create form. Drawn
// from already-configured webhooks + the ledger (always available); the create
// form still accepts free text.
func (s *Server) memoryWebhookWings(w http.ResponseWriter, r *http.Request) {
	if _, err := s.requireUser(r); err != nil {
		writeError(w, http.StatusUnauthorized, "unauthorized")
		return
	}
	wings := []string{}
	if s.memWebhooks != nil {
		w2, err := s.memWebhooks.KnownWings(r.Context())
		if err == nil {
			wings = w2
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{"wings": wings})
}

// ---- helpers ----------------------------------------------------------------

func createStatus(err error) int {
	switch {
	case errors.Is(err, memwebhook.ErrExists):
		return http.StatusConflict
	default:
		return http.StatusBadRequest
	}
}

func notFoundStatus(err error) int {
	if errors.Is(err, memwebhook.ErrNotFound) {
		return http.StatusNotFound
	}
	return http.StatusBadRequest
}

func memWebhookCurl(publicURL, token string) string {
	base := strings.TrimRight(publicURL, "/")
	if base == "" {
		base = "http://localhost:8787"
	}
	body, _ := json.Marshal(map[string]any{
		"title":      "Weekly sync",
		"transcript": "…meeting transcript text…",
	})
	// Ingest is async: this POST returns 202 with {job_id, status_url}; poll
	// status_url (GET, same Bearer token) until status is succeeded/failed.
	return "curl -X POST " + base + MemoryWebhookIngestPath +
		" -H 'Authorization: Bearer " + token + "'" +
		" -H 'Content-Type: application/json'" +
		" -H 'X-Toolyard-Source: n8n-meeting-job'" +
		" -d '" + string(body) + "'" +
		"   # -> 202 {\"job_id\":\"req_…\",\"status\":\"queued\",\"status_url\":\"" + MemoryWebhookJobsPrefix + "req_…\"}" +
		"; then: curl " + base + MemoryWebhookJobsPrefix + "req_… -H 'Authorization: Bearer " + token + "'"
}
