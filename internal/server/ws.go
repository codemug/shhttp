package server

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"sync"
	"time"

	"github.com/codemug/shhttp/internal/session"
	"github.com/codemug/shhttp/pkg/api"
	"github.com/coder/websocket"
	"github.com/danielgtaylor/huma/v2"
	"github.com/danielgtaylor/huma/v2/adapters/humago"
)

const (
	// wsMaxMessage bounds a client message, such as one stdin chunk.
	wsMaxMessage = 1 << 20
	// wsStartTimeout is how long /v2/exec waits for the start message.
	wsStartTimeout = 10 * time.Second
	// wsPingInterval detects clients that vanished without closing.
	wsPingInterval = 30 * time.Second
)

type execInput struct{}

type attachInput struct {
	SessionPath
	From     uint64 `query:"from" default:"1" doc:"First sequence number to send."`
	ReadOnly bool   `query:"readonly" doc:"Only receive events; stdin and signal messages are rejected. Needs only the sessions:read scope."`
}

func (s *Server) registerWebSocket() {
	upgrade := map[string]*huma.Response{
		"101": {Description: "Switching Protocols: the connection is now a WebSocket using the " + api.WSSubprotocol + " protocol."},
	}

	op := operation("exec", http.MethodGet, "/v2/exec", "Start a session over a WebSocket", "WebSocket",
		access{scope: api.ScopeSessionsRun, websocket: true})
	op.Description = "Upgrades to a WebSocket. Offer the `" + api.WSSubprotocol + "` subprotocol; browsers, which cannot set " +
		"an Authorization header, also offer `" + api.WSAuthSubprotocolPrefix + "<key>`. " +
		"The first client message must be `{\"type\":\"start\",\"spec\":{…}}`. The server answers with a `session` message " +
		"followed by the session's events. Clients then send `stdin`, `stdin_close`, `signal` and, for TTY sessions, `resize` messages. " +
		"The server closes the connection with status 1000 when the session ends. See docs/v2-design.md for the full protocol."
	op.Responses = upgrade
	huma.Register(s.api, op, func(ctx context.Context, _ *execInput) (*streamOutput, error) {
		return &streamOutput{Body: s.serveExec}, nil
	})

	op = operation("attach", http.MethodGet, "/v2/sessions/{id}/attach", "Attach to a session over a WebSocket", "WebSocket",
		access{scope: api.ScopeSessionsRead, websocket: true})
	op.Description = "Upgrades to a WebSocket that replays the session's events from `from` and then follows them. " +
		"Unless `readonly=true`, the client may also send stdin and signals, which needs the sessions:run scope and " +
		"ownership of the session. Attaching to a finished session replays its events and closes."
	op.Responses = upgrade
	op.Errors = append(op.Errors, http.StatusNotFound)
	huma.Register(s.api, op, func(ctx context.Context, in *attachInput) (*streamOutput, error) {
		p := principalFrom(ctx)
		if !in.ReadOnly && !p.Has(api.ScopeSessionsRun) {
			return nil, huma.Error403Forbidden("sending input needs the sessions:run scope; attach with readonly=true to only watch")
		}
		sess, err := s.loadSession(ctx, in.ID, !in.ReadOnly)
		if err != nil {
			return nil, err
		}
		return &streamOutput{Body: func(hctx huma.Context) {
			conn, ctx, ok := s.accept(hctx)
			if !ok {
				return
			}
			s.log.Info("session attached", "audit", true, "session", sess.ID, "key", p.KeyID(), "readonly", in.ReadOnly)
			s.serveSession(ctx, conn, sess, in.From, in.ReadOnly, "")
		}}, nil
	})
}

// accept upgrades the connection. On failure the websocket library has
// already answered the request.
func (s *Server) accept(hctx huma.Context) (*websocket.Conn, context.Context, bool) {
	r, w := humago.Unwrap(hctx)
	conn, err := websocket.Accept(w, r, &websocket.AcceptOptions{
		Subprotocols:   []string{api.WSSubprotocol},
		OriginPatterns: s.opts.AllowedOrigins,
	})
	if err != nil {
		s.log.Debug("websocket upgrade failed", "err", err)
		return nil, nil, false
	}
	conn.SetReadLimit(wsMaxMessage)
	return conn, hctx.Context(), true
}

func (s *Server) serveExec(hctx huma.Context) {
	conn, ctx, ok := s.accept(hctx)
	if !ok {
		return
	}
	startCtx, cancel := context.WithTimeout(ctx, wsStartTimeout)
	msg, err := readMessage(startCtx, conn)
	cancel()
	if err != nil {
		conn.Close(websocket.StatusPolicyViolation, "expected a start message")
		return
	}
	fail := func(status int, detail string) {
		writeMessage(ctx, conn, api.ServerMessage{Type: api.MsgProtocolError, Status: status, Error: detail})
		conn.Close(websocket.StatusPolicyViolation, "session not started")
	}
	if msg.Type != api.MsgStart || msg.Spec == nil {
		fail(http.StatusBadRequest, `the first message must be {"type":"start","spec":{…}}`)
		return
	}
	if _, err := parseOnDisconnect(msg.OnDisconnect); err != nil {
		fail(http.StatusBadRequest, err.Error())
		return
	}
	p := principalFrom(ctx)
	sess, err := s.sessions.Create(ctx, session.Owner{KeyID: p.KeyID(), Policy: p.Key.Policy}, *msg.Spec)
	if err != nil {
		var se huma.StatusError
		if errors.As(s.apiError(err), &se) {
			fail(se.GetStatus(), se.Error())
		}
		return
	}
	s.serveSession(ctx, conn, sess, 1, false, msg.OnDisconnect)
}

// parseOnDisconnect returns how long to wait before killing a session whose
// last client left: 0 to kill at once, -1 to keep it.
func parseOnDisconnect(v string) (time.Duration, error) {
	switch v {
	case "", "keep":
		return -1, nil
	case "kill":
		return 0, nil
	}
	d, err := time.ParseDuration(v)
	if err != nil || d <= 0 {
		return 0, fmt.Errorf(`on_disconnect must be "keep", "kill" or a positive duration such as "1m", not %q`, v)
	}
	return d, nil
}

// serveSession sends the session message and the session's events from
// `from`, and handles client messages, until the session ends or either
// side closes.
func (s *Server) serveSession(ctx context.Context, conn *websocket.Conn, sess api.Session, from uint64, readonly bool, onDisconnect string) {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	defer conn.CloseNow()
	s.wsConns.Add(1)
	defer s.wsConns.Add(-1)

	if !readonly {
		s.attached.add(sess.ID)
		defer s.detach(sess.ID, onDisconnect)
	}
	l, err := s.sessions.Log(ctx, sess.ID)
	if err != nil {
		conn.Close(websocket.StatusInternalError, "session log unavailable")
		return
	}
	if err := writeMessage(ctx, conn, api.ServerMessage{Type: api.MsgSession, Session: &sess}); err != nil {
		return
	}
	go s.readClient(ctx, cancel, conn, sess.ID, readonly)
	go keepAlive(ctx, conn)

	for {
		evs, err := l.Read(ctx, from, 256, true)
		if err == io.EOF {
			conn.Close(websocket.StatusNormalClosure, "session ended")
			return
		}
		if err != nil {
			return // the client left or the server is stopping
		}
		for _, e := range evs {
			if err := writeMessage(ctx, conn, e); err != nil {
				return
			}
		}
		from = evs[len(evs)-1].Seq + 1
	}
}

type stdinItem struct {
	data  []byte
	close bool
}

// readClient handles client messages until the connection closes, then
// cancels the session's connection context. Stdin is written by a separate
// goroutine so that a process not reading its input cannot block signals.
func (s *Server) readClient(ctx context.Context, cancel context.CancelFunc, conn *websocket.Conn, sid string, readonly bool) {
	defer cancel()
	reject := func(status int, detail string) {
		writeMessage(ctx, conn, api.ServerMessage{Type: api.MsgProtocolError, Status: status, Error: detail})
	}
	stdin := make(chan stdinItem, 16)
	defer close(stdin)
	go func() {
		for item := range stdin {
			var err error
			if item.close {
				_, err = s.sessions.WriteStdin(sid, bytes.NewReader(nil), true)
			} else {
				_, err = s.sessions.WriteStdin(sid, bytes.NewReader(item.data), false)
			}
			if err != nil {
				var se huma.StatusError
				if errors.As(s.apiError(err), &se) {
					reject(se.GetStatus(), se.Error())
				}
			}
		}
	}()

	for {
		msg, err := readMessage(ctx, conn)
		var bad *badMessageError
		if errors.As(err, &bad) {
			reject(http.StatusBadRequest, bad.Error())
			continue
		}
		if err != nil {
			return
		}
		if readonly {
			reject(http.StatusForbidden, "this connection is read-only")
			continue
		}
		switch msg.Type {
		case api.MsgStdin:
			data := msg.DataB64
			if msg.Data != "" {
				data = []byte(msg.Data)
			}
			select {
			case stdin <- stdinItem{data: data}:
			case <-ctx.Done():
				return
			}
		case api.MsgStdinClose:
			select {
			case stdin <- stdinItem{close: true}:
			case <-ctx.Done():
				return
			}
		case api.MsgSignal:
			if err := s.sessions.Signal(sid, msg.Signal); err != nil {
				var se huma.StatusError
				if errors.As(s.apiError(err), &se) {
					reject(se.GetStatus(), se.Error())
				}
				continue
			}
			s.log.Info("session signalled", "audit", true, "session", sid, "key", principalFrom(ctx).KeyID(), "signal", msg.Signal)
		case api.MsgResize:
			if err := s.sessions.Resize(sid, api.TTYSize{Cols: msg.Cols, Rows: msg.Rows}); err != nil {
				var se huma.StatusError
				if errors.As(s.apiError(err), &se) {
					reject(se.GetStatus(), se.Error())
				}
			}
		case api.MsgStart:
			reject(http.StatusConflict, "the session has already started")
		default:
			reject(http.StatusBadRequest, fmt.Sprintf("unknown message type %q", msg.Type))
		}
	}
}

func keepAlive(ctx context.Context, conn *websocket.Conn) {
	t := time.NewTicker(wsPingInterval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			pingCtx, cancel := context.WithTimeout(ctx, wsPingInterval)
			err := conn.Ping(pingCtx)
			cancel()
			if err != nil {
				conn.CloseNow()
				return
			}
		}
	}
}

// badMessageError is a message that arrived but could not be decoded. The
// connection stays usable.
type badMessageError struct{ err error }

func (e *badMessageError) Error() string { return "invalid message: " + e.err.Error() }

// readMessage reads one client message. Errors other than *badMessageError
// mean the connection is gone.
func readMessage(ctx context.Context, conn *websocket.Conn) (api.ClientMessage, error) {
	var msg api.ClientMessage
	typ, b, err := conn.Read(ctx)
	if err != nil {
		return msg, err
	}
	if typ != websocket.MessageText {
		return msg, &badMessageError{errors.New("messages must be JSON text, not binary")}
	}
	dec := json.NewDecoder(bytes.NewReader(b))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&msg); err != nil {
		return msg, &badMessageError{err}
	}
	return msg, nil
}

func writeMessage(ctx context.Context, conn *websocket.Conn, v any) error {
	b, err := api.Marshal(v)
	if err != nil {
		return err
	}
	return conn.Write(ctx, websocket.MessageText, b)
}

// attachments counts the interactive connections of each session, so that
// on_disconnect can tell whether a client is still attached.
type attachments struct {
	mu sync.Mutex
	n  map[string]int
}

func newAttachments() *attachments { return &attachments{n: map[string]int{}} }

func (a *attachments) add(sid string) {
	a.mu.Lock()
	a.n[sid]++
	a.mu.Unlock()
}

// remove returns how many connections remain.
func (a *attachments) remove(sid string) int {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.n[sid]--
	n := a.n[sid]
	if n <= 0 {
		delete(a.n, sid)
	}
	return n
}

func (a *attachments) count(sid string) int {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.n[sid]
}

// detach applies the connection's on_disconnect policy.
func (s *Server) detach(sid, onDisconnect string) {
	remaining := s.attached.remove(sid)
	wait, _ := parseOnDisconnect(onDisconnect)
	if wait < 0 || remaining > 0 {
		return
	}
	kill := func() {
		if s.attached.count(sid) == 0 && s.sessions.Kill(sid) == nil {
			s.log.Info("session killed after its client disconnected", "audit", true, "session", sid, "on_disconnect", onDisconnect)
		}
	}
	if wait == 0 {
		kill()
		return
	}
	time.AfterFunc(wait, kill)
}
