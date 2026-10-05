package client

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/codemug/shhttp/pkg/api"
	"github.com/coder/websocket"
)

// ProtocolError is a protocol_error message from the server: a rejected
// message, or a session that could not be started. Status is the HTTP status
// the same failure gets over HTTP.
type ProtocolError struct {
	Status  int
	Message string
}

func (e *ProtocolError) Error() string {
	return fmt.Sprintf("%d %s: %s", e.Status, http.StatusText(e.Status), e.Message)
}

// Conn is a WebSocket connection to a session.
type Conn struct {
	ws      *websocket.Conn
	session api.Session
}

// ExecOptions tune Exec.
type ExecOptions struct {
	// OnDisconnect is what happens to the session when this connection
	// closes: "keep" (default), "kill", or a duration such as "1m" after
	// which it is killed unless a client has attached.
	OnDisconnect string
}

// Exec starts a session over a WebSocket. It returns once the server has
// started the session; a session the server refuses to start is returned as
// a *ProtocolError.
func (c *Client) Exec(ctx context.Context, spec api.SessionSpec, opts *ExecOptions) (*Conn, error) {
	ws, err := c.dial(ctx, "/v2/exec", nil)
	if err != nil {
		return nil, err
	}
	start := api.ClientMessage{Type: api.MsgStart, Spec: &spec}
	if opts != nil {
		start.OnDisconnect = opts.OnDisconnect
	}
	conn := &Conn{ws: ws}
	if err := conn.send(ctx, start); err != nil {
		ws.CloseNow()
		return nil, err
	}
	return conn, conn.readSession(ctx)
}

// AttachOptions tune Attach.
type AttachOptions struct {
	// From is the first sequence number to receive (default 1).
	From uint64
	// ReadOnly only receives events; it needs just the sessions:read scope.
	ReadOnly bool
}

// Attach connects to an existing session. Events from From onwards are
// replayed, then followed live.
func (c *Client) Attach(ctx context.Context, id string, opts *AttachOptions) (*Conn, error) {
	q := url.Values{}
	if opts != nil {
		if opts.From > 0 {
			q.Set("from", strconv.FormatUint(opts.From, 10))
		}
		if opts.ReadOnly {
			q.Set("readonly", "true")
		}
	}
	ws, err := c.dial(ctx, "/v2/sessions/"+url.PathEscape(id)+"/attach", q)
	if err != nil {
		return nil, err
	}
	conn := &Conn{ws: ws}
	return conn, conn.readSession(ctx)
}

func (c *Client) dial(ctx context.Context, path string, query url.Values) (*websocket.Conn, error) {
	u := c.baseURL + path
	switch {
	case strings.HasPrefix(u, "https://"):
		u = "wss://" + strings.TrimPrefix(u, "https://")
	case strings.HasPrefix(u, "http://"):
		u = "ws://" + strings.TrimPrefix(u, "http://")
	}
	if len(query) > 0 {
		u += "?" + query.Encode()
	}
	h := http.Header{}
	if c.key != "" {
		h.Set("Authorization", "Bearer "+c.key)
	}
	ws, resp, err := websocket.Dial(ctx, u, &websocket.DialOptions{
		HTTPClient:   c.http,
		HTTPHeader:   h,
		Subprotocols: []string{api.WSSubprotocol},
	})
	if err != nil {
		if resp != nil && resp.StatusCode >= 400 && resp.Body != nil {
			return nil, problemFrom(resp.StatusCode, resp.Body)
		}
		return nil, err
	}
	// Output chunks can be up to a few tens of kilobytes; leave room.
	ws.SetReadLimit(16 << 20)
	return ws, nil
}

// readSession reads the first server message, which describes the session.
func (c *Conn) readSession(ctx context.Context) error {
	raw, err := c.read(ctx)
	if err == nil {
		var m api.ServerMessage
		if err = json.Unmarshal(raw, &m); err == nil {
			switch {
			case m.Type == api.MsgSession && m.Session != nil:
				c.session = *m.Session
				return nil
			case m.Type == api.MsgProtocolError:
				err = &ProtocolError{Status: m.Status, Message: m.Error}
			default:
				err = fmt.Errorf("unexpected first message %q", m.Type)
			}
		}
	}
	c.ws.CloseNow()
	return err
}

func (c *Conn) read(ctx context.Context) ([]byte, error) {
	_, b, err := c.ws.Read(ctx)
	if err != nil {
		if websocket.CloseStatus(err) == websocket.StatusNormalClosure {
			return nil, io.EOF
		}
		return nil, err
	}
	return b, nil
}

func (c *Conn) send(ctx context.Context, msg api.ClientMessage) error {
	b, err := json.Marshal(msg)
	if err != nil {
		return err
	}
	return c.ws.Write(ctx, websocket.MessageText, b)
}

// Session returns the session as it was when the connection opened.
func (c *Conn) Session() api.Session { return c.session }

// Recv returns the next event. It returns io.EOF after the session's exit
// event, when the server closes the connection. A rejected message arrives
// as a *ProtocolError; the connection remains usable after one.
//
// If ctx is cancelled or times out while Recv waits, the connection is
// closed. To wait with a deadline without losing the connection, call Recv
// from its own goroutine.
func (c *Conn) Recv(ctx context.Context) (api.Event, error) {
	raw, err := c.read(ctx)
	if err != nil {
		return api.Event{}, err
	}
	var m api.ServerMessage
	if err := json.Unmarshal(raw, &m); err != nil {
		return api.Event{}, err
	}
	if m.Type == api.MsgProtocolError {
		return api.Event{}, &ProtocolError{Status: m.Status, Message: m.Error}
	}
	var e api.Event
	return e, json.Unmarshal(raw, &e)
}

// Stdin sends input to the process.
func (c *Conn) Stdin(ctx context.Context, data []byte) error {
	if utf8.Valid(data) {
		return c.send(ctx, api.ClientMessage{Type: api.MsgStdin, Data: string(data)})
	}
	return c.send(ctx, api.ClientMessage{Type: api.MsgStdin, DataB64: data})
}

// CloseStdin closes the process's stdin.
func (c *Conn) CloseStdin(ctx context.Context) error {
	return c.send(ctx, api.ClientMessage{Type: api.MsgStdinClose})
}

// Signal sends a signal, such as "SIGINT", to the session's process group.
func (c *Conn) Signal(ctx context.Context, signal string) error {
	return c.send(ctx, api.ClientMessage{Type: api.MsgSignal, Signal: signal})
}

// Close closes the connection. What happens to the session depends on
// ExecOptions.OnDisconnect. Closing a connection the server already closed
// is not an error.
func (c *Conn) Close() {
	if c.ws.Close(websocket.StatusNormalClosure, "") != nil {
		c.ws.CloseNow()
	}
}

// Resize changes the terminal size of a TTY session.
func (c *Conn) Resize(ctx context.Context, cols, rows uint16) error {
	return c.send(ctx, api.ClientMessage{Type: api.MsgResize, Cols: cols, Rows: rows})
}
