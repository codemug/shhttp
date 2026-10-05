package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"os"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/codemug/shhttp/internal/testserver"
	"github.com/codemug/shhttp/pkg/api"
)

type harness struct {
	t      *testing.T
	env    map[string]string
	srvURL string
}

type result struct {
	code           int
	stdout, stderr string
}

func newHarness(t *testing.T) *harness {
	t.Helper()
	srv := testserver.Start(t)
	h := &harness{t: t, srvURL: srv.URL, env: map[string]string{
		"SHHTTP_URL":        srv.URL,
		"SHHTTP_MASTER_KEY": srv.MasterKey,
	}}
	r := h.run("", "key", "create", "-name", "cli", "-scope", strings.Join(api.AllScopes, ","), "-q")
	if r.code != 0 {
		t.Fatalf("key create: %+v", r)
	}
	h.env["SHHTTP_KEY"] = strings.TrimSpace(r.stdout)
	return h
}

func (h *harness) runWith(stdin io.Reader, sigs <-chan os.Signal, args ...string) result {
	h.t.Helper()
	var out, errOut bytes.Buffer
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	code := Main(ctx, args, Env{
		Stdin:   stdin,
		Stdout:  &out,
		Stderr:  &errOut,
		Getenv:  func(k string) string { return h.env[k] },
		Signals: sigs,
	})
	return result{code, out.String(), errOut.String()}
}

func (h *harness) run(stdin string, args ...string) result {
	h.t.Helper()
	return h.runWith(strings.NewReader(stdin), nil, args...)
}

func TestRun(t *testing.T) {
	h := newHarness(t)
	r := h.run("", "run", "-n", "echo", "hello world")
	if r.code != 0 || r.stdout != "hello world\n" {
		t.Fatalf("echo: %+v", r)
	}
	r = h.run("", "run", "-n", "-c", "echo out; echo err >&2; exit 7")
	if r.code != 7 || r.stdout != "out\n" || r.stderr != "err\n" {
		t.Fatalf("exit code: %+v", r)
	}
	// Local stdin is streamed to the process and closed at EOF.
	r = h.run("line one\nline two\n", "run", "wc", "-l")
	if r.code != 0 || strings.TrimSpace(r.stdout) != "2" {
		t.Fatalf("stdin: %+v", r)
	}
	r = h.run("", "run", "-n", "-e", "A=1", "-e", "B=2", "-c", `echo "$A$B"`)
	if r.stdout != "12\n" {
		t.Fatalf("env: %+v", r)
	}
	// Killed by a signal: 128 + the signal number.
	r = h.run("", "run", "-n", "-timeout", "100ms", "sleep", "30")
	if r.code != 128+15 {
		t.Fatalf("timeout: %+v", r)
	}
	// A refused session is reported with the server's reason.
	r = h.run("", "run", "-n", "-C", "relative", "true")
	if r.code != 1 || !strings.Contains(r.stderr, "cwd must be an absolute path") {
		t.Fatalf("refused: %+v", r)
	}
	// A process that cannot start.
	r = h.run("", "run", "-n", "-C", "/nonexistent-dir", "true")
	if r.code != 255 || !strings.Contains(r.stderr, "failed to start") {
		t.Fatalf("failed to start: %+v", r)
	}
	// Usage errors.
	if r := h.run("", "run"); r.code != 2 {
		t.Fatalf("no command: %+v", r)
	}
	if r := h.run("", "run", "-c", "x", "y"); r.code != 2 {
		t.Fatalf("both -c and a program: %+v", r)
	}
}

func TestRunForwardsSignals(t *testing.T) {
	h := newHarness(t)
	sigs := make(chan os.Signal, 1)
	pr, pw := io.Pipe()
	defer pw.Close()
	done := make(chan result)
	go func() {
		done <- h.runWith(pr, sigs, "run", "-c", `trap 'echo got INT; exit 3' INT; echo ready; while :; do sleep 0.05; done`)
	}()
	time.Sleep(500 * time.Millisecond) // let the trap be installed
	sigs <- os.Interrupt
	r := <-done
	if r.code != 3 || !strings.Contains(r.stdout, "got INT") {
		t.Fatalf("result %+v", r)
	}
}

func TestDetachLogsPsKill(t *testing.T) {
	h := newHarness(t)
	r := h.run("", "run", "-d", "-label", "job=x", "-c", "echo started; sleep 30")
	id := strings.TrimSpace(r.stdout)
	if r.code != 0 || !strings.HasPrefix(id, "ses_") {
		t.Fatalf("detach: %+v", r)
	}
	r = h.run("", "ps")
	if !strings.Contains(r.stdout, id) || !strings.Contains(r.stdout, "running") {
		t.Fatalf("ps: %+v", r)
	}
	if r := h.run("", "ps", "-label", "job=y"); strings.Contains(r.stdout, id) {
		t.Fatalf("ps label filter: %+v", r)
	}
	if r := h.run("", "kill", id); r.code != 0 {
		t.Fatalf("kill: %+v", r)
	}
	r = h.run("", "logs", "-f", id)
	if r.code != 0 || r.stdout != "started\n" {
		t.Fatalf("logs: %+v", r)
	}
	if r := h.run("", "ps"); strings.Contains(r.stdout, id) {
		t.Fatalf("killed session still listed as running: %+v", r)
	}
	r = h.run("", "ps", "-a", "-json")
	var list []api.Session
	if err := json.Unmarshal([]byte(r.stdout), &list); err != nil || len(list) != 1 || list[0].State != api.StateKilled {
		t.Fatalf("ps -a -json: %+v, %v", r, err)
	}
	r = h.run("", "get", id)
	if !strings.Contains(r.stdout, `"state": "killed"`) {
		t.Fatalf("get: %+v", r)
	}
	if r := h.run("", "rm", id); r.code != 0 {
		t.Fatalf("rm: %+v", r)
	}
	if r := h.run("", "get", id); r.code != 1 || !strings.Contains(r.stderr, "HTTP 404") {
		t.Fatalf("get after rm: %+v", r)
	}
}

func TestAttach(t *testing.T) {
	h := newHarness(t)
	id := strings.TrimSpace(h.run("", "run", "-d", "cat").stdout)
	r := h.run("typed\n", "attach", id)
	if r.code != 0 || r.stdout != "typed\n" {
		t.Fatalf("attach: %+v", r)
	}
	// Watching a finished session replays it.
	r = h.run("", "attach", "-readonly", id)
	if r.code != 0 || r.stdout != "typed\n" {
		t.Fatalf("readonly replay: %+v", r)
	}
}

func TestKeyCommands(t *testing.T) {
	h := newHarness(t)
	r := h.run(`{"allow_shell": false}`, "key", "create", "-name", "ro", "-scope", "sessions:read", "-policy", "-")
	var k api.KeyWithSecret
	if err := json.Unmarshal([]byte(r.stdout), &k); err != nil || k.Policy.ShellAllowed() {
		t.Fatalf("key create: %+v, %v", r, err)
	}
	r = h.run("", "key", "ls")
	if !strings.Contains(r.stdout, k.ID) || !strings.Contains(r.stdout, "active") {
		t.Fatalf("key ls: %+v", r)
	}
	r = h.run("", "key", "rotate", "-q", k.ID)
	if !strings.HasPrefix(r.stdout, "shh_") {
		t.Fatalf("key rotate: %+v", r)
	}
	r = h.run("", "key", "revoke", k.ID)
	if r.code != 0 || !strings.Contains(r.stdout, "revoked_at") {
		t.Fatalf("key revoke: %+v", r)
	}
	if r := h.run("", "key", "ls"); !strings.Contains(r.stdout, "revoked") {
		t.Fatalf("key ls after revoke: %+v", r)
	}
	// Schema errors list the failing field.
	r = h.run("", "key", "create", "-name", "x", "-scope", "root")
	if r.code != 1 || !strings.Contains(r.stderr, "body.scopes") {
		t.Fatalf("bad scope: %+v", r)
	}
	// Without the master key, key commands are refused.
	delete(h.env, "SHHTTP_MASTER_KEY")
	if r := h.run("", "key", "ls"); r.code != 1 || !strings.Contains(r.stderr, "HTTP 403") {
		t.Fatalf("key ls with an API key: %+v", r)
	}
}

func TestMisc(t *testing.T) {
	h := newHarness(t)
	r := h.run("", "whoami")
	if r.code != 0 || !strings.Contains(r.stdout, `"name": "cli"`) {
		t.Fatalf("whoami: %+v", r)
	}
	r = h.run("", "version")
	if r.code != 0 || !strings.Contains(r.stdout, "server test") {
		t.Fatalf("version: %+v", r)
	}
	if r := h.run("", "bogus"); r.code != 2 {
		t.Fatalf("unknown command: %+v", r)
	}
	if r := h.run(""); r.code != 2 || !strings.Contains(r.stderr, "Usage") {
		t.Fatalf("no command: %+v", r)
	}
	h.env["SHHTTP_KEY"] = "shh_wrong_key"
	if r := h.run("", "ps"); r.code != 1 || !strings.Contains(r.stderr, "HTTP 401") {
		t.Fatalf("bad key: %+v", r)
	}
}

func TestInterruptStopsNonInteractiveCommands(t *testing.T) {
	h := newHarness(t)
	id := strings.TrimSpace(h.run("", "run", "-d", "sleep", "30").stdout)
	sigs := make(chan os.Signal, 1)
	done := make(chan result)
	go func() { done <- h.runWith(strings.NewReader(""), sigs, "logs", "-f", id) }()
	time.Sleep(200 * time.Millisecond)
	sigs <- syscall.SIGTERM
	select {
	case r := <-done:
		if r.code != 130 {
			t.Fatalf("logs -f after interrupt: %+v", r)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("logs -f ignored the interrupt")
	}
}

func TestJobCommands(t *testing.T) {
	h := newHarness(t)
	spec := `{"name": "ci", "steps": [
		{"name": "build", "spec": {"shell": "echo building"}},
		{"name": "test", "spec": {"shell": "echo testing; exit 2"}}
	]}`
	r := h.run(spec, "job", "submit", "-w")
	if r.code != 1 || !strings.Contains(r.stdout, "build                succeeded (exit 0)") || !strings.Contains(r.stdout, "test                 failed (exit 2)") {
		t.Fatalf("job submit -w: %+v", r)
	}
	r = h.run("", "job", "ls")
	if !strings.Contains(r.stdout, "failed") || !strings.Contains(r.stdout, "2/2") {
		t.Fatalf("job ls: %+v", r)
	}
	var jobs []api.Job
	json.Unmarshal([]byte(h.run("", "job", "ls", "-json").stdout), &jobs)
	id := jobs[0].ID
	r = h.run("", "job", "logs", id)
	if r.stdout != "==> build (succeeded)\nbuilding\n==> test (failed)\ntesting\n" {
		t.Fatalf("job logs: %q", r.stdout)
	}
	if r := h.run("", "job", "logs", "-step", "test", id); r.stdout != "testing\n" {
		t.Fatalf("job logs -step: %q", r.stdout)
	}
	if r := h.run(`{"steps": [{"spec": {"argv": ["sleep", "30"]}}]}`, "job", "submit"); !strings.HasPrefix(r.stdout, "job_") {
		t.Fatalf("job submit: %+v", r)
	} else {
		id := strings.TrimSpace(r.stdout)
		if r := h.run("", "job", "cancel", id); !strings.Contains(r.stdout, "cancelled") {
			t.Fatalf("job cancel: %+v", r)
		}
		if r := h.run("", "job", "rm", id); r.code != 0 {
			t.Fatalf("job rm: %+v", r)
		}
	}
	if r := h.run("{bad json", "job", "submit"); r.code != 1 {
		t.Fatalf("bad JSON: %+v", r)
	}
}

func TestQueueAndTemplateCommands(t *testing.T) {
	h := newHarness(t)
	if r := h.run("", "queue", "set", "deploys", "3"); r.code != 0 {
		t.Fatalf("queue set: %+v", r)
	}
	if r := h.run("", "queue", "ls"); !strings.Contains(r.stdout, "deploys  3") {
		t.Fatalf("queue ls: %+v", r)
	}
	if r := h.run("", "queue", "rm", "deploys"); r.code != 0 {
		t.Fatalf("queue rm: %+v", r)
	}

	tpl := `{"description": "greet someone", "params": {"who": {"pattern": "[a-z]+"}},
		"session": {"argv": ["echo", "hello {{who}}"]}}`
	if r := h.run(tpl, "template", "put", "greet"); r.code != 0 {
		t.Fatalf("template put: %+v", r)
	}
	if r := h.run("", "template", "ls"); !strings.Contains(r.stdout, "greet  session  who") {
		t.Fatalf("template ls: %+v", r)
	}
	r := h.run("", "template", "run", "-n", "-p", "who=ada", "greet")
	if r.code != 0 || r.stdout != "hello ada\n" {
		t.Fatalf("template run: %+v", r)
	}
	if r := h.run("", "template", "run", "-p", "who=A!", "greet"); r.code != 1 || !strings.Contains(r.stderr, "does not match") {
		t.Fatalf("bad parameter: %+v", r)
	}
	jobTpl := `{"job": {"steps": [{"spec": {"argv": ["true"]}}]}}`
	h.run(jobTpl, "template", "put", "noop")
	if r := h.run("", "template", "run", "noop"); r.code != 0 || !strings.Contains(r.stdout, "succeeded") {
		t.Fatalf("job template run: %+v", r)
	}
	if r := h.run("", "template", "rm", "greet"); r.code != 0 {
		t.Fatalf("template rm: %+v", r)
	}
}

func TestRunTTY(t *testing.T) {
	h := newHarness(t)
	r := h.run("", "run", "-t", "-n", "-c", `[ -t 1 ] && echo "on a tty"; stty size`)
	if r.code != 0 || !strings.Contains(r.stdout, "on a tty") || !strings.Contains(r.stdout, "24 80") {
		t.Fatalf("run -t: %+v", r)
	}
}

func TestOutputLimitIsReported(t *testing.T) {
	h := newHarness(t)
	var k api.KeyWithSecret
	r := h.run(`{"max_output_bytes": 100}`, "key", "create", "-name", "small", "-scope", "sessions:run", "-policy", "-")
	json.Unmarshal([]byte(r.stdout), &k)
	h.env["SHHTTP_KEY"] = k.Secret
	r = h.run("", "run", "-n", "-c", "while :; do echo spam; done")
	if r.code == 0 || !strings.Contains(r.stderr, "output limit of 100 bytes exceeded") {
		t.Fatalf("result %+v", r)
	}
}
