package session

import (
	"context"
	"errors"
	"os"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/codemug/shhttp/internal/policy"
	"github.com/codemug/shhttp/pkg/api"
)

func TestTTYSession(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("no TTY on Windows")
	}
	e := setup(t)
	s := e.create(t, api.SessionSpec{
		Shell: `[ -t 0 ] && echo "tty $TERM"; stty size; read x; stty size; echo "read $x"`,
		TTY:   &api.TTYSize{Cols: 100, Rows: 30},
	})
	waitForOutput(t, e, s.ID, "30 100")
	if err := e.m.Resize(s.ID, api.TTYSize{Cols: 120, Rows: 40}); err != nil {
		t.Fatal(err)
	}
	if _, err := e.m.WriteStdin(s.ID, strings.NewReader("hello\n"), false); err != nil {
		t.Fatal(err)
	}
	s = e.wait(t, s.ID)
	out := output(e.events(t, s.ID), api.EventStdout)
	for _, want := range []string{"tty xterm-256color", "30 100", "40 120", "read hello"} {
		if !strings.Contains(out, want) {
			t.Errorf("output %q lacks %q", out, want)
		}
	}
	if s.State != api.StateExited || *s.ExitCode != 0 {
		t.Fatalf("session %+v", s)
	}
	if err := e.m.Resize(s.ID, api.TTYSize{Cols: 1, Rows: 1}); !errors.Is(err, ErrNotRunning) {
		t.Fatalf("resize finished session: %v", err)
	}
}

func TestTTYStdinCloseSendsEOF(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("no TTY on Windows")
	}
	e := setup(t)
	s := e.create(t, api.SessionSpec{Argv: []string{"cat"}, TTY: &api.TTYSize{Cols: 80, Rows: 24}})
	e.m.WriteStdin(s.ID, strings.NewReader("line\n"), true)
	s = e.wait(t, s.ID)
	if s.State != api.StateExited || *s.ExitCode != 0 {
		t.Fatalf("cat did not see EOF: %+v", s)
	}
}

func TestResizeNeedsTTY(t *testing.T) {
	e := setup(t)
	s := e.create(t, api.SessionSpec{Argv: []string{"sleep", "30"}})
	if err := e.m.Resize(s.ID, api.TTYSize{Cols: 80, Rows: 24}); !errors.Is(err, ErrNotTTY) {
		t.Fatalf("resize without TTY: %v", err)
	}
}

func TestTTYPolicy(t *testing.T) {
	e := setup(t)
	no := false
	e.owner.Policy.AllowTTY = &no
	_, err := e.m.Create(context.Background(), e.owner, api.SessionSpec{Argv: []string{"true"}, TTY: &api.TTYSize{Cols: 80, Rows: 24}})
	var d *policy.DeniedError
	if !errors.As(err, &d) {
		t.Fatalf("TTY with allow_tty=false: %v", err)
	}
}

func TestOutputLimit(t *testing.T) {
	e := setup(t)
	e.owner.Policy.MaxOutputBytes = 1000
	s := e.create(t, api.SessionSpec{Shell: "while :; do echo xxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxx; done"})
	s = e.wait(t, s.ID)
	evs := e.events(t, s.ID)
	if n := len(output(evs, api.EventStdout)); n > 1000 || n == 0 {
		t.Fatalf("kept %d bytes of output", n)
	}
	if s.State != api.StateKilled || !strings.Contains(s.Error, "output limit of 1000 bytes") {
		t.Fatalf("session %+v", s)
	}
	if last := evs[len(evs)-1]; last.Type != api.EventExit || last.Error == "" {
		t.Fatalf("exit event %+v", last)
	}
}

func TestSessionIDIsInEnvironment(t *testing.T) {
	e := setup(t)
	s := e.create(t, api.SessionSpec{Shell: `echo "$SHHTTP_SESSION_ID"`})
	e.wait(t, s.ID)
	if got := output(e.events(t, s.ID), api.EventStdout); got != s.ID+"\n" {
		t.Fatalf("SHHTTP_SESSION_ID = %q, want %s", got, s.ID)
	}
}

func TestRunAs(t *testing.T) {
	if runtime.GOOS == "windows" || os.Geteuid() != 0 {
		t.Skip("run_as needs root")
	}
	e := setup(t)
	e.owner.Policy.RunAs = "nobody"
	s := e.create(t, api.SessionSpec{Shell: `id -un; echo "$USER"`, Cwd: "/"})
	e.wait(t, s.ID)
	if got := output(e.events(t, s.ID), api.EventStdout); got != "nobody\nnobody\n" {
		t.Fatalf("output %q", got)
	}
	e.owner.Policy.RunAs = "no-such-user-shhttp"
	if _, err := e.m.Create(context.Background(), e.owner, api.SessionSpec{Argv: []string{"true"}}); err == nil {
		t.Fatal("unknown run_as user accepted")
	}
}

func TestOrphansAreReapedAfterACrash(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("orphan reaping is Linux only")
	}
	e := setup(t)
	// The shell's background child survives its parent being killed, as
	// it would when a crashed server's Pdeathsig kills only the shell.
	s := e.create(t, api.SessionSpec{Shell: "sleep 60 & echo $!; wait"})
	waitForOutput(t, e, s.ID, "\n")
	l, _ := e.m.Log(context.Background(), s.ID)
	sofar, _ := l.Read(context.Background(), 1, 10, false)
	pid := strings.TrimSpace(output(sofar, api.EventStdout))
	if _, err := os.Stat("/proc/" + pid); err != nil {
		t.Fatalf("child %s not running: %v", pid, err)
	}
	// A second manager on the same store is what a restarted server does.
	if _, err := NewManager(context.Background(), Config{DataDir: e.dir, Logger: e.m.log}, e.st); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if _, err := os.Stat("/proc/" + pid); err != nil {
			return
		}
		if b, _ := os.ReadFile("/proc/" + pid + "/stat"); strings.Contains(string(b), ") Z ") {
			return // killed; waiting to be reaped by its parent
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("orphan %s still running", pid)
}
