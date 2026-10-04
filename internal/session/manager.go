// Package session runs processes and records everything that happens to them
// in an event log.
package session

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/codemug/shhttp/internal/eventlog"
	"github.com/codemug/shhttp/internal/id"
	"github.com/codemug/shhttp/internal/policy"
	"github.com/codemug/shhttp/internal/store"
	"github.com/codemug/shhttp/pkg/api"
)

var (
	// ErrNotFound means the session does not exist.
	ErrNotFound = errors.New("session not found")
	// ErrNotRunning means the operation needs a running process.
	ErrNotRunning = errors.New("the session is not running")
	// ErrStdinClosed means stdin was already closed.
	ErrStdinClosed = errors.New("stdin is closed")
	// ErrShuttingDown means the server is stopping and accepts no new sessions.
	ErrShuttingDown = errors.New("the server is shutting down")
)

// InvalidError reports a malformed request, such as a bad spec or signal name.
type InvalidError struct{ Reason string }

func (e *InvalidError) Error() string { return e.Reason }

func invalid(format string, args ...any) error {
	return &InvalidError{Reason: fmt.Sprintf(format, args...)}
}

// LimitError reports that a concurrency limit was reached.
type LimitError struct{ Reason string }

func (e *LimitError) Error() string { return e.Reason }

// Config configures a Manager.
type Config struct {
	// DataDir holds the session event logs, in DataDir/sessions.
	DataDir string
	// DefaultRetention applies to sessions that set no retention.
	DefaultRetention time.Duration
	// KillGrace is the time between SIGTERM and SIGKILL when a session is
	// killed or times out.
	KillGrace time.Duration
	// MaxSessions caps running sessions across all keys. Zero means no cap.
	MaxSessions int
	Logger      *slog.Logger
}

// Owner is the API key starting a session.
type Owner struct {
	KeyID  string
	Policy api.Policy
}

// Manager owns every running session.
type Manager struct {
	cfg   Config
	store *store.Store
	log   *slog.Logger

	mu       sync.Mutex
	running  map[string]*proc
	closing  bool
	finished sync.WaitGroup
}

// NewManager creates a Manager. Sessions left running by a previous server
// are marked lost.
func NewManager(ctx context.Context, cfg Config, st *store.Store) (*Manager, error) {
	if cfg.Logger == nil {
		cfg.Logger = slog.Default()
	}
	if cfg.KillGrace <= 0 {
		cfg.KillGrace = 10 * time.Second
	}
	if cfg.DefaultRetention <= 0 {
		cfg.DefaultRetention = 24 * time.Hour
	}
	if err := os.MkdirAll(filepath.Join(cfg.DataDir, "sessions"), 0o700); err != nil {
		return nil, err
	}
	lost, err := st.MarkUnfinishedLost(ctx, time.Now(), cfg.DefaultRetention)
	if err != nil {
		return nil, err
	}
	if len(lost) > 0 {
		cfg.Logger.Warn("marked sessions from the previous run as lost", "count", len(lost))
	}
	return &Manager{cfg: cfg, store: st, log: cfg.Logger, running: map[string]*proc{}}, nil
}

func (m *Manager) logPath(sessionID string) string {
	return filepath.Join(m.cfg.DataDir, "sessions", sessionID+".log")
}

// prepared is a validated spec ready to start.
type prepared struct {
	path    string
	args    []string
	dir     string
	env     []string
	timeout time.Duration
	stdin   []byte
}

func (m *Manager) prepare(spec *api.SessionSpec, owner Owner) (prepared, error) {
	var p prepared
	switch {
	case len(spec.Argv) > 0 && spec.Shell != "":
		return p, invalid("set either argv or shell, not both")
	case len(spec.Argv) == 0 && spec.Shell == "":
		return p, invalid("argv or shell is required")
	case len(spec.Argv) > 0 && spec.Argv[0] == "":
		return p, invalid("argv[0] must not be empty")
	}
	if spec.Stdin != "" && spec.StdinB64 != nil {
		return p, invalid("set either stdin or stdin_b64, not both")
	}
	for k, v := range spec.Env {
		if k == "" || strings.ContainsAny(k, "=\x00") || strings.Contains(v, "\x00") {
			return p, invalid("invalid environment variable %q", k)
		}
	}
	if spec.Cwd == "" {
		spec.Cwd = policy.DefaultCwd(owner.Policy)
	}
	if spec.Cwd != "" && !filepath.IsAbs(spec.Cwd) {
		return p, invalid("cwd must be an absolute path")
	}
	p.dir = spec.Cwd

	if spec.Shell != "" {
		p.path, p.args = shellCommand(spec.Shell)
	} else {
		name := spec.Argv[0]
		var err error
		switch {
		case filepath.IsAbs(name):
			p.path = name
		case strings.ContainsRune(name, filepath.Separator) || strings.ContainsRune(name, '/'):
			base := p.dir
			if base == "" {
				base, _ = os.Getwd()
			}
			p.path = filepath.Join(base, name)
		default:
			// PATH lookup uses the server's PATH, not one set in spec.Env.
			if p.path, err = exec.LookPath(name); err != nil {
				return p, invalid("command %q not found in PATH", name)
			}
			if p.path, err = filepath.Abs(p.path); err != nil {
				return p, invalid("command %q: %v", name, err)
			}
		}
		p.args = append([]string{name}, spec.Argv[1:]...)
	}

	envKeys := make([]string, 0, len(spec.Env))
	for k := range spec.Env {
		envKeys = append(envKeys, k)
	}
	sort.Strings(envKeys)
	timeout, err := policy.Check(owner.Policy, policy.Request{
		Shell:   spec.Shell != "",
		Path:    p.path,
		Cwd:     p.dir,
		EnvKeys: envKeys,
		Timeout: spec.Timeout.Std(),
	}, filepath.EvalSymlinks)
	if err != nil {
		return p, err
	}
	p.timeout = timeout
	spec.Timeout = api.Duration(timeout)

	if spec.InheritEnv == nil || *spec.InheritEnv {
		p.env = os.Environ()
	}
	for _, k := range envKeys {
		p.env = append(p.env, k+"="+spec.Env[k])
	}
	if p.env == nil {
		p.env = []string{}
	}
	p.stdin = spec.StdinB64
	if spec.Stdin != "" {
		p.stdin = []byte(spec.Stdin)
	}
	return p, nil
}

// Create validates spec, records the session and starts its process. A
// process that fails to start yields a session in state failed_to_start, not
// an error.
func (m *Manager) Create(ctx context.Context, owner Owner, spec api.SessionSpec) (api.Session, error) {
	prep, err := m.prepare(&spec, owner)
	if err != nil {
		return api.Session{}, err
	}

	sid := id.New(id.Session)
	l, err := eventlog.Create(m.logPath(sid))
	if err != nil {
		return api.Session{}, err
	}
	discardLog := func() {
		l.Close()
		os.Remove(l.Path())
	}

	m.mu.Lock()
	if m.closing {
		m.mu.Unlock()
		discardLog()
		return api.Session{}, ErrShuttingDown
	}
	if m.cfg.MaxSessions > 0 && len(m.running) >= m.cfg.MaxSessions {
		m.mu.Unlock()
		discardLog()
		return api.Session{}, &LimitError{fmt.Sprintf("the server is running its maximum of %d sessions", m.cfg.MaxSessions)}
	}
	if max := owner.Policy.MaxConcurrentSessions; max > 0 && m.countLocked(owner.KeyID) >= max {
		m.mu.Unlock()
		discardLog()
		return api.Session{}, &LimitError{fmt.Sprintf("this key may run at most %d sessions at once", max)}
	}
	p := &proc{
		m:    m,
		log:  l,
		done: make(chan struct{}),
		meta: api.Session{
			ID:        sid,
			KeyID:     owner.KeyID,
			Spec:      spec,
			State:     api.StatePending,
			CreatedAt: time.Now().UTC(),
		},
	}
	// Reserve the slot before releasing the lock so limits hold under
	// concurrent creates.
	m.running[sid] = p
	m.finished.Add(1)
	m.mu.Unlock()

	if err := m.store.InsertSession(ctx, p.meta); err != nil {
		m.forget(p)
		discardLog()
		return api.Session{}, err
	}
	p.start(prep)
	m.log.Info("session started", "audit", true, "session", p.meta.ID, "key", owner.KeyID,
		"argv", spec.Argv, "shell", spec.Shell, "cwd", prep.dir, "state", p.snapshot().State)
	return p.snapshot(), nil
}

func (m *Manager) countLocked(keyID string) int {
	n := 0
	for _, p := range m.running {
		if p.meta.KeyID == keyID {
			n++
		}
	}
	return n
}

// forget removes a session from the running set.
func (m *Manager) forget(p *proc) {
	m.mu.Lock()
	delete(m.running, p.meta.ID)
	m.mu.Unlock()
	m.finished.Done()
}

func (m *Manager) lookup(sessionID string) *proc {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.running[sessionID]
}

// Get returns a session's metadata.
func (m *Manager) Get(ctx context.Context, sessionID string) (api.Session, error) {
	if p := m.lookup(sessionID); p != nil {
		return p.snapshot(), nil
	}
	s, err := m.store.GetSession(ctx, sessionID)
	if errors.Is(err, store.ErrNotFound) {
		return s, ErrNotFound
	}
	return s, err
}

// List returns sessions newest first.
func (m *Manager) List(ctx context.Context, f store.SessionFilter) ([]api.Session, error) {
	list, err := m.store.ListSessions(ctx, f)
	if err != nil {
		return nil, err
	}
	for i := range list {
		if p := m.lookup(list[i].ID); p != nil {
			list[i] = p.snapshot()
		}
	}
	return list, nil
}

// Log returns a session's event log. Logs of finished sessions are opened
// from disk.
func (m *Manager) Log(ctx context.Context, sessionID string) (*eventlog.Log, error) {
	if p := m.lookup(sessionID); p != nil {
		return p.log, nil
	}
	if _, err := m.Get(ctx, sessionID); err != nil {
		return nil, err
	}
	l, err := eventlog.Open(m.logPath(sessionID))
	if errors.Is(err, os.ErrNotExist) {
		return nil, ErrNotFound
	}
	return l, err
}

// Wait blocks until the session's process has ended or ctx is done, then
// returns the session.
func (m *Manager) Wait(ctx context.Context, sessionID string) (api.Session, error) {
	if p := m.lookup(sessionID); p != nil {
		select {
		case <-p.done:
		case <-ctx.Done():
			return p.snapshot(), ctx.Err()
		}
	}
	return m.Get(context.WithoutCancel(ctx), sessionID)
}

// WriteStdin copies r to the session's stdin and closes stdin afterwards if
// closeAfter is set.
func (m *Manager) WriteStdin(sessionID string, r io.Reader, closeAfter bool) (int64, error) {
	p, err := m.mustRun(sessionID)
	if err != nil {
		return 0, err
	}
	return p.writeStdin(r, closeAfter)
}

// Signal sends a signal to the session's process group.
func (m *Manager) Signal(sessionID, name string) error {
	p, err := m.mustRun(sessionID)
	if err != nil {
		return err
	}
	return p.signal(name)
}

// Kill terminates the session: SIGTERM, then SIGKILL after the grace period.
func (m *Manager) Kill(sessionID string) error {
	p, err := m.mustRun(sessionID)
	if err != nil {
		return err
	}
	p.terminate(api.StateKilled)
	return nil
}

func (m *Manager) mustRun(sessionID string) (*proc, error) {
	if p := m.lookup(sessionID); p != nil {
		return p, nil
	}
	if _, err := m.Get(context.Background(), sessionID); err != nil {
		return nil, err
	}
	return nil, ErrNotRunning
}

// Delete kills the session if it is running, then removes its record and
// output.
func (m *Manager) Delete(ctx context.Context, sessionID string) error {
	if p := m.lookup(sessionID); p != nil {
		p.terminate(api.StateKilled)
		select {
		case <-p.done:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	if err := m.store.DeleteSession(ctx, sessionID); err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return ErrNotFound
		}
		return err
	}
	if err := os.Remove(m.logPath(sessionID)); err != nil && !errors.Is(err, os.ErrNotExist) {
		m.log.Error("removing session log", "session", sessionID, "err", err)
	}
	return nil
}

// KillByKey kills every running session started by keyID and returns how
// many there were.
func (m *Manager) KillByKey(keyID string) int {
	m.mu.Lock()
	var ps []*proc
	for _, p := range m.running {
		if p.meta.KeyID == keyID {
			ps = append(ps, p)
		}
	}
	m.mu.Unlock()
	for _, p := range ps {
		p.terminate(api.StateKilled)
	}
	return len(ps)
}

// Sweep deletes finished sessions whose retention has ended.
func (m *Manager) Sweep(ctx context.Context, now time.Time) {
	ids, err := m.store.ExpiredSessions(ctx, now)
	if err != nil {
		m.log.Error("listing expired sessions", "err", err)
		return
	}
	for _, sid := range ids {
		if err := m.Delete(ctx, sid); err != nil && !errors.Is(err, ErrNotFound) {
			m.log.Error("deleting expired session", "session", sid, "err", err)
		}
	}
	if len(ids) > 0 {
		m.log.Info("deleted expired sessions", "count", len(ids))
	}
}

// Shutdown stops accepting sessions, terminates the running ones and waits
// for them to end or for ctx to be done.
func (m *Manager) Shutdown(ctx context.Context) error {
	m.mu.Lock()
	m.closing = true
	ps := make([]*proc, 0, len(m.running))
	for _, p := range m.running {
		ps = append(ps, p)
	}
	m.mu.Unlock()
	for _, p := range ps {
		p.terminate(api.StateKilled)
	}
	done := make(chan struct{})
	go func() {
		m.finished.Wait()
		close(done)
	}()
	select {
	case <-done:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}
