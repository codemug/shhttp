package job

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/codemug/shhttp/v2/internal/session"
	"github.com/codemug/shhttp/v2/internal/store"
	"github.com/codemug/shhttp/v2/pkg/api"
)

type env struct {
	t        *testing.T
	dir      string
	st       *store.Store
	sessions *session.Manager
	r        *Runner
	revoked  atomic.Bool
}

func setup(t *testing.T) *env {
	t.Helper()
	dir := t.TempDir()
	st, err := store.Open(context.Background(), filepath.Join(dir, "db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	e := &env{t: t, dir: dir, st: st}
	e.start()
	return e
}

// start creates a session manager and runner on the env's storage, as a
// server start does.
func (e *env) start() {
	e.t.Helper()
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	m, err := session.NewManager(context.Background(), session.Config{DataDir: e.dir, KillGrace: 200 * time.Millisecond, Logger: logger}, e.st)
	if err != nil {
		e.t.Fatal(err)
	}
	r, err := New(Config{DataDir: e.dir, Logger: logger}, e.st, m, func(ctx context.Context, keyID string) (api.Policy, error) {
		if e.revoked.Load() {
			return api.Policy{}, errors.New("key revoked")
		}
		return api.Policy{AllowShell: nil}, nil
	})
	if err != nil {
		e.t.Fatal(err)
	}
	if err := r.Start(context.Background()); err != nil {
		e.t.Fatal(err)
	}
	e.sessions, e.r = m, r
	e.t.Cleanup(e.stop)
}

// stop shuts down like the server does.
func (e *env) stop() {
	e.r.Stop()
	e.sessions.Shutdown(context.Background())
	e.r.Wait(context.Background())
}

func sh(name, cmd string, deps ...string) api.JobStep {
	return api.JobStep{Name: name, Spec: api.SessionSpec{Shell: cmd}, DependsOn: deps}
}

func (e *env) submit(spec api.JobSpec) api.Job {
	e.t.Helper()
	j, err := e.r.Submit(context.Background(), "key_test", spec)
	if err != nil {
		e.t.Fatalf("Submit: %v", err)
	}
	return j
}

func (e *env) wait(id string) api.Job {
	e.t.Helper()
	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) {
		j, err := e.r.Get(context.Background(), id)
		if err != nil {
			e.t.Fatal(err)
		}
		if j.State.Finished() {
			return j
		}
		time.Sleep(10 * time.Millisecond)
	}
	e.t.Fatalf("job %s did not finish", id)
	return api.Job{}
}

func states(j api.Job) string {
	var out []string
	for _, s := range j.Steps {
		out = append(out, s.Name+"="+string(s.State))
	}
	return strings.Join(out, " ")
}

func (e *env) stdout(sessionID string) string {
	e.t.Helper()
	l, err := e.sessions.Log(context.Background(), sessionID)
	if err != nil {
		e.t.Fatal(err)
	}
	var b strings.Builder
	from := uint64(1)
	for {
		evs, err := l.Read(context.Background(), from, 100, false)
		if err != nil {
			return b.String()
		}
		for _, ev := range evs {
			if ev.Type == api.EventStdout {
				b.Write(ev.Data)
			}
		}
		from = evs[len(evs)-1].Seq + 1
	}
}

func TestSequential(t *testing.T) {
	e := setup(t)
	j := e.wait(e.submit(api.JobSpec{Steps: []api.JobStep{sh("", "echo one"), sh("", "echo two")}}).ID)
	if j.State != api.JobSucceeded || states(j) != "step-1=succeeded step-2=succeeded" {
		t.Fatalf("job %s: %s", j.State, states(j))
	}
	if j.Steps[1].StartedAt.Before(*j.Steps[0].EndedAt) {
		t.Fatal("step 2 started before step 1 ended")
	}
	if got := e.stdout(j.Steps[1].SessionID); got != "two\n" {
		t.Fatalf("step 2 output %q", got)
	}
	s, _ := e.sessions.Get(context.Background(), j.Steps[0].SessionID)
	if s.Spec.Labels["shhttp.job"] != j.ID || s.Spec.Labels["shhttp.step"] != "step-1" {
		t.Fatalf("step session labels %v", s.Spec.Labels)
	}
}

func TestFailureSkipsTheRest(t *testing.T) {
	e := setup(t)
	j := e.wait(e.submit(api.JobSpec{Steps: []api.JobStep{sh("a", "exit 3"), sh("b", "true")}}).ID)
	if j.State != api.JobFailed || states(j) != "a=failed b=skipped" || *j.Steps[0].ExitCode != 3 {
		t.Fatalf("job %s: %s", j.State, states(j))
	}
}

func TestAllowFailure(t *testing.T) {
	e := setup(t)
	a := sh("a", "exit 1")
	a.AllowFailure = true
	j := e.wait(e.submit(api.JobSpec{Steps: []api.JobStep{a, sh("b", "true")}}).ID)
	if j.State != api.JobSucceeded || states(j) != "a=failed b=succeeded" {
		t.Fatalf("job %s: %s", j.State, states(j))
	}
}

func TestDependencyGraphRunsInParallel(t *testing.T) {
	e := setup(t)
	start := time.Now()
	j := e.wait(e.submit(api.JobSpec{Steps: []api.JobStep{
		sh("a", "sleep 0.4"),
		sh("b", "sleep 0.4"),
		sh("c", "true", "a", "b"),
	}}).ID)
	if j.State != api.JobSucceeded {
		t.Fatalf("job %s: %s", j.State, states(j))
	}
	if d := time.Since(start); d > 750*time.Millisecond {
		t.Fatalf("a and b did not run in parallel: %s", d)
	}
	if j.Steps[2].StartedAt.Before(*j.Steps[0].EndedAt) || j.Steps[2].StartedAt.Before(*j.Steps[1].EndedAt) {
		t.Fatal("c started before its dependencies ended")
	}
}

func TestFailureInGraphLetsRunningStepsFinish(t *testing.T) {
	e := setup(t)
	j := e.wait(e.submit(api.JobSpec{Steps: []api.JobStep{
		sh("fails", "exit 1"),
		sh("slow", "sleep 0.3"),
		sh("after", "true", "slow"),
	}}).ID)
	if j.State != api.JobFailed || states(j) != "fails=failed slow=succeeded after=skipped" {
		t.Fatalf("job %s: %s", j.State, states(j))
	}
}

func TestEnvIsMerged(t *testing.T) {
	e := setup(t)
	step := sh("s", `echo "$A $B"`)
	step.Spec.Env = map[string]string{"B": "step"}
	j := e.wait(e.submit(api.JobSpec{Env: map[string]string{"A": "job", "B": "job"}, Steps: []api.JobStep{step}}).ID)
	if got := e.stdout(j.Steps[0].SessionID); got != "job step\n" {
		t.Fatalf("output %q", got)
	}
}

func TestValidation(t *testing.T) {
	e := setup(t)
	for _, spec := range []api.JobSpec{
		{},
		{Steps: []api.JobStep{sh("a", "true"), sh("a", "true")}},
		{Steps: []api.JobStep{sh("a", "true", "missing")}},
		{Steps: []api.JobStep{sh("a", "true", "b"), sh("b", "true", "a")}},
		{Steps: []api.JobStep{sh("a", "true", "a")}},
		{Steps: []api.JobStep{sh("bad name", "true")}},
		{Steps: []api.JobStep{{Name: "empty"}}},
		{Queue: "nope", Steps: []api.JobStep{sh("a", "true")}},
		{OnRestart: "maybe", Steps: []api.JobStep{sh("a", "true")}},
	} {
		if _, err := e.r.Submit(context.Background(), "key_test", spec); err == nil {
			t.Errorf("Submit(%+v) succeeded", spec)
		}
	}
}

func TestQueueSerializesJobs(t *testing.T) {
	e := setup(t)
	var ids []string
	for i := 0; i < 3; i++ {
		ids = append(ids, e.submit(api.JobSpec{Queue: "default", Steps: []api.JobStep{sh("s", "sleep 0.15")}}).ID)
	}
	queues, _ := e.r.Queues(context.Background())
	if len(queues) != 1 || queues[0].Running != 1 || queues[0].Queued != 2 {
		t.Fatalf("queues %+v", queues)
	}
	var jobs []api.Job
	for _, id := range ids {
		jobs = append(jobs, e.wait(id))
	}
	for i := 1; i < len(jobs); i++ {
		if jobs[i].StartedAt.Before(*jobs[i-1].EndedAt) {
			t.Fatalf("job %d started before job %d ended", i, i-1)
		}
	}

	// With concurrency 2, two jobs overlap.
	if err := e.r.PutQueue(context.Background(), "pair", 2); err != nil {
		t.Fatal(err)
	}
	a := e.submit(api.JobSpec{Queue: "pair", Steps: []api.JobStep{sh("s", "sleep 0.3")}})
	b := e.submit(api.JobSpec{Queue: "pair", Steps: []api.JobStep{sh("s", "sleep 0.3")}})
	ja, jb := e.wait(a.ID), e.wait(b.ID)
	if !jb.StartedAt.Before(*ja.EndedAt) {
		t.Fatal("jobs in a queue of concurrency 2 did not overlap")
	}
}

func TestQueueManagement(t *testing.T) {
	e := setup(t)
	ctx := context.Background()
	if err := e.r.PutQueue(ctx, "bad name", 1); err == nil {
		t.Fatal("bad queue name accepted")
	}
	if err := e.r.DeleteQueue(ctx, "default"); err == nil {
		t.Fatal("default queue deleted")
	}
	e.r.PutQueue(ctx, "busy", 1)
	j := e.submit(api.JobSpec{Queue: "busy", Steps: []api.JobStep{sh("s", "sleep 30")}})
	if err := e.r.DeleteQueue(ctx, "busy"); !errors.Is(err, ErrQueueInUse) {
		t.Fatalf("delete busy queue: %v", err)
	}
	e.r.Cancel(ctx, j.ID)
	if err := e.r.DeleteQueue(ctx, "busy"); err != nil {
		t.Fatalf("delete idle queue: %v", err)
	}
	if err := e.r.DeleteQueue(ctx, "busy"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("delete missing queue: %v", err)
	}
}

func TestCancel(t *testing.T) {
	e := setup(t)
	ctx := context.Background()
	running := e.submit(api.JobSpec{Queue: "default", Steps: []api.JobStep{sh("s", "sleep 30"), sh("t", "true")}})
	queued := e.submit(api.JobSpec{Queue: "default", Steps: []api.JobStep{sh("s", "true")}})
	if queued.State != api.JobQueued {
		t.Fatalf("second job state %s", queued.State)
	}
	j, err := e.r.Cancel(ctx, queued.ID)
	if err != nil || j.State != api.JobCancelled || states(j) != "s=cancelled" {
		t.Fatalf("cancel queued: %+v, %v", j, err)
	}
	time.Sleep(100 * time.Millisecond) // let the first step start
	j, err = e.r.Cancel(ctx, running.ID)
	if err != nil || j.State != api.JobCancelled || states(j) != "s=cancelled t=cancelled" {
		t.Fatalf("cancel running: %s %s, %v", j.State, states(j), err)
	}
	if _, err := e.r.Cancel(ctx, running.ID); !errors.Is(err, ErrFinished) {
		t.Fatalf("cancel finished: %v", err)
	}
}

func TestRevokedKeyCannotStartSteps(t *testing.T) {
	e := setup(t)
	j := e.submit(api.JobSpec{Steps: []api.JobStep{sh("a", "sleep 0.3"), sh("b", "true")}})
	for deadline := time.Now().Add(5 * time.Second); time.Now().Before(deadline); time.Sleep(5 * time.Millisecond) {
		if got, _ := e.r.Get(context.Background(), j.ID); got.Steps[0].State == api.StepRunning {
			break
		}
	}
	e.revoked.Store(true)
	j = e.wait(j.ID)
	if j.State != api.JobFailed || states(j) != "a=succeeded b=failed" || !strings.Contains(j.Steps[1].Error, "revoked") {
		t.Fatalf("job %s: %s (%s)", j.State, states(j), j.Steps[1].Error)
	}
}

func TestEventsAndDelete(t *testing.T) {
	e := setup(t)
	ctx := context.Background()
	j := e.wait(e.submit(api.JobSpec{Steps: []api.JobStep{sh("a", "true"), sh("b", "true")}}).ID)
	l, err := e.r.Log(ctx, j.ID)
	if err != nil {
		t.Fatal(err)
	}
	evs, _ := l.Read(ctx, 1, 100, false)
	var types []string
	for _, ev := range evs {
		types = append(types, string(ev.Type)+":"+ev.Step)
	}
	want := "job_started: step_started:a step_finished:a step_started:b step_finished:b job_finished:"
	if strings.Join(types, " ") != want {
		t.Fatalf("events %v", types)
	}
	if err := e.r.Delete(ctx, j.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := e.r.Get(ctx, j.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("get deleted job: %v", err)
	}
	if _, err := e.sessions.Get(ctx, j.Steps[0].SessionID); !errors.Is(err, session.ErrNotFound) {
		t.Fatalf("step session not deleted: %v", err)
	}
}

func TestRestartFailsOrResumes(t *testing.T) {
	e := setup(t)
	flag := filepath.Join(e.dir, "flag")
	// The second step sleeps the first time and succeeds the second time.
	second := sh("second", `if [ -f `+flag+` ]; then echo resumed; else touch `+flag+`; sleep 30; fi`)
	failJob := e.submit(api.JobSpec{Steps: []api.JobStep{sh("first", "true"), sh("block", "sleep 30")}})
	resumeJob := e.submit(api.JobSpec{OnRestart: "resume", Steps: []api.JobStep{sh("first", "true"), second}})
	// Wait until both second steps run.
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		a, _ := e.r.Get(context.Background(), failJob.ID)
		b, _ := e.r.Get(context.Background(), resumeJob.ID)
		_, flagErr := os.Stat(flag) // the resumable step has really begun
		if a.Steps[1].State == api.StepRunning && b.Steps[1].State == api.StepRunning && flagErr == nil {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}

	e.stop()
	if j, _ := e.st.GetJob(context.Background(), failJob.ID); j.State != api.JobRunning {
		t.Fatalf("after shutdown the job is %s, want running", j.State)
	}
	e.start()

	j := e.wait(failJob.ID)
	if j.State != api.JobLost || states(j) != "first=succeeded block=failed" {
		t.Fatalf("on_restart=fail: %s %s", j.State, states(j))
	}
	j = e.wait(resumeJob.ID)
	if j.State != api.JobSucceeded || states(j) != "first=succeeded second=succeeded" {
		t.Fatalf("on_restart=resume: %s %s", j.State, states(j))
	}
	if got := e.stdout(j.Steps[1].SessionID); got != "resumed\n" {
		t.Fatalf("resumed step output %q", got)
	}
}
