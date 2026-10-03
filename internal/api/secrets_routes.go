package api

import (
	"context"
	"errors"
	"net/http"
	"sort"
	"strings"

	"github.com/tusharbhardwaj/toolyard/internal/identity"
	"github.com/tusharbhardwaj/toolyard/internal/secrets"
	"github.com/tusharbhardwaj/toolyard/internal/upstreams"
)

// secretsCollection handles GET (list names + metadata + used_by) and POST
// (create). Values are never returned — there is no reveal endpoint.
func (s *Server) secretsCollection(w http.ResponseWriter, r *http.Request) {
	if _, err := s.requireUser(r); err != nil {
		writeError(w, http.StatusUnauthorized, "unauthorized")
		return
	}
	if s.secrets == nil {
		writeError(w, http.StatusServiceUnavailable, "secrets broker is disabled")
		return
	}
	switch r.Method {
	case http.MethodGet:
		metas, err := s.secrets.List(r.Context())
		if err != nil {
			writeError(w, http.StatusInternalServerError, err.Error())
			return
		}
		usedBy := s.secretUsage(r)
		out := make([]map[string]any, 0, len(metas))
		for _, m := range metas {
			row := map[string]any{
				"name":        m.Name,
				"description": m.Description,
				"created_at":  m.CreatedAt,
				"updated_at":  m.UpdatedAt,
				"used_by":     usedBy[m.Name],
			}
			if m.LastUsedAt > 0 {
				row["last_used_at"] = m.LastUsedAt
			}
			if m.Pending {
				row["pending"] = true
				row["requested_by"] = m.RequestedBy
			}
			out = append(out, row)
		}
		writeJSON(w, http.StatusOK, out)
	case http.MethodPost:
		var body struct {
			Name        string `json:"name"`
			Value       string `json:"value"`
			Description string `json:"description"`
			// Request creates a value-less placeholder for the owner to fill.
			Request bool `json:"request"`
		}
		if err := decode(r, &body); err != nil {
			writeError(w, http.StatusBadRequest, err.Error())
			return
		}
		if body.Request {
			if body.Value != "" {
				writeError(w, http.StatusBadRequest, "a secret request carries no value")
				return
			}
			s.secretsRequest(w, r, body.Name, body.Description)
			return
		}
		if t := operatorFromContext(r.Context()); t != nil && !t.HasScope(identity.ScopeOwner) {
			writeJSON(w, http.StatusForbidden, map[string]any{
				"error": "operator_scope", "required_scope": identity.ScopeOwner,
				"hint": `secret values come from the owner: POST {"name":..., "description":..., "request":true} and ask them to fill it in`,
			})
			return
		}
		m, err := s.secrets.Create(r.Context(), body.Name, body.Value, body.Description)
		if err != nil {
			status := http.StatusBadRequest
			if errors.Is(err, secrets.ErrExists) {
				status = http.StatusConflict
			}
			writeError(w, status, err.Error())
			return
		}
		writeJSON(w, http.StatusOK, m)
	default:
		writeError(w, http.StatusMethodNotAllowed, "GET or POST")
	}
}

// secretsItem handles PUT (rotate, optional ?reconnect=1) and DELETE (409 with
// used_by unless ?force=1) for /v1/secrets/{name}.
func (s *Server) secretsItem(w http.ResponseWriter, r *http.Request) {
	if _, err := s.requireUser(r); err != nil {
		writeError(w, http.StatusUnauthorized, "unauthorized")
		return
	}
	if s.secrets == nil {
		writeError(w, http.StatusServiceUnavailable, "secrets broker is disabled")
		return
	}
	name := strings.TrimPrefix(r.URL.Path, "/v1/secrets/")
	if name == "" || strings.Contains(name, "/") {
		http.NotFound(w, r)
		return
	}
	switch r.Method {
	case http.MethodPut:
		var body struct {
			Value       string  `json:"value"`
			Description *string `json:"description"`
		}
		if err := decode(r, &body); err != nil {
			writeError(w, http.StatusBadRequest, err.Error())
			return
		}
		wasPending := false
		if prev, perr := s.secrets.Get(r.Context(), name); perr == nil {
			wasPending = prev.Pending
		}
		m, err := s.secrets.Update(r.Context(), name, body.Value, body.Description)
		if err != nil {
			status := http.StatusBadRequest
			if errors.Is(err, secrets.ErrNotFound) {
				status = http.StatusNotFound
			}
			writeError(w, status, err.Error())
			return
		}
		// Optional: reconnect every upstream that references this secret so
		// the rotated value takes effect without a manual reconnect click.
		reconnected := []string{}
		// Filling a requested secret always reconnects: the servers that
		// reference it have been waiting for exactly this value.
		if (wasPending || r.URL.Query().Get("reconnect") == "1") && s.upstreams != nil {
			for _, srvName := range s.secretUsage(r)[name] {
				if _, rerr := s.upstreams.Reconnect(r.Context(), srvName); rerr == nil {
					reconnected = append(reconnected, srvName)
				}
			}
		}
		writeJSON(w, http.StatusOK, map[string]any{"secret": m, "reconnected": reconnected})
	case http.MethodDelete:
		used := s.secretUsage(r)[name]
		if len(used) > 0 && r.URL.Query().Get("force") != "1" {
			writeJSON(w, http.StatusConflict, map[string]any{
				"error":   "secret is still referenced",
				"used_by": used,
			})
			return
		}
		if err := s.secrets.Delete(r.Context(), name); err != nil {
			status := http.StatusBadRequest
			if errors.Is(err, secrets.ErrNotFound) {
				status = http.StatusNotFound
			}
			writeError(w, status, err.Error())
			return
		}
		writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
	default:
		writeError(w, http.StatusMethodNotAllowed, "PUT or DELETE")
	}
}

// secretUsage returns secret-name -> []server-name for every secret:// ref in
// any upstream's env or headers. Drives the used_by display and the
// delete-protection / rotate-reconnect flows.
func (s *Server) secretUsage(r *http.Request) map[string][]string {
	out := map[string][]string{}
	if s.upstreams == nil {
		return out
	}
	servers, err := s.upstreams.List(r.Context())
	if err != nil {
		return out
	}
	add := func(server string, m map[string]string) {
		for _, v := range m {
			names, _ := secrets.Refs(v)
			for _, n := range names {
				if !contains(out[n], server) {
					out[n] = append(out[n], server)
				}
			}
		}
	}
	for _, srv := range servers {
		add(srv.Name, srv.Env)
		add(srv.Name, srv.Headers)
	}
	for k := range out {
		sort.Strings(out[k])
	}
	return out
}

func contains(s []string, v string) bool {
	for _, x := range s {
		if x == v {
			return true
		}
	}
	return false
}

// serversConvertEnv handles POST /v1/servers/{name}/convert-env
// {env_key, secret_name}: extract the plaintext env value, store it as a
// secret, rewrite the env entry to a ref, and reconnect.
func (s *Server) serversConvertEnv(w http.ResponseWriter, r *http.Request, name string) {
	if r.Method != http.MethodPost {
		writeError(w, http.StatusMethodNotAllowed, "POST only")
		return
	}
	if s.secrets == nil {
		writeError(w, http.StatusServiceUnavailable, "secrets broker is disabled")
		return
	}
	var body struct {
		EnvKey     string `json:"env_key"`
		SecretName string `json:"secret_name"`
	}
	if err := decode(r, &body); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	srv, err := s.upstreams.ConvertEnvToSecret(r.Context(), name, body.EnvKey, body.SecretName,
		func(ctx context.Context, n, v, d string) error {
			_, cerr := s.secrets.Create(ctx, n, v, d)
			return cerr
		})
	if err != nil {
		if srv != nil {
			writeJSON(w, http.StatusAccepted, map[string]any{"server": upstreams.Masked(*srv), "warning": serverWarning(err, *srv)})
			return
		}
		status := http.StatusBadRequest
		if errors.Is(err, secrets.ErrExists) {
			status = http.StatusConflict
		}
		writeError(w, status, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, upstreams.Masked(*srv))
}

// secretsRequest creates a value-less secret the owner fills in from the
// dashboard. Agents use it to wire servers to credentials they never see:
// reference secret://NAME (or ${secret://NAME}) straight away; the server
// connects once the owner sets the value.
func (s *Server) secretsRequest(w http.ResponseWriter, r *http.Request, name, description string) {
	requestedBy := ""
	if t := operatorFromContext(r.Context()); t != nil {
		requestedBy = "operator: " + t.Name
	} else if u, ok := s.sessionUser(r); ok {
		requestedBy = u.Label()
	}
	m, err := s.secrets.Request(r.Context(), name, description, requestedBy)
	if err != nil {
		status := http.StatusBadRequest
		if errors.Is(err, secrets.ErrExists) {
			status = http.StatusConflict
		}
		writeError(w, status, err.Error())
		return
	}
	out := map[string]any{
		"secret": m,
		"ref":    secrets.RefPrefix + m.Name,
		"next":   "reference the ref in a server's env or headers now; ask the owner to set the value in Settings → Secrets",
	}
	if base := strings.TrimRight(s.security.PublicURL, "/"); base != "" {
		out["fill_url"] = base + "/#settings"
	}
	writeJSON(w, http.StatusOK, out)
}
