package session

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/codemug/shhttp/internal/policy"
	"github.com/codemug/shhttp/internal/store"
	"github.com/codemug/shhttp/pkg/api"
)

type env struct {
	m     *Manager
	st    *store.Store
	dir   string
	owner Owner
}

func setup(t *testing.T) *env {
	t.Helper()
	dir := t.TempDir()
	st, err := store.Open(context.Background(), filepath.Join(dir, "db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	m, err := NewManager(context.Background(), Config{
		DataDir:   dir,
		KillGrace: 300 * time.Millisecond,
		Logger:    slog.New(slog.NewTextHandler(io.Discard, nil)),
	}, st)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { m.Shutdown(context.Background()) })
	return &env{m: m, st: st, dir: dir, owner: Owner{KeyID: "key_test"}}
}

func (e *env) create(t *testing.T, spec api.SessionSpec) api.Session {
	t.Helper()
	s, err := e.m.Create(context.Background(), e.owner, spec)
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	return s
}

func (e *env) wait(t *testing.T, id string) api.Session {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	s, err := e.m.Wait(ctx, id)
	if err != nil {
		t.Fatalf("Wait: %v", err)
	}
	return s
}

// events returns every event of a session, waiting for the log to close.
func (e *env) events(t *testing.T, id string) []api.Event {
	t.Helper()
	l, err := e.m.Log(context.Background(), id)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	var out []api.Event
	from := uint64(1)
	for {
		evs, err := l.Read(ctx, from, 100, true)
		if err == io.EOF {
			return out
		}
		if err != nil {
			t.Fatal(err)
		}
		out = append(out, evs...)
		from = evs[len(evs)-1].Seq + 1
	}
}

func output(evs []api.Event, typ api.EventType) string {
	var b strings.Builder
	for _, e := range evs {
		if e.Type == typ {
			b.Write(e.Data)
		}
	}
	return b.String()
}

func types(evs []api.Event) []api.EventType {
	var out []api.EventType
	for _, e := range evs {
		if e.Type != api.EventStdout && e.Type != api.EventStderr {
			out = append(out, e.Type)
		}
	}
	return out
}

func TestRunEcho(t *testing.T) {
	e := setup(t)
	s := e.create(t, api.SessionSpec{Argv: []string{"echo", "hello world"}})
	if s.PID == 0 || s.StartedAt == nil {
		t.Fatalf("created session = %+v", s)
	}
	s = e.wait(t, s.ID)
	if s.State != api.StateExited || s.ExitCode == nil || *s.ExitCode != 0 || s.ExpiresAt == nil {
		t.Fatalf("final session = %+v", s)
	}
	evs := e.events(t, s.ID)
	if got := output(evs, api.EventStdout); got != "hello world\n" {
		t.Fatalf("stdout = %q", got)
	}
	if ts := types(evs); len(ts) != 2 || ts[0] != api.EventStarted || ts[1] != api.EventExit {
		t.Fatalf("event types = %v", ts)
	}
}

// v1 replaced stderr with "exit status N" when a command failed.
func TestFailureKeepsStderr(t *testing.T) {
	e := setup(t)
	s := e.create(t, api.SessionSpec{Shell: "echo out; echo oops >&2; exit 3"})
	s = e.wait(t, s.ID)
	if *s.ExitCode != 3 {
		t.Fatalf("exit code = %d", *s.ExitCode)
	}
	evs := e.events(t, s.ID)
	if output(evs, api.EventStderr) != "oops\n" || output(evs, api.EventStdout) != "out\n" {
		t.Fatalf("stdout %q, stderr %q", output(evs, api.EventStdout), output(evs, api.EventStderr))
	}
}

// v1 kept only one of several environment variables.
func TestEnv(t *testing.T) {
	e := setup(t)
	inherit := false
	s := e.create(t, api.SessionSpec{
		Shell:      `echo "$A-$B-$C-$HOME"`,
		Env:        map[string]string{"A": "1", "B": "2", "C": "3"},
		InheritEnv: &inherit,
	})
	e.wait(t, s.ID)
	if got := output(e.events(t, s.ID), api.EventStdout); got != "1-2-3-\n" {
		t.Fatalf("stdout = %q", got)
	}
}

func TestInteractiveStdin(t *testing.T) {
	e := setup(t)
	s := e.create(t, api.SessionSpec{Argv: []string{"cat"}})
	if !s.StdinOpen {
		t.Fatal("stdin should be open")
	}
	if _, err := e.m.WriteStdin(s.ID, strings.NewReader("first "), false); err != nil {
		t.Fatal(err)
	}
	if _, err := e.m.WriteStdin(s.ID, strings.NewReader("second"), true); err != nil {
		t.Fatal(err)
	}
	if _, err := e.m.WriteStdin(s.ID, strings.NewReader("late"), false); !errors.Is(err, ErrStdinClosed) && !errors.Is(err, ErrNotRunning) {
		t.Fatalf("write after close: %v", err)
	}
	s = e.wait(t, s.ID)
	evs := e.events(t, s.ID)
	if got := output(evs, api.EventStdout); got != "first second" {
		t.Fatalf("stdout = %q", got)
	}
	if *s.ExitCode != 0 {
		t.Fatalf("exit code %d", *s.ExitCode)
	}
	if _, err := e.m.WriteStdin(s.ID, strings.NewReader("x"), false); !errors.Is(err, ErrNotRunning) {
		t.Fatalf("write to finished session: %v", err)
	}
}

func TestInitialStdinAndClose(t *testing.T) {
	e := setup(t)
	s := e.create(t, api.SessionSpec{Argv: []string{"wc", "-l"}, Stdin: "a\nb\nc\n", StdinClose: true})
	e.wait(t, s.ID)
	evs := e.events(t, s.ID)
	if got := strings.TrimSpace(output(evs, api.EventStdout)); got != "3" {
		t.Fatalf("stdout = %q", got)
	}
	ts := types(evs)
	if len(ts) != 3 || ts[1] != api.EventStdinClosed {
		t.Fatalf("event types = %v", ts)
	}
}

func TestMergeStderrKeepsOrder(t *testing.T) {
	e := setup(t)
	s := e.create(t, api.SessionSpec{Shell: "echo 1; echo 2 >&2; echo 3; echo 4 >&2", MergeStderr: true})
	e.wait(t, s.ID)
	evs := e.events(t, s.ID)
	if got := output(evs, api.EventStdout); got != "1\n2\n3\n4\n" {
		t.Fatalf("merged output = %q", got)
	}
	if output(evs, api.EventStderr) != "" {
		t.Fatal("unexpected stderr events")
	}
}

func TestTimeout(t *testing.T) {
	e := setup(t)
	start := time.Now()
	s := e.create(t, api.SessionSpec{Argv: []string{"sleep", "30"}, Timeout: api.Duration(100 * time.Millisecond)})
	s = e.wait(t, s.ID)
	if s.State != api.StateTimedOut || s.Signal != "SIGTERM" || s.ExitCode != nil {
		t.Fatalf("session = %+v", s)
	}
	if time.Since(start) > 5*time.Second {
		t.Fatal("timeout took too long")
	}
}

func TestKillEscalatesToSIGKILL(t *testing.T) {
	e := setup(t)
	s := e.create(t, api.SessionSpec{Shell: `trap "" TERM; echo ready; while :; do sleep 0.05; done`})
	waitForOutput(t, e, s.ID, "ready")
	if err := e.m.Kill(s.ID); err != nil {
		t.Fatal(err)
	}
	s = e.wait(t, s.ID)
	if s.State != api.StateKilled || s.Signal != "SIGKILL" {
		t.Fatalf("session = %+v", s)
	}
}

func TestSignal(t *testing.T) {
	e := setup(t)
	s := e.create(t, api.SessionSpec{Argv: []string{"sleep", "30"}})
	if err := e.m.Signal(s.ID, "bogus"); err == nil {
		t.Fatal("bogus signal accepted")
	}
	if err := e.m.Signal(s.ID, "int"); err != nil {
		t.Fatal(err)
	}
	s = e.wait(t, s.ID)
	if s.State != api.StateExited || s.Signal != "SIGINT" {
		t.Fatalf("session = %+v", s)
	}
	if err := e.m.Signal(s.ID, "INT"); !errors.Is(err, ErrNotRunning) {
		t.Fatalf("signal finished session: %v", err)
	}
}

func TestBackgroundChildrenDoNotHoldSessionOpen(t *testing.T) {
	e := setup(t)
	start := time.Now()
	s := e.create(t, api.SessionSpec{Shell: "sleep 30 & echo started"})
	s = e.wait(t, s.ID)
	if s.State != api.StateExited || time.Since(start) > waitDelay+3*time.Second {
		t.Fatalf("session = %+v after %s", s, time.Since(start))
	}
}

func TestFailedToStart(t *testing.T) {
	e := setup(t)
	s := e.create(t, api.SessionSpec{Argv: []string{"true"}, Cwd: filepath.Join(e.dir, "missing")})
	if s.State != api.StateFailedToStart || s.Error == "" {
		t.Fatalf("session = %+v", s)
	}
	evs := e.events(t, s.ID)
	if len(evs) != 1 || evs[0].Type != api.EventError {
		t.Fatalf("events = %+v", evs)
	}
}

func TestInvalidSpecs(t *testing.T) {
	e := setup(t)
	for _, spec := range []api.SessionSpec{
		{},
		{Argv: []string{"echo"}, Shell: "echo"},
		{Argv: []string{""}},
		{Argv: []string{"definitely-not-a-command-shhttp"}},
		{Argv: []string{"echo"}, Cwd: "relative"},
		{Argv: []string{"echo"}, Env: map[string]string{"A=B": "x"}},
		{Argv: []string{"echo"}, Stdin: "a", StdinB64: []byte("b")},
	} {
		_, err := e.m.Create(context.Background(), e.owner, spec)
		var inv *InvalidError
		if !errors.As(err, &inv) {
			t.Errorf("Create(%+v) = %v, want InvalidError", spec, err)
		}
	}
}

func TestPolicyIsEnforced(t *testing.T) {
	e := setup(t)
	no := false
	e.owner.Policy = api.Policy{AllowShell: &no, Commands: []string{".*/echo"}, CwdRoots: []string{e.dir}}
	s := e.create(t, api.SessionSpec{Argv: []string{"echo", "ok"}})
	if s.Spec.Cwd != e.dir {
		t.Fatalf("cwd not defaulted to the first root: %q", s.Spec.Cwd)
	}
	for _, spec := range []api.SessionSpec{
		{Shell: "echo hi"},
		{Argv: []string{"cat"}},
		{Argv: []string{"echo"}, Cwd: "/"},
	} {
		_, err := e.m.Create(context.Background(), e.owner, spec)
		var d *policy.DeniedError
		if !errors.As(err, &d) {
			t.Errorf("Create(%+v) = %v, want DeniedError", spec, err)
		}
	}
}

func TestConcurrencyLimit(t *testing.T) {
	e := setup(t)
	e.owner.Policy.MaxConcurrentSessions = 1
	s := e.create(t, api.SessionSpec{Argv: []string{"sleep", "30"}})
	_, err := e.m.Create(context.Background(), e.owner, api.SessionSpec{Argv: []string{"true"}})
	var lim *LimitError
	if !errors.As(err, &lim) {
		t.Fatalf("second session: %v", err)
	}
	// Another key is not affected.
	other := Owner{KeyID: "key_other"}
	if _, err := e.m.Create(context.Background(), other, api.SessionSpec{Argv: []string{"true"}}); err != nil {
		t.Fatal(err)
	}
	e.m.Kill(s.ID)
	e.wait(t, s.ID)
	e.create(t, api.SessionSpec{Argv: []string{"true"}})
}

func TestKillByKey(t *testing.T) {
	e := setup(t)
	a := e.create(t, api.SessionSpec{Argv: []string{"sleep", "30"}})
	b, _ := e.m.Create(context.Background(), Owner{KeyID: "key_other"}, api.SessionSpec{Argv: []string{"sleep", "30"}})
	if n := e.m.KillByKey(e.owner.KeyID); n != 1 {
		t.Fatalf("killed %d", n)
	}
	if s := e.wait(t, a.ID); s.State != api.StateKilled {
		t.Fatalf("state = %s", s.State)
	}
	if s, _ := e.m.Get(context.Background(), b.ID); s.State != api.StateRunning {
		t.Fatalf("other key's session state = %s", s.State)
	}
}

func TestDeleteAndSweep(t *testing.T) {
	e := setup(t)
	s := e.create(t, api.SessionSpec{Argv: []string{"sleep", "30"}})
	if err := e.m.Delete(context.Background(), s.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := e.m.Get(context.Background(), s.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("Get after delete: %v", err)
	}
	if _, err := os.Stat(e.m.logPath(s.ID)); !os.IsNotExist(err) {
		t.Fatalf("log not removed: %v", err)
	}

	s = e.create(t, api.SessionSpec{Argv: []string{"true"}, Retention: api.Duration(time.Millisecond)})
	keep := e.create(t, api.SessionSpec{Argv: []string{"true"}})
	e.wait(t, s.ID)
	e.wait(t, keep.ID)
	e.m.Sweep(context.Background(), time.Now().Add(time.Second))
	if _, err := e.m.Get(context.Background(), s.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("expired session still present: %v", err)
	}
	if _, err := e.m.Get(context.Background(), keep.ID); err != nil {
		t.Fatalf("session within retention removed: %v", err)
	}
}

func TestRestartMarksRunningSessionsLost(t *testing.T) {
	e := setup(t)
	s := e.create(t, api.SessionSpec{Argv: []string{"sleep", "30"}})
	// Simulate a crash: a second manager starts on the same store while the
	// first still believes the session is running.
	m2, err := NewManager(context.Background(), Config{DataDir: e.dir, Logger: e.m.log}, e.st)
	if err != nil {
		t.Fatal(err)
	}
	got, err := m2.Get(context.Background(), s.ID)
	if err != nil || got.State != api.StateLost {
		t.Fatalf("after restart: %+v, %v", got, err)
	}
}

func TestList(t *testing.T) {
	e := setup(t)
	a := e.create(t, api.SessionSpec{Argv: []string{"true"}, Labels: map[string]string{"team": "a"}})
	b := e.create(t, api.SessionSpec{Argv: []string{"sleep", "30"}, Labels: map[string]string{"team": "b"}})
	e.wait(t, a.ID)
	list, err := e.m.List(context.Background(), store.SessionFilter{KeyID: e.owner.KeyID, Limit: 10})
	if err != nil || len(list) != 2 || list[0].ID != b.ID || list[0].State != api.StateRunning {
		t.Fatalf("list = %+v, %v", list, err)
	}
	list, _ = e.m.List(context.Background(), store.SessionFilter{Labels: map[string]string{"team": "a"}, Limit: 10})
	if len(list) != 1 || list[0].ID != a.ID {
		t.Fatalf("label filter = %+v", list)
	}
	list, _ = e.m.List(context.Background(), store.SessionFilter{Before: b.ID, Limit: 10})
	if len(list) != 1 || list[0].ID != a.ID {
		t.Fatalf("cursor = %+v", list)
	}
}

func TestCompleteUTF8(t *testing.T) {
	euro := []byte("€") // 3 bytes
	cases := []struct {
		in   []byte
		want int
	}{
		{[]byte("abc"), 3},
		{append([]byte("a"), euro[:1]...), 1},
		{append([]byte("a"), euro[:2]...), 1},
		{append([]byte("a"), euro...), 4},
		{[]byte{0xff, 0xfe}, 2}, // invalid bytes are passed through
		{nil, 0},
	}
	for _, c := range cases {
		if got := completeUTF8(c.in); got != c.want {
			t.Errorf("completeUTF8(%v) = %d, want %d", c.in, got, c.want)
		}
	}
}

func TestSplitUTF8IsReassembled(t *testing.T) {
	e := setup(t)
	// printf writes the euro sign's bytes in two separate writes.
	s := e.create(t, api.SessionSpec{Shell: `printf '\342'; sleep 0.1; printf '\202\254\n'`})
	e.wait(t, s.ID)
	for _, ev := range e.events(t, s.ID) {
		if ev.Type == api.EventStdout && string(ev.Data) != "€\n" {
			t.Fatalf("chunk %q was not reassembled", ev.Data)
		}
	}
}

func waitForOutput(t *testing.T, e *env, id, want string) {
	t.Helper()
	l, err := e.m.Log(context.Background(), id)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	var got strings.Builder
	from := uint64(1)
	for !strings.Contains(got.String(), want) {
		evs, err := l.Read(ctx, from, 100, true)
		if err != nil {
			t.Fatalf("waiting for %q: %v (got %q)", want, err, got.String())
		}
		got.WriteString(output(evs, api.EventStdout))
		from = evs[len(evs)-1].Seq + 1
	}
}
