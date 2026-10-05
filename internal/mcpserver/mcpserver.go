// Package mcpserver exposes an shhttp server to AI agents as Model Context
// Protocol tools. `shhttp mcp` serves them over stdio.
package mcpserver

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/codemug/shhttp/v2/pkg/api"
	"github.com/codemug/shhttp/v2/pkg/client"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

const (
	defaultMaxOutput = 16 << 10
	defaultWait      = 60 * time.Second
	maxWait          = 10 * time.Minute
)

// CommandInput describes a process to run.
type CommandInput struct {
	Argv           []string          `json:"argv,omitempty" jsonschema:"program and arguments, e.g. [\"ls\", \"-l\"]; set argv or shell"`
	Shell          string            `json:"shell,omitempty" jsonschema:"a command line run with sh -c, e.g. \"ls -l | wc -l\"; set argv or shell"`
	Cwd            string            `json:"cwd,omitempty" jsonschema:"absolute working directory"`
	Env            map[string]string `json:"env,omitempty" jsonschema:"extra environment variables"`
	Stdin          string            `json:"stdin,omitempty" jsonschema:"text written to the process's standard input"`
	TimeoutSeconds int               `json:"timeout_seconds,omitempty" jsonschema:"kill the process after this many seconds"`
}

func (in CommandInput) spec() api.SessionSpec {
	return api.SessionSpec{
		Argv:    in.Argv,
		Shell:   in.Shell,
		Cwd:     in.Cwd,
		Env:     in.Env,
		Stdin:   in.Stdin,
		Timeout: api.Duration(time.Duration(in.TimeoutSeconds) * time.Second),
	}
}

// RunInput is the input of run_command.
type RunInput struct {
	CommandInput
	WaitSeconds    int `json:"wait_seconds,omitempty" jsonschema:"how long to wait for the command to finish (default 60, at most 600); a command still running is left running and its session_id returned"`
	MaxOutputBytes int `json:"max_output_bytes,omitempty" jsonschema:"keep at most this many bytes of the end of stdout and of stderr (default 16384)"`
}

// RunOutput is the result of run_command.
type RunOutput struct {
	SessionID       string `json:"session_id"`
	State           string `json:"state" jsonschema:"exited, running (wait_seconds passed), killed, timed_out or failed_to_start"`
	ExitCode        *int   `json:"exit_code,omitempty"`
	Signal          string `json:"signal,omitempty"`
	Error           string `json:"error,omitempty"`
	Stdout          string `json:"stdout"`
	Stderr          string `json:"stderr"`
	StdoutTruncated bool   `json:"stdout_truncated,omitempty"`
	StderrTruncated bool   `json:"stderr_truncated,omitempty"`
}

// StartInput is the input of start_session.
type StartInput struct {
	CommandInput
	TTY bool `json:"tty,omitempty" jsonschema:"run on an 80x24 pseudo-terminal, for programs that need one"`
}

// SessionRef names a session.
type SessionRef struct {
	SessionID string `json:"session_id"`
}

// SessionInfo summarizes a session.
type SessionInfo struct {
	SessionID string `json:"session_id"`
	State     string `json:"state"`
	Command   string `json:"command"`
	ExitCode  *int   `json:"exit_code,omitempty"`
	Signal    string `json:"signal,omitempty"`
	Error     string `json:"error,omitempty"`
	StdinOpen bool   `json:"stdin_open"`
}

func infoOf(s api.Session) SessionInfo {
	cmd := s.Spec.Shell
	if cmd == "" {
		cmd = fmt.Sprint(s.Spec.Argv)
	}
	return SessionInfo{SessionID: s.ID, State: string(s.State), Command: cmd, ExitCode: s.ExitCode, Signal: s.Signal, Error: s.Error, StdinOpen: s.StdinOpen}
}

// InputInput is the input of send_input.
type InputInput struct {
	SessionID string `json:"session_id"`
	Data      string `json:"data,omitempty" jsonschema:"text to write to standard input; include \\n to end a line"`
	Close     bool   `json:"close,omitempty" jsonschema:"close standard input afterwards (end of file)"`
}

// ReadInput is the input of read_output.
type ReadInput struct {
	SessionID      string `json:"session_id"`
	FromSeq        uint64 `json:"from_seq,omitempty" jsonschema:"first event to read: use next_seq from the previous call; 0 or 1 reads from the start"`
	WaitSeconds    int    `json:"wait_seconds,omitempty" jsonschema:"if no new output is available, wait up to this many seconds for some (default 0, at most 600)"`
	MaxOutputBytes int    `json:"max_output_bytes,omitempty" jsonschema:"return at most this many bytes of output (default 16384); call again with next_seq for more"`
}

// ReadOutput is the result of read_output.
type ReadOutput struct {
	Output   string `json:"output" jsonschema:"stdout and stderr in the order they were written"`
	NextSeq  uint64 `json:"next_seq" jsonschema:"pass as from_seq to continue"`
	State    string `json:"state"`
	ExitCode *int   `json:"exit_code,omitempty"`
	Signal   string `json:"signal,omitempty"`
	More     bool   `json:"more,omitempty" jsonschema:"true when output was cut at max_output_bytes"`
}

// SignalInput is the input of send_signal.
type SignalInput struct {
	SessionID string `json:"session_id"`
	Signal    string `json:"signal" jsonschema:"signal name such as INT, TERM or HUP"`
}

// ListInput is the input of list_sessions.
type ListInput struct {
	State string `json:"state,omitempty" jsonschema:"only sessions in this state, e.g. running"`
	Limit int    `json:"limit,omitempty" jsonschema:"most sessions to return (default 20)"`
}

// ListOutput is the result of list_sessions.
type ListOutput struct {
	Sessions []SessionInfo `json:"sessions"`
}

// TemplateInput is the input of run_template.
type TemplateInput struct {
	Name   string            `json:"name"`
	Params map[string]string `json:"params,omitempty"`
}

// TemplateOutput is the result of run_template.
type TemplateOutput struct {
	SessionID string `json:"session_id,omitempty"`
	JobID     string `json:"job_id,omitempty"`
}

// JobRef names a job.
type JobRef struct {
	JobID string `json:"job_id"`
}

// TemplateList lists templates.
type TemplateList struct {
	Templates []api.Template `json:"templates"`
}

func boolPtr(b bool) *bool { return &b }

// New returns an MCP server whose tools call the shhttp server through c.
func New(c *client.Client, version string) *mcp.Server {
	s := mcp.NewServer(&mcp.Implementation{Name: "shhttp", Version: version}, &mcp.ServerOptions{
		Instructions: "Tools that run commands on the machine where the shhttp server runs. Use run_command for commands " +
			"that finish on their own. For long-running or interactive programs use start_session, then send_input and " +
			"read_output (passing next_seq back as from_seq), and kill_session when done.",
	})
	sideEffects := &mcp.ToolAnnotations{DestructiveHint: boolPtr(true), OpenWorldHint: boolPtr(true)}
	readOnly := &mcp.ToolAnnotations{ReadOnlyHint: true, OpenWorldHint: boolPtr(false)}

	mcp.AddTool(s, &mcp.Tool{
		Name:        "run_command",
		Description: "Run a command and wait for it to finish (up to wait_seconds). Returns its exit code and the end of its stdout and stderr. Standard input is closed after `stdin`.",
		Annotations: sideEffects,
	}, func(ctx context.Context, _ *mcp.CallToolRequest, in RunInput) (*mcp.CallToolResult, RunOutput, error) {
		wait := clampWait(in.WaitSeconds, defaultWait)
		limit := in.MaxOutputBytes
		if limit <= 0 {
			limit = defaultMaxOutput
		}
		res, err := c.Run(ctx, in.spec(), &client.RunOptions{WaitTimeout: wait, MaxOutput: limit})
		if err != nil {
			return nil, RunOutput{}, describe(err)
		}
		out := RunOutput{
			SessionID: res.Session.ID, State: string(res.Session.State), ExitCode: res.Session.ExitCode,
			Signal: res.Session.Signal, Error: res.Session.Error,
			Stdout: text(res.Stdout, res.StdoutB64), Stderr: text(res.Stderr, res.StderrB64),
			StdoutTruncated: res.StdoutTruncated, StderrTruncated: res.StderrTruncated,
		}
		return nil, out, nil
	})

	mcp.AddTool(s, &mcp.Tool{
		Name:        "start_session",
		Description: "Start a command without waiting for it, e.g. a server, a REPL or a long build. Returns its session_id for send_input, read_output, send_signal and kill_session. Standard input stays open.",
		Annotations: sideEffects,
	}, func(ctx context.Context, _ *mcp.CallToolRequest, in StartInput) (*mcp.CallToolResult, SessionInfo, error) {
		spec := in.spec()
		if in.TTY {
			spec.TTY = &api.TTYSize{Cols: 80, Rows: 24}
		}
		sess, err := c.Create(ctx, spec)
		if err != nil {
			return nil, SessionInfo{}, describe(err)
		}
		return nil, infoOf(sess), nil
	})

	mcp.AddTool(s, &mcp.Tool{
		Name:        "send_input",
		Description: "Write text to a running session's standard input, optionally closing it afterwards.",
		Annotations: sideEffects,
	}, func(ctx context.Context, _ *mcp.CallToolRequest, in InputInput) (*mcp.CallToolResult, SessionInfo, error) {
		if _, err := c.WriteStdin(ctx, in.SessionID, strings.NewReader(in.Data), in.Close); err != nil {
			return nil, SessionInfo{}, describe(err)
		}
		sess, err := c.Get(ctx, in.SessionID)
		if err != nil {
			return nil, SessionInfo{}, describe(err)
		}
		return nil, infoOf(sess), nil
	})

	mcp.AddTool(s, &mcp.Tool{
		Name:        "read_output",
		Description: "Read a session's output from from_seq onwards, optionally waiting for new output. Returns next_seq to continue from and the session's state.",
		Annotations: readOnly,
	}, func(ctx context.Context, _ *mcp.CallToolRequest, in ReadInput) (*mcp.CallToolResult, ReadOutput, error) {
		out, err := readOutput(ctx, c, in)
		if err != nil {
			return nil, ReadOutput{}, describe(err)
		}
		return nil, out, nil
	})

	mcp.AddTool(s, &mcp.Tool{
		Name:        "send_signal",
		Description: "Send a signal (e.g. INT to interrupt) to a running session's process group.",
		Annotations: sideEffects,
	}, func(ctx context.Context, _ *mcp.CallToolRequest, in SignalInput) (*mcp.CallToolResult, SessionInfo, error) {
		sess, err := c.Signal(ctx, in.SessionID, in.Signal)
		if err != nil {
			return nil, SessionInfo{}, describe(err)
		}
		return nil, infoOf(sess), nil
	})

	mcp.AddTool(s, &mcp.Tool{
		Name:        "kill_session",
		Description: "Stop a running session: SIGTERM, then SIGKILL after a grace period.",
		Annotations: sideEffects,
	}, func(ctx context.Context, _ *mcp.CallToolRequest, in SessionRef) (*mcp.CallToolResult, SessionInfo, error) {
		sess, err := c.Kill(ctx, in.SessionID)
		if err != nil {
			return nil, SessionInfo{}, describe(err)
		}
		return nil, infoOf(sess), nil
	})

	mcp.AddTool(s, &mcp.Tool{
		Name:        "list_sessions",
		Description: "List recent sessions, newest first.",
		Annotations: readOnly,
	}, func(ctx context.Context, _ *mcp.CallToolRequest, in ListInput) (*mcp.CallToolResult, ListOutput, error) {
		limit := in.Limit
		if limit <= 0 {
			limit = 20
		}
		list, err := c.List(ctx, &client.ListOptions{State: api.SessionState(in.State), Limit: limit})
		if err != nil {
			return nil, ListOutput{}, describe(err)
		}
		out := ListOutput{Sessions: []SessionInfo{}}
		for _, s := range list.Sessions {
			out.Sessions = append(out.Sessions, infoOf(s))
		}
		return nil, out, nil
	})

	mcp.AddTool(s, &mcp.Tool{
		Name:        "list_templates",
		Description: "List the templates (approved, parameterized commands and jobs) that run_template can start.",
		Annotations: readOnly,
	}, func(ctx context.Context, _ *mcp.CallToolRequest, _ struct{}) (*mcp.CallToolResult, TemplateList, error) {
		ts, err := c.ListTemplates(ctx)
		if err != nil {
			return nil, TemplateList{}, describe(err)
		}
		if ts == nil {
			ts = []api.Template{}
		}
		return nil, TemplateList{Templates: ts}, nil
	})

	mcp.AddTool(s, &mcp.Tool{
		Name:        "run_template",
		Description: "Start a template with parameter values. Returns a session_id (use read_output) or a job_id (use get_job).",
		Annotations: sideEffects,
	}, func(ctx context.Context, _ *mcp.CallToolRequest, in TemplateInput) (*mcp.CallToolResult, TemplateOutput, error) {
		r, err := c.RunTemplate(ctx, in.Name, in.Params)
		if err != nil {
			return nil, TemplateOutput{}, describe(err)
		}
		var out TemplateOutput
		if r.Session != nil {
			out.SessionID = r.Session.ID
		}
		if r.Job != nil {
			out.JobID = r.Job.ID
		}
		return nil, out, nil
	})

	mcp.AddTool(s, &mcp.Tool{
		Name:        "get_job",
		Description: "Get a job's state and the state, exit code and session_id of each step.",
		Annotations: readOnly,
	}, func(ctx context.Context, _ *mcp.CallToolRequest, in JobRef) (*mcp.CallToolResult, api.Job, error) {
		j, err := c.GetJob(ctx, in.JobID)
		if err != nil {
			return nil, api.Job{}, describe(err)
		}
		return nil, j, nil
	})
	return s
}

func clampWait(seconds int, def time.Duration) time.Duration {
	if seconds <= 0 {
		return def
	}
	return min(time.Duration(seconds)*time.Second, maxWait)
}

// readOutput collects events from in.FromSeq until the output limit, the
// end of the session, or (when nothing has arrived yet) the wait.
func readOutput(ctx context.Context, c *client.Client, in ReadInput) (ReadOutput, error) {
	limit := in.MaxOutputBytes
	if limit <= 0 {
		limit = defaultMaxOutput
	}
	from := max(in.FromSeq, 1)
	out := ReadOutput{NextSeq: from}
	var data []byte
	collect := func(follow bool, waitCtx context.Context) error {
		for e, err := range c.Events(waitCtx, in.SessionID, &client.EventsOptions{From: out.NextSeq, Follow: follow}) {
			if err != nil {
				if waitCtx.Err() != nil && ctx.Err() == nil {
					return nil // the wait ended
				}
				return err
			}
			if e.Type == api.EventStdout || e.Type == api.EventStderr {
				if len(data)+len(e.Data) > limit && len(data) > 0 {
					out.More = true
					return nil
				}
				data = append(data, e.Data...)
			}
			out.NextSeq = e.Seq + 1
			if follow && len(data) > 0 {
				return nil // new output arrived; return it now
			}
		}
		return nil
	}
	if err := collect(false, ctx); err != nil {
		return out, err
	}
	if len(data) == 0 && !out.More && in.WaitSeconds > 0 {
		waitCtx, cancel := context.WithTimeout(ctx, clampWait(in.WaitSeconds, 0))
		err := collect(true, waitCtx)
		cancel()
		if err != nil {
			return out, err
		}
	}
	sess, err := c.Get(ctx, in.SessionID)
	if err != nil {
		return out, err
	}
	out.Output = textBytes(data)
	out.State, out.ExitCode, out.Signal = string(sess.State), sess.ExitCode, sess.Signal
	return out, nil
}

// text returns output as a string; binary output is described, not dumped.
func text(s *string, b []byte) string {
	if s != nil {
		return *s
	}
	return textBytes(b)
}

func textBytes(b []byte) string {
	if utf8.Valid(b) {
		return string(b)
	}
	return fmt.Sprintf("[%d bytes of binary output]", len(b))
}

// describe turns server problems into messages an agent can act on.
func describe(err error) error {
	var p *api.Problem
	if errors.As(err, &p) {
		msg := p.Detail
		for _, d := range p.Errors {
			msg += fmt.Sprintf("; %s: %s", d.Location, d.Message)
		}
		return fmt.Errorf("%s (HTTP %d)", msg, p.Status)
	}
	return err
}
