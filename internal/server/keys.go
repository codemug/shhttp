package server

import (
	"net/http"

	"github.com/codemug/shhttp/internal/auth"
	"github.com/codemug/shhttp/internal/id"
	"github.com/codemug/shhttp/pkg/api"
)

func (s *Server) createKey(w http.ResponseWriter, r *http.Request, _ auth.Principal) {
	var req api.CreateKeyRequest
	if err := decodeJSON(w, r, &req, maxJSONBody, false); err != nil {
		s.writeError(w, err)
		return
	}
	k, err := s.auth.CreateKey(r.Context(), req)
	if err != nil {
		s.writeError(w, err)
		return
	}
	s.log.Info("key created", "audit", true, "key", k.ID, "name", k.Name, "scopes", k.Scopes)
	w.Header().Set("Location", "/v2/keys/"+k.ID)
	writeJSON(w, http.StatusCreated, k)
}

func (s *Server) listKeys(w http.ResponseWriter, r *http.Request, _ auth.Principal) {
	keys, err := s.auth.ListKeys(r.Context())
	if err != nil {
		s.writeError(w, err)
		return
	}
	if keys == nil {
		keys = []api.Key{}
	}
	writeJSON(w, http.StatusOK, api.KeyList{Keys: keys})
}

// keyID returns the {id} path value, or "" after answering 404 when it is not
// a key id.
func keyID(w http.ResponseWriter, r *http.Request) string {
	v := r.PathValue("id")
	if !id.Valid(id.Key, v) {
		writeProblem(w, http.StatusNotFound, "not found")
		return ""
	}
	return v
}

func (s *Server) getKey(w http.ResponseWriter, r *http.Request, _ auth.Principal) {
	kid := keyID(w, r)
	if kid == "" {
		return
	}
	k, err := s.auth.GetKey(r.Context(), kid)
	if err != nil {
		s.writeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, k)
}

func (s *Server) updateKey(w http.ResponseWriter, r *http.Request, _ auth.Principal) {
	kid := keyID(w, r)
	if kid == "" {
		return
	}
	var req api.UpdateKeyRequest
	if err := decodeJSON(w, r, &req, maxJSONBody, false); err != nil {
		s.writeError(w, err)
		return
	}
	k, err := s.auth.UpdateKey(r.Context(), kid, req)
	if err != nil {
		s.writeError(w, err)
		return
	}
	s.log.Info("key updated", "audit", true, "key", k.ID, "scopes", k.Scopes)
	writeJSON(w, http.StatusOK, k)
}

func (s *Server) rotateKey(w http.ResponseWriter, r *http.Request, _ auth.Principal) {
	kid := keyID(w, r)
	if kid == "" {
		return
	}
	var req api.RotateKeyRequest
	if err := decodeJSON(w, r, &req, maxJSONBody, true); err != nil {
		s.writeError(w, err)
		return
	}
	k, err := s.auth.RotateKey(r.Context(), kid, req.Grace.Std())
	if err != nil {
		s.writeError(w, err)
		return
	}
	s.log.Info("key rotated", "audit", true, "key", k.ID, "grace", req.Grace.Std().String())
	writeJSON(w, http.StatusOK, k)
}

func (s *Server) revokeKey(w http.ResponseWriter, r *http.Request, _ auth.Principal) {
	kid := keyID(w, r)
	if kid == "" {
		return
	}
	kill, err := boolParam(r, "kill_sessions")
	if err != nil {
		s.writeError(w, err)
		return
	}
	k, err := s.auth.RevokeKey(r.Context(), kid)
	if err != nil {
		s.writeError(w, err)
		return
	}
	resp := api.RevokeKeyResponse{Key: k}
	if kill {
		resp.KilledSessions = s.sessions.KillByKey(kid)
	}
	s.log.Info("key revoked", "audit", true, "key", k.ID, "killed_sessions", resp.KilledSessions)
	writeJSON(w, http.StatusOK, resp)
}
