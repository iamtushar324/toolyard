package api

import (
	"encoding/json"
	"errors"
	"log"
	"net/http"
	"strings"
	"time"

	"github.com/tusharbhardwaj/toolyard/internal/audit"
	"github.com/tusharbhardwaj/toolyard/internal/inbox"
)

// PUT /v1/uploads/{ticket} receives the bytes of a native Inbox upload.
//
// The agent got the ticket from the inbox.upload MCP tool. The ticket is
// the only credential: no cookie, operator token or agent bearer is read,
// so the route is exempt from the CSRF header, Origin and JSON checks
// (security.go) and never reachable with an operator token
// (operator_routes.go). The ticket is single-use, expires after 15
// minutes, and is bound to one agent, size, sha256 and content type; the
// inbox service checks all of them and sniffs the real type before it
// stores anything.
//
//	201 {"ref":"upload:<sha256>", "sha256", "size", "content_type"}
//	400 size, hash or type mismatch     404 unknown ticket
//	405 not PUT                         409 another PUT on this ticket runs
//	410 ticket used or expired          503 too many uploads, server-wide
//	                                        or for this agent (Retry-After)
const uploadsPathPrefix = "/v1/uploads/"

// EventInboxUpload is the audit event for a stored upload.
const EventInboxUpload = "inbox.upload"

// uploadReadTimeout replaces the server's 60 s read timeout for an upload
// body (up to 50 MB).
const uploadReadTimeout = 5 * time.Minute

func (s *Server) uploadRoutes(mux *http.ServeMux) {
	mux.HandleFunc("/v1/uploads/", s.inboxUpload)
}

func (s *Server) inboxUpload(w http.ResponseWriter, r *http.Request) {
	if s.inbox == nil {
		http.NotFound(w, r)
		return
	}
	if r.Method != http.MethodPut {
		w.Header().Set("Allow", http.MethodPut)
		writeError(w, http.StatusMethodNotAllowed, "PUT only")
		return
	}
	ticket := strings.TrimPrefix(r.URL.Path, uploadsPathPrefix)
	if ticket == "" || strings.Contains(ticket, "/") {
		writeError(w, http.StatusNotFound, inbox.ErrUploadUnknown.Error())
		return
	}
	// A bounded number of bodies are read at once; the rest are refused
	// now rather than queued, so slow uploads can't pile up memory, disk
	// or file descriptors.
	release, ok := s.inbox.AcquireUploadSlot()
	if !ok {
		w.Header().Set("Retry-After", "5")
		writeError(w, http.StatusServiceUnavailable, inbox.ErrUploadServerBusy.Error())
		return
	}
	defer release()
	// Claim the ticket before reading anything: a second PUT on the same
	// ticket is refused (409) while this one runs; CompleteUpload releases
	// the claim on any failure.
	pu, err := s.inbox.ClaimUpload(r.Context(), ticket)
	if err != nil {
		writeUploadErr(w, err)
		return
	}
	// And at most MaxConcurrentUploadsPerAgent of them per agent, so one
	// agent cannot hold every global slot.
	releaseAgent, ok := s.inbox.AcquireAgentUploadSlot(pu.AgentID)
	if !ok {
		s.inbox.ReleaseUpload(pu)
		w.Header().Set("Retry-After", "5")
		writeError(w, http.StatusServiceUnavailable, "this agent already has uploads in progress; retry shortly")
		return
	}
	defer releaseAgent()
	if r.ContentLength >= 0 && r.ContentLength != pu.Size {
		s.inbox.ReleaseUpload(pu)
		writeError(w, http.StatusBadRequest, "Content-Length doesn't match the size the ticket was issued for")
		return
	}
	_ = http.NewResponseController(w).SetReadDeadline(time.Now().Add(uploadReadTimeout))
	body := http.MaxBytesReader(w, r.Body, pu.Size)
	res, err := s.inbox.CompleteUpload(r.Context(), pu, r.Header.Get("Content-Type"), body)
	if err != nil {
		writeUploadErr(w, err)
		return
	}
	if s.audit != nil {
		// The audit redactor replaces any 40+ hex run (a full sha256
		// included), so the event keeps a 16-character prefix; the full
		// hash stays in inbox_uploads next to the agent.
		args, _ := json.Marshal(map[string]any{"sha256_prefix": res.SHA256[:16], "size": res.Size, "content_type": res.ContentType, "name": res.Name})
		if err := s.audit.Write(r.Context(), audit.Event{
			EventType:    EventInboxUpload,
			AgentID:      res.AgentID,
			UpstreamName: "inbox",
			ToolName:     "inbox.upload",
			Decision:     "stored",
			Arguments:    args,
		}); err != nil {
			log.Printf("inbox upload: audit: %v", err)
		}
	}
	writeJSON(w, http.StatusCreated, res)
}

func writeUploadErr(w http.ResponseWriter, err error) {
	var ue *inbox.UploadError
	switch {
	case errors.As(err, &ue):
		writeError(w, http.StatusBadRequest, ue.Msg)
	case errors.Is(err, inbox.ErrUploadUnknown), errors.Is(err, inbox.ErrUploadUnavailable):
		writeError(w, http.StatusNotFound, err.Error())
	case errors.Is(err, inbox.ErrUploadGone):
		writeError(w, http.StatusGone, err.Error())
	case errors.Is(err, inbox.ErrUploadBusy):
		writeError(w, http.StatusConflict, err.Error())
	default:
		log.Printf("inbox upload: %v", err)
		writeError(w, http.StatusInternalServerError, "upload failed")
	}
}
