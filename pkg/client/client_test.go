package client_test

import (
	"context"
	"errors"
	"io"
	"strings"
	"testing"
	"time"

	"github.com/codemug/shhttp/internal/testserver"
	"github.com/codemug/shhttp/pkg/api"
	"github.com/codemug/shhttp/pkg/client"
)

func setup(t *testing.T) (master, user *client.Client, ctx context.Context) {
	t.Helper()
	srv := testserver.Start(t)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	t.Cleanup(cancel)
	master = client.New(srv.URL, srv.MasterKey)
	k, err := master.CreateKey(ctx, api.CreateKeyRequest{Name: "test", Scopes: []string{api.ScopeSessionsRun, api.ScopeSessionsRead}})
	if err != nil {
		t.Fatal(err)
	}
	return master, client.New(srv.URL, k.Secret), ctx
}

func TestRunAndErrors(t *testing.T) {
	master, c, ctx := setup(t)

	res, err := c.Run(ctx, api.SessionSpec{Shell: "echo out; echo err >&2; exit 2"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if *res.Stdout != "out\n" || *res.Stderr != "err\n" || *res.Session.ExitCode != 2 {
		t.Fatalf("result = %+v", res)
	}
	res, err = c.Run(ctx, api.SessionSpec{Shell: "seq 1 100"}, &client.RunOptions{MaxOutput: 4, KeepHead: true})
	if err != nil || *res.Stdout != "1\n2\n" || !res.StdoutTruncated {
		t.Fatalf("head = %+v, %v", res, err)
	}

	// Server errors are *api.Problem values.
	_, err = c.Get(ctx, "ses_00000000000000000000000000")
	var p *api.Problem
	if !errors.As(err, &p) || p.Status != 404 || !client.IsStatus(err, 404) {
		t.Fatalf("missing session: %v", err)
	}
	if _, err := c.Create(ctx, api.SessionSpec{}); !client.IsStatus(err, 400) {
		t.Fatalf("no command: %v", err)
	}
	if _, err := c.List(ctx, &client.ListOptions{Limit: 1000}); !client.IsStatus(err, 422) || !errors.As(err, &p) || len(p.Errors) == 0 {
		t.Fatalf("limit out of range: %v", err)
	}
	if _, err := c.ListKeys(ctx); !client.IsStatus(err, 403) {
		t.Fatalf("API key listing keys: %v", err)
	}
	if _, err := master.List(ctx, nil); !client.IsStatus(err, 403) {
		t.Fatalf("master key listing sessions: %v", err)
	}

	who, err := c.Whoami(ctx)
	if err != nil || who.Master || who.Key == nil {
		t.Fatalf("whoami = %+v, %v", who, err)
	}
	if v, err := c.Version(ctx); err != nil || v != "test" {
		t.Fatalf("version = %q, %v", v, err)
	}
}

func TestStreamingOverHTTP(t *testing.T) {
	_, c, ctx := setup(t)
	s, err := c.Create(ctx, api.SessionSpec{Argv: []string{"cat"}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := c.WriteStdin(ctx, s.ID, strings.NewReader("one\n"), false); err != nil {
		t.Fatal(err)
	}
	if err := c.CloseStdin(ctx, s.ID); err != nil {
		t.Fatal(err)
	}
	var out strings.Builder
	var last api.Event
	for e, err := range c.Events(ctx, s.ID, &client.EventsOptions{Follow: true}) {
		if err != nil {
			t.Fatal(err)
		}
		out.Write(e.Data)
		last = e
	}
	if out.String() != "one\n" || last.Type != api.EventExit {
		t.Fatalf("output %q, last event %+v", out.String(), last)
	}

	// Breaking out of the loop early is fine.
	n := 0
	for range c.Events(ctx, s.ID, nil) {
		n++
		break
	}
	if n != 1 {
		t.Fatalf("read %d events before break", n)
	}

	if _, err := c.WriteStdin(ctx, s.ID, strings.NewReader("late"), false); !client.IsStatus(err, 409) {
		t.Fatalf("stdin after exit: %v", err)
	}
	if err := c.Delete(ctx, s.ID); err != nil {
		t.Fatal(err)
	}
	for _, err := range c.Events(ctx, s.ID, nil) {
		if !client.IsStatus(err, 404) {
			t.Fatalf("events of a deleted session: %v", err)
		}
	}
}

func TestExecAndAttach(t *testing.T) {
	_, c, ctx := setup(t)
	conn, err := c.Exec(ctx, api.SessionSpec{Argv: []string{"cat"}}, &client.ExecOptions{OnDisconnect: "kill"})
	if err != nil {
		t.Fatal(err)
	}
	id := conn.Session().ID
	if id == "" {
		t.Fatal("no session id")
	}
	if e, err := conn.Recv(ctx); err != nil || e.Type != api.EventStarted {
		t.Fatalf("first event %+v, %v", e, err)
	}
	conn.Stdin(ctx, []byte("text\n"))
	if e, _ := conn.Recv(ctx); string(e.Data) != "text\n" {
		t.Fatalf("echo %+v", e)
	}

	// A second client attaches read-only and sees the rest.
	watch, err := c.Attach(ctx, id, &client.AttachOptions{ReadOnly: true, From: 2})
	if err != nil {
		t.Fatal(err)
	}
	if err := watch.Stdin(ctx, []byte("x")); err != nil {
		t.Fatal(err)
	}
	// The replayed event and the rejection can arrive in either order.
	var replayed, rejected bool
	for i := 0; i < 2; i++ {
		e, err := watch.Recv(ctx)
		var pe *client.ProtocolError
		switch {
		case errors.As(err, &pe) && pe.Status == 403:
			rejected = true
		case err == nil && e.Seq == 2 && string(e.Data) == "text\n":
			replayed = true
		default:
			t.Fatalf("watcher message %d: %+v, %v", i, e, err)
		}
	}
	if !replayed || !rejected {
		t.Fatalf("replayed=%v rejected=%v", replayed, rejected)
	}

	conn.Stdin(ctx, []byte{0xff, '\n'})
	conn.CloseStdin(ctx)
	var data []byte
	for {
		e, err := conn.Recv(ctx)
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		data = append(data, e.Data...)
	}
	if string(data) != "\xff\n" {
		t.Fatalf("binary echo %q", data)
	}
	for {
		if _, err := watch.Recv(ctx); err != nil {
			if err != io.EOF {
				t.Fatalf("watcher ended with %v", err)
			}
			break
		}
	}
	conn.Close()
	watch.Close()
}

func TestExecErrors(t *testing.T) {
	master, c, ctx := setup(t)
	_, err := c.Exec(ctx, api.SessionSpec{}, nil)
	var pe *client.ProtocolError
	if !errors.As(err, &pe) || pe.Status != 400 {
		t.Fatalf("empty spec: %v", err)
	}
	if _, err := master.Exec(ctx, api.SessionSpec{Argv: []string{"true"}}, nil); !client.IsStatus(err, 403) {
		t.Fatalf("master key exec: %v", err)
	}
	if _, err := c.Attach(ctx, "ses_00000000000000000000000000", nil); !client.IsStatus(err, 404) {
		t.Fatalf("attach to missing session: %v", err)
	}
}

func TestKeys(t *testing.T) {
	master, _, ctx := setup(t)
	k, err := master.CreateKey(ctx, api.CreateKeyRequest{Name: "k", Scopes: []string{api.ScopeSessionsRead}})
	if err != nil {
		t.Fatal(err)
	}
	name := "renamed"
	if got, err := master.UpdateKey(ctx, k.ID, api.UpdateKeyRequest{Name: &name}); err != nil || got.Name != name {
		t.Fatalf("update: %+v, %v", got, err)
	}
	r, err := master.RotateKey(ctx, k.ID, time.Minute)
	if err != nil || r.PreviousSecretValidUntil == nil {
		t.Fatalf("rotate: %+v, %v", r, err)
	}
	old, rotated := client.New(urlOf(t, master), k.Secret), client.New(urlOf(t, master), r.Secret)
	if _, err := old.Whoami(ctx); err != nil {
		t.Fatalf("old secret within grace: %v", err)
	}
	if _, err := master.RevokeKey(ctx, k.ID, true); err != nil {
		t.Fatal(err)
	}
	if _, err := rotated.Whoami(ctx); !client.IsStatus(err, 401) {
		t.Fatalf("revoked key: %v", err)
	}
	keys, err := master.ListKeys(ctx)
	if err != nil || len(keys) != 2 {
		t.Fatalf("keys = %+v, %v", keys, err)
	}
	if got, err := master.GetKey(ctx, k.ID); err != nil || got.RevokedAt == nil {
		t.Fatalf("get revoked key: %+v, %v", got, err)
	}
}

// urlOf recovers the server URL for building more clients.
func urlOf(t *testing.T, c *client.Client) string {
	t.Helper()
	return c.BaseURL()
}
