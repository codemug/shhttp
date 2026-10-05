// Package job runs jobs: sets of steps, each a session, run in order or as
// a dependency graph, optionally through named queues.
package job

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"maps"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/codemug/shhttp/internal/eventlog"
	"github.com/codemug/shhttp/internal/id"
	"github.com/codemug/shhttp/internal/metrics"
	"github.com/codemug/shhttp/internal/session"
	"github.com/codemug/shhttp/internal/store"
	"github.com/codemug/shhttp/pkg/api"
)

var (
	// ErrNotFound means the job or queue does not exist.
	ErrNotFound = errors.New("not found")
	// ErrFinished means the job has already ended.
	ErrFinished = errors.New("the job has already finished")
	// ErrQueueInUse means a queue still has jobs.
	ErrQueueInUse = errors.New("the queue still has queued or running jobs")
)

// InvalidError reports a malformed job or queue request.
type InvalidError struct{ Reason string }

func (e *InvalidError) Error() string { return e.Reason }

func invalid(format string, args ...any) error {
	return &InvalidError{Reason: fmt.Sprintf(format, args...)}
}

// PolicyFunc returns the current policy of a key, or an error if the key
// may no longer start sessions.
type PolicyFunc func(ctx context.Context, keyID string) (api.Policy, error)

// Config configures a Runner.
type Config struct {
	DataDir          string // job event logs go in DataDir/jobs
	DefaultRetention time.Duration
	Logger           *slog.Logger
}

// Runner owns every queued and running job.
type Runner struct {
	cfg      Config
	store    *store.Store
	sessions *session.Manager
	policy   PolicyFunc
	log      *slog.Logger

	mu      sync.Mutex
	active  map[string]*run
	closing bool
	wg      sync.WaitGroup
}

// run is a job being executed.
type run struct {
	mu       sync.Mutex
	job      api.Job
	log      *eventlog.Log
	cancel   chan struct{}
	stopOnce sync.Once
	done     chan struct{}
}

func (r *run) snapshot() api.Job {
	r.mu.Lock()
	defer r.mu.Unlock()
	j := r.job
	j.Steps = append([]api.StepStatus(nil), r.job.Steps...)
	return j
}

// New creates a Runner. Call Start to resume work left by a previous
// server.
func New(cfg Config, st *store.Store, sessions *session.Manager, policy PolicyFunc) (*Runner, error) {
	if cfg.Logger == nil {
		cfg.Logger = slog.Default()
	}
	if cfg.DefaultRetention <= 0 {
		cfg.DefaultRetention = 24 * time.Hour
	}
	if err := os.MkdirAll(filepath.Join(cfg.DataDir, "jobs"), 0o700); err != nil {
		return nil, err
	}
	return &Runner{cfg: cfg, store: st, sessions: sessions, policy: policy, log: cfg.Logger, active: map[string]*run{}}, nil
}

func (r *Runner) logPath(jobID string) string {
	return filepath.Join(r.cfg.DataDir, "jobs", jobID+".log")
}

var stepName = regexp.MustCompile(`^[A-Za-z0-9_.-]{1,64}$`)

// normalize names steps, checks dependencies and returns each step's
// effective dependencies: those it declares or, when no step declares any,
// the previous step.
func normalize(spec *api.JobSpec) (map[string][]string, error) {
	if len(spec.Steps) == 0 {
		return nil, invalid("a job needs at least one step")
	}
	explicit := false
	seen := map[string]bool{}
	for i := range spec.Steps {
		st := &spec.Steps[i]
		if st.Name == "" {
			st.Name = fmt.Sprintf("step-%d", i+1)
		}
		if !stepName.MatchString(st.Name) {
			return nil, invalid("step name %q must be 1-64 letters, digits, '_', '.' or '-'", st.Name)
		}
		if seen[st.Name] {
			return nil, invalid("two steps are named %q", st.Name)
		}
		seen[st.Name] = true
		if len(st.DependsOn) > 0 {
			explicit = true
		}
	}
	deps := map[string][]string{}
	for i, st := range spec.Steps {
		switch {
		case explicit:
			for _, d := range st.DependsOn {
				if !seen[d] {
					return nil, invalid("step %q depends on unknown step %q", st.Name, d)
				}
				if d == st.Name {
					return nil, invalid("step %q depends on itself", st.Name)
				}
			}
			deps[st.Name] = st.DependsOn
		case i > 0:
			deps[st.Name] = []string{spec.Steps[i-1].Name}
		}
	}
	// Reject cycles: repeatedly remove steps whose dependencies are gone.
	remaining := maps.Clone(seen)
	for len(remaining) > 0 {
		progress := false
		for name := range remaining {
			ready := true
			for _, d := range deps[name] {
				if remaining[d] {
					ready = false
				}
			}
			if ready {
				delete(remaining, name)
				progress = true
			}
		}
		if !progress {
			return nil, invalid("the step dependencies form a cycle")
		}
	}
	return deps, nil
}

// stepSpec is the session spec a step runs: the job's env merged in, and
// labels linking the session to its job.
func stepSpec(j api.Job, st api.JobStep) api.SessionSpec {
	spec := st.Spec
	if len(j.Spec.Env) > 0 {
		env := maps.Clone(j.Spec.Env)
		maps.Copy(env, spec.Env)
		spec.Env = env
	}
	spec.Labels = maps.Clone(spec.Labels)
	if spec.Labels == nil {
		spec.Labels = map[string]string{}
	}
	spec.Labels["shhttp.job"] = j.ID
	spec.Labels["shhttp.step"] = st.Name
	return spec
}

// Submit validates and stores a job, then starts it or queues it.
func (r *Runner) Submit(ctx context.Context, keyID string, spec api.JobSpec) (api.Job, error) {
	if _, err := normalize(&spec); err != nil {
		return api.Job{}, err
	}
	if spec.OnRestart != "" && spec.OnRestart != "fail" && spec.OnRestart != "resume" {
		return api.Job{}, invalid(`on_restart must be "fail" or "resume"`)
	}
	if spec.Queue != "" {
		queues, err := r.store.Queues(ctx)
		if err != nil {
			return api.Job{}, err
		}
		if _, ok := queues[spec.Queue]; !ok {
			return api.Job{}, invalid("queue %q does not exist", spec.Queue)
		}
	}
	pol, err := r.policy(ctx, keyID)
	if err != nil {
		return api.Job{}, err
	}
	j := api.Job{
		ID:        id.New(id.Job),
		KeyID:     keyID,
		Spec:      spec,
		State:     api.JobQueued,
		CreatedAt: time.Now().UTC(),
	}
	// Check every step now, so that a typo or a policy violation fails the
	// submission instead of a step an hour later.
	for _, st := range spec.Steps {
		if err := r.sessions.Validate(session.Owner{KeyID: keyID, Policy: pol}, stepSpec(j, st)); err != nil {
			return api.Job{}, fmt.Errorf("step %q: %w", st.Name, err)
		}
		j.Steps = append(j.Steps, api.StepStatus{Name: st.Name, State: api.StepPending})
	}

	r.mu.Lock()
	closing := r.closing
	r.mu.Unlock()
	if closing {
		return api.Job{}, session.ErrShuttingDown
	}
	l, err := eventlog.Create(r.logPath(j.ID))
	if err != nil {
		return api.Job{}, err
	}
	l.Close() // reopened for appending when the job starts
	if err := r.store.InsertJob(ctx, j); err != nil {
		os.Remove(l.Path())
		return api.Job{}, err
	}
	r.log.Info("job submitted", "audit", true, "job", j.ID, "key", keyID, "queue", spec.Queue, "steps", len(spec.Steps))
	r.schedule()
	return r.Get(ctx, j.ID)
}

// schedule starts queued jobs that may run: jobs without a queue at once,
// queued jobs in submission order while their queue has room.
func (r *Runner) schedule() {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.closing {
		return
	}
	ctx := context.Background()
	queued, err := r.store.ListJobs(ctx, store.JobFilter{States: []api.JobState{api.JobQueued}, Oldest: true})
	if err != nil {
		r.log.Error("listing queued jobs", "err", err)
		return
	}
	if len(queued) == 0 {
		return
	}
	limits, err := r.store.Queues(ctx)
	if err != nil {
		r.log.Error("listing queues", "err", err)
		return
	}
	running := map[string]int{}
	for _, rn := range r.active {
		running[rn.job.Spec.Queue]++
	}
	for _, j := range queued {
		if _, ok := r.active[j.ID]; ok {
			continue
		}
		q := j.Spec.Queue
		if q != "" {
			limit, ok := limits[q]
			if !ok {
				limit = 1 // the queue was deleted after the job was queued
			}
			if running[q] >= limit {
				continue
			}
		}
		running[q]++
		r.startLocked(j)
	}
}

// startLocked launches a job. Callers hold r.mu.
func (r *Runner) startLocked(j api.Job) {
	l, err := eventlog.OpenAppend(r.logPath(j.ID))
	if err != nil {
		r.log.Error("opening job log", "job", j.ID, "err", err)
		return
	}
	rn := &run{job: j, log: l, cancel: make(chan struct{}), done: make(chan struct{})}
	r.active[j.ID] = rn
	r.wg.Add(1)
	go r.execute(rn)
}

type stepResult struct {
	name string
	sess api.Session
	err  error
}

func (r *Runner) save(rn *run) {
	if err := r.store.UpdateJob(context.Background(), rn.snapshot()); err != nil {
		r.log.Error("saving job", "job", rn.job.ID, "err", err)
	}
}

func (r *Runner) emit(rn *run, e api.Event) {
	if _, err := rn.log.Append(e); err != nil {
		r.log.Error("appending job event", "job", rn.job.ID, "err", err)
	}
}

func (r *Runner) isClosing() bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.closing
}

// execute runs a job's steps until it ends, is cancelled, or the server
// stops. On a server stop the job is left as running so that the next
// server applies its on_restart setting.
func (r *Runner) execute(rn *run) {
	defer r.wg.Done()
	defer close(rn.done)
	ctx := context.Background()
	deps, _ := normalize(&rn.job.Spec)

	rn.mu.Lock()
	if rn.job.StartedAt == nil {
		now := time.Now().UTC()
		rn.job.StartedAt = &now
	}
	rn.job.State = api.JobRunning
	rn.mu.Unlock()
	r.emit(rn, api.Event{Type: api.EventJobStarted})
	r.save(rn)

	results := make(chan stepResult)
	cancelCh := rn.cancel
	running := 0
	stopping, cancelled := false, false
	steps := map[string]api.JobStep{}
	for _, st := range rn.job.Spec.Steps {
		steps[st.Name] = st
	}
	status := func(name string) *api.StepStatus {
		for i := range rn.job.Steps {
			if rn.job.Steps[i].Name == name {
				return &rn.job.Steps[i]
			}
		}
		return nil
	}
	// ok reports whether a finished step lets its dependents run.
	ok := func(name string) bool {
		st := status(name)
		return st.State == api.StepSucceeded || (st.State == api.StepFailed && steps[name].AllowFailure)
	}

	for {
		if r.isClosing() {
			stopping = true // leave pending steps for on_restart
		}
		if !stopping {
			for _, name := range r.ready(rn, deps, ok) {
				started := r.startStep(ctx, rn, steps[name], results)
				if started {
					running++
				} else if !steps[name].AllowFailure {
					stopping = true
				}
			}
		}
		if running == 0 {
			break
		}
		select {
		case res := <-results:
			running--
			if r.isClosing() {
				// The server is stopping and killed this step's session;
				// leave the step as running for on_restart.
				continue
			}
			failed := r.finishStep(rn, res, cancelled)
			if failed && !steps[res.name].AllowFailure {
				stopping = true
			}
		case <-cancelCh:
			cancelCh = nil // stop selecting on the closed channel
			stopping, cancelled = true, true
			for _, st := range rn.snapshot().Steps {
				if st.State == api.StepRunning {
					r.sessions.Kill(st.SessionID)
				}
			}
		}
	}

	if r.isClosing() {
		rn.log.Close()
		r.forget(rn)
		return
	}
	r.finishJob(rn, cancelled)
}

// ready returns pending steps whose dependencies allow them to start.
func (r *Runner) ready(rn *run, deps map[string][]string, ok func(string) bool) []string {
	rn.mu.Lock()
	defer rn.mu.Unlock()
	var out []string
	for _, st := range rn.job.Steps {
		if st.State != api.StepPending {
			continue
		}
		all := true
		for _, d := range deps[st.Name] {
			if !ok(d) {
				all = false
				break
			}
		}
		if all {
			out = append(out, st.Name)
		}
	}
	return out
}

// startStep starts a step's session and reports whether it is running. A
// step that cannot start is marked failed.
func (r *Runner) startStep(ctx context.Context, rn *run, st api.JobStep, results chan<- stepResult) bool {
	j := rn.snapshot()
	now := time.Now().UTC()
	fail := func(err error) bool {
		rn.mu.Lock()
		s := stepStatus(&rn.job, st.Name)
		s.State, s.Error, s.StartedAt, s.EndedAt = api.StepFailed, err.Error(), &now, &now
		rn.mu.Unlock()
		r.emit(rn, api.Event{Type: api.EventStepFinished, Step: st.Name, State: api.SessionState(api.StepFailed), Error: err.Error()})
		r.save(rn)
		return false
	}
	pol, err := r.policy(ctx, j.KeyID)
	if err != nil {
		return fail(err)
	}
	sess, err := r.sessions.Create(ctx, session.Owner{KeyID: j.KeyID, Policy: pol}, stepSpec(j, st))
	if err != nil {
		return fail(err)
	}
	rn.mu.Lock()
	s := stepStatus(&rn.job, st.Name)
	s.State, s.SessionID, s.StartedAt, s.Error = api.StepRunning, sess.ID, &now, ""
	s.ExitCode, s.Signal, s.EndedAt = nil, "", nil
	rn.mu.Unlock()
	r.emit(rn, api.Event{Type: api.EventStepStarted, Step: st.Name, SessionID: sess.ID})
	r.save(rn)
	go func() {
		final, err := r.sessions.Wait(context.Background(), sess.ID)
		results <- stepResult{name: st.Name, sess: final, err: err}
	}()
	return true
}

func stepStatus(j *api.Job, name string) *api.StepStatus {
	for i := range j.Steps {
		if j.Steps[i].Name == name {
			return &j.Steps[i]
		}
	}
	panic("unknown step " + name)
}

// finishStep records a step's outcome and reports whether it failed. Steps
// that end unsuccessfully after the job was cancelled count as cancelled.
func (r *Runner) finishStep(rn *run, res stepResult, cancelled bool) bool {
	rn.mu.Lock()
	s := stepStatus(&rn.job, res.name)
	now := time.Now().UTC()
	s.EndedAt = &now
	switch {
	case res.err != nil:
		s.State, s.Error = api.StepFailed, res.err.Error()
	case res.sess.State == api.StateExited && res.sess.ExitCode != nil && *res.sess.ExitCode == 0:
		s.State = api.StepSucceeded
	default:
		s.State = api.StepFailed
		s.Error = res.sess.Error
		if s.Error == "" && res.sess.State != api.StateExited {
			s.Error = "session " + string(res.sess.State)
		}
	}
	s.ExitCode, s.Signal = res.sess.ExitCode, res.sess.Signal
	if cancelled && s.State == api.StepFailed {
		s.State, s.Error = api.StepCancelled, "the job was cancelled"
	}
	st := *s
	rn.mu.Unlock()
	r.emit(rn, api.Event{Type: api.EventStepFinished, Step: st.Name, SessionID: st.SessionID,
		State: api.SessionState(st.State), ExitCode: st.ExitCode, Signal: st.Signal, Error: st.Error})
	r.save(rn)
	return st.State == api.StepFailed
}

func (r *Runner) retention(j api.Job) time.Duration {
	if d := j.Spec.Retention.Std(); d > 0 {
		return d
	}
	return r.cfg.DefaultRetention
}

// finishJob settles the remaining steps and the job's final state.
func (r *Runner) finishJob(rn *run, cancelled bool) {
	rn.mu.Lock()
	failed := false
	for i := range rn.job.Steps {
		s := &rn.job.Steps[i]
		switch s.State {
		case api.StepPending:
			s.State = api.StepSkipped
			if cancelled {
				s.State = api.StepCancelled
			}
		case api.StepFailed:
			for _, st := range rn.job.Spec.Steps {
				if st.Name == s.Name && !st.AllowFailure {
					failed = true
				}
			}
		}
	}
	switch {
	case cancelled:
		rn.job.State = api.JobCancelled
	case failed:
		rn.job.State = api.JobFailed
	default:
		rn.job.State = api.JobSucceeded
	}
	now := time.Now().UTC()
	expires := now.Add(r.retention(rn.job))
	rn.job.EndedAt, rn.job.ExpiresAt = &now, &expires
	state := rn.job.State
	rn.mu.Unlock()

	r.emit(rn, api.Event{Type: api.EventJobFinished, State: api.SessionState(state)})
	metrics.JobsFinished.Inc(string(state))
	rn.log.Close()
	r.save(rn)
	r.log.Info("job finished", "audit", true, "job", rn.job.ID, "key", rn.job.KeyID, "state", state)
	r.forget(rn)
	r.schedule()
}

func (r *Runner) forget(rn *run) {
	r.mu.Lock()
	delete(r.active, rn.job.ID)
	r.mu.Unlock()
}

// Get returns a job.
func (r *Runner) Get(ctx context.Context, jobID string) (api.Job, error) {
	r.mu.Lock()
	rn := r.active[jobID]
	r.mu.Unlock()
	if rn != nil {
		return rn.snapshot(), nil
	}
	j, err := r.store.GetJob(ctx, jobID)
	if errors.Is(err, store.ErrNotFound) {
		return j, ErrNotFound
	}
	return j, err
}

// List returns jobs, newest first, with running jobs' live progress.
func (r *Runner) List(ctx context.Context, f store.JobFilter) ([]api.Job, error) {
	jobs, err := r.store.ListJobs(ctx, f)
	if err != nil {
		return nil, err
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	for i := range jobs {
		if rn := r.active[jobs[i].ID]; rn != nil {
			jobs[i] = rn.snapshot()
		}
	}
	return jobs, nil
}

// Log returns the job's event log.
func (r *Runner) Log(ctx context.Context, jobID string) (*eventlog.Log, error) {
	r.mu.Lock()
	rn := r.active[jobID]
	r.mu.Unlock()
	if rn != nil {
		return rn.log, nil
	}
	if _, err := r.Get(ctx, jobID); err != nil {
		return nil, err
	}
	l, err := eventlog.Open(r.logPath(jobID))
	if errors.Is(err, os.ErrNotExist) {
		return nil, ErrNotFound
	}
	return l, err
}

// Cancel stops a job: a queued job is cancelled at once, a running job's
// sessions are killed. It returns once the job has ended.
func (r *Runner) Cancel(ctx context.Context, jobID string) (api.Job, error) {
	r.mu.Lock()
	rn := r.active[jobID]
	if rn == nil {
		defer r.mu.Unlock()
		j, err := r.store.GetJob(ctx, jobID)
		if errors.Is(err, store.ErrNotFound) {
			return j, ErrNotFound
		}
		if err != nil {
			return j, err
		}
		if j.State != api.JobQueued {
			return j, ErrFinished
		}
		// Still queued: holding r.mu keeps schedule from starting it.
		now := time.Now().UTC()
		expires := now.Add(r.retention(j))
		for i := range j.Steps {
			j.Steps[i].State = api.StepCancelled
		}
		j.State, j.EndedAt, j.ExpiresAt = api.JobCancelled, &now, &expires
		if l, err := eventlog.OpenAppend(r.logPath(j.ID)); err == nil {
			l.Append(api.Event{Type: api.EventJobFinished, State: api.SessionState(api.JobCancelled)})
			l.Close()
		}
		if err := r.store.UpdateJob(ctx, j); err != nil {
			return j, err
		}
		metrics.JobsFinished.Inc(string(api.JobCancelled))
		r.log.Info("job cancelled", "audit", true, "job", j.ID)
		return j, nil
	}
	r.mu.Unlock()
	rn.stopOnce.Do(func() { close(rn.cancel) })
	select {
	case <-rn.done:
	case <-ctx.Done():
		return rn.snapshot(), ctx.Err()
	}
	r.log.Info("job cancelled", "audit", true, "job", jobID)
	return r.Get(context.WithoutCancel(ctx), jobID)
}

// Delete cancels the job if needed and deletes it, its event log and its
// step sessions.
func (r *Runner) Delete(ctx context.Context, jobID string) error {
	j, err := r.Get(ctx, jobID)
	if err != nil {
		return err
	}
	if !j.State.Finished() {
		if j, err = r.Cancel(ctx, jobID); err != nil && !errors.Is(err, ErrFinished) {
			return err
		}
	}
	for _, st := range j.Steps {
		if st.SessionID != "" {
			if err := r.sessions.Delete(ctx, st.SessionID); err != nil && !errors.Is(err, session.ErrNotFound) {
				r.log.Error("deleting step session", "job", jobID, "session", st.SessionID, "err", err)
			}
		}
	}
	if err := r.store.DeleteJob(ctx, jobID); err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return ErrNotFound
		}
		return err
	}
	os.Remove(r.logPath(jobID))
	return nil
}

// Sweep deletes finished jobs whose retention has ended.
func (r *Runner) Sweep(ctx context.Context, now time.Time) {
	ids, err := r.store.ExpiredJobs(ctx, now)
	if err != nil {
		r.log.Error("listing expired jobs", "err", err)
		return
	}
	for _, jid := range ids {
		if err := r.Delete(ctx, jid); err != nil && !errors.Is(err, ErrNotFound) {
			r.log.Error("deleting expired job", "job", jid, "err", err)
		}
	}
}

// Start applies on_restart to jobs the previous server left running and
// starts queued jobs. Sessions must already have been marked lost.
func (r *Runner) Start(ctx context.Context) error {
	running, err := r.store.ListJobs(ctx, store.JobFilter{States: []api.JobState{api.JobRunning}, Oldest: true})
	if err != nil {
		return err
	}
	for _, j := range running {
		now := time.Now().UTC()
		if j.Spec.OnRestart == "resume" {
			for i := range j.Steps {
				if j.Steps[i].State == api.StepRunning {
					j.Steps[i] = api.StepStatus{Name: j.Steps[i].Name, State: api.StepPending}
				}
			}
			if err := r.store.UpdateJob(ctx, j); err != nil {
				return err
			}
			r.mu.Lock()
			r.startLocked(j)
			r.mu.Unlock()
			r.log.Info("resuming job after restart", "job", j.ID)
			continue
		}
		for i := range j.Steps {
			switch j.Steps[i].State {
			case api.StepRunning:
				j.Steps[i].State, j.Steps[i].Error, j.Steps[i].EndedAt = api.StepFailed, "the server stopped while the step was running", &now
			case api.StepPending:
				j.Steps[i].State = api.StepSkipped
			}
		}
		expires := now.Add(r.retention(j))
		j.State, j.Error, j.EndedAt, j.ExpiresAt = api.JobLost, "the server stopped while the job was running", &now, &expires
		if l, err := eventlog.OpenAppend(r.logPath(j.ID)); err == nil {
			l.Append(api.Event{Type: api.EventJobFinished, State: api.SessionState(api.JobLost), Error: j.Error})
			l.Close()
		}
		if err := r.store.UpdateJob(ctx, j); err != nil {
			return err
		}
	}
	if len(running) > 0 {
		r.log.Warn("applied on_restart to jobs from the previous run", "count", len(running))
	}
	r.schedule()
	return nil
}

// Stop makes the runner start no more jobs or steps. Call it before stopping
// the session manager, so that jobs whose sessions are killed by the
// shutdown are left for on_restart instead of failing; then call Wait.
func (r *Runner) Stop() {
	r.mu.Lock()
	r.closing = true
	r.mu.Unlock()
}

// Wait waits for running jobs to notice that their sessions ended.
func (r *Runner) Wait(ctx context.Context) error {
	done := make(chan struct{})
	go func() {
		r.wg.Wait()
		close(done)
	}()
	select {
	case <-done:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// Counts returns how many jobs are running and queued.
func (r *Runner) Counts(ctx context.Context) (running, queued int) {
	r.mu.Lock()
	running = len(r.active)
	r.mu.Unlock()
	q, _ := r.store.ListJobs(ctx, store.JobFilter{States: []api.JobState{api.JobQueued}})
	return running, len(q)
}

// Queues lists queues with their current load.
func (r *Runner) Queues(ctx context.Context) ([]api.Queue, error) {
	limits, err := r.store.Queues(ctx)
	if err != nil {
		return nil, err
	}
	queued, err := r.store.ListJobs(ctx, store.JobFilter{States: []api.JobState{api.JobQueued}})
	if err != nil {
		return nil, err
	}
	counts := map[string]*api.Queue{}
	for name, c := range limits {
		counts[name] = &api.Queue{Name: name, Concurrency: c}
	}
	for _, j := range queued {
		if q := counts[j.Spec.Queue]; q != nil {
			q.Queued++
		}
	}
	r.mu.Lock()
	for _, rn := range r.active {
		if q := counts[rn.job.Spec.Queue]; q != nil {
			q.Running++
		}
	}
	r.mu.Unlock()
	out := make([]api.Queue, 0, len(counts))
	for _, q := range counts {
		out = append(out, *q)
	}
	slices.SortFunc(out, func(a, b api.Queue) int { return strings.Compare(a.Name, b.Name) })
	return out, nil
}

var queueName = regexp.MustCompile(`^[A-Za-z0-9_.-]{1,64}$`)

// PutQueue creates or resizes a queue.
func (r *Runner) PutQueue(ctx context.Context, name string, concurrency int) error {
	if !queueName.MatchString(name) {
		return invalid("queue names are 1-64 letters, digits, '_', '.' or '-'")
	}
	if concurrency < 1 {
		return invalid("concurrency must be at least 1")
	}
	if err := r.store.PutQueue(ctx, name, concurrency); err != nil {
		return err
	}
	r.schedule()
	return nil
}

// DeleteQueue removes an empty queue other than "default".
func (r *Runner) DeleteQueue(ctx context.Context, name string) error {
	if name == "default" {
		return invalid("the default queue cannot be deleted")
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, rn := range r.active {
		if rn.job.Spec.Queue == name {
			return ErrQueueInUse
		}
	}
	queued, err := r.store.ListJobs(ctx, store.JobFilter{States: []api.JobState{api.JobQueued}, Queue: &name, Limit: 1})
	if err != nil {
		return err
	}
	if len(queued) > 0 {
		return ErrQueueInUse
	}
	if err := r.store.DeleteQueue(ctx, name); err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return ErrNotFound
		}
		return err
	}
	return nil
}
