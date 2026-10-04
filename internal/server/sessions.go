package server

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/codemug/shhttp/internal/auth"
	"github.com/codemug/shhttp/internal/eventlog"
	"github.com/codemug/shhttp/internal/id"
	"github.com/codemug/shhttp/internal/session"
	"github.com/codemug/shhttp/internal/store"
	"github.com/codemug/shhttp/pkg/api"
)

const (
	defaultMaxOutput = 1 << 20
	maxMaxOutput     = 64 << 20
	defaultListLimit = 50
	maxListLimit     = 500
	// sseKeepAlive is how often an idle SSE stream sends a comment so that
	// proxies do not close it.
	sseKeepAlive = 15 * time.Second
)

func boolParam(r *http.Request, name string) (bool, error) {
	v := r.URL.Query().Get(name)
	if v == "" {
		return false, nil
	}
	b, err := strconv.ParseBool(v)
	if err != nil {
		return false, badRequest("query parameter %s must be true or false", name)
	}
	return b, nil
}

func durationParam(r *http.Request, name string) (time.Duration, error) {
	v := r.URL.Query().Get(name)
	if v == "" {
		return 0, nil
	}
	d, err := time.ParseDuration(v)
	if err != nil || d < 0 {
		return 0, badRequest("query parameter %s must be a duration such as 30s", name)
	}
	return d, nil
}

func intParam(r *http.Request, name string, def, min, max int) (int, error) {
	v := r.URL.Query().Get(name)
	if v == "" {
		return def, nil
	}
	n, err := strconv.Atoi(v)
	if err != nil || n < min || n > max {
		return 0, badRequest("query parameter %s must be an integer from %d to %d", name, min, max)
	}
	return n, nil
}

// visible reports whether p may read sess.
func visible(p auth.Principal, sess api.Session) bool {
	return sess.KeyID == p.KeyID() || p.Has(api.ScopeAdminRead)
}

// loadSession returns the {id} session if p may see it (or, with write set,
// change it). Otherwise it answers 404, so other keys' sessions are not
// revealed.
func (s *Server) loadSession(w http.ResponseWriter, r *http.Request, p auth.Principal, write bool) (api.Session, bool) {
	sid := r.PathValue("id")
	if !id.Valid(id.Session, sid) {
		writeProblem(w, http.StatusNotFound, "not found")
		return api.Session{}, false
	}
	sess, err := s.sessions.Get(r.Context(), sid)
	if err != nil {
		s.writeError(w, err)
		return api.Session{}, false
	}
	if (write && sess.KeyID != p.KeyID()) || !visible(p, sess) {
		writeProblem(w, http.StatusNotFound, "not found")
		return api.Session{}, false
	}
	return sess, true
}

func (s *Server) createSession(w http.ResponseWriter, r *http.Request, p auth.Principal) {
	var spec api.SessionSpec
	if err := decodeJSON(w, r, &spec, maxSessionBody, false); err != nil {
		s.writeError(w, err)
		return
	}
	wait, err := boolParam(r, "wait")
	if err != nil {
		s.writeError(w, err)
		return
	}
	waitTimeout, err := durationParam(r, "wait_timeout")
	if err != nil {
		s.writeError(w, err)
		return
	}
	maxOutput, err := intParam(r, "max_output", defaultMaxOutput, 0, maxMaxOutput)
	if err != nil {
		s.writeError(w, err)
		return
	}
	keep := r.URL.Query().Get("keep")
	if keep == "" {
		keep = "tail"
	}
	if keep != "head" && keep != "tail" {
		s.writeError(w, badRequest("query parameter keep must be head or tail"))
		return
	}
	if wait {
		// Nobody else knows the session id yet, so nobody could send more
		// input: a program reading stdin would wait forever.
		spec.StdinClose = true
	}

	sess, err := s.sessions.Create(r.Context(), session.Owner{KeyID: p.KeyID(), Policy: p.Key.Policy}, spec)
	if err != nil {
		s.writeError(w, err)
		return
	}
	if !wait {
		w.Header().Set("Location", "/v2/sessions/"+sess.ID)
		writeJSON(w, http.StatusCreated, sess)
		return
	}

	ctx := r.Context()
	if waitTimeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, waitTimeout)
		defer cancel()
	}
	sess, err = s.sessions.Wait(ctx, sess.ID)
	if err != nil && !errors.Is(err, context.DeadlineExceeded) {
		s.writeError(w, err)
		return
	}
	l, err := s.sessions.Log(r.Context(), sess.ID)
	if err != nil {
		s.writeError(w, err)
		return
	}
	out, err := collectOutput(r.Context(), l, maxOutput, keep == "tail")
	if err != nil {
		s.writeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, api.RunResult{Session: sess, Output: out})
}

// collectOutput gathers the stdout and stderr written so far, keeping at
// most max bytes of each.
func collectOutput(ctx context.Context, l *eventlog.Log, max int, keepTail bool) (api.Output, error) {
	stdout := &capped{max: max, tail: keepTail}
	stderr := &capped{max: max, tail: keepTail}
	from := uint64(1)
	for {
		evs, err := l.Read(ctx, from, 1024, false)
		if err == io.EOF {
			break
		}
		if err != nil {
			return api.Output{}, err
		}
		for _, e := range evs {
			switch e.Type {
			case api.EventStdout:
				stdout.add(e.Data)
			case api.EventStderr:
				stderr.add(e.Data)
			}
		}
		from = evs[len(evs)-1].Seq + 1
	}
	var out api.Output
	out.SetStdout(stdout.result())
	out.SetStderr(stderr.result())
	return out, nil
}

// capped keeps the first or last max bytes written to it.
type capped struct {
	buf       []byte
	max       int
	tail      bool
	truncated bool
}

func (c *capped) add(b []byte) {
	if !c.tail {
		room := c.max - len(c.buf)
		if len(b) > room {
			b = b[:max(room, 0)]
			c.truncated = true
		}
		c.buf = append(c.buf, b...)
		return
	}
	c.buf = append(c.buf, b...)
	if len(c.buf) > 2*c.max {
		c.buf = append([]byte(nil), c.buf[len(c.buf)-c.max:]...)
		c.truncated = true
	}
}

// result returns the kept bytes, trimmed so that truncation does not split a
// UTF-8 character.
func (c *capped) result() ([]byte, bool) {
	b := c.buf
	if c.tail && len(b) > c.max {
		b = b[len(b)-c.max:]
		c.truncated = true
	}
	if !c.truncated || !utf8.Valid(c.buf) {
		return b, c.truncated
	}
	if c.tail {
		for i := 0; i < utf8.UTFMax && len(b) > 0 && !utf8.RuneStart(b[0]); i++ {
			b = b[1:]
		}
	} else {
		for i := 0; i < utf8.UTFMax && len(b) > 0 && !utf8.Valid(b); i++ {
			b = b[:len(b)-1]
		}
	}
	return b, true
}

func (s *Server) listSessions(w http.ResponseWriter, r *http.Request, p auth.Principal) {
	q := r.URL.Query()
	f := store.SessionFilter{KeyID: p.KeyID(), State: api.SessionState(q.Get("state"))}
	if p.Has(api.ScopeAdminRead) {
		f.KeyID = q.Get("key_id") // empty lists every key's sessions
	} else if k := q.Get("key_id"); k != "" && k != p.KeyID() {
		writeProblem(w, http.StatusForbidden, "listing another key's sessions needs the admin:read scope")
		return
	}
	switch f.State {
	case "", api.StatePending, api.StateRunning, api.StateExited, api.StateFailedToStart,
		api.StateKilled, api.StateTimedOut, api.StateLost:
	default:
		s.writeError(w, badRequest("unknown state %q", f.State))
		return
	}
	for _, l := range q["label"] {
		k, v, ok := strings.Cut(l, "=")
		if !ok || k == "" {
			s.writeError(w, badRequest("label filters look like label=key=value"))
			return
		}
		if f.Labels == nil {
			f.Labels = map[string]string{}
		}
		f.Labels[k] = v
	}
	if c := q.Get("cursor"); c != "" {
		if !id.Valid(id.Session, c) {
			s.writeError(w, badRequest("invalid cursor"))
			return
		}
		f.Before = c
	}
	var err error
	if f.Limit, err = intParam(r, "limit", defaultListLimit, 1, maxListLimit); err != nil {
		s.writeError(w, err)
		return
	}
	list, err := s.sessions.List(r.Context(), f)
	if err != nil {
		s.writeError(w, err)
		return
	}
	resp := api.SessionList{Sessions: list}
	if len(list) == f.Limit {
		resp.NextCursor = list[len(list)-1].ID
	}
	writeJSON(w, http.StatusOK, resp)
}

func (s *Server) getSession(w http.ResponseWriter, r *http.Request, p auth.Principal) {
	if sess, ok := s.loadSession(w, r, p, false); ok {
		writeJSON(w, http.StatusOK, sess)
	}
}

func (s *Server) deleteSession(w http.ResponseWriter, r *http.Request, p auth.Principal) {
	sess, ok := s.loadSession(w, r, p, true)
	if !ok {
		return
	}
	if err := s.sessions.Delete(r.Context(), sess.ID); err != nil {
		s.writeError(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) sessionKill(w http.ResponseWriter, r *http.Request, p auth.Principal) {
	sess, ok := s.loadSession(w, r, p, true)
	if !ok {
		return
	}
	if err := s.sessions.Kill(sess.ID); err != nil {
		s.writeError(w, err)
		return
	}
	s.log.Info("session kill requested", "audit", true, "session", sess.ID, "key", p.KeyID())
	sess, err := s.sessions.Get(r.Context(), sess.ID)
	if err != nil {
		s.writeError(w, err)
		return
	}
	writeJSON(w, http.StatusAccepted, sess)
}

func (s *Server) sessionSignal(w http.ResponseWriter, r *http.Request, p auth.Principal) {
	sess, ok := s.loadSession(w, r, p, true)
	if !ok {
		return
	}
	var req api.SignalRequest
	if err := decodeJSON(w, r, &req, maxJSONBody, false); err != nil {
		s.writeError(w, err)
		return
	}
	if err := s.sessions.Signal(sess.ID, req.Signal); err != nil {
		s.writeError(w, err)
		return
	}
	s.log.Info("session signalled", "audit", true, "session", sess.ID, "key", p.KeyID(), "signal", req.Signal)
	sess, err := s.sessions.Get(r.Context(), sess.ID)
	if err != nil {
		s.writeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, sess)
}

func (s *Server) sessionStdin(w http.ResponseWriter, r *http.Request, p auth.Principal) {
	sess, ok := s.loadSession(w, r, p, true)
	if !ok {
		return
	}
	closeAfter, err := boolParam(r, "close")
	if err != nil {
		s.writeError(w, err)
		return
	}
	n, err := s.sessions.WriteStdin(sess.ID, r.Body, closeAfter)
	if err != nil {
		s.writeError(w, err)
		return
	}
	sess, err = s.sessions.Get(r.Context(), sess.ID)
	if err != nil {
		s.writeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, api.StdinResponse{Bytes: n, StdinOpen: sess.StdinOpen})
}

type streamFormat int

const (
	formatNDJSON streamFormat = iota
	formatSSE
	formatRaw
)

func eventFormat(r *http.Request) (streamFormat, error) {
	switch r.URL.Query().Get("format") {
	case "ndjson":
		return formatNDJSON, nil
	case "sse":
		return formatSSE, nil
	case "raw":
		return formatRaw, nil
	case "":
	default:
		return 0, badRequest("format must be ndjson, sse or raw")
	}
	accept := r.Header.Get("Accept")
	switch {
	case strings.Contains(accept, "text/event-stream"):
		return formatSSE, nil
	case strings.Contains(accept, "application/octet-stream"):
		return formatRaw, nil
	}
	return formatNDJSON, nil
}

func (s *Server) sessionEvents(w http.ResponseWriter, r *http.Request, p auth.Principal) {
	sess, ok := s.loadSession(w, r, p, false)
	if !ok {
		return
	}
	format, err := eventFormat(r)
	if err != nil {
		s.writeError(w, err)
		return
	}
	follow, err := boolParam(r, "follow")
	if err != nil {
		s.writeError(w, err)
		return
	}
	var from uint64 = 1
	if v := r.URL.Query().Get("from"); v != "" {
		if from, err = strconv.ParseUint(v, 10, 64); err != nil {
			s.writeError(w, badRequest("from must be a sequence number"))
			return
		}
	}
	if v := r.Header.Get("Last-Event-ID"); v != "" {
		last, err := strconv.ParseUint(v, 10, 64)
		if err != nil {
			s.writeError(w, badRequest("Last-Event-ID must be a sequence number"))
			return
		}
		from = last + 1
	}
	rawStream := api.EventStdout
	if format == formatRaw {
		switch r.URL.Query().Get("stream") {
		case "", "stdout":
		case "stderr":
			rawStream = api.EventStderr
		default:
			s.writeError(w, badRequest("stream must be stdout or stderr"))
			return
		}
	}
	l, err := s.sessions.Log(r.Context(), sess.ID)
	if err != nil {
		s.writeError(w, err)
		return
	}

	h := w.Header()
	switch format {
	case formatNDJSON:
		h.Set("Content-Type", "application/x-ndjson")
	case formatSSE:
		h.Set("Content-Type", "text/event-stream")
	case formatRaw:
		h.Set("Content-Type", "application/octet-stream")
	}
	h.Set("Cache-Control", "no-cache")
	h.Set("X-Accel-Buffering", "no") // ask nginx-style proxies not to buffer
	w.WriteHeader(http.StatusOK)
	rc := http.NewResponseController(w)
	if follow {
		rc.Flush()
	}

	ctx := r.Context()
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
				s.log.Error("reading session events", "session", sess.ID, "err", err)
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
	switch format {
	case formatRaw:
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
