package store

import (
	"context"
	"errors"
	"path/filepath"
	"testing"
	"time"

	"github.com/codemug/shhttp/v2/pkg/api"
)

func TestReopenKeepsData(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "db")
	s, err := Open(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC().Truncate(time.Microsecond)
	code := 3
	sess := api.Session{ID: "ses_1", KeyID: "key_1", State: api.StateExited, ExitCode: &code,
		Spec: api.SessionSpec{Argv: []string{"x"}}, CreatedAt: now, StartedAt: &now, EndedAt: &now}
	if err := s.InsertSession(ctx, sess); err != nil {
		t.Fatal(err)
	}
	s.Close()

	s, err = Open(ctx, path) // migrations must not re-run
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	got, err := s.GetSession(ctx, "ses_1")
	if err != nil {
		t.Fatal(err)
	}
	if *got.ExitCode != 3 || !got.CreatedAt.Equal(now) || got.Spec.Argv[0] != "x" || *got.DurationMS != 0 {
		t.Fatalf("got %+v", got)
	}
	if err := s.UpdateSession(ctx, api.Session{ID: "missing"}); !errors.Is(err, ErrNotFound) {
		t.Fatalf("update missing: %v", err)
	}
}

func TestMarkUnfinishedLost(t *testing.T) {
	ctx := context.Background()
	s, err := Open(ctx, filepath.Join(t.TempDir(), "db"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	now := time.Now()
	for i, st := range []api.SessionState{api.StateRunning, api.StatePending, api.StateExited} {
		s.InsertSession(ctx, api.Session{ID: "ses_" + string(rune('a'+i)), State: st, CreatedAt: now})
	}
	ids, err := s.MarkUnfinishedLost(ctx, now, time.Hour)
	if err != nil || len(ids) != 2 {
		t.Fatalf("ids = %v, %v", ids, err)
	}
	got, _ := s.GetSession(ctx, "ses_a")
	if got.State != api.StateLost || got.ExpiresAt == nil {
		t.Fatalf("got %+v", got)
	}
	got, _ = s.GetSession(ctx, "ses_c")
	if got.State != api.StateExited {
		t.Fatalf("finished session changed: %+v", got)
	}
}
