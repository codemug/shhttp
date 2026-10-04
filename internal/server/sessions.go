package server

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"reflect"
	"strconv"
	"strings"
	"time"

	"github.com/codemug/shhttp/internal/auth"
	"github.com/codemug/shhttp/internal/eventlog"
	"github.com/codemug/shhttp/internal/id"
	"github.com/codemug/shhttp/internal/session"
	"github.com/codemug/shhttp/internal/store"
	"github.com/codemug/shhttp/pkg/api"
	"github.com/danielgtaylor/huma/v2"
	"github.com/danielgtaylor/huma/v2/adapters/humago"
)

const (
	defaultListLimit = 50
	// sseKeepAlive is how often an idle SSE stream sends a comment so that
	// proxies do not close it.
	sseKeepAlive = 15 * time.Second
)

// visible reports whether p may read sess.
func visible(p auth.Principal, sess api.Session) bool {
	return sess.KeyID == p.KeyID() || p.Has(api.ScopeAdminRead)
}

// SessionPath is the {id} of session operations (exported for embedding, as
// KeyPath).
type SessionPath struct {
	ID string `path:"id" doc:"Session id" example:"ses_01j9z3k5q8m2x7v4w6t0b1c3d5"`
}

// loadSession returns the session if the caller may see it (or, with write
// set, change it). Otherwise it returns 404, so other keys' sessions are not
// revealed.
func (s *Server) loadSession(ctx context.Context, sid string, write bool) (api.Session, error) {
	if !id.Valid(id.Session, sid) {
		return api.Session{}, huma.Error404NotFound("not found")
	}
	sess, err := s.sessions.Get(ctx, sid)
	if err != nil {
		return api.Session{}, s.apiError(err)
	}
	p := principalFrom(ctx)
	if (write && sess.KeyID != p.KeyID()) || !visible(p, sess) {
		return api.Session{}, huma.Error404NotFound("not found")
	}
	return sess, nil
}

type createSessionInput struct {
	Wait        bool   `query:"wait" doc:"Wait for the session to end and return its output. Stdin is closed after the initial input."`
	WaitTimeout string `query:"wait_timeout" doc:"With wait, return after this long even if the session is still running." example:"30s"`
	MaxOutput   int    `query:"max_output" minimum:"0" maximum:"67108864" default:"1048576" doc:"With wait, the most bytes of each stream to return."`
	Keep        string `query:"keep" enum:"head,tail" default:"tail" doc:"With wait, which part of truncated output to keep."`
	Body        api.SessionSpec
}

type createSessionOutput struct {
	Status   int
	Location string `header:"Location"`
	// Body is an api.Session (201) or an api.RunResult (200, with wait).
	Body any
}

type sessionOutput struct{ Body api.Session }

type listSessionsInput struct {
	State  string   `query:"state" enum:"pending,running,exited,failed_to_start,killed,timed_out,lost" doc:"Only sessions in this state."`
	KeyID  string   `query:"key_id" doc:"Only sessions of this key. Other keys' sessions need the admin:read scope."`
	Label  []string `query:"label,explode" doc:"Only sessions with this label, written key=value. Repeat for several." example:"team=infra"`
	Limit  int      `query:"limit" minimum:"1" maximum:"500" default:"50"`
	Cursor string   `query:"cursor" doc:"The next_cursor of the previous page."`
}

type sessionListOutput struct{ Body api.SessionList }

type eventsInput struct {
	SessionPath
	From        uint64 `query:"from" doc:"First sequence number to return." default:"1"`
	Follow      bool   `query:"follow" doc:"Keep the response open and stream new events until the session ends."`
	Format      string `query:"format" enum:"ndjson,sse,raw" doc:"Overrides the Accept header."`
	Stream      string `query:"stream" enum:"stdout,stderr" default:"stdout" doc:"For the raw format, which stream to return."`
	Accept      string `header:"Accept"`
	LastEventID string `header:"Last-Event-ID" doc:"Server-sent events resume after this sequence number."`
}

type streamOutput struct {
	Body func(huma.Context)
}

// stdinInput streams the request body into the process instead of letting
// huma read it into memory.
type stdinInput struct {
	SessionPath
	Close bool `query:"close" doc:"Close stdin after writing the body."`
	body  io.Reader
}

func (in *stdinInput) Resolve(ctx huma.Context) []error {
	in.body = ctx.BodyReader()
	return nil
}

type stdinOutput struct{ Body api.StdinResponse }

type signalInput struct {
	SessionPath
	Body api.SignalRequest
}

// Event documents the JSON form of api.Event, which has a custom encoder, in
// the OpenAPI document. It is not used at run time.
type Event struct {
	Seq        uint64           `json:"seq" doc:"Sequence number, starting at 1 with no gaps."`
	Time       time.Time        `json:"time"`
	Type       api.EventType    `json:"type" enum:"started,stdout,stderr,stdin_closed,signal,exit,error"`
	Data       string           `json:"data,omitempty" doc:"stdout and stderr: the output chunk, when it is valid UTF-8."`
	DataB64    []byte           `json:"data_b64,omitempty" doc:"stdout and stderr: the output chunk, when it is not valid UTF-8."`
	PID        int              `json:"pid,omitempty" doc:"started: the process id."`
	Signal     string           `json:"signal,omitempty" doc:"signal: the signal sent. exit: the signal that ended the process."`
	State      api.SessionState `json:"state,omitempty" doc:"exit: the final session state."`
	ExitCode   *int             `json:"exit_code,omitempty" doc:"exit: the exit code, unless a signal ended the process."`
	DurationMS *int64           `json:"duration_ms,omitempty" doc:"exit: run time in milliseconds."`
	Error      string           `json:"error,omitempty" doc:"error: what went wrong."`
}

func (s *Server) registerSessions() {
	reg := s.api.OpenAPI().Components.Schemas
	run := access{scope: api.ScopeSessionsRun}
	read := access{scope: api.ScopeSessionsRead}
	jsonOf := func(t reflect.Type, hint string) map[string]*huma.MediaType {
		return map[string]*huma.MediaType{"application/json": {Schema: reg.Schema(t, true, hint)}}
	}

	op := operation("create-session", http.MethodPost, "/v2/sessions", "Start a session", "Sessions", run)
	op.Description = "Starts a process. Without `wait` it returns the session at once (201). " +
		"With `wait=true` it returns the session and its output when the process ends or `wait_timeout` passes (200). " +
		"A process that fails to start still creates a session, in state `failed_to_start`."
	op.DefaultStatus = http.StatusCreated
	op.MaxBodyBytes = maxSessionBody
	op.Errors = append(op.Errors, http.StatusBadRequest, http.StatusTooManyRequests, http.StatusServiceUnavailable)
	op.Responses = map[string]*huma.Response{
		"201": {Description: "The session was started.", Content: jsonOf(reflect.TypeFor[api.Session](), "Session")},
		"200": {Description: "With wait=true: the session and its output.", Content: jsonOf(reflect.TypeFor[api.RunResult](), "RunResult")},
	}
	huma.Register(s.api, op, s.createSession)

	op = operation("list-sessions", http.MethodGet, "/v2/sessions", "List sessions", "Sessions", read)
	op.Description = "Newest first. A key sees its own sessions; `admin:read` sees every key's."
	huma.Register(s.api, op, s.listSessions)

	op = operation("get-session", http.MethodGet, "/v2/sessions/{id}", "Get a session", "Sessions", read)
	op.Errors = append(op.Errors, http.StatusNotFound)
	huma.Register(s.api, op, func(ctx context.Context, in *SessionPath) (*sessionOutput, error) {
		sess, err := s.loadSession(ctx, in.ID, false)
		if err != nil {
			return nil, err
		}
		return &sessionOutput{Body: sess}, nil
	})

	op = operation("get-session-events", http.MethodGet, "/v2/sessions/{id}/events", "Read or follow a session's events", "Sessions", read)
	op.Description = "Returns events from `from` onwards; with `follow=true` keeps streaming until the session ends. " +
		"Formats: newline-delimited JSON (default), server-sent events (`Accept: text/event-stream` or `format=sse`; " +
		"each event's `id` is its sequence number) or raw output bytes of one stream (`format=raw`)."
	op.Errors = append(op.Errors, http.StatusNotFound)
	op.Responses = map[string]*huma.Response{
		"200": {
			Description: "The events.",
			Content: map[string]*huma.MediaType{
				"application/x-ndjson":     {Schema: reg.Schema(reflect.TypeFor[Event](), true, "Event")},
				"text/event-stream":        {Schema: &huma.Schema{Type: "string", Description: "`id: <seq>` and `data: <event JSON>` per event."}},
				"application/octet-stream": {Schema: &huma.Schema{Type: "string", Format: "binary"}},
			},
		},
	}
	huma.Register(s.api, op, s.sessionEvents)

	op = operation("write-session-stdin", http.MethodPost, "/v2/sessions/{id}/stdin", "Write to a session's stdin", "Sessions", run)
	op.Description = "Streams the request body into the process as it arrives. Writes from several requests are not interleaved."
	op.RequestBody = &huma.RequestBody{
		Description: "Bytes to write.",
		Content:     map[string]*huma.MediaType{"application/octet-stream": {Schema: &huma.Schema{Type: "string", Format: "binary"}}},
	}
	op.Errors = append(op.Errors, http.StatusNotFound, http.StatusConflict)
	huma.Register(s.api, op, s.sessionStdin)

	op = operation("signal-session", http.MethodPost, "/v2/sessions/{id}/signal", "Send a signal", "Sessions", run)
	op.Description = "Sends the signal to the session's process group. Names may omit the SIG prefix."
	op.MaxBodyBytes = maxJSONBody
	op.Errors = append(op.Errors, http.StatusBadRequest, http.StatusNotFound, http.StatusConflict)
	huma.Register(s.api, op, s.sessionSignal)

	op = operation("kill-session", http.MethodPost, "/v2/sessions/{id}/kill", "Kill a session", "Sessions", run)
	op.Description = "Sends SIGTERM, then SIGKILL after the server's grace period. The session and its output are kept."
	op.DefaultStatus = http.StatusAccepted
	op.Errors = append(op.Errors, http.StatusNotFound, http.StatusConflict)
	huma.Register(s.api, op, func(ctx context.Context, in *SessionPath) (*sessionOutput, error) {
		sess, err := s.loadSession(ctx, in.ID, true)
		if err != nil {
			return nil, err
		}
		if err := s.sessions.Kill(sess.ID); err != nil {
			return nil, s.apiError(err)
		}
		s.log.Info("session kill requested", "audit", true, "session", sess.ID, "key", principalFrom(ctx).KeyID())
		if sess, err = s.sessions.Get(ctx, sess.ID); err != nil {
			return nil, s.apiError(err)
		}
		return &sessionOutput{Body: sess}, nil
	})

	op = operation("delete-session", http.MethodDelete, "/v2/sessions/{id}", "Delete a session", "Sessions", run)
	op.Description = "Kills the process if it is running, then deletes the session and its output."
	op.Errors = append(op.Errors, http.StatusNotFound)
	huma.Register(s.api, op, func(ctx context.Context, in *SessionPath) (*struct{}, error) {
		sess, err := s.loadSession(ctx, in.ID, true)
		if err != nil {
			return nil, err
		}
		if err := s.sessions.Delete(ctx, sess.ID); err != nil {
			return nil, s.apiError(err)
		}
		return nil, nil
	})
}

func (s *Server) createSession(ctx context.Context, in *createSessionInput) (*createSessionOutput, error) {
	var waitTimeout time.Duration
	if in.WaitTimeout != "" {
		d, err := time.ParseDuration(in.WaitTimeout)
		if err != nil || d < 0 {
			return nil, huma.Error400BadRequest("wait_timeout must be a duration such as 30s")
		}
		waitTimeout = d
	}
	spec := in.Body
	if in.Wait {
		// Nobody else knows the session id yet, so nobody could send more
		// input: a program reading stdin would wait forever.
		spec.StdinClose = true
	}
	p := principalFrom(ctx)
	sess, err := s.sessions.Create(ctx, session.Owner{KeyID: p.KeyID(), Policy: p.Key.Policy}, spec)
	if err != nil {
		return nil, s.apiError(err)
	}
	out := &createSessionOutput{Location: "/v2/sessions/" + sess.ID}
	if !in.Wait {
		out.Status, out.Body = http.StatusCreated, sess
		return out, nil
	}

	waitCtx := ctx
	if waitTimeout > 0 {
		var cancel context.CancelFunc
		waitCtx, cancel = context.WithTimeout(ctx, waitTimeout)
		defer cancel()
	}
	sess, err = s.sessions.Wait(waitCtx, sess.ID)
	if err != nil && !errors.Is(err, context.DeadlineExceeded) {
		return nil, s.apiError(err)
	}
	l, err := s.sessions.Log(ctx, sess.ID)
	if err != nil {
		return nil, s.apiError(err)
	}
	output, err := collectOutput(ctx, l, in.MaxOutput, in.Keep != "head")
	if err != nil {
		return nil, s.apiError(err)
	}
	out.Status, out.Body = http.StatusOK, api.RunResult{Session: sess, Output: output}
	return out, nil
}

func (s *Server) listSessions(ctx context.Context, in *listSessionsInput) (*sessionListOutput, error) {
	p := principalFrom(ctx)
	f := store.SessionFilter{KeyID: p.KeyID(), State: api.SessionState(in.State), Limit: in.Limit}
	if f.Limit == 0 {
		f.Limit = defaultListLimit
	}
	if p.Has(api.ScopeAdminRead) {
		f.KeyID = in.KeyID // empty lists every key's sessions
	} else if in.KeyID != "" && in.KeyID != p.KeyID() {
		return nil, huma.Error403Forbidden("listing another key's sessions needs the admin:read scope")
	}
	for _, l := range in.Label {
		k, v, ok := strings.Cut(l, "=")
		if !ok || k == "" {
			return nil, huma.Error400BadRequest("label filters look like label=key=value")
		}
		if f.Labels == nil {
			f.Labels = map[string]string{}
		}
		f.Labels[k] = v
	}
	if in.Cursor != "" {
		if !id.Valid(id.Session, in.Cursor) {
			return nil, huma.Error400BadRequest("invalid cursor")
		}
		f.Before = in.Cursor
	}
	list, err := s.sessions.List(ctx, f)
	if err != nil {
		return nil, s.apiError(err)
	}
	resp := api.SessionList{Sessions: list}
	if len(list) == f.Limit {
		resp.NextCursor = list[len(list)-1].ID
	}
	return &sessionListOutput{Body: resp}, nil
}

func (s *Server) sessionSignal(ctx context.Context, in *signalInput) (*sessionOutput, error) {
	sess, err := s.loadSession(ctx, in.ID, true)
	if err != nil {
		return nil, err
	}
	if err := s.sessions.Signal(sess.ID, in.Body.Signal); err != nil {
		return nil, s.apiError(err)
	}
	s.log.Info("session signalled", "audit", true, "session", sess.ID, "key", principalFrom(ctx).KeyID(), "signal", in.Body.Signal)
	if sess, err = s.sessions.Get(ctx, sess.ID); err != nil {
		return nil, s.apiError(err)
	}
	return &sessionOutput{Body: sess}, nil
}

func (s *Server) sessionStdin(ctx context.Context, in *stdinInput) (*stdinOutput, error) {
	sess, err := s.loadSession(ctx, in.ID, true)
	if err != nil {
		return nil, err
	}
	n, err := s.sessions.WriteStdin(sess.ID, in.body, in.Close)
	if err != nil {
		return nil, s.apiError(err)
	}
	if sess, err = s.sessions.Get(ctx, sess.ID); err != nil {
		return nil, s.apiError(err)
	}
	return &stdinOutput{Body: api.StdinResponse{Bytes: n, StdinOpen: sess.StdinOpen}}, nil
}

type streamFormat int

const (
	formatNDJSON streamFormat = iota
	formatSSE
	formatRaw
)

func eventFormat(in *eventsInput) streamFormat {
	switch in.Format {
	case "ndjson":
		return formatNDJSON
	case "sse":
		return formatSSE
	case "raw":
		return formatRaw
	}
	switch {
	case strings.Contains(in.Accept, "text/event-stream"):
		return formatSSE
	case strings.Contains(in.Accept, "application/octet-stream"):
		return formatRaw
	}
	return formatNDJSON
}

func (s *Server) sessionEvents(ctx context.Context, in *eventsInput) (*streamOutput, error) {
	sess, err := s.loadSession(ctx, in.ID, false)
	if err != nil {
		return nil, err
	}
	from := in.From
	if in.LastEventID != "" {
		last, err := strconv.ParseUint(in.LastEventID, 10, 64)
		if err != nil {
			return nil, huma.Error400BadRequest("Last-Event-ID must be a sequence number")
		}
		from = last + 1
	}
	format := eventFormat(in)
	rawStream := api.EventStdout
	if in.Stream == "stderr" {
		rawStream = api.EventStderr
	}
	l, err := s.sessions.Log(ctx, sess.ID)
	if err != nil {
		return nil, s.apiError(err)
	}
	return &streamOutput{Body: func(hctx huma.Context) {
		s.streamEvents(hctx, l, sess.ID, from, in.Follow, format, rawStream)
	}}, nil
}

func (s *Server) streamEvents(hctx huma.Context, l *eventlog.Log, sid string, from uint64, follow bool, format streamFormat, rawStream api.EventType) {
	switch format {
	case formatNDJSON:
		hctx.SetHeader("Content-Type", "application/x-ndjson")
	case formatSSE:
		hctx.SetHeader("Content-Type", "text/event-stream")
	case formatRaw:
		hctx.SetHeader("Content-Type", "application/octet-stream")
	}
	hctx.SetHeader("Cache-Control", "no-cache")
	hctx.SetHeader("X-Accel-Buffering", "no") // ask nginx-style proxies not to buffer
	hctx.SetStatus(http.StatusOK)
	_, rw := humago.Unwrap(hctx)
	rc := http.NewResponseController(rw)
	w := hctx.BodyWriter()
	if follow {
		rc.Flush()
	}

	ctx := hctx.Context()
	for {
		readCtx, cancel := ctx, context.CancelFunc(func() {})
		if format == formatSSE && follow {
			readCtx, cancel = context.WithTimeout(ctx, sseKeepAlive)
		}
		evs, err := l.Read(readCtx, from, 256, follow)
		cancel()
		if err != nil {
			if format == formatSSE && errors.Is(err, context.DeadlineExceeded) && ctx.Err() == nil {
				if _, err := io.WriteString(w, ": keep-alive\n\n"); err != nil || rc.Flush() != nil {
					return
				}
				continue
			}
			if err != io.EOF && ctx.Err() == nil {
				s.log.Error("reading session events", "session", sid, "err", err)
			}
			return
		}
		for _, e := range evs {
			if err := writeEvent(w, format, rawStream, e); err != nil {
				return
			}
		}
		if err := rc.Flush(); err != nil {
			return
		}
		from = evs[len(evs)-1].Seq + 1
	}
}

func writeEvent(w io.Writer, format streamFormat, rawStream api.EventType, e api.Event) error {
	if format == formatRaw {
		if e.Type == rawStream {
			_, err := w.Write(e.Data)
			return err
		}
		return nil
	}
	b, err := api.Marshal(e)
	if err != nil {
		return err
	}
	if format == formatSSE {
		_, err = fmt.Fprintf(w, "id: %d\ndata: %s\n\n", e.Seq, b)
		return err
	}
	_, err = w.Write(append(b, '\n'))
	return err
}
