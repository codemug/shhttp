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
	"github.com/codemug/shhttp/internal/server"
	"github.com/codemug/shhttp/internal/session"
	"github.com/codemug/shhttp/internal/store"
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
	ts := httptest.NewServer(server.New(a, m, logger, server.Options{Version: "test"}).Handler())
	t.Cleanup(func() {
		m.Shutdown(context.Background())
		ts.Close()
		st.Close()
	})
	return &Server{URL: ts.URL, MasterKey: master}
}
