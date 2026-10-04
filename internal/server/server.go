// Package server implements the HTTP API.
package server

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"runtime/debug"
	"strings"
	"time"

	"github.com/codemug/shhttp/internal/auth"
	"github.com/codemug/shhttp/internal/policy"
	"github.com/codemug/shhttp/internal/session"
	"github.com/codemug/shhttp/internal/store"
	"github.com/codemug/shhttp/pkg/api"
)

// Body size limits for JSON requests. Session creation allows more because
// the spec may carry initial stdin.
const (
	maxJSONBody    = 1 << 20
	maxSessionBody = 16 << 20
)

// Server serves the v2 API.
type Server struct {
	auth     *auth.Authenticator
	sessions *session.Manager
	log      *slog.Logger
	version  string
	mux      *http.ServeMux
}

// New returns a Server.
func New(a *auth.Authenticator, m *session.Manager, logger *slog.Logger, version string) *Server {
	s := &Server{auth: a, sessions: m, log: logger, version: version, mux: http.NewServeMux()}
	s.routes()
	return s
}

func (s *Server) routes() {
	s.mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/plain")
		io.WriteString(w, "ok\n")
	})
	s.mux.HandleFunc("GET /v2/version", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, api.Version{Version: s.version})
	})
	s.mux.HandleFunc("GET /v2/whoami", s.authed(s.whoami))

	s.mux.HandleFunc("POST /v2/keys", s.master(s.createKey))
	s.mux.HandleFunc("GET /v2/keys", s.master(s.listKeys))
	s.mux.HandleFunc("GET /v2/keys/{id}", s.master(s.getKey))
	s.mux.HandleFunc("PATCH /v2/keys/{id}", s.master(s.updateKey))
	s.mux.HandleFunc("DELETE /v2/keys/{id}", s.master(s.revokeKey))
	s.mux.HandleFunc("POST /v2/keys/{id}/rotate", s.master(s.rotateKey))

	s.mux.HandleFunc("POST /v2/sessions", s.scoped(api.ScopeSessionsRun, s.createSession))
	s.mux.HandleFunc("GET /v2/sessions", s.scoped(api.ScopeSessionsRead, s.listSessions))
	s.mux.HandleFunc("GET /v2/sessions/{id}", s.scoped(api.ScopeSessionsRead, s.getSession))
	s.mux.HandleFunc("DELETE /v2/sessions/{id}", s.scoped(api.ScopeSessionsRun, s.deleteSession))
	s.mux.HandleFunc("GET /v2/sessions/{id}/events", s.scoped(api.ScopeSessionsRead, s.sessionEvents))
	s.mux.HandleFunc("POST /v2/sessions/{id}/stdin", s.scoped(api.ScopeSessionsRun, s.sessionStdin))
	s.mux.HandleFunc("POST /v2/sessions/{id}/signal", s.scoped(api.ScopeSessionsRun, s.sessionSignal))
	s.mux.HandleFunc("POST /v2/sessions/{id}/kill", s.scoped(api.ScopeSessionsRun, s.sessionKill))
}

// Handler returns the HTTP handler with logging and panic recovery.
func (s *Server) Handler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		sw := &statusWriter{ResponseWriter: w, status: http.StatusOK}
		defer func() {
			if v := recover(); v != nil {
				if v == http.ErrAbortHandler {
					panic(v)
				}
				s.log.Error("panic serving request", "panic", v, "stack", string(debug.Stack()))
				if !sw.wrote {
					writeProblem(sw, http.StatusInternalServerError, "internal error")
				}
			}
			s.log.Debug("request", "method", r.Method, "path", r.URL.Path, "status", sw.status,
				"duration_ms", time.Since(start).Milliseconds(), "remote", r.RemoteAddr)
		}()
		if h, pattern := s.mux.Handler(r); pattern == "" {
			// No route matched: let the mux pick 404 or 405 (with its Allow
			// header), then answer in the API's error format.
			rec := &statusRecorder{header: http.Header{}}
			h.ServeHTTP(rec, r)
			if rec.status != http.StatusNotFound && rec.status != http.StatusMethodNotAllowed {
				// For example a redirect to the cleaned form of the path.
				h.ServeHTTP(sw, r)
				return
			}
			if allow := rec.header.Get("Allow"); allow != "" {
				sw.Header().Set("Allow", allow)
			}
			writeProblem(sw, rec.status, "no such endpoint or method")
			return
		}
		s.mux.ServeHTTP(sw, r)
	})
}

// statusRecorder captures the status and headers of the mux's built-in
// not-found and method-not-allowed responses.
type statusRecorder struct {
	header http.Header
	status int
}

func (r *statusRecorder) Header() http.Header         { return r.header }
func (r *statusRecorder) Write(b []byte) (int, error) { return len(b), nil }
func (r *statusRecorder) WriteHeader(code int)        { r.status = code }

// statusWriter records the response status. Unwrap lets
// http.ResponseController reach the underlying writer for flushing.
type statusWriter struct {
	http.ResponseWriter
	status int
	wrote  bool
}

func (w *statusWriter) WriteHeader(code int) {
	if !w.wrote {
		w.status, w.wrote = code, true
	}
	w.ResponseWriter.WriteHeader(code)
}

func (w *statusWriter) Write(b []byte) (int, error) {
	w.wrote = true
	return w.ResponseWriter.Write(b)
}

func (w *statusWriter) Unwrap() http.ResponseWriter { return w.ResponseWriter }

type handler func(w http.ResponseWriter, r *http.Request, p auth.Principal)

// authed requires any valid credential.
func (s *Server) authed(h handler) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		token, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
		if !ok || token == "" {
			w.Header().Set("WWW-Authenticate", `Bearer realm="shhttp"`)
			writeProblem(w, http.StatusUnauthorized, "an Authorization: Bearer <key> header is required")
			return
		}
		p, err := s.auth.Authenticate(r.Context(), strings.TrimSpace(token))
		if errors.Is(err, auth.ErrUnauthorized) {
			w.Header().Set("WWW-Authenticate", `Bearer realm="shhttp", error="invalid_token"`)
			writeProblem(w, http.StatusUnauthorized, "the key is invalid, expired or revoked")
			return
		}
		if err != nil {
			s.internalError(w, err)
			return
		}
		h(w, r, p)
	}
}

// master requires the master key.
func (s *Server) master(h handler) http.HandlerFunc {
	return s.authed(func(w http.ResponseWriter, r *http.Request, p auth.Principal) {
		if !p.Master {
			writeProblem(w, http.StatusForbidden, "only the master key can manage API keys")
			return
		}
		h(w, r, p)
	})
}

// scoped requires an API key holding scope.
func (s *Server) scoped(scope string, h handler) http.HandlerFunc {
	return s.authed(func(w http.ResponseWriter, r *http.Request, p auth.Principal) {
		if p.Master {
			writeProblem(w, http.StatusForbidden, "the master key can only manage API keys; create an API key to use this endpoint")
			return
		}
		if !p.Has(scope) {
			writeProblem(w, http.StatusForbidden, fmt.Sprintf("this key lacks the %s scope", scope))
			return
		}
		h(w, r, p)
	})
}

func (s *Server) whoami(w http.ResponseWriter, r *http.Request, p auth.Principal) {
	if p.Master {
		writeJSON(w, http.StatusOK, api.Whoami{Master: true})
		return
	}
	writeJSON(w, http.StatusOK, api.Whoami{Key: &p.Key.Key, Notes: policy.Notes(p.Key.Policy)})
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	b, err := api.Marshal(v)
	if err != nil {
		writeProblem(w, http.StatusInternalServerError, "encoding response: "+err.Error())
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	w.Write(append(b, '\n'))
}

func writeProblem(w http.ResponseWriter, status int, detail string) {
	b, _ := json.Marshal(api.Problem{Title: http.StatusText(status), Status: status, Detail: detail})
	w.Header().Set("Content-Type", "application/problem+json")
	w.Header().Del("Content-Length")
	w.WriteHeader(status)
	w.Write(append(b, '\n'))
}

func (s *Server) internalError(w http.ResponseWriter, err error) {
	s.log.Error("internal error", "err", err)
	writeProblem(w, http.StatusInternalServerError, "internal error")
}

// writeError maps a domain error to a problem response.
func (s *Server) writeError(w http.ResponseWriter, err error) {
	var (
		sessInvalid *session.InvalidError
		keyInvalid  *auth.InvalidError
		denied      *policy.DeniedError
		limit       *session.LimitError
		bad         *badRequestError
	)
	switch {
	case errors.Is(err, session.ErrNotFound), errors.Is(err, store.ErrNotFound):
		writeProblem(w, http.StatusNotFound, "not found")
	case errors.Is(err, session.ErrNotRunning), errors.Is(err, session.ErrStdinClosed), errors.Is(err, auth.ErrRevoked):
		writeProblem(w, http.StatusConflict, err.Error())
	case errors.As(err, &sessInvalid), errors.As(err, &keyInvalid), errors.As(err, &bad):
		writeProblem(w, http.StatusBadRequest, err.Error())
	case errors.As(err, &denied):
		writeProblem(w, http.StatusForbidden, err.Error())
	case errors.As(err, &limit):
		writeProblem(w, http.StatusTooManyRequests, err.Error())
	case errors.Is(err, session.ErrShuttingDown):
		writeProblem(w, http.StatusServiceUnavailable, err.Error())
	case errors.Is(err, context.Canceled):
		// The client went away; nobody reads the response.
	default:
		s.internalError(w, err)
	}
}

type badRequestError struct{ msg string }

func (e *badRequestError) Error() string { return e.msg }

func badRequest(format string, args ...any) error {
	return &badRequestError{fmt.Sprintf(format, args...)}
}

// decodeJSON decodes a single JSON object, rejecting unknown fields. An
// empty body leaves v unchanged when optional is true.
func decodeJSON(w http.ResponseWriter, r *http.Request, v any, limit int64, optional bool) error {
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, limit))
	if err != nil {
		var tooBig *http.MaxBytesError
		if errors.As(err, &tooBig) {
			return badRequest("request body exceeds %d bytes", limit)
		}
		return err
	}
	if len(bytes.TrimSpace(body)) == 0 {
		if optional {
			return nil
		}
		return badRequest("a JSON request body is required")
	}
	dec := json.NewDecoder(bytes.NewReader(body))
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil {
		return badRequest("invalid JSON body: %v", err)
	}
	if dec.More() {
		return badRequest("invalid JSON body: unexpected data after the object")
	}
	return nil
}
