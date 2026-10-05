package session

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"slices"
	"sync"
	"sync/atomic"
	"time"
	"unicode/utf8"

	"github.com/codemug/shhttp/internal/eventlog"
	"github.com/codemug/shhttp/internal/metrics"
	"github.com/codemug/shhttp/pkg/api"
)

// waitDelay bounds how long a session waits, after its process exits, for
// other processes that inherited its stdout or stderr to close them.
const waitDelay = 2 * time.Second

// proc is a running session.
type proc struct {
	m    *Manager
	log  *eventlog.Log
	done chan struct{}

	mu          sync.Mutex
	meta        api.Session
	stdinOpen   bool
	terminating bool
	endState    api.SessionState // set by terminate

	// stdinMu serialises writers so that each request's input stays
	// contiguous and the initial input comes first.
	stdinMu sync.Mutex
	stdin   io.WriteCloser

	tty       *os.File // the pty master of a TTY session
	maxOutput int64
	outBytes  atomic.Int64
	limitOnce sync.Once
}

func (p *proc) snapshot() api.Session {
	p.mu.Lock()
	defer p.mu.Unlock()
	s := p.meta
	s.StdinOpen = p.stdinOpen && s.State == api.StateRunning
	return s
}

func (p *proc) emit(e api.Event) {
	if _, err := p.log.Append(e); err != nil {
		p.m.log.Error("appending session event", "session", p.meta.ID, "type", e.Type, "err", err)
	}
}

func (p *proc) save() {
	if err := p.m.store.UpdateSession(context.Background(), p.snapshot()); err != nil {
		p.m.log.Error("saving session", "session", p.meta.ID, "err", err)
	}
}

func (p *proc) start(prep prepared) {
	spec := p.meta.Spec
	cmd := &exec.Cmd{
		Path:      prep.path,
		Args:      prep.args,
		Dir:       prep.dir,
		Env:       append(slices.Clone(prep.env), sessionEnv+"="+p.meta.ID),
		WaitDelay: waitDelay,
	}
	p.maxOutput = prep.maxOutput
	stdout := &outWriter{p: p, typ: api.EventStdout}
	stderr := stdout
	if !spec.MergeStderr && prep.tty == nil {
		stderr = &outWriter{p: p, typ: api.EventStderr}
	}
	var stdin io.WriteCloser
	var ttyDone chan struct{}
	var err error
	if prep.tty != nil {
		var master *os.File
		master, err = startPTY(cmd, sysProcAttr(true, prep.cred), prep.tty.Cols, prep.tty.Rows)
		if err == nil {
			p.tty = master
			stdin = ttyInput{master}
			ttyDone = make(chan struct{})
			go func() {
				defer close(ttyDone)
				io.Copy(stdout, master) // ends with EIO once the terminal is closed
			}()
		}
	} else {
		cmd.SysProcAttr = sysProcAttr(false, prep.cred)
		// Identical writers make exec use a single pipe for both streams.
		cmd.Stdout, cmd.Stderr = stdout, stderr
		stdin, err = cmd.StdinPipe()
		if err == nil {
			err = cmd.Start()
		}
	}
	if err != nil {
		p.failStart(err)
		return
	}

	now := time.Now().UTC()
	p.mu.Lock()
	p.meta.State = api.StateRunning
	p.meta.PID = cmd.Process.Pid
	p.meta.StartedAt = &now
	p.stdin = stdin
	p.stdinOpen = true
	p.mu.Unlock()
	p.emit(api.Event{Type: api.EventStarted, PID: cmd.Process.Pid})
	p.save()

	if len(prep.stdin) > 0 || spec.StdinClose {
		p.stdinMu.Lock()
		go func() {
			defer p.stdinMu.Unlock()
			if len(prep.stdin) > 0 {
				// A process that exits without reading its input is not an
				// error worth reporting.
				stdin.Write(prep.stdin)
			}
			if spec.StdinClose {
				p.closeStdinLocked()
			}
		}()
	}

	var timer *time.Timer
	if prep.timeout > 0 {
		timer = time.AfterFunc(prep.timeout, func() { p.terminate(api.StateTimedOut) })
	}
	go p.wait(cmd, timer, stdout, stderr, ttyDone)

	p.m.mu.Lock()
	closing := p.m.closing
	p.m.mu.Unlock()
	if closing {
		p.terminate(api.StateKilled)
	}
}

func (p *proc) failStart(err error) {
	now := time.Now().UTC()
	p.mu.Lock()
	p.meta.State = api.StateFailedToStart
	p.meta.Error = err.Error()
	p.meta.EndedAt = &now
	expires := now.Add(p.retention())
	p.meta.ExpiresAt = &expires
	p.mu.Unlock()
	p.emit(api.Event{Type: api.EventError, Error: err.Error()})
	metrics.SessionsFinished.Inc(string(api.StateFailedToStart))
	p.log.Close()
	p.save()
	p.m.forget(p)
	close(p.done)
}

func (p *proc) retention() time.Duration {
	if r := p.meta.Spec.Retention.Std(); r > 0 {
		return r
	}
	return p.m.cfg.DefaultRetention
}

func (p *proc) wait(cmd *exec.Cmd, timer *time.Timer, stdout, stderr *outWriter, ttyDone chan struct{}) {
	err := cmd.Wait()
	if timer != nil {
		timer.Stop()
	}
	if ttyDone != nil {
		// Collect the terminal's remaining output; processes still holding
		// the terminal get as long as WaitDelay before it is closed.
		select {
		case <-ttyDone:
		case <-time.After(waitDelay):
		}
		p.tty.Close()
		<-ttyDone
	}
	stdout.flush()
	if stderr != stdout {
		stderr.flush()
	}
	// The session owns its process group: anything the process left behind
	// is stopped with it.
	killGroup(cmd.Process.Pid)

	code, sig := exitInfo(cmd.ProcessState)
	now := time.Now().UTC()
	p.mu.Lock()
	p.meta.State = api.StateExited
	if p.endState != "" {
		p.meta.State = p.endState
	}
	p.meta.ExitCode = code
	p.meta.Signal = sig
	p.meta.EndedAt = &now
	d := now.Sub(*p.meta.StartedAt).Milliseconds()
	p.meta.DurationMS = &d
	expires := now.Add(p.retention())
	p.meta.ExpiresAt = &expires
	p.stdinOpen = false
	var exitErr *exec.ExitError
	if err != nil && !errors.As(err, &exitErr) && !errors.Is(err, exec.ErrWaitDelay) {
		p.meta.Error = err.Error()
	}
	meta := p.meta
	p.mu.Unlock()

	p.emit(api.Event{Type: api.EventExit, State: meta.State, ExitCode: code, Signal: sig, DurationMS: &d, Error: meta.Error})
	metrics.SessionsFinished.Inc(string(meta.State))
	p.log.Close()
	p.save()
	p.m.log.Info("session ended", "audit", true, "session", meta.ID, "key", meta.KeyID,
		"state", meta.State, "exit_code", code, "signal", sig, "duration_ms", d)
	p.m.forget(p)
	close(p.done)
}

// terminate sends SIGTERM to the process group and SIGKILL after the grace
// period. state is recorded as the final state.
func (p *proc) terminate(state api.SessionState) {
	p.mu.Lock()
	if p.terminating || p.meta.State != api.StateRunning {
		p.mu.Unlock()
		return
	}
	p.terminating = true
	p.endState = state
	pid := p.meta.PID
	p.mu.Unlock()

	if name, err := terminateGroup(pid); err == nil {
		p.emit(api.Event{Type: api.EventSignal, Signal: name})
	}
	go func() {
		select {
		case <-p.done:
		case <-time.After(p.m.cfg.KillGrace):
			killGroup(pid)
			p.emit(api.Event{Type: api.EventSignal, Signal: "SIGKILL"})
		}
	}()
}

func (p *proc) resize(size api.TTYSize) error {
	if p.tty == nil {
		return ErrNotTTY
	}
	if p.snapshot().State != api.StateRunning {
		return ErrNotRunning
	}
	return resizePTY(p.tty, size.Cols, size.Rows)
}

// outputLimit stops a session whose output exceeded its limit.
func (p *proc) outputLimit() {
	p.limitOnce.Do(func() {
		p.mu.Lock()
		p.meta.Error = fmt.Sprintf("output limit of %d bytes exceeded", p.maxOutput)
		p.mu.Unlock()
		go p.terminate(api.StateKilled)
	})
}

// ttyInput writes to a terminal. Closing it sends end-of-file (Ctrl-D)
// rather than closing the terminal, which would hang up the session.
type ttyInput struct{ f *os.File }

func (t ttyInput) Write(b []byte) (int, error) { return t.f.Write(b) }
func (t ttyInput) Close() error {
	_, err := t.f.Write([]byte{4})
	return err
}

func (p *proc) signal(name string) error {
	sig, canonical, err := parseSignal(name)
	if err != nil {
		return invalid("%v", err)
	}
	p.mu.Lock()
	running := p.meta.State == api.StateRunning
	pid := p.meta.PID
	p.mu.Unlock()
	if !running {
		return ErrNotRunning
	}
	if err := signalGroup(pid, sig); err != nil {
		return err
	}
	p.emit(api.Event{Type: api.EventSignal, Signal: canonical})
	return nil
}

func (p *proc) writeStdin(r io.Reader, closeAfter bool) (int64, error) {
	p.stdinMu.Lock()
	defer p.stdinMu.Unlock()
	s := p.snapshot()
	if s.State != api.StateRunning {
		return 0, ErrNotRunning
	}
	if !s.StdinOpen {
		return 0, ErrStdinClosed
	}
	w := &trackingWriter{w: p.stdin}
	n, err := io.Copy(w, r)
	if w.err != nil {
		// The process exited or closed its end of the pipe.
		if p.snapshot().State != api.StateRunning {
			return n, ErrNotRunning
		}
		p.closeStdinLocked()
		return n, ErrStdinClosed
	}
	if err != nil {
		return n, err
	}
	if closeAfter {
		p.closeStdinLocked()
	}
	return n, nil
}

// closeStdinLocked closes stdin. Callers hold stdinMu.
func (p *proc) closeStdinLocked() {
	p.stdin.Close()
	p.mu.Lock()
	wasOpen := p.stdinOpen
	p.stdinOpen = false
	p.mu.Unlock()
	if wasOpen {
		p.emit(api.Event{Type: api.EventStdinClosed})
	}
}

type trackingWriter struct {
	w   io.Writer
	err error
}

func (t *trackingWriter) Write(b []byte) (int, error) {
	n, err := t.w.Write(b)
	if err != nil {
		t.err = err
	}
	return n, err
}

// outWriter turns process output into events. It holds back an incomplete
// UTF-8 sequence at the end of a chunk until the rest arrives, so that text
// split across reads is still sent as text.
type outWriter struct {
	p       *proc
	typ     api.EventType
	mu      sync.Mutex
	pending []byte
}

func (w *outWriter) Write(b []byte) (int, error) {
	n := len(b)
	if limit := w.p.maxOutput; limit > 0 {
		before := w.p.outBytes.Add(int64(n)) - int64(n)
		if before >= limit {
			return n, nil // over the limit: discard while the session stops
		}
		if before+int64(n) > limit {
			b = b[:limit-before]
			defer w.p.outputLimit()
		}
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	buf := b
	if len(w.pending) > 0 {
		buf = append(w.pending, b...)
		w.pending = nil
	}
	cut := completeUTF8(buf)
	if cut < len(buf) {
		w.pending = bytes.Clone(buf[cut:])
	}
	if cut > 0 {
		w.p.emit(api.Event{Type: w.typ, Data: bytes.Clone(buf[:cut])})
		metrics.OutputBytes.Add(float64(cut))
	}
	// Errors are logged by emit; failing here would kill the process with
	// SIGPIPE.
	return n, nil
}

func (w *outWriter) flush() {
	w.mu.Lock()
	defer w.mu.Unlock()
	if len(w.pending) > 0 {
		w.p.emit(api.Event{Type: w.typ, Data: w.pending})
		w.pending = nil
	}
}

// completeUTF8 returns the length of b without a trailing incomplete UTF-8
// sequence.
func completeUTF8(b []byte) int {
	for i := len(b) - 1; i >= 0 && i >= len(b)-utf8.UTFMax+1; i-- {
		if utf8.RuneStart(b[i]) {
			if !utf8.FullRune(b[i:]) {
				return i
			}
			break
		}
	}
	return len(b)
}
