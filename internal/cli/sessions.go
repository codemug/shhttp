package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
	"syscall"
	"text/tabwriter"
	"time"

	"github.com/codemug/shhttp/v2/pkg/api"
	"github.com/codemug/shhttp/v2/pkg/client"
	"golang.org/x/term"
)

func (a *app) run(args []string) error {
	fs := a.flags("run", "[flags] [--] program [args...]\n       shhttp run [flags] -c 'command line'")
	shell := fs.String("c", "", "run this command line with sh -c instead of a program")
	var envs, labels multiFlag
	fs.Var(&envs, "e", "set an environment variable, KEY=VALUE (repeatable)")
	fs.Var(&labels, "label", "add a label, KEY=VALUE (repeatable)")
	cwd := fs.String("C", "", "working directory (absolute)")
	timeout := fs.Duration("timeout", 0, "stop the process after this long")
	noStdin := fs.Bool("n", false, "do not send local stdin; close the remote stdin at once")
	detach := fs.Bool("d", false, "start the session in the background and print its id")
	merge := fs.Bool("merge", false, "merge stderr into stdout, preserving their order")
	tty := fs.Bool("t", false, "run on a terminal (for editors, REPLs, password prompts); puts the local terminal in raw mode")
	onDisconnect := fs.String("on-disconnect", "kill", `what happens to the session if this client disconnects: "kill", "keep" or a grace period such as "1m"`)
	if err := parse(fs, args, 0, -1); err != nil {
		return err
	}
	if (*shell == "") == (fs.NArg() == 0) {
		fmt.Fprintln(a.env.Stderr, "shhttp run: give either a program with arguments or -c 'command line'")
		return errUsage
	}
	env, err := keyValues("e", envs)
	if err != nil {
		return err
	}
	labelMap, err := keyValues("label", labels)
	if err != nil {
		return err
	}
	spec := api.SessionSpec{
		Argv:        fs.Args(),
		Shell:       *shell,
		Env:         env,
		Cwd:         *cwd,
		Timeout:     api.Duration(*timeout),
		MergeStderr: *merge,
		Labels:      labelMap,
		StdinClose:  *noStdin,
	}
	if *tty {
		cols, rows := a.termSize()
		spec.TTY = &api.TTYSize{Cols: cols, Rows: rows}
	}
	c := a.client()
	if *detach {
		s, err := c.Create(a.ctx, spec)
		if err != nil {
			return err
		}
		fmt.Fprintln(a.env.Stdout, s.ID)
		return nil
	}
	conn, err := c.Exec(a.ctx, spec, &client.ExecOptions{OnDisconnect: *onDisconnect})
	if err != nil {
		return err
	}
	defer conn.Close()
	if *tty {
		defer a.rawTerminal()()
	}
	return a.pump(conn, !*noStdin, true)
}

func (a *app) attach(args []string) error {
	fs := a.flags("attach", "[flags] <id>")
	from := fs.Uint64("from", 1, "first event to replay; use a large number to see only new output")
	readonly := fs.Bool("readonly", false, "only watch: send no input or signals")
	noStdin := fs.Bool("n", false, "do not send local stdin")
	if err := parse(fs, args, 1, 1); err != nil {
		return err
	}
	conn, err := a.client().Attach(a.ctx, fs.Arg(0), &client.AttachOptions{From: *from, ReadOnly: *readonly})
	if err != nil {
		return err
	}
	defer conn.Close()
	if conn.Session().Spec.TTY != nil && !*readonly {
		defer a.rawTerminal()()
		cols, rows := a.termSize()
		conn.Resize(a.ctx, cols, rows)
	}
	return a.pump(conn, !*readonly && !*noStdin, !*readonly)
}

// pump copies local stdin to the session, forwards local signals, and writes
// the session's output locally until it ends. It returns an exitError with
// the remote exit status.
func (a *app) pump(conn *client.Conn, sendStdin, forwardSignals bool) error {
	ctx, cancel := context.WithCancel(a.ctx)
	defer cancel()

	if sendStdin {
		go func() {
			buf := make([]byte, 32<<10)
			for {
				n, err := a.env.Stdin.Read(buf)
				if n > 0 && conn.Stdin(ctx, buf[:n]) != nil {
					return
				}
				if err != nil {
					// EOF on local stdin means EOF for the remote process.
					conn.CloseStdin(ctx)
					return
				}
			}
		}()
	}
	interrupted := make(chan os.Signal, 1)
	go func() {
		for {
			select {
			case <-ctx.Done():
				return
			case sig, ok := <-a.env.Signals:
				if !ok {
					return
				}
				if winch != nil && sig == winch {
					if forwardSignals {
						cols, rows := a.termSize()
						conn.Resize(ctx, cols, rows)
					}
					continue
				}
				if !forwardSignals {
					interrupted <- sig
					cancel()
					return
				}
				conn.Signal(ctx, signalName(sig))
			}
		}
	}()

	for {
		e, err := conn.Recv(ctx)
		if err == io.EOF {
			return nil // closed without an exit event: nothing more to report
		}
		var pe *client.ProtocolError
		if errors.As(err, &pe) {
			fmt.Fprintln(a.env.Stderr, "shhttp:", pe.Message)
			continue
		}
		if err != nil {
			select {
			case sig := <-interrupted:
				return exitError{128 + signalNumber(signalName(sig))}
			default:
			}
			return err
		}
		switch e.Type {
		case api.EventStdout:
			a.env.Stdout.Write(e.Data)
		case api.EventStderr:
			a.env.Stderr.Write(e.Data)
		case api.EventError:
			fmt.Fprintln(a.env.Stderr, "shhttp: session failed to start:", e.Error)
			return exitError{255}
		case api.EventExit:
			if e.Error != "" {
				fmt.Fprintln(a.env.Stderr, "shhttp:", e.Error)
			}
			return exitError{exitCode(e)}
		}
	}
}

// exitCode follows shell conventions: the exit code, or 128 plus the number
// of the signal that ended the process.
func exitCode(e api.Event) int {
	if e.ExitCode != nil {
		return *e.ExitCode
	}
	if e.Signal != "" {
		return 128 + signalNumber(e.Signal)
	}
	return 255
}

// Signal numbers that are the same on all common Unix systems.
var signalNumbers = map[string]int{
	"SIGHUP": 1, "SIGINT": 2, "SIGQUIT": 3, "SIGILL": 4, "SIGTRAP": 5, "SIGABRT": 6,
	"SIGKILL": 9, "SIGSEGV": 11, "SIGPIPE": 13, "SIGALRM": 14, "SIGTERM": 15,
}

func signalNumber(name string) int {
	if n, ok := signalNumbers[name]; ok {
		return n
	}
	return 127
}

func signalName(sig os.Signal) string {
	switch sig {
	case os.Interrupt:
		return "SIGINT"
	case syscall.SIGTERM:
		return "SIGTERM"
	case syscall.SIGHUP:
		return "SIGHUP"
	}
	return "SIGTERM"
}

func (a *app) logs(args []string) error {
	fs := a.flags("logs", "[flags] <id>")
	follow := fs.Bool("f", false, "keep printing new output until the session ends")
	from := fs.Uint64("from", 1, "first event to print")
	if err := parse(fs, args, 1, 1); err != nil {
		return err
	}
	for e, err := range a.client().Events(a.ctx, fs.Arg(0), &client.EventsOptions{From: *from, Follow: *follow}) {
		if err != nil {
			return err
		}
		switch e.Type {
		case api.EventStdout:
			a.env.Stdout.Write(e.Data)
		case api.EventStderr:
			a.env.Stderr.Write(e.Data)
		}
	}
	return nil
}

func (a *app) ps(args []string) error {
	fs := a.flags("ps", "[flags]")
	all := fs.Bool("a", false, "include finished sessions")
	state := fs.String("state", "", "only sessions in this state")
	var labels multiFlag
	fs.Var(&labels, "label", "only sessions with this label, KEY=VALUE (repeatable)")
	limit := fs.Int("limit", 50, "most sessions to show")
	asJSON := fs.Bool("json", false, "print JSON")
	if err := parse(fs, args, 0, 0); err != nil {
		return err
	}
	labelMap, err := keyValues("label", labels)
	if err != nil {
		return err
	}
	opts := &client.ListOptions{State: api.SessionState(*state), Labels: labelMap, Limit: *limit}
	if opts.State == "" && !*all {
		opts.State = api.StateRunning
	}
	list, err := a.client().List(a.ctx, opts)
	if err != nil {
		return err
	}
	if *asJSON {
		return a.printJSON(list.Sessions)
	}
	w := tabwriter.NewWriter(a.env.Stdout, 0, 4, 2, ' ', 0)
	fmt.Fprintln(w, "ID\tSTATE\tEXIT\tCREATED\tCOMMAND")
	for _, s := range list.Sessions {
		fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\n", s.ID, s.State, exitText(s), ago(s.CreatedAt), commandText(s.Spec))
	}
	return w.Flush()
}

func exitText(s api.Session) string {
	switch {
	case s.ExitCode != nil:
		return fmt.Sprint(*s.ExitCode)
	case s.Signal != "":
		return s.Signal
	}
	return "-"
}

func ago(t time.Time) string {
	d := time.Since(t)
	switch {
	case d < time.Minute:
		return fmt.Sprintf("%ds ago", int(d.Seconds()))
	case d < time.Hour:
		return fmt.Sprintf("%dm ago", int(d.Minutes()))
	case d < 48*time.Hour:
		return fmt.Sprintf("%dh ago", int(d.Hours()))
	}
	return fmt.Sprintf("%dd ago", int(d.Hours()/24))
}

func commandText(spec api.SessionSpec) string {
	s := spec.Shell
	if s == "" {
		s = strings.Join(spec.Argv, " ")
	}
	if len(s) > 60 {
		s = s[:57] + "..."
	}
	return s
}

func (a *app) get(args []string) error {
	fs := a.flags("get", "<id>")
	if err := parse(fs, args, 1, 1); err != nil {
		return err
	}
	s, err := a.client().Get(a.ctx, fs.Arg(0))
	if err != nil {
		return err
	}
	return a.printJSON(s)
}

func (a *app) kill(args []string) error {
	fs := a.flags("kill", "<id>...")
	if err := parse(fs, args, 1, -1); err != nil {
		return err
	}
	c := a.client()
	var failed error
	for _, id := range fs.Args() {
		if _, err := c.Kill(a.ctx, id); err != nil {
			fmt.Fprintf(a.env.Stderr, "shhttp: %s: %s\n", id, describe(err))
			failed = exitError{1}
		}
	}
	return failed
}

func (a *app) signal(args []string) error {
	fs := a.flags("signal", "<id> <signal>")
	if err := parse(fs, args, 2, 2); err != nil {
		return err
	}
	_, err := a.client().Signal(a.ctx, fs.Arg(0), fs.Arg(1))
	return err
}

func (a *app) rm(args []string) error {
	fs := a.flags("rm", "<id>...")
	if err := parse(fs, args, 1, -1); err != nil {
		return err
	}
	c := a.client()
	var failed error
	for _, id := range fs.Args() {
		if err := c.Delete(a.ctx, id); err != nil {
			fmt.Fprintf(a.env.Stderr, "shhttp: %s: %s\n", id, describe(err))
			failed = exitError{1}
		}
	}
	return failed
}

// terminal returns the file descriptor of stdin if it is a terminal.
func (a *app) terminal() (int, bool) {
	f, ok := a.env.Stdin.(*os.File)
	if !ok || !term.IsTerminal(int(f.Fd())) {
		return 0, false
	}
	return int(f.Fd()), true
}

// termSize returns the local terminal's size, or 80x24.
func (a *app) termSize() (cols, rows uint16) {
	if fd, ok := a.terminal(); ok {
		if w, h, err := term.GetSize(fd); err == nil && w > 0 && h > 0 {
			return uint16(w), uint16(h)
		}
	}
	return 80, 24
}

// rawTerminal puts a local terminal in raw mode, so keys such as Ctrl-C
// reach the remote program, and returns a function that restores it.
func (a *app) rawTerminal() func() {
	fd, ok := a.terminal()
	if !ok {
		return func() {}
	}
	state, err := term.MakeRaw(fd)
	if err != nil {
		return func() {}
	}
	return func() { term.Restore(fd, state) }
}
