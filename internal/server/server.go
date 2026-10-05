// Package server implements the HTTP API on top of huma, which validates
// requests against the operations' schemas and serves the OpenAPI document.
package server

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"reflect"
	"runtime/debug"
	"slices"
	"strings"
	"time"

	"github.com/codemug/shhttp/internal/auth"
	"github.com/codemug/shhttp/internal/job"
	"github.com/codemug/shhttp/internal/policy"
	"github.com/codemug/shhttp/internal/session"
	"github.com/codemug/shhttp/internal/store"
	"github.com/codemug/shhttp/internal/template"
	"github.com/codemug/shhttp/pkg/api"
	"github.com/danielgtaylor/huma/v2"
	"github.com/danielgtaylor/huma/v2/adapters/humago"
)

// Body size limits for JSON requests. Session creation allows more because
// the spec may carry initial stdin.
const (
	maxJSONBody    = 1 << 20
	maxSessionBody = 16 << 20
)

// APIVersion is the version of the API contract, as published in the OpenAPI
// document. The server's own version is served at /v2/version.
const APIVersion = "2.0"

// Paths of the generated API description.
const (
	OpenAPIPath = "/v2/openapi" // serves .json and .yaml
	DocsPath    = "/v2/docs"
)

// Options configures a Server.
type Options struct {
	// Version is the server version reported by /v2/version.
	Version string
	// AllowedOrigins are host patterns (such as "app.example.com" or
	// "https://*.example.com") of web pages allowed to open WebSocket
	// connections. Same-origin pages and non-browser clients are always
	// allowed.
	AllowedOrigins []string
}

// Deps are the services the server exposes.
type Deps struct {
	Auth      *auth.Authenticator
	Sessions  *session.Manager
	Jobs      *job.Runner
	Templates *template.Service
}

// Server serves the v2 API.
type Server struct {
	auth      *auth.Authenticator
	sessions  *session.Manager
	jobs      *job.Runner
	templates *template.Service
	log       *slog.Logger
	opts      Options
	mux       *http.ServeMux
	api       huma.API
	attached  *attachments
}

// New returns a Server.
func New(d Deps, logger *slog.Logger, opts Options) *Server {
	s := &Server{
		auth: d.Auth, sessions: d.Sessions, jobs: d.Jobs, templates: d.Templates,
		log: logger, opts: opts, mux: http.NewServeMux(), attached: newAttachments(),
	}

	config := huma.DefaultConfig("shhttp", APIVersion)
	config.Info.Description = "Run commands remotely, stream their output and send them input. " +
		"See https://github.com/codemug/shhttp/blob/v2/docs/v2-design.md."
	config.OpenAPIPath = OpenAPIPath
	config.DocsPath = DocsPath
	config.SchemasPath = "/v2/schemas"
	// The default hooks add a "$schema" field to every response body.
	config.CreateHooks = nil
	config.Components.SecuritySchemes = map[string]*huma.SecurityScheme{
		"bearer": {
			Type:        "http",
			Scheme:      "bearer",
			Description: "The master key (`shhm_…`) for /v2/keys, an API key (`shh_…`) for everything else.",
		},
	}
	// Durations are written as Go duration strings such as "1m30s".
	config.Components.Schemas.RegisterTypeAlias(reflect.TypeFor[api.Duration](), reflect.TypeFor[string]())

	s.api = humago.New(s.mux, config)
	s.api.UseMiddleware(s.authenticate)
	s.registerMeta(opts.Version)
	s.registerKeys()
	s.registerSessions()
	s.registerWebSocket()
	s.registerJobs()
	s.registerTemplates()
	return s
}

// OpenAPIYAML returns the OpenAPI document without starting a server.
func OpenAPIYAML() ([]byte, error) {
	return New(Deps{}, slog.New(slog.DiscardHandler), Options{}).api.OpenAPI().YAML()
}

// Handler returns the HTTP handler with logging, panic recovery and
// problem+json responses for unknown routes.
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

func writeProblem(w http.ResponseWriter, status int, detail string) {
	b, _ := json.Marshal(api.Problem{Title: http.StatusText(status), Status: status, Detail: detail})
	w.Header().Set("Content-Type", "application/problem+json")
	w.WriteHeader(status)
	w.Write(append(b, '\n'))
}

// access is the credential an operation requires. It is stored in the
// operation's metadata and checked by authenticate.
type access struct {
	public    bool     // no credential
	master    bool     // the master key
	scope     string   // an API key holding this scope; "" means any credential
	anyOf     []string // an API key holding at least one of these scopes
	websocket bool     // the key may also arrive as a WebSocket subprotocol
}

const accessKey = "access"

type principalKey struct{}

func principalFrom(ctx context.Context) auth.Principal {
	p, _ := ctx.Value(principalKey{}).(auth.Principal)
	return p
}

// operation fills in the parts every operation shares.
func operation(id, method, path, summary, tag string, a access) huma.Operation {
	op := huma.Operation{
		OperationID: id,
		Method:      method,
		Path:        path,
		Summary:     summary,
		Tags:        []string{tag},
		Metadata:    map[string]any{accessKey: a},
	}
	if !a.public {
		op.Security = []map[string][]string{{"bearer": {}}}
		op.Errors = []int{http.StatusUnauthorized, http.StatusForbidden}
	}
	return op
}

// authenticate is huma middleware enforcing each operation's access. It runs
// before the request body is read.
func (s *Server) authenticate(ctx huma.Context, next func(huma.Context)) {
	a, _ := ctx.Operation().Metadata[accessKey].(access)
	if a.public || ctx.Operation().Metadata[accessKey] == nil {
		next(ctx)
		return
	}
	token, ok := strings.CutPrefix(ctx.Header("Authorization"), "Bearer ")
	if !ok && a.websocket {
		token, ok = subprotocolKey(ctx.Header("Sec-WebSocket-Protocol"))
	}
	if !ok || strings.TrimSpace(token) == "" {
		ctx.SetHeader("WWW-Authenticate", `Bearer realm="shhttp"`)
		huma.WriteErr(s.api, ctx, http.StatusUnauthorized, "an Authorization: Bearer <key> header is required")
		return
	}
	p, err := s.auth.Authenticate(ctx.Context(), strings.TrimSpace(token))
	if errors.Is(err, auth.ErrUnauthorized) {
		ctx.SetHeader("WWW-Authenticate", `Bearer realm="shhttp", error="invalid_token"`)
		huma.WriteErr(s.api, ctx, http.StatusUnauthorized, "the key is invalid, expired or revoked")
		return
	}
	if err != nil {
		s.log.Error("authenticating", "err", err)
		huma.WriteErr(s.api, ctx, http.StatusInternalServerError, "internal error")
		return
	}
	switch {
	case a.master && !p.Master:
		huma.WriteErr(s.api, ctx, http.StatusForbidden, "only the master key can manage API keys")
		return
	case (a.scope != "" || len(a.anyOf) > 0) && p.Master:
		huma.WriteErr(s.api, ctx, http.StatusForbidden, "the master key can only manage API keys; create an API key to use this endpoint")
		return
	case a.scope != "" && !p.Has(a.scope):
		huma.WriteErr(s.api, ctx, http.StatusForbidden, "this key lacks the "+a.scope+" scope")
		return
	case len(a.anyOf) > 0 && !slices.ContainsFunc(a.anyOf, p.Has):
		huma.WriteErr(s.api, ctx, http.StatusForbidden, "this key needs one of the scopes "+strings.Join(a.anyOf, ", "))
		return
	}
	next(huma.WithValue(ctx, principalKey{}, p))
}

// subprotocolKey finds a key offered as a WebSocket subprotocol, which is how
// browsers, unable to set headers on WebSocket requests, authenticate.
func subprotocolKey(header string) (string, bool) {
	for _, p := range strings.Split(header, ",") {
		if key, ok := strings.CutPrefix(strings.TrimSpace(p), api.WSAuthSubprotocolPrefix); ok {
			return key, true
		}
	}
	return "", false
}

type healthOutput struct {
	ContentType string `header:"Content-Type"`
	Body        []byte
}

type versionOutput struct{ Body api.Version }

type whoamiOutput struct{ Body api.Whoami }

func (s *Server) registerMeta(version string) {
	huma.Register(s.api, operation("health", http.MethodGet, "/healthz", "Health check", "Server", access{public: true}),
		func(ctx context.Context, _ *struct{}) (*healthOutput, error) {
			return &healthOutput{ContentType: "text/plain", Body: []byte("ok\n")}, nil
		})
	huma.Register(s.api, operation("get-version", http.MethodGet, "/v2/version", "Server version", "Server", access{public: true}),
		func(ctx context.Context, _ *struct{}) (*versionOutput, error) {
			return &versionOutput{Body: api.Version{Version: version}}, nil
		})
	op := operation("whoami", http.MethodGet, "/v2/whoami", "Describe the calling key", "Keys", access{})
	op.Description = "Works with the master key and with API keys. For API keys, notes point out consequences of the policy that are easy to miss."
	huma.Register(s.api, op, func(ctx context.Context, _ *struct{}) (*whoamiOutput, error) {
		p := principalFrom(ctx)
		if p.Master {
			return &whoamiOutput{Body: api.Whoami{Master: true}}, nil
		}
		return &whoamiOutput{Body: api.Whoami{Key: &p.Key.Key, Notes: policy.Notes(p.Key.Policy)}}, nil
	})
}

// apiError maps a domain error to a huma error with the right status.
func (s *Server) apiError(err error) error {
	var (
		sessInvalid *session.InvalidError
		keyInvalid  *auth.InvalidError
		denied      *policy.DeniedError
		limit       *session.LimitError
		jobInvalid  *job.InvalidError
		tplInvalid  *template.InvalidError
		statusErr   huma.StatusError
	)
	switch {
	case errors.As(err, &statusErr):
		return err
	case errors.Is(err, session.ErrNotFound), errors.Is(err, store.ErrNotFound),
		errors.Is(err, job.ErrNotFound), errors.Is(err, template.ErrNotFound):
		return huma.Error404NotFound("not found")
	case errors.Is(err, session.ErrNotRunning), errors.Is(err, session.ErrStdinClosed), errors.Is(err, auth.ErrRevoked),
		errors.Is(err, job.ErrFinished), errors.Is(err, job.ErrQueueInUse):
		return huma.Error409Conflict(err.Error())
	case errors.As(err, &sessInvalid), errors.As(err, &keyInvalid), errors.As(err, &jobInvalid), errors.As(err, &tplInvalid):
		return huma.Error400BadRequest(err.Error())
	case errors.As(err, &denied):
		return huma.Error403Forbidden(err.Error())
	case errors.As(err, &limit):
		return huma.Error429TooManyRequests(err.Error())
	case errors.Is(err, auth.ErrKeyInactive):
		return huma.Error403Forbidden(err.Error())
	case errors.Is(err, session.ErrShuttingDown):
		return huma.Error503ServiceUnavailable(err.Error())
	case errors.Is(err, context.Canceled):
		// The client went away; nobody reads the response.
		return huma.Error400BadRequest("request cancelled")
	}
	s.log.Error("internal error", "err", err)
	return huma.Error500InternalServerError("internal error")
}
