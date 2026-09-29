package api

import "net/http"

// serversPatch edits a saved server in place: PATCH /v1/servers/{name}.
func (s *Server) serversPatch(w http.ResponseWriter, r *http.Request, name string) {
	writeError(w, http.StatusNotImplemented, "not implemented")
}
