// Package cli implements the shhttp command-line client.
package cli

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"strings"
	"sync/atomic"

	"github.com/codemug/shhttp/pkg/api"
	"github.com/codemug/shhttp/pkg/client"
)

// Version is the client version, set by the shhttp command.
var Version = "dev"

// Env is everything the CLI touches outside itself.
type Env struct {
	Stdin  io.Reader
	Stdout io.Writer
	Stderr io.Writer
	Getenv func(string) string
	// Signals delivers local signals (SIGINT, SIGTERM, SIGHUP) to forward to
	// the remote session. It may be nil.
	Signals <-chan os.Signal
}

// errUsage reports a command-line mistake; the usage has been printed.
var errUsage = errors.New("usage")

// exitError ends the program with a specific code and no message.
type exitError struct{ code int }

func (e exitError) Error() string { return fmt.Sprintf("exit %d", e.code) }

const usage = `Usage: shhttp [-url URL] [-key KEY] <command> [arguments]

Sessions:
  run [flags] [--] program [args...]   run a program, streaming stdin, stdout and stderr
  run [flags] -c 'command line'        run a command line with sh -c
  attach [flags] <id>                  connect to a running session
  logs [-f] [-from N] <id>             print a session's output
  ps [flags]                           list sessions
  get <id>                             show a session as JSON
  kill <id>...                         stop sessions (SIGTERM, then SIGKILL)
  signal <id> <signal>                 send a signal, e.g. INT or SIGHUP
  rm <id>...                           delete sessions and their output

Keys (use the master key):
  key create -name NAME -scope SCOPE [flags]
  key ls | key get <id> | key rotate [-grace D] <id> | key revoke [-kill-sessions] <id>

Other:
  whoami                               describe the key in use
  version                              print client and server versions

Environment:
  SHHTTP_URL          server URL (default http://127.0.0.1:2112)
  SHHTTP_KEY          API key
  SHHTTP_MASTER_KEY   master key, used by the key commands when set

Run 'shhttp <command> -h' for the flags of a command.
`

type app struct {
	env    Env
	url    string
	key    string
	master string
	ctx    context.Context
}

// Main runs the CLI and returns the process exit code.
func Main(ctx context.Context, args []string, env Env) int {
	a := &app{env: env, ctx: ctx}
	fs := flag.NewFlagSet("shhttp", flag.ContinueOnError)
	fs.SetOutput(env.Stderr)
	fs.Usage = func() { fmt.Fprint(env.Stderr, usage) }
	fs.StringVar(&a.url, "url", env.Getenv("SHHTTP_URL"), "server URL")
	fs.StringVar(&a.key, "key", env.Getenv("SHHTTP_KEY"), "API key or master key")
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		return 2
	}
	if a.url == "" {
		a.url = "http://127.0.0.1:2112"
	}
	a.master = env.Getenv("SHHTTP_MASTER_KEY")
	if fs.NArg() == 0 {
		fmt.Fprint(env.Stderr, usage)
		return 2
	}

	cmd, rest := fs.Arg(0), fs.Args()[1:]
	commands := map[string]func([]string) error{
		"run":     a.run,
		"attach":  a.attach,
		"logs":    a.logs,
		"ps":      a.ps,
		"get":     a.get,
		"kill":    a.kill,
		"signal":  a.signal,
		"rm":      a.rm,
		"key":     a.keyCmd,
		"whoami":  a.whoami,
		"version": a.version,
	}
	run, ok := commands[cmd]
	if !ok {
		fmt.Fprintf(env.Stderr, "shhttp: unknown command %q\n\n%s", cmd, usage)
		return 2
	}
	// run and attach forward local signals to the session; every other
	// command stops on the first one.
	var interrupted atomic.Bool
	if cmd != "run" && cmd != "attach" && env.Signals != nil {
		ctx, cancel := context.WithCancel(ctx)
		defer cancel()
		a.ctx = ctx
		go func() {
			select {
			case <-env.Signals:
				interrupted.Store(true)
				cancel()
			case <-ctx.Done():
			}
		}()
	}
	err := run(rest)
	if interrupted.Load() {
		return 130
	}
	var exit exitError
	switch {
	case err == nil:
		return 0
	case errors.As(err, &exit):
		return exit.code
	case errors.Is(err, errUsage):
		return 2
	case errors.Is(err, flag.ErrHelp):
		return 0
	}
	fmt.Fprintln(env.Stderr, "shhttp:", describe(err))
	return 1
}

// describe turns server problems into one readable line.
func describe(err error) string {
	var p *api.Problem
	if errors.As(err, &p) {
		msg := p.Detail
		if msg == "" {
			msg = p.Title
		}
		for _, d := range p.Errors {
			msg += fmt.Sprintf("; %s: %s", d.Location, d.Message)
		}
		return fmt.Sprintf("%s (HTTP %d)", msg, p.Status)
	}
	return err.Error()
}

func (a *app) client() *client.Client { return client.New(a.url, a.key) }

// masterClient uses SHHTTP_MASTER_KEY when set, unless -key was given.
func (a *app) masterClient() *client.Client {
	if a.master != "" && a.key == a.env.Getenv("SHHTTP_KEY") {
		return client.New(a.url, a.master)
	}
	return a.client()
}

// flags returns a flag set for a command that prints its usage on -h.
func (a *app) flags(name, args string) *flag.FlagSet {
	fs := flag.NewFlagSet(name, flag.ContinueOnError)
	fs.SetOutput(a.env.Stderr)
	fs.Usage = func() {
		fmt.Fprintf(a.env.Stderr, "Usage: shhttp %s %s\n", name, args)
		fs.PrintDefaults()
	}
	return fs
}

// parse parses flags and checks the number of positional arguments.
func parse(fs *flag.FlagSet, args []string, minArgs, maxArgs int) error {
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return err
		}
		return errUsage
	}
	if fs.NArg() < minArgs || (maxArgs >= 0 && fs.NArg() > maxArgs) {
		fs.Usage()
		return errUsage
	}
	return nil
}

func (a *app) printJSON(v any) error {
	enc := json.NewEncoder(a.env.Stdout)
	enc.SetIndent("", "  ")
	enc.SetEscapeHTML(false)
	return enc.Encode(v)
}

// multiFlag collects a repeatable flag.
type multiFlag []string

func (m *multiFlag) String() string     { return strings.Join(*m, ",") }
func (m *multiFlag) Set(v string) error { *m = append(*m, v); return nil }

// keyValues parses repeated KEY=VALUE flags.
func keyValues(name string, values []string) (map[string]string, error) {
	if len(values) == 0 {
		return nil, nil
	}
	out := map[string]string{}
	for _, v := range values {
		k, val, ok := strings.Cut(v, "=")
		if !ok || k == "" {
			return nil, fmt.Errorf("-%s takes KEY=VALUE, not %q", name, v)
		}
		out[k] = val
	}
	return out, nil
}

func (a *app) whoami(args []string) error {
	if err := parse(a.flags("whoami", ""), args, 0, 0); err != nil {
		return err
	}
	w, err := a.client().Whoami(a.ctx)
	if err != nil {
		return err
	}
	return a.printJSON(w)
}

func (a *app) version(args []string) error {
	if err := parse(a.flags("version", ""), args, 0, 0); err != nil {
		return err
	}
	fmt.Fprintf(a.env.Stdout, "client %s\n", Version)
	v, err := a.client().Version(a.ctx)
	if err != nil {
		return err
	}
	fmt.Fprintf(a.env.Stdout, "server %s\n", v)
	return nil
}
