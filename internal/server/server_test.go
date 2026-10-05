package server

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/codemug/shhttp/internal/auth"
	"github.com/codemug/shhttp/internal/job"
	"github.com/codemug/shhttp/internal/session"
	"github.com/codemug/shhttp/internal/store"
	"github.com/codemug/shhttp/internal/template"
	"github.com/codemug/shhttp/pkg/api"
)

type testServer struct {
	*httptest.Server
	master string
}

func newServer(t *testing.T) *testServer {
	t.Helper()
	dir := t.TempDir()
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	st, err := store.Open(context.Background(), filepath.Join(dir, "db"))
	if err != nil {
		t.Fatal(err)
	}
	master := auth.GenerateMasterKey()
	a, err := auth.New(st, master)
	if err != nil {
		t.Fatal(err)
	}
	m, err := session.NewManager(context.Background(), session.Config{DataDir: dir, KillGrace: 300 * time.Millisecond, Logger: logger}, st)
	if err != nil {
		t.Fatal(err)
	}
	jobs, err := job.New(job.Config{DataDir: dir, Logger: logger}, st, m, a.Policy)
	if err != nil {
		t.Fatal(err)
	}
	if err := jobs.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	deps := Deps{Auth: a, Sessions: m, Jobs: jobs, Templates: template.NewService(st)}
	ts := httptest.NewServer(New(deps, logger, Options{Version: "test"}).Handler())
	t.Cleanup(func() {
		jobs.Stop()
		m.Shutdown(context.Background())
		jobs.Wait(context.Background())
		ts.Close()
		st.Close()
	})
	return &testServer{Server: ts, master: master}
}

// do sends a request and decodes a JSON response into out (if non-nil). It
// returns the status code.
func (ts *testServer) do(t *testing.T, token, method, path string, body any, out any) int {
	t.Helper()
	var rd io.Reader
	switch b := body.(type) {
	case nil:
	case string:
		rd = strings.NewReader(b)
	default:
		j, _ := json.Marshal(b)
		rd = bytes.NewReader(j)
	}
	req, _ := http.NewRequest(method, ts.URL+path, rd)
	if body != nil {
		// The stdin endpoint ignores it; JSON endpoints require it.
		req.Header.Set("Content-Type", "application/json")
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := ts.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	data, _ := io.ReadAll(resp.Body)
	if out != nil && len(data) > 0 {
		if err := json.Unmarshal(data, out); err != nil {
			t.Fatalf("%s %s: decoding %q: %v", method, path, data, err)
		}
	}
	if resp.StatusCode >= 400 && !strings.HasPrefix(resp.Header.Get("Content-Type"), "application/problem+json") {
		t.Fatalf("%s %s: error %d without problem+json: %q", method, path, resp.StatusCode, data)
	}
	return resp.StatusCode
}

func (ts *testServer) newKey(t *testing.T, req api.CreateKeyRequest) string {
	t.Helper()
	if req.Name == "" {
		req.Name = "test"
	}
	if req.Scopes == nil {
		req.Scopes = []string{api.ScopeSessionsRun, api.ScopeSessionsRead}
	}
	var k api.KeyWithSecret
	if code := ts.do(t, ts.master, "POST", "/v2/keys", req, &k); code != http.StatusCreated {
		t.Fatalf("create key: %d", code)
	}
	return k.Secret
}

func expect(t *testing.T, got, want int) {
	t.Helper()
	if got != want {
		t.Fatalf("status %d, want %d", got, want)
	}
}

func TestAuthentication(t *testing.T) {
	ts := newServer(t)
	key := ts.newKey(t, api.CreateKeyRequest{})

	expect(t, ts.do(t, "", "GET", "/v2/sessions", nil, nil), 401)
	expect(t, ts.do(t, "shh_bogus_x", "GET", "/v2/sessions", nil, nil), 401)
	// The master key manages keys only.
	expect(t, ts.do(t, ts.master, "GET", "/v2/sessions", nil, nil), 403)
	expect(t, ts.do(t, ts.master, "POST", "/v2/sessions", api.SessionSpec{Argv: []string{"true"}}, nil), 403)
	// API keys cannot manage keys.
	expect(t, ts.do(t, key, "GET", "/v2/keys", nil, nil), 403)

	var who api.Whoami
	expect(t, ts.do(t, ts.master, "GET", "/v2/whoami", nil, &who), 200)
	if !who.Master {
		t.Fatal("whoami: master not reported")
	}
	expect(t, ts.do(t, key, "GET", "/v2/whoami", nil, &who), 200)
	if who.Master || who.Key == nil || len(who.Key.Scopes) != 2 {
		t.Fatalf("whoami = %+v", who)
	}

	// Scopes are enforced.
	readOnly := ts.newKey(t, api.CreateKeyRequest{Scopes: []string{api.ScopeSessionsRead}})
	expect(t, ts.do(t, readOnly, "POST", "/v2/sessions", api.SessionSpec{Argv: []string{"true"}}, nil), 403)
	expect(t, ts.do(t, readOnly, "GET", "/v2/sessions", nil, nil), 200)

	// Unauthenticated endpoints.
	expect(t, ts.do(t, "", "GET", "/healthz", nil, nil), 200)
	expect(t, ts.do(t, "", "GET", "/v2/version", nil, nil), 200)
}

func TestJSONContentTypeIsRequired(t *testing.T) {
	ts := newServer(t)
	req, _ := http.NewRequest("POST", ts.URL+"/v2/keys", strings.NewReader(`{"name":"x","scopes":["sessions:run"]}`))
	req.Header.Set("Authorization", "Bearer "+ts.master)
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	resp, err := ts.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusUnsupportedMediaType {
		t.Fatalf("status %d, want 415", resp.StatusCode)
	}
}

func TestRoutingErrorsUseProblemJSON(t *testing.T) {
	ts := newServer(t)
	expect(t, ts.do(t, "", "GET", "/nope", nil, nil), 404)
	expect(t, ts.do(t, "", "PUT", "/v2/sessions", nil, nil), 405)
}

func TestKeyLifecycle(t *testing.T) {
	ts := newServer(t)
	var k api.KeyWithSecret
	expect(t, ts.do(t, ts.master, "POST", "/v2/keys", api.CreateKeyRequest{Name: "ci", Scopes: []string{api.ScopeSessionsRun}}, &k), 201)
	// Schema violations are 422 with the failing field; rule violations are 400.
	var prob api.Problem
	expect(t, ts.do(t, ts.master, "POST", "/v2/keys", `{"name":"x","scopes":["sessions:run"],"bogus":1}`, &prob), 422)
	if len(prob.Errors) != 1 || prob.Errors[0].Location != "body.bogus" {
		t.Fatalf("problem = %+v", prob)
	}
	expect(t, ts.do(t, ts.master, "POST", "/v2/keys", api.CreateKeyRequest{Name: "x", Scopes: []string{"root"}}, nil), 422)
	expect(t, ts.do(t, ts.master, "POST", "/v2/keys", api.CreateKeyRequest{Name: "x", Scopes: []string{"sessions:run"}, Policy: api.Policy{Commands: []string{"("}}}, nil), 400)

	var list api.KeyList
	expect(t, ts.do(t, ts.master, "GET", "/v2/keys", nil, &list), 200)
	if len(list.Keys) != 1 || list.Keys[0].ID != k.ID {
		t.Fatalf("list = %+v", list)
	}
	if strings.Contains(fmt.Sprint(list), k.Secret) {
		t.Fatal("secret leaked in list")
	}

	name := "renamed"
	var got api.Key
	expect(t, ts.do(t, ts.master, "PATCH", "/v2/keys/"+k.ID, api.UpdateKeyRequest{Name: &name, Scopes: &[]string{api.ScopeSessionsRead}}, &got), 200)
	if got.Name != "renamed" || got.Scopes[0] != api.ScopeSessionsRead {
		t.Fatalf("patched = %+v", got)
	}

	var rotated api.KeyWithSecret
	expect(t, ts.do(t, ts.master, "POST", "/v2/keys/"+k.ID+"/rotate", nil, &rotated), 200)
	expect(t, ts.do(t, k.Secret, "GET", "/v2/whoami", nil, nil), 401)
	expect(t, ts.do(t, rotated.Secret, "GET", "/v2/whoami", nil, nil), 200)

	var rev api.RevokeKeyResponse
	expect(t, ts.do(t, ts.master, "DELETE", "/v2/keys/"+k.ID, nil, &rev), 200)
	if rev.Key.RevokedAt == nil {
		t.Fatal("not revoked")
	}
	expect(t, ts.do(t, rotated.Secret, "GET", "/v2/whoami", nil, nil), 401)
	expect(t, ts.do(t, ts.master, "GET", "/v2/keys/key_00000000000000000000000000", nil, nil), 404)
	expect(t, ts.do(t, ts.master, "GET", "/v2/keys/key_..%2Fetc", nil, nil), 404)
}

func TestRevokeKeepsOrKillsSessions(t *testing.T) {
	ts := newServer(t)
	var k1, k2 api.KeyWithSecret
	ts.do(t, ts.master, "POST", "/v2/keys", api.CreateKeyRequest{Name: "a", Scopes: []string{api.ScopeSessionsRun}}, &k1)
	ts.do(t, ts.master, "POST", "/v2/keys", api.CreateKeyRequest{Name: "b", Scopes: []string{api.ScopeSessionsRun}}, &k2)
	admin := ts.newKey(t, api.CreateKeyRequest{Scopes: []string{api.ScopeSessionsRead, api.ScopeAdminRead}})
	var s1, s2 api.Session
	ts.do(t, k1.Secret, "POST", "/v2/sessions", api.SessionSpec{Argv: []string{"sleep", "30"}}, &s1)
	ts.do(t, k2.Secret, "POST", "/v2/sessions", api.SessionSpec{Argv: []string{"sleep", "30"}}, &s2)

	var rev api.RevokeKeyResponse
	expect(t, ts.do(t, ts.master, "DELETE", "/v2/keys/"+k1.ID, nil, &rev), 200)
	expect(t, ts.do(t, ts.master, "DELETE", "/v2/keys/"+k2.ID+"?kill_sessions=true", nil, &rev), 200)
	if rev.KilledSessions != 1 {
		t.Fatalf("killed %d sessions", rev.KilledSessions)
	}
	var got api.Session
	ts.do(t, admin, "GET", "/v2/sessions/"+s1.ID, nil, &got)
	if got.State != api.StateRunning {
		t.Fatalf("session of revoked key: %s, want running", got.State)
	}
	deadline := time.Now().Add(5 * time.Second)
	for ts.do(t, admin, "GET", "/v2/sessions/"+s2.ID, nil, &got); got.State == api.StateRunning && time.Now().Before(deadline); {
		time.Sleep(20 * time.Millisecond)
		ts.do(t, admin, "GET", "/v2/sessions/"+s2.ID, nil, &got)
	}
	if got.State != api.StateKilled {
		t.Fatalf("killed session: %s", got.State)
	}
}

func TestRunAndWait(t *testing.T) {
	ts := newServer(t)
	key := ts.newKey(t, api.CreateKeyRequest{})
	var res api.RunResult
	expect(t, ts.do(t, key, "POST", "/v2/sessions?wait=true", api.SessionSpec{Shell: "echo hi; echo err >&2; exit 4"}, &res), 200)
	if res.Session.State != api.StateExited || *res.Session.ExitCode != 4 || *res.Stdout != "hi\n" || *res.Stderr != "err\n" {
		t.Fatalf("result = %+v", res)
	}

	// Input is closed after the initial stdin, so cat ends.
	expect(t, ts.do(t, key, "POST", "/v2/sessions?wait=true", api.SessionSpec{Argv: []string{"cat"}, Stdin: "piped"}, &res), 200)
	if *res.Stdout != "piped" {
		t.Fatalf("stdout = %q", *res.Stdout)
	}

	// Output caps keep the tail by default, or the head on request.
	spec := api.SessionSpec{Shell: "seq 1 1000"}
	expect(t, ts.do(t, key, "POST", "/v2/sessions?wait=true&max_output=10", spec, &res), 200)
	if !res.StdoutTruncated || *res.Stdout != "\n999\n1000\n" {
		t.Fatalf("tail = %q truncated=%v", *res.Stdout, res.StdoutTruncated)
	}
	expect(t, ts.do(t, key, "POST", "/v2/sessions?wait=true&max_output=6&keep=head", spec, &res), 200)
	if *res.Stdout != "1\n2\n3\n" {
		t.Fatalf("head = %q", *res.Stdout)
	}

	// wait_timeout returns early with the session still running.
	expect(t, ts.do(t, key, "POST", "/v2/sessions?wait=true&wait_timeout=100ms", api.SessionSpec{Shell: "echo early; sleep 30"}, &res), 200)
	if res.Session.State != api.StateRunning || *res.Stdout != "early\n" {
		t.Fatalf("result = %+v", res)
	}

	// Binary output is base64 encoded.
	res = api.RunResult{}
	expect(t, ts.do(t, key, "POST", "/v2/sessions?wait=true", api.SessionSpec{Shell: `printf '\377\376'`}, &res), 200)
	if res.Stdout != nil || !bytes.Equal(res.StdoutB64, []byte{0xff, 0xfe}) {
		t.Fatalf("binary result = %+v", res.Output)
	}
}

func TestBadRequests(t *testing.T) {
	ts := newServer(t)
	key := ts.newKey(t, api.CreateKeyRequest{})
	// Missing or malformed JSON, and rules the schema cannot express: 400.
	expect(t, ts.do(t, key, "POST", "/v2/sessions", nil, nil), 400)
	expect(t, ts.do(t, key, "POST", "/v2/sessions", `{"argv":["true"]}{}`, nil), 400)
	expect(t, ts.do(t, key, "POST", "/v2/sessions", api.SessionSpec{}, nil), 400)
	expect(t, ts.do(t, key, "POST", "/v2/sessions?wait=true&wait_timeout=soon", api.SessionSpec{Argv: []string{"true"}}, nil), 400)
	// Schema violations: 422.
	expect(t, ts.do(t, key, "POST", "/v2/sessions", `{"argv":["true"],"unknown":1}`, nil), 422)
	expect(t, ts.do(t, key, "POST", "/v2/sessions", `{"argv":["true"],"timeout":"soon"}`, nil), 422)
	expect(t, ts.do(t, key, "POST", "/v2/sessions", `{"argv":[]}`, nil), 422)
	expect(t, ts.do(t, key, "POST", "/v2/sessions?wait=maybe", api.SessionSpec{Argv: []string{"true"}}, nil), 422)
	expect(t, ts.do(t, key, "POST", "/v2/sessions?wait=true&keep=middle", api.SessionSpec{Argv: []string{"true"}}, nil), 422)
	expect(t, ts.do(t, key, "GET", "/v2/sessions?state=nope", nil, nil), 422)
	expect(t, ts.do(t, key, "GET", "/v2/sessions?limit=0", nil, nil), 422)
	expect(t, ts.do(t, key, "GET", "/v2/sessions/ses_bogus", nil, nil), 404)
}

func TestPolicyDenial(t *testing.T) {
	ts := newServer(t)
	no := false
	key := ts.newKey(t, api.CreateKeyRequest{Policy: api.Policy{AllowShell: &no}})
	expect(t, ts.do(t, key, "POST", "/v2/sessions", api.SessionSpec{Shell: "true"}, nil), 403)
	expect(t, ts.do(t, key, "POST", "/v2/sessions", api.SessionSpec{Argv: []string{"true"}}, nil), 201)

	limited := ts.newKey(t, api.CreateKeyRequest{Policy: api.Policy{MaxConcurrentSessions: 1}})
	expect(t, ts.do(t, limited, "POST", "/v2/sessions", api.SessionSpec{Argv: []string{"sleep", "30"}}, nil), 201)
	expect(t, ts.do(t, limited, "POST", "/v2/sessions", api.SessionSpec{Argv: []string{"sleep", "30"}}, nil), 429)
}

func TestSessionsAreIsolatedBetweenKeys(t *testing.T) {
	ts := newServer(t)
	owner := ts.newKey(t, api.CreateKeyRequest{})
	other := ts.newKey(t, api.CreateKeyRequest{})
	admin := ts.newKey(t, api.CreateKeyRequest{Scopes: []string{api.ScopeSessionsRun, api.ScopeSessionsRead, api.ScopeAdminRead}})
	var s api.Session
	expect(t, ts.do(t, owner, "POST", "/v2/sessions", api.SessionSpec{Argv: []string{"sleep", "30"}}, &s), 201)

	expect(t, ts.do(t, other, "GET", "/v2/sessions/"+s.ID, nil, nil), 404)
	expect(t, ts.do(t, other, "GET", "/v2/sessions/"+s.ID+"/events", nil, nil), 404)
	expect(t, ts.do(t, other, "POST", "/v2/sessions/"+s.ID+"/kill", nil, nil), 404)
	var list api.SessionList
	expect(t, ts.do(t, other, "GET", "/v2/sessions", nil, &list), 200)
	if len(list.Sessions) != 0 {
		t.Fatalf("other key sees %d sessions", len(list.Sessions))
	}
	expect(t, ts.do(t, other, "GET", "/v2/sessions?key_id=key_x", nil, nil), 403)

	// admin:read can read but not change other keys' sessions.
	expect(t, ts.do(t, admin, "GET", "/v2/sessions/"+s.ID, nil, nil), 200)
	expect(t, ts.do(t, admin, "GET", "/v2/sessions", nil, &list), 200)
	if len(list.Sessions) != 1 {
		t.Fatalf("admin sees %d sessions", len(list.Sessions))
	}
	expect(t, ts.do(t, admin, "POST", "/v2/sessions/"+s.ID+"/stdin", "x", nil), 404)
	expect(t, ts.do(t, admin, "DELETE", "/v2/sessions/"+s.ID, nil, nil), 404)

	expect(t, ts.do(t, owner, "POST", "/v2/sessions/"+s.ID+"/kill", nil, nil), 202)
}

// streamLines reads NDJSON events from the session until the exit event.
func streamEvents(t *testing.T, ts *testServer, key, path string, header http.Header) []api.Event {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, "GET", ts.URL+path, nil)
	req.Header.Set("Authorization", "Bearer "+key)
	for k, v := range header {
		req.Header[k] = v
	}
	resp, err := ts.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var out []api.Event
	sc := bufio.NewScanner(resp.Body)
	for sc.Scan() {
		line := sc.Text()
		if strings.HasPrefix(line, "data: ") {
			line = strings.TrimPrefix(line, "data: ")
		} else if !strings.HasPrefix(line, "{") {
			continue
		}
		var e api.Event
		if err := json.Unmarshal([]byte(line), &e); err != nil {
			t.Fatalf("decoding %q: %v", line, err)
		}
		out = append(out, e)
	}
	return out
}

func TestInteractiveSessionOverHTTP(t *testing.T) {
	ts := newServer(t)
	key := ts.newKey(t, api.CreateKeyRequest{})
	var s api.Session
	expect(t, ts.do(t, key, "POST", "/v2/sessions", api.SessionSpec{Argv: []string{"sh", "-c", `while read l; do echo "got $l"; done; echo bye`}}, &s), 201)

	done := make(chan []api.Event)
	go func() { done <- streamEvents(t, ts, key, "/v2/sessions/"+s.ID+"/events?follow=true", nil) }()

	var in api.StdinResponse
	expect(t, ts.do(t, key, "POST", "/v2/sessions/"+s.ID+"/stdin", "one\n", &in), 200)
	if in.Bytes != 4 || !in.StdinOpen {
		t.Fatalf("stdin response = %+v", in)
	}
	expect(t, ts.do(t, key, "POST", "/v2/sessions/"+s.ID+"/stdin?close=true", "two\n", &in), 200)
	if in.StdinOpen {
		t.Fatal("stdin still open after close=true")
	}

	evs := <-done
	var out strings.Builder
	for _, e := range evs {
		out.Write(e.Data)
	}
	if out.String() != "got one\ngot two\nbye\n" {
		t.Fatalf("output = %q", out.String())
	}
	last := evs[len(evs)-1]
	if last.Type != api.EventExit || *last.ExitCode != 0 {
		t.Fatalf("last event = %+v", last)
	}
	expect(t, ts.do(t, key, "POST", "/v2/sessions/"+s.ID+"/stdin", "late", nil), 409)

	// Replays: from a sequence number, via SSE Last-Event-ID, and raw.
	replay := streamEvents(t, ts, key, fmt.Sprintf("/v2/sessions/%s/events?from=%d", s.ID, last.Seq), nil)
	if len(replay) != 1 || replay[0].Seq != last.Seq {
		t.Fatalf("replay from seq = %+v", replay)
	}
	sse := streamEvents(t, ts, key, "/v2/sessions/"+s.ID+"/events?follow=true", http.Header{
		"Accept":        {"text/event-stream"},
		"Last-Event-Id": {fmt.Sprint(last.Seq - 1)},
	})
	if len(sse) != 1 || sse[0].Type != api.EventExit {
		t.Fatalf("SSE replay = %+v", sse)
	}
	req, _ := http.NewRequest("GET", ts.URL+"/v2/sessions/"+s.ID+"/events?format=raw", nil)
	req.Header.Set("Authorization", "Bearer "+key)
	resp, err := ts.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if string(raw) != "got one\ngot two\nbye\n" {
		t.Fatalf("raw = %q", raw)
	}
}

func TestSignalKillDelete(t *testing.T) {
	ts := newServer(t)
	key := ts.newKey(t, api.CreateKeyRequest{})
	var s api.Session
	ts.do(t, key, "POST", "/v2/sessions", api.SessionSpec{Argv: []string{"sleep", "30"}}, &s)
	expect(t, ts.do(t, key, "POST", "/v2/sessions/"+s.ID+"/signal", api.SignalRequest{Signal: "NOPE"}, nil), 400)
	expect(t, ts.do(t, key, "POST", "/v2/sessions/"+s.ID+"/signal", api.SignalRequest{Signal: "SIGINT"}, nil), 200)
	evs := streamEvents(t, ts, key, "/v2/sessions/"+s.ID+"/events?follow=true", nil)
	if last := evs[len(evs)-1]; last.Type != api.EventExit || last.Signal != "SIGINT" {
		t.Fatalf("last event = %+v", last)
	}
	expect(t, ts.do(t, key, "POST", "/v2/sessions/"+s.ID+"/kill", nil, nil), 409)
	expect(t, ts.do(t, key, "DELETE", "/v2/sessions/"+s.ID, nil, nil), 204)
	expect(t, ts.do(t, key, "GET", "/v2/sessions/"+s.ID, nil, nil), 404)
}

func TestListPagination(t *testing.T) {
	ts := newServer(t)
	key := ts.newKey(t, api.CreateKeyRequest{})
	var ids []string
	for i := 0; i < 5; i++ {
		var s api.Session
		ts.do(t, key, "POST", "/v2/sessions", api.SessionSpec{Argv: []string{"true"}, Labels: map[string]string{"n": fmt.Sprint(i % 2)}}, &s)
		ids = append(ids, s.ID)
	}
	var page api.SessionList
	expect(t, ts.do(t, key, "GET", "/v2/sessions?limit=2", nil, &page), 200)
	var seen []string
	for {
		for _, s := range page.Sessions {
			seen = append(seen, s.ID)
		}
		if page.NextCursor == "" {
			break
		}
		path := "/v2/sessions?limit=2&cursor=" + page.NextCursor
		page = api.SessionList{}
		expect(t, ts.do(t, key, "GET", path, nil, &page), 200)
	}
	if len(seen) != 5 || seen[0] != ids[4] || seen[4] != ids[0] {
		t.Fatalf("pages = %v, created = %v", seen, ids)
	}
	expect(t, ts.do(t, key, "GET", "/v2/sessions?label=n=1", nil, &page), 200)
	if len(page.Sessions) != 2 {
		t.Fatalf("label filter returned %d", len(page.Sessions))
	}
}

// Stdin must reach the process while the request body is still being sent.
func TestStdinIsStreamed(t *testing.T) {
	ts := newServer(t)
	key := ts.newKey(t, api.CreateKeyRequest{})
	var s api.Session
	expect(t, ts.do(t, key, "POST", "/v2/sessions", api.SessionSpec{Argv: []string{"cat"}}, &s), 201)

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, "GET", ts.URL+"/v2/sessions/"+s.ID+"/events?follow=true&format=raw", nil)
	req.Header.Set("Authorization", "Bearer "+key)
	events, err := ts.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer events.Body.Close()
	out := bufio.NewReader(events.Body)

	pr, pw := io.Pipe()
	stdinDone := make(chan int)
	go func() {
		req, _ := http.NewRequestWithContext(ctx, "POST", ts.URL+"/v2/sessions/"+s.ID+"/stdin?close=true", pr)
		req.Header.Set("Authorization", "Bearer "+key)
		resp, err := ts.Client().Do(req)
		if err != nil {
			stdinDone <- 0
			return
		}
		resp.Body.Close()
		stdinDone <- resp.StatusCode
	}()
	for _, line := range []string{"first\n", "second\n"} {
		pw.Write([]byte(line))
		got, err := out.ReadString('\n')
		if err != nil || got != line {
			t.Fatalf("echoed %q, %v; want %q while the request is still open", got, err, line)
		}
	}
	pw.Close()
	if code := <-stdinDone; code != 200 {
		t.Fatalf("stdin request status %d", code)
	}
}

func TestOpenAPIDocument(t *testing.T) {
	ts := newServer(t)
	resp, err := ts.Client().Get(ts.URL + "/v2/openapi.json")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var doc struct {
		OpenAPI string                    `json:"openapi"`
		Paths   map[string]map[string]any `json:"paths"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&doc); err != nil {
		t.Fatal(err)
	}
	if doc.OpenAPI != "3.1.0" {
		t.Fatalf("openapi = %q", doc.OpenAPI)
	}
	for path, method := range map[string]string{
		"/v2/sessions":             "post",
		"/v2/sessions/{id}/events": "get",
		"/v2/sessions/{id}/stdin":  "post",
		"/v2/keys/{id}/rotate":     "post",
		"/v2/whoami":               "get",
	} {
		if doc.Paths[path][method] == nil {
			t.Errorf("%s %s missing from the OpenAPI document", method, path)
		}
	}
	if r, _ := ts.Client().Get(ts.URL + "/v2/docs"); r == nil || r.StatusCode != 200 {
		t.Error("/v2/docs not served")
	}
}
