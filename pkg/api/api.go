// Package api defines the request, response and event types of the shhttp v2
// API. It is shared by the server and by clients.
package api

import (
	"bytes"
	"encoding/json"
	"fmt"
	"time"
	"unicode/utf8"
)

// Marshal encodes v as JSON without escaping <, > and &, which keeps command
// output readable. The result has no trailing newline.
func Marshal(v any) ([]byte, error) {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(v); err != nil {
		return nil, err
	}
	return bytes.TrimSuffix(buf.Bytes(), []byte("\n")), nil
}

// Duration is a time.Duration that is written in JSON as a Go duration
// string such as "1m30s".
type Duration time.Duration

func (d Duration) MarshalJSON() ([]byte, error) {
	return json.Marshal(time.Duration(d).String())
}

func (d *Duration) UnmarshalJSON(b []byte) error {
	var s string
	if err := json.Unmarshal(b, &s); err != nil {
		return fmt.Errorf("duration must be a string such as \"30s\": %w", err)
	}
	v, err := time.ParseDuration(s)
	if err != nil {
		return err
	}
	if v < 0 {
		return fmt.Errorf("duration %q is negative", s)
	}
	*d = Duration(v)
	return nil
}

// Std returns d as a time.Duration.
func (d Duration) Std() time.Duration { return time.Duration(d) }

// SessionSpec describes the process a session runs.
type SessionSpec struct {
	// Argv is the program and its arguments. Exactly one of Argv and Shell
	// must be set.
	Argv []string `json:"argv,omitempty" minItems:"1" doc:"Program and arguments. Set exactly one of argv and shell." example:"[\"ls\", \"-l\"]"`
	// Shell is a command line run with "sh -c" ("cmd /C" on Windows).
	Shell string `json:"shell,omitempty" doc:"Command line run with sh -c (cmd /C on Windows)." example:"ls -l | wc -l"`
	// Env holds extra environment variables.
	Env map[string]string `json:"env,omitempty" doc:"Extra environment variables."`
	// InheritEnv starts the process from the server's environment. Defaults
	// to true.
	InheritEnv *bool `json:"inherit_env,omitempty" doc:"Start from the server's environment. Defaults to true."`
	// Cwd is the working directory.
	Cwd string `json:"cwd,omitempty" doc:"Absolute working directory." example:"/srv/app"`
	// Stdin is written to the process's stdin when it starts. StdinB64 is
	// the same for binary input. At most one may be set.
	Stdin    string `json:"stdin,omitempty" doc:"Written to stdin when the process starts."`
	StdinB64 []byte `json:"stdin_b64,omitempty" doc:"Binary initial stdin. At most one of stdin and stdin_b64."`
	// StdinClose closes stdin after the initial input is written.
	StdinClose bool `json:"stdin_close,omitempty" doc:"Close stdin after the initial input."`
	// MergeStderr sends stderr through the stdout pipe, which preserves the
	// relative order of the two.
	MergeStderr bool `json:"merge_stderr,omitempty" doc:"Send stderr through the stdout pipe, preserving their relative order."`
	// Timeout stops the process after this long. Zero means no timeout,
	// unless the key's policy sets a maximum.
	Timeout Duration `json:"timeout,omitempty" doc:"Stop the process after this long." example:"10m"`
	// Retention is how long the session and its output are kept after the
	// process ends. Zero means the server default.
	Retention Duration `json:"retention,omitempty" doc:"Keep the session and its output this long after it ends. Defaults to the server setting." example:"24h"`
	// Labels are free-form metadata usable as list filters.
	Labels map[string]string `json:"labels,omitempty" doc:"Free-form metadata usable as list filters."`
}

// SessionState is the lifecycle state of a session.
type SessionState string

const (
	StatePending       SessionState = "pending"
	StateRunning       SessionState = "running"
	StateExited        SessionState = "exited"
	StateFailedToStart SessionState = "failed_to_start"
	StateKilled        SessionState = "killed"
	StateTimedOut      SessionState = "timed_out"
	StateLost          SessionState = "lost"
)

// Finished reports whether the state is terminal.
func (s SessionState) Finished() bool {
	return s != StatePending && s != StateRunning
}

// Session is a session's metadata.
type Session struct {
	ID         string       `json:"id"`
	KeyID      string       `json:"key_id"`
	Spec       SessionSpec  `json:"spec"`
	State      SessionState `json:"state" enum:"pending,running,exited,failed_to_start,killed,timed_out,lost"`
	PID        int          `json:"pid,omitempty"`
	ExitCode   *int         `json:"exit_code,omitempty" doc:"Set when the process exited normally."`
	Signal     string       `json:"signal,omitempty" doc:"The signal that ended the process."`
	Error      string       `json:"error,omitempty" doc:"Why the session failed to start or was lost."`
	CreatedAt  time.Time    `json:"created_at"`
	StartedAt  *time.Time   `json:"started_at,omitempty"`
	EndedAt    *time.Time   `json:"ended_at,omitempty"`
	DurationMS *int64       `json:"duration_ms,omitempty"`
	ExpiresAt  *time.Time   `json:"expires_at,omitempty"`
	// StdinOpen reports whether the process's stdin still accepts input.
	StdinOpen bool `json:"stdin_open" doc:"Whether stdin still accepts input."`
}

// EventType identifies the kind of a session event.
type EventType string

const (
	EventStarted     EventType = "started"
	EventStdout      EventType = "stdout"
	EventStderr      EventType = "stderr"
	EventStdinClosed EventType = "stdin_closed"
	EventSignal      EventType = "signal"
	EventExit        EventType = "exit"
	EventError       EventType = "error"
)

// Event is one entry in a session's event log. Seq starts at 1 and has no
// gaps.
type Event struct {
	Seq  uint64    `json:"seq"`
	Time time.Time `json:"time"`
	Type EventType `json:"type"`
	// Data is the output chunk of stdout and stderr events. In JSON it is
	// written as "data" when it is valid UTF-8 and as "data_b64" otherwise.
	Data []byte `json:"-"`
	// started
	PID int `json:"pid,omitempty"`
	// signal (the signal that was sent) and exit (the signal that killed the
	// process, if any)
	Signal string `json:"signal,omitempty"`
	// exit
	State      SessionState `json:"state,omitempty"`
	ExitCode   *int         `json:"exit_code,omitempty"`
	DurationMS *int64       `json:"duration_ms,omitempty"`
	// error
	Error string `json:"error,omitempty"`
}

type eventJSON struct {
	*eventAlias
	Data    *string `json:"data,omitempty"`
	DataB64 []byte  `json:"data_b64,omitempty"`
}

type eventAlias Event

func (e Event) MarshalJSON() ([]byte, error) {
	out := eventJSON{eventAlias: (*eventAlias)(&e)}
	if e.Data != nil {
		if utf8.Valid(e.Data) {
			s := string(e.Data)
			out.Data = &s
		} else {
			out.DataB64 = e.Data
		}
	}
	return Marshal(out)
}

func (e *Event) UnmarshalJSON(b []byte) error {
	in := eventJSON{eventAlias: (*eventAlias)(e)}
	if err := json.Unmarshal(b, &in); err != nil {
		return err
	}
	switch {
	case in.Data != nil:
		e.Data = []byte(*in.Data)
	case in.DataB64 != nil:
		e.Data = in.DataB64
	}
	return nil
}

// RunResult is the response to POST /v2/sessions?wait=true.
type RunResult struct {
	Session Session `json:"session"`
	Output
}

// Output holds collected output. Each stream is written as a string when it
// is valid UTF-8 and as base64 in the *_b64 field otherwise.
type Output struct {
	Stdout          *string `json:"stdout,omitempty"`
	StdoutB64       []byte  `json:"stdout_b64,omitempty"`
	StdoutTruncated bool    `json:"stdout_truncated,omitempty"`
	Stderr          *string `json:"stderr,omitempty"`
	StderrB64       []byte  `json:"stderr_b64,omitempty"`
	StderrTruncated bool    `json:"stderr_truncated,omitempty"`
}

// SetStdout stores b in the field that suits its encoding.
func (o *Output) SetStdout(b []byte, truncated bool) {
	o.Stdout, o.StdoutB64 = textOrBinary(b)
	o.StdoutTruncated = truncated
}

// SetStderr stores b in the field that suits its encoding.
func (o *Output) SetStderr(b []byte, truncated bool) {
	o.Stderr, o.StderrB64 = textOrBinary(b)
	o.StderrTruncated = truncated
}

func textOrBinary(b []byte) (*string, []byte) {
	if utf8.Valid(b) {
		s := string(b)
		return &s, nil
	}
	return nil, b
}

// SignalRequest is the body of POST /v2/sessions/{id}/signal.
type SignalRequest struct {
	Signal string `json:"signal" doc:"Signal name, with or without the SIG prefix." example:"SIGINT"`
}

// SessionList is a page of sessions.
type SessionList struct {
	Sessions   []Session `json:"sessions"`
	NextCursor string    `json:"next_cursor,omitempty" doc:"Pass as cursor to get the next page."`
}

// Scopes an API key can hold.
const (
	ScopeSessionsRun  = "sessions:run"
	ScopeSessionsRead = "sessions:read"
	ScopeAdminRead    = "admin:read"
)

// AllScopes lists every scope the server currently understands.
var AllScopes = []string{ScopeSessionsRun, ScopeSessionsRead, ScopeAdminRead}

// Policy restricts what sessions an API key may start. Empty fields impose
// no restriction.
type Policy struct {
	// AllowShell permits SessionSpec.Shell. A key that can use a shell can
	// run any program, so Commands only constrains keys without it.
	AllowShell *bool `json:"allow_shell,omitempty" doc:"Allow shell sessions. Defaults to true. Must be false for commands to restrict anything."`
	// Commands are regular expressions matched against the absolute path of
	// argv[0] after PATH lookup. The pattern must match the whole path.
	Commands []string `json:"commands,omitempty" doc:"Regular expressions; the absolute program path must fully match one." example:"[\"/usr/bin/(git|make)\"]"`
	// CwdRoots are directories a session's working directory must be inside.
	// The first one is the default working directory.
	CwdRoots []string `json:"cwd_roots,omitempty" doc:"Working directories must be inside one of these. The first is the default."`
	// EnvAllow are regular expressions that every key of SessionSpec.Env
	// must fully match.
	EnvAllow []string `json:"env_allow,omitempty" doc:"Regular expressions every env variable name must fully match."`
	// MaxTimeout caps SessionSpec.Timeout and is used when none is given.
	MaxTimeout Duration `json:"max_timeout,omitempty" doc:"Maximum session timeout, also used when none is given." example:"30m"`
	// MaxConcurrentSessions caps how many sessions of this key run at once.
	MaxConcurrentSessions int `json:"max_concurrent_sessions,omitempty" minimum:"0" doc:"Maximum running sessions for this key."`
}

// ShellAllowed reports whether the policy permits shell sessions. Shell is
// allowed unless explicitly disabled.
func (p Policy) ShellAllowed() bool { return p.AllowShell == nil || *p.AllowShell }

// Key is an API key's metadata. The secret is never included.
type Key struct {
	ID         string     `json:"id"`
	Name       string     `json:"name"`
	Scopes     []string   `json:"scopes"`
	Policy     Policy     `json:"policy"`
	CreatedAt  time.Time  `json:"created_at"`
	ExpiresAt  *time.Time `json:"expires_at,omitempty"`
	LastUsedAt *time.Time `json:"last_used_at,omitempty"`
	RevokedAt  *time.Time `json:"revoked_at,omitempty"`
	// PreviousSecretValidUntil is set after a rotation with a grace period.
	PreviousSecretValidUntil *time.Time `json:"previous_secret_valid_until,omitempty"`
}

// CreateKeyRequest is the body of POST /v2/keys.
type CreateKeyRequest struct {
	Name      string   `json:"name" minLength:"1" maxLength:"100" example:"ci-runner"`
	Scopes    []string `json:"scopes" minItems:"1" enum:"sessions:run,sessions:read,admin:read" example:"[\"sessions:run\", \"sessions:read\"]"`
	Policy    Policy   `json:"policy,omitzero"`
	ExpiresIn Duration `json:"expires_in,omitempty" doc:"The key stops working after this long." example:"720h"`
}

// UpdateKeyRequest is the body of PATCH /v2/keys/{id}. Omitted fields are
// left unchanged.
type UpdateKeyRequest struct {
	Name   *string   `json:"name,omitempty"`
	Scopes *[]string `json:"scopes,omitempty" enum:"sessions:run,sessions:read,admin:read"`
	Policy *Policy   `json:"policy,omitempty"`
	// ExpiresAt sets a new expiry. Use ClearExpiry to remove it.
	ExpiresAt   *time.Time `json:"expires_at,omitempty"`
	ClearExpiry bool       `json:"clear_expiry,omitempty" doc:"Remove the expiry."`
}

// RotateKeyRequest is the body of POST /v2/keys/{id}/rotate.
type RotateKeyRequest struct {
	// Grace keeps the previous secret valid for this long.
	Grace Duration `json:"grace,omitempty" doc:"Keep the previous secret valid for this long." example:"1h"`
}

// KeyWithSecret is returned when a key is created or rotated. Secret is the
// full bearer token and is shown only in this response.
type KeyWithSecret struct {
	Key
	Secret string `json:"key" doc:"The full bearer token. Shown only in this response."`
}

// KeyList is the response to GET /v2/keys.
type KeyList struct {
	Keys []Key `json:"keys"`
}

// Whoami is the response to GET /v2/whoami.
type Whoami struct {
	Master bool `json:"master"`
	Key    *Key `json:"key,omitempty"`
	// Notes describe consequences of the key's policy, for example that
	// shell access makes a command allowlist ineffective.
	Notes []string `json:"notes,omitempty"`
}

// Problem is an RFC 9457 problem details error response. Validation errors
// list each invalid field in Errors.
type Problem struct {
	Type     string          `json:"type,omitempty"`
	Title    string          `json:"title"`
	Status   int             `json:"status"`
	Detail   string          `json:"detail,omitempty"`
	Instance string          `json:"instance,omitempty"`
	Errors   []ProblemDetail `json:"errors,omitempty"`
}

// ProblemDetail describes one invalid part of a request.
type ProblemDetail struct {
	Message  string `json:"message,omitempty"`
	Location string `json:"location,omitempty"`
	Value    any    `json:"value,omitempty"`
}

func (p *Problem) Error() string {
	if p.Detail != "" {
		return fmt.Sprintf("%d %s: %s", p.Status, p.Title, p.Detail)
	}
	return fmt.Sprintf("%d %s", p.Status, p.Title)
}

// RevokeKeyResponse is the response to DELETE /v2/keys/{id}.
type RevokeKeyResponse struct {
	Key Key `json:"key"`
	// KilledSessions is how many running sessions were killed
	// (?kill_sessions=true).
	KilledSessions int `json:"killed_sessions"`
}

// StdinResponse is the response to POST /v2/sessions/{id}/stdin.
type StdinResponse struct {
	Bytes     int64 `json:"bytes"`
	StdinOpen bool  `json:"stdin_open"`
}

// Version is the response to GET /v2/version.
type Version struct {
	Version string `json:"version"`
}

// WebSocket protocol. Clients connect to /v2/exec (start a session) or
// /v2/sessions/{id}/attach and exchange JSON text messages.
const (
	// WSSubprotocol must be offered by clients.
	WSSubprotocol = "shhttp.v2.json"
	// WSAuthSubprotocolPrefix carries the key for clients that cannot set
	// an Authorization header (browsers): offer WSAuthSubprotocolPrefix+key
	// alongside WSSubprotocol. The server never echoes it back.
	WSAuthSubprotocolPrefix = "shhttp.v2.auth."
)

// Client message types.
const (
	MsgStart      = "start"
	MsgStdin      = "stdin"
	MsgStdinClose = "stdin_close"
	MsgSignal     = "signal"
)

// ClientMessage is a message from a WebSocket client.
type ClientMessage struct {
	Type string `json:"type"`
	// start (only on /v2/exec, and only as the first message)
	Spec *SessionSpec `json:"spec,omitempty"`
	// OnDisconnect says what happens to the session when this connection
	// closes: "keep" (the default), "kill", or a duration such as "1m"
	// after which the session is killed unless a client has attached.
	OnDisconnect string `json:"on_disconnect,omitempty"`
	// stdin
	Data    string `json:"data,omitempty"`
	DataB64 []byte `json:"data_b64,omitempty"`
	// signal
	Signal string `json:"signal,omitempty"`
}

// Server message types besides the session event types.
const (
	// MsgSession is sent first on every connection and describes the session.
	MsgSession = "session"
	// MsgProtocolError reports a rejected message or a session that could
	// not be started. Status is the HTTP status the same failure gets over
	// HTTP.
	MsgProtocolError = "protocol_error"
)

// ServerMessage is a WebSocket server message that is not a session event.
// Every other server message is an Event.
type ServerMessage struct {
	Type    string   `json:"type"`
	Session *Session `json:"session,omitempty"`
	Error   string   `json:"error,omitempty"`
	Status  int      `json:"status,omitempty"`
}
