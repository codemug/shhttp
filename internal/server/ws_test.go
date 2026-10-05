package server

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/codemug/shhttp/v2/pkg/api"
	"github.com/coder/websocket"
)

type wsClient struct {
	t    *testing.T
	conn *websocket.Conn
	ctx  context.Context
}

func dialWS(t *testing.T, ts *testServer, path, key string, header http.Header) (*wsClient, *http.Response, error) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	t.Cleanup(cancel)
	h := http.Header{}
	for k, v := range header {
		h[k] = v
	}
	if key != "" {
		h.Set("Authorization", "Bearer "+key)
	}
	conn, resp, err := websocket.Dial(ctx, "ws"+strings.TrimPrefix(ts.URL, "http")+path, &websocket.DialOptions{
		HTTPHeader:   h,
		Subprotocols: []string{api.WSSubprotocol},
	})
	if err != nil {
		return nil, resp, err
	}
	t.Cleanup(func() { conn.CloseNow() })
	return &wsClient{t: t, conn: conn, ctx: ctx}, resp, nil
}

func mustDialWS(t *testing.T, ts *testServer, path, key string) *wsClient {
	t.Helper()
	c, resp, err := dialWS(t, ts, path, key, nil)
	if err != nil {
		t.Fatalf("dial %s: %v (status %v)", path, err, resp)
	}
	return c
}

func (c *wsClient) send(msg api.ClientMessage) {
	c.t.Helper()
	b, _ := json.Marshal(msg)
	if err := c.conn.Write(c.ctx, websocket.MessageText, b); err != nil {
		c.t.Fatalf("send: %v", err)
	}
}

// wsMsg holds any server message.
type wsMsg struct {
	api.ServerMessage
	Event api.Event
}

func (c *wsClient) recv() (wsMsg, error) {
	_, b, err := c.conn.Read(c.ctx)
	if err != nil {
		return wsMsg{}, err
	}
	var m wsMsg
	if err := json.Unmarshal(b, &m.ServerMessage); err != nil {
		c.t.Fatalf("decoding %s: %v", b, err)
	}
	if m.Type != api.MsgSession && m.Type != api.MsgProtocolError {
		if err := json.Unmarshal(b, &m.Event); err != nil {
			c.t.Fatalf("decoding event %s: %v", b, err)
		}
	}
	return m, nil
}

func (c *wsClient) mustRecv() wsMsg {
	c.t.Helper()
	m, err := c.recv()
	if err != nil {
		c.t.Fatalf("recv: %v", err)
	}
	return m
}

// drain reads until the server closes and returns the messages and the
// close status.
func (c *wsClient) drain() ([]wsMsg, websocket.StatusCode) {
	c.t.Helper()
	var out []wsMsg
	for {
		m, err := c.recv()
		if err != nil {
			return out, websocket.CloseStatus(err)
		}
		out = append(out, m)
	}
}

func stdoutOf(msgs []wsMsg) string {
	var b strings.Builder
	for _, m := range msgs {
		if m.Event.Type == api.EventStdout {
			b.Write(m.Event.Data)
		}
	}
	return b.String()
}

func TestExecInteractive(t *testing.T) {
	ts := newServer(t)
	key := ts.newKey(t, api.CreateKeyRequest{})
	c := mustDialWS(t, ts, "/v2/exec", key)
	if c.conn.Subprotocol() != api.WSSubprotocol {
		t.Fatalf("subprotocol = %q", c.conn.Subprotocol())
	}
	c.send(api.ClientMessage{Type: api.MsgStart, Spec: &api.SessionSpec{Argv: []string{"cat"}}})
	first := c.mustRecv()
	if first.Type != api.MsgSession || first.Session == nil || first.Session.ID == "" {
		t.Fatalf("first message = %+v", first)
	}
	if m := c.mustRecv(); m.Event.Type != api.EventStarted {
		t.Fatalf("second message = %+v", m)
	}
	c.send(api.ClientMessage{Type: api.MsgStdin, Data: "hello\n"})
	if m := c.mustRecv(); string(m.Event.Data) != "hello\n" {
		t.Fatalf("echo = %+v", m)
	}
	// Bad messages are answered without closing the connection.
	c.send(api.ClientMessage{Type: "bogus"})
	if m := c.mustRecv(); m.Type != api.MsgProtocolError || m.Status != 400 {
		t.Fatalf("bogus message reply = %+v", m)
	}
	c.conn.Write(c.ctx, websocket.MessageText, []byte(`{"type":"stdin","extra":1}`))
	if m := c.mustRecv(); m.Type != api.MsgProtocolError {
		t.Fatalf("unknown field reply = %+v", m)
	}
	c.send(api.ClientMessage{Type: api.MsgStdin, DataB64: []byte("bin\n")})
	c.send(api.ClientMessage{Type: api.MsgStdinClose})
	rest, status := c.drain()
	if status != websocket.StatusNormalClosure {
		t.Fatalf("close status %v", status)
	}
	if stdoutOf(rest) != "bin\n" {
		t.Fatalf("stdout after close = %q", stdoutOf(rest))
	}
	last := rest[len(rest)-1].Event
	if last.Type != api.EventExit || *last.ExitCode != 0 {
		t.Fatalf("last event = %+v", last)
	}
}

func TestExecSignal(t *testing.T) {
	ts := newServer(t)
	key := ts.newKey(t, api.CreateKeyRequest{})
	c := mustDialWS(t, ts, "/v2/exec", key)
	c.send(api.ClientMessage{Type: api.MsgStart, Spec: &api.SessionSpec{Argv: []string{"sleep", "30"}}})
	c.mustRecv() // session
	c.mustRecv() // started
	c.send(api.ClientMessage{Type: api.MsgSignal, Signal: "INT"})
	msgs, _ := c.drain()
	last := msgs[len(msgs)-1].Event
	if last.Type != api.EventExit || last.Signal != "SIGINT" {
		t.Fatalf("last event = %+v", last)
	}
}

func TestExecRejectsBadStarts(t *testing.T) {
	ts := newServer(t)
	no := false
	key := ts.newKey(t, api.CreateKeyRequest{Policy: api.Policy{AllowShell: &no}})
	for _, tc := range []struct {
		msg    api.ClientMessage
		status int
	}{
		{api.ClientMessage{Type: api.MsgStdin, Data: "x"}, 400},
		{api.ClientMessage{Type: api.MsgStart, Spec: &api.SessionSpec{}}, 400},
		{api.ClientMessage{Type: api.MsgStart, Spec: &api.SessionSpec{Argv: []string{"true"}}, OnDisconnect: "soon"}, 400},
		{api.ClientMessage{Type: api.MsgStart, Spec: &api.SessionSpec{Shell: "true"}}, 403},
	} {
		c := mustDialWS(t, ts, "/v2/exec", key)
		c.send(tc.msg)
		m := c.mustRecv()
		if m.Type != api.MsgProtocolError || m.Status != tc.status {
			t.Errorf("%+v: reply %+v, want status %d", tc.msg, m, tc.status)
		}
		if _, status := c.drain(); status != websocket.StatusPolicyViolation {
			t.Errorf("%+v: close status %v", tc.msg, status)
		}
	}
}

func TestWebSocketAuthentication(t *testing.T) {
	ts := newServer(t)
	key := ts.newKey(t, api.CreateKeyRequest{})

	_, resp, err := dialWS(t, ts, "/v2/exec", "", nil)
	if err == nil || resp == nil || resp.StatusCode != 401 {
		t.Fatalf("no key: %v, %v", err, resp)
	}
	if _, resp, _ = dialWS(t, ts, "/v2/exec", ts.master, nil); resp == nil || resp.StatusCode != 403 {
		t.Fatalf("master key: %v", resp)
	}

	// Browsers pass the key as a subprotocol, which is never echoed back.
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	conn, _, err := websocket.Dial(ctx, "ws"+strings.TrimPrefix(ts.URL, "http")+"/v2/exec", &websocket.DialOptions{
		Subprotocols: []string{api.WSSubprotocol, api.WSAuthSubprotocolPrefix + key},
	})
	if err != nil {
		t.Fatalf("subprotocol auth: %v", err)
	}
	if conn.Subprotocol() != api.WSSubprotocol {
		t.Fatalf("selected subprotocol %q", conn.Subprotocol())
	}
	conn.CloseNow()
}

func TestWebSocketOrigins(t *testing.T) {
	ts := newServer(t)
	key := ts.newKey(t, api.CreateKeyRequest{})
	_, resp, err := dialWS(t, ts, "/v2/exec", key, http.Header{"Origin": {"http://evil.example"}})
	if err == nil || resp == nil || resp.StatusCode != 403 {
		t.Fatalf("cross-origin: %v, %v", err, resp)
	}
	host := strings.TrimPrefix(ts.URL, "http://")
	if _, _, err := dialWS(t, ts, "/v2/exec", key, http.Header{"Origin": {"http://" + host}}); err != nil {
		t.Fatalf("same origin: %v", err)
	}
}

func TestAttach(t *testing.T) {
	ts := newServer(t)
	owner := ts.newKey(t, api.CreateKeyRequest{})
	other := ts.newKey(t, api.CreateKeyRequest{})
	reader := ts.newKey(t, api.CreateKeyRequest{Scopes: []string{api.ScopeSessionsRead}})
	var s api.Session
	expect(t, ts.do(t, owner, "POST", "/v2/sessions", api.SessionSpec{Argv: []string{"cat"}}, &s), 201)
	path := "/v2/sessions/" + s.ID + "/attach"

	if _, resp, _ := dialWS(t, ts, path, other, nil); resp == nil || resp.StatusCode != 404 {
		t.Fatalf("other key: %v", resp)
	}
	if _, resp, _ := dialWS(t, ts, path, reader, nil); resp == nil || resp.StatusCode != 403 {
		t.Fatalf("read-only key, interactive attach: %v", resp)
	}

	watcher := mustDialWS(t, ts, path+"?readonly=true", owner)
	if m := watcher.mustRecv(); m.Type != api.MsgSession {
		t.Fatalf("watcher first message %+v", m)
	}
	watcher.mustRecv() // started
	watcher.send(api.ClientMessage{Type: api.MsgStdin, Data: "x"})
	if m := watcher.mustRecv(); m.Type != api.MsgProtocolError || m.Status != 403 {
		t.Fatalf("read-only stdin reply %+v", m)
	}

	c := mustDialWS(t, ts, path, owner)
	c.mustRecv() // session
	c.mustRecv() // started
	c.send(api.ClientMessage{Type: api.MsgStdin, Data: "from attach\n"})
	c.send(api.ClientMessage{Type: api.MsgStdinClose})
	got, _ := c.drain()
	seen, _ := watcher.drain()
	if stdoutOf(got) != "from attach\n" || stdoutOf(seen) != "from attach\n" {
		t.Fatalf("attached %q, watcher %q", stdoutOf(got), stdoutOf(seen))
	}

	// Attaching to a finished session replays from `from` and closes.
	replay := mustDialWS(t, ts, path+"?from=2&readonly=true", owner)
	msgs, status := replay.drain()
	if status != websocket.StatusNormalClosure || msgs[0].Type != api.MsgSession || msgs[1].Event.Seq != 2 {
		t.Fatalf("replay = %+v, %v", msgs, status)
	}
}

func waitState(t *testing.T, ts *testServer, key, id string, want api.SessionState) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	var s api.Session
	for time.Now().Before(deadline) {
		ts.do(t, key, "GET", "/v2/sessions/"+id, nil, &s)
		if s.State == want {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("session state %s, want %s", s.State, want)
}

func TestOnDisconnect(t *testing.T) {
	ts := newServer(t)
	key := ts.newKey(t, api.CreateKeyRequest{})
	start := func(onDisconnect string) (*wsClient, string) {
		c := mustDialWS(t, ts, "/v2/exec", key)
		c.send(api.ClientMessage{Type: api.MsgStart, Spec: &api.SessionSpec{Argv: []string{"sleep", "30"}}, OnDisconnect: onDisconnect})
		return c, c.mustRecv().Session.ID
	}

	// keep (the default): the session outlives the connection.
	c, kept := start("")
	c.conn.Close(websocket.StatusNormalClosure, "bye")
	time.Sleep(100 * time.Millisecond)
	waitState(t, ts, key, kept, api.StateRunning)

	// kill: the session dies with the connection.
	c, killed := start("kill")
	c.conn.Close(websocket.StatusNormalClosure, "bye")
	waitState(t, ts, key, killed, api.StateKilled)

	// A grace period spares a session that a client re-attaches to.
	c, spared := start("300ms")
	c.conn.Close(websocket.StatusNormalClosure, "bye")
	again := mustDialWS(t, ts, "/v2/sessions/"+spared+"/attach", key)
	again.mustRecv()
	time.Sleep(600 * time.Millisecond)
	waitState(t, ts, key, spared, api.StateRunning)

	// ...and kills one nobody returns to.
	c, abandoned := start("100ms")
	c.conn.Close(websocket.StatusNormalClosure, "bye")
	waitState(t, ts, key, abandoned, api.StateKilled)
}

func TestWebSocketEndpointsAreDocumented(t *testing.T) {
	doc, err := OpenAPIYAML()
	if err != nil {
		t.Fatal(err)
	}
	for _, p := range []string{"/v2/exec:", "/v2/sessions/{id}/attach:"} {
		if !strings.Contains(string(doc), p) {
			t.Errorf("%s missing from the OpenAPI document", p)
		}
	}
}

func TestTTYOverWebSocketAndHTTP(t *testing.T) {
	ts := newServer(t)
	key := ts.newKey(t, api.CreateKeyRequest{})
	c := mustDialWS(t, ts, "/v2/exec", key)
	c.send(api.ClientMessage{Type: api.MsgStart, Spec: &api.SessionSpec{
		Shell: `read x; stty size; read y; stty size`,
		TTY:   &api.TTYSize{Cols: 90, Rows: 20},
	}})
	sess := c.mustRecv().Session
	c.send(api.ClientMessage{Type: api.MsgResize, Cols: 100, Rows: 30})
	time.Sleep(50 * time.Millisecond)
	c.send(api.ClientMessage{Type: api.MsgStdin, Data: "a\r"})
	// Resizing over HTTP works too.
	time.Sleep(200 * time.Millisecond)
	expect(t, ts.do(t, key, "POST", "/v2/sessions/"+sess.ID+"/resize", api.ResizeRequest{Cols: 110, Rows: 35}, nil), 200)
	c.send(api.ClientMessage{Type: api.MsgStdin, Data: "b\r"})
	msgs, _ := c.drain()
	out := stdoutOf(msgs)
	if !strings.Contains(out, "30 100") || !strings.Contains(out, "35 110") {
		t.Fatalf("tty output %q", out)
	}

	// Resizing a session without a TTY is a conflict.
	var plain api.Session
	expect(t, ts.do(t, key, "POST", "/v2/sessions", api.SessionSpec{Argv: []string{"sleep", "30"}}, &plain), 201)
	expect(t, ts.do(t, key, "POST", "/v2/sessions/"+plain.ID+"/resize", api.ResizeRequest{Cols: 80, Rows: 24}, nil), 409)
}
