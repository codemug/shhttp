// Package testserver starts a complete shhttp server for tests.
package testserver

import (
	"context"
	"io"
	"log/slog"
	"net/http/httptest"
	"path/filepath"
	"testing"
	"time"

	"github.com/codemug/shhttp/internal/auth"
	"github.com/codemug/shhttp/internal/job"
	"github.com/codemug/shhttp/internal/server"
	"github.com/codemug/shhttp/internal/session"
	"github.com/codemug/shhttp/internal/store"
	"github.com/codemug/shhttp/internal/template"
)

// Server is a running test server.
type Server struct {
	URL       string
	MasterKey string
}

// Start runs a server with fresh storage until the test ends.
func Start(t testing.TB) *Server {
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
	deps := server.Deps{Auth: a, Sessions: m, Jobs: jobs, Templates: template.NewService(st)}
	ts := httptest.NewServer(server.New(deps, logger, server.Options{Version: "test"}).Handler())
	t.Cleanup(func() {
		jobs.Stop()
		m.Shutdown(context.Background())
		jobs.Wait(context.Background())
		ts.Close()
		st.Close()
	})
	return &Server{URL: ts.URL, MasterKey: master}
}
