# shhttp v2 — design

Status: draft for review. v2 is a clean break from v1: new API under `/v2`, new storage, no compatibility layer.

## Goals

- Run commands remotely and **stream their output as it is produced**, with **stdin supplied while the process runs**.
- Be **client-agnostic**: usable from curl and shell scripts, programs in any language, interactive terminals and AI agents. No feature depends on a specific client.
- Be **deployment-agnostic**: a single static binary that works on a laptop, a server, a container sidecar or a CI runner, configured by flags, env vars or a config file.
- Be **secure by default**: every request is authenticated, keys are scoped and managed through an API, and the master key only manages keys.
- Keep v1's features (jobs, queues, saved jobs) and rebuild them on the new core.

Non-goals for v2.0: clustering or multi-node scheduling, user accounts or OAuth, a web UI (the API must make one easy to build later).

## Core model

Everything is built on one primitive, the **session**: a single process plus an ordered, durable **event log** of everything that happens to it.

```
                ┌─────────────────────── Session ses_… ───────────────────────┐
 stdin writes ─▶│ stdin pipe ─▶ process (own process group) ─▶ stdout/stderr  │
 (any client)   │                                              pumps          │
                │                                                 │           │
                │   event log: seq 1..N  {started, stdout, stderr, exit, …}   │
                │      ├── in-memory tail buffer (fast fan-out)               │
                │      └── append-only file on disk (replay, detached runs)   │
                └──────────────────────────────┬──────────────────────────────┘
                                               ▼
                    0..N subscribers (WebSocket, NDJSON, SSE, wait=true)
```

- The process lifetime is independent of any connection. Clients **attach, detach and re-attach from any `seq`** without losing output.
- Output is always drained to disk, so a process never blocks because no client is reading.
- A **job** is a set of steps, each step runs as a session. A **template** is a saved, parameterised job or session spec. A **queue** limits how many jobs run at once.

### Session states

`pending → running → exited | failed_to_start | killed | timed_out | lost`

`lost` means the server stopped while the process was running. On a clean shutdown the server stops every session first. After a crash, the session's own process dies with the server on Linux (`Pdeathsig`), but processes it started itself can survive; see Known limitations.

## API overview

Base path `/v2`. JSON everywhere except the streaming formats listed below. Errors use RFC 9457 `application/problem+json`: 422 with a per-field `errors` list when a request does not match the schema, 400 for malformed JSON and for rules the schema cannot express, 415 when a JSON body lacks `Content-Type: application/json`.

The HTTP layer is built with [huma](https://github.com/danielgtaylor/huma) on the standard `net/http` mux. Each operation is declared with typed Go input and output structs; huma validates requests against the JSON Schema derived from them and generates the OpenAPI 3.1 document, served at `/v2/openapi.json` and `/v2/openapi.yaml`, with an interactive reference at `/v2/docs`. Streaming endpoints use huma's stream responses, and the stdin endpoint reads the raw request body itself so input reaches the process while it is still being sent. IDs are prefixed and time-sortable: `ses_…`, `job_…`, `tpl_…`, `key_…`. Lists use cursor pagination (`?limit=&cursor=`).

| Area | Endpoint | Purpose |
|---|---|---|
| Sessions | `POST /v2/sessions` | Create a session. `?wait=true` blocks and returns the full result (v1 `exec` behaviour). |
| | `GET /v2/sessions` | List (filter by `state`, `label`). |
| | `GET /v2/sessions/{id}` | Metadata, state, exit info. |
| | `GET /v2/sessions/{id}/events` | Output and state events. `?from=<seq>&follow=true`. Format chosen by `Accept`. |
| | `POST /v2/sessions/{id}/stdin` | Write the request body (streamed) to stdin. `?close=true` closes stdin afterwards. |
| | `POST /v2/sessions/{id}/signal` | `{"signal":"SIGINT"}` |
| | `POST /v2/sessions/{id}/resize` | `{"cols":120,"rows":40}` (TTY sessions only; 409 otherwise). |
| | `POST /v2/sessions/{id}/kill` | SIGTERM, then SIGKILL after the grace period. The record and output are kept. |
| | `DELETE /v2/sessions/{id}` | Kill if running, then delete the record and its log. |
| | `GET /v2/sessions/{id}/attach` | WebSocket: attach to an existing session. |
| | `GET /v2/exec` | WebSocket: create and attach in one round trip. |
| Jobs | `POST /v2/jobs`, `GET /v2/jobs`, `GET /v2/jobs/{id}` | Multi-step jobs. |
| | `POST /v2/jobs/{id}/cancel` | Cancel; kills the running step. |
| | `GET /v2/jobs/{id}/events` | Job state changes (step started/finished), same streaming formats. |
| Queues | `GET /v2/queues`, `PUT /v2/queues/{name}` | Named queues with a concurrency limit. |
| Templates | `PUT/GET/DELETE /v2/templates/{name}`, `GET /v2/templates` | Saved specs with typed parameters. |
| | `POST /v2/templates/{name}/run` | Run with parameter values; returns the session or job. |
| Keys | `POST/GET /v2/keys`, `GET/PATCH/DELETE /v2/keys/{id}` | API key management (master key only). |
| | `POST /v2/keys/{id}/rotate` | Issue a new secret, optionally keeping the old one valid for a grace period. |
| | `GET /v2/whoami` | The calling key's id, name, scopes and policy. |
| Ops | `GET /healthz`, `GET /v2/version`, `GET /metrics` | Health and version (no auth), Prometheus metrics (`admin:read`). |

### Session spec

```jsonc
{
  "argv": ["python3", "-u", "script.py"],   // exactly one of argv / shell
  "shell": null,                            // e.g. "ls -la | grep foo" → run with sh -c (cmd /C on Windows)
  "env": {"FOO": "bar"},
  "inherit_env": true,                      // start from the server's environment (subject to policy)
  "cwd": "/srv/app",
  "stdin": "initial input\n",               // optional; or "stdin_b64"
  "stdin_close": false,                     // close stdin after the initial input
  "tty": null,                              // or {"cols":80,"rows":24} to run under a pseudo-terminal
  "merge_stderr": false,                    // send stderr through the stdout pipe to keep ordering
  "timeout": "10m",
  "retention": "24h",                       // how long the record and output are kept after exit
  "labels": {"team": "infra"}
}
```

`argv` replaces v1's `Command`/`Args`/`Shell` combination, which joined arguments with spaces and so could not pass an argument containing a space without shell quoting.

### Events

Every session event has a global, gap-free sequence number:

```jsonc
{"seq":1, "time":"2026-10-04T12:00:00.000Z", "type":"started", "pid":4242}
{"seq":2, "time":"…", "type":"stdout", "data":"hello\n"}
{"seq":3, "time":"…", "type":"stderr", "data_b64":"…"}       // data_b64 when not valid UTF-8
{"seq":4, "time":"…", "type":"stdin_closed"}
{"seq":5, "time":"…", "type":"exit", "exit_code":0, "signal":null, "duration_ms":812}
```

- Output is sent as **chunks**, not lines: programs print partial lines, progress bars, binary data and terminal escape codes. `?lines=true` makes the server re-chunk output into one event per complete line for clients that want lines. A UTF-8 character split across two reads is held back and sent whole.
- A client resumes with `?from=<last seq + 1>` (or `Last-Event-ID` for SSE).
- A subscriber that falls too far behind the in-memory buffer is served from the disk log transparently; it never misses events.

### Streaming formats for `GET …/events`

| `Accept` | Format | Typical client |
|---|---|---|
| `application/x-ndjson` (default) | One event JSON object per line | curl, scripts, any language |
| `text/event-stream` | Server-sent events, `id:` = seq | Browsers, SSE libraries, auto-reconnect |
| `application/octet-stream` | Raw stdout bytes only (`?stream=stderr` for stderr) | `curl … \| tail`, piping into other tools |

### `?wait=true` (request/response mode)

Blocks until the session ends (or `wait_timeout`), then returns metadata plus collected output. Output is capped (`max_output`, default 1 MiB per stream) with `stdout_truncated`/`stderr_truncated`, keeping the tail by default or the head with `keep=head`. Because nobody else knows the session id yet, stdin is closed after any initial `stdin`. If the client disconnects, the session keeps running. This is the simplest interface for scripts and for agents that call a tool and read the result.

### WebSocket protocol

Endpoints: `GET /v2/exec` starts a session; `GET /v2/sessions/{id}/attach?from=<seq>&readonly=<bool>` connects to an existing one. Clients offer the subprotocol `shhttp.v2.json`; all messages are JSON text frames.

Client → server:

```jsonc
{"type":"start", "spec":{…session spec…}, "on_disconnect":"kill"}   // first message on /v2/exec only
{"type":"stdin", "data":"print(1)\n"}                               // or "data_b64" for binary input
{"type":"stdin_close"}
{"type":"signal", "signal":"SIGINT"}
```

Server → client:

```jsonc
{"type":"session", "session":{…}}                    // always first: the session, including its id
{"seq":1, "type":"started", "pid":4242, …}             // then the session's events, exactly as in GET …/events
{"type":"protocol_error", "status":403, "error":"…"}  // a rejected message; status is what HTTP would answer
```

- The server closes the connection with status 1000 after the `exit` event (or at once, after replaying, for a finished session). A start that fails gets a `protocol_error` and close status 1008.
- `on_disconnect` (exec only) decides what happens when the connection closes: `keep` (default), `kill`, or a duration such as `"1m"` after which the session is killed unless another interactive client is attached by then.
- Several clients can attach to one session. All receive output; interactive ones (the session's own key with `sessions:run`) may send stdin and signals; `readonly=true` only watches and needs just `sessions:read`.
- **Closing stdin is an explicit message.** Closing the WebSocket is not the same as EOF on stdin.
- Bad messages (unknown type, unknown field, binary frame, stdin after close) get a `protocol_error`; the connection stays open.
- Flow control is WebSocket backpressure. Because output is on disk, a slow client cannot stall the process; it just falls behind and is served from the log. Stdin is written by a separate goroutine per connection, so a process that stops reading its input cannot block signals.
- Browsers cannot set headers on WebSocket requests, so a key may also be offered as the subprotocol `shhttp.v2.auth.<key>` alongside `shhttp.v2.json`; the server never echoes it. The `Origin` header must match the request host or one of `allowed_origins`.
- The server pings every 30 seconds and drops clients that do not answer.
- `{"type":"resize","cols":120,"rows":40}` resizes a TTY session.
- Later: a binary subprotocol (`shhttp.v2.binary`, one channel byte + payload) for high-throughput output without base64.

### Plain-HTTP equivalent of the WebSocket flow

For clients that cannot use WebSocket, the same session can be driven entirely over HTTP/1.1:

```sh
id=$(curl -s -H "$AUTH" --json '{"argv":["python3","-i"]}' $URL/v2/sessions | jq -r .id)
curl -sN -H "$AUTH" "$URL/v2/sessions/$id/events?follow=true" &   # stream output
curl -s  -H "$AUTH" --data-binary 'print(1+1)' "$URL/v2/sessions/$id/stdin"
curl -s  -H "$AUTH" -X POST "$URL/v2/sessions/$id/stdin?close=true"
```

## Authentication and key management

### Master key

- Supplied with `--master-key-file`, or the `SHHTTP_MASTER_KEY` env var.
- If neither is set and no master key exists yet, the server generates one on first start, writes it to `<data-dir>/master.key` with permissions `0600`, and logs the file path (never the key).
- The master key **can only manage API keys** (`/v2/keys*`, `/v2/whoami`). It cannot run commands. This keeps the most powerful secret out of day-to-day use: an operator mints a scoped key for each client with a single call.
- Rotation: restart with a new value. Rotating the master key does not invalidate API keys.

### API keys

- Format: `shh_<key id>_<secret>`. The fixed prefix makes leaked keys easy for secret scanners to detect. The secret is 32 random bytes, base64url-encoded.
- The full key is returned **once**, at creation or rotation. The server stores only the SHA-256 of the secret (the secret is random and high-entropy, so a slow hash is not needed) and compares in constant time.
- Sent as `Authorization: Bearer <key>`. Browsers cannot set headers on WebSocket connections, so the key is also accepted as a WebSocket subprotocol `shhttp.v2.auth.<key>` (the approach Kubernetes uses). Keys are never accepted in URLs, where they end up in logs.
- Stored fields: `id, name, scopes, policy, created_at, expires_at, last_used_at, revoked_at, previous_secret_valid_until` (for rotation grace periods).

Create:

```jsonc
POST /v2/keys            (Authorization: Bearer <master key>)
{
  "name": "ci-runner",
  "scopes": ["sessions:run", "sessions:read", "jobs:run"],
  "expires_in": "720h",
  "policy": {
    "allow_shell": false,
    "commands": ["^/usr/bin/(git|make)$"],
    "cwd_roots": ["/srv/build"],
    "env_allow": ["^CI_"],
    "tty": false,
    "max_concurrent_sessions": 4,
    "max_timeout": "30m"
  }
}
→ 201 {"id":"key_…", "key":"shh_key…_…", …}
```

### Scopes

| Scope | Allows |
|---|---|
| `sessions:run` | Create sessions, write stdin, send signals, resize |
| `sessions:read` | List and read sessions and their events |
| `jobs:run`, `jobs:read` | Submit, cancel and delete jobs / read jobs, their events and queues |
| `queues:write` | Create, resize and delete queues |
| `templates:run` | Run templates (and list them). A key with only this scope can start sessions and jobs only through templates; policy `templates` narrows which |
| `templates:write`, `templates:read` | Create and delete templates / read them |
| `admin:read` | See all keys' sessions and jobs, read metrics |

By default a key sees only the sessions and jobs it created. `admin:read` lifts that for reading; no key can write to another key's sessions.

### Policies

A policy is enforced when a session starts, after template parameters are filled in:

- `commands` matches against the **resolved absolute path** of `argv[0]` (so `PATH` tricks don't bypass it).
- `allow_shell: false` is needed for `commands` to mean anything: with shell access a client can run any program, so the docs and `whoami` say so explicitly.
- `cwd_roots`, `env_allow`, `allow_tty`, `max_timeout`, `max_concurrent_sessions`.
- `max_output_bytes`: a session whose stdout and stderr together exceed it is killed (state `killed`, `error` says why, and the exit event carries the error). The server-wide `max_output_bytes` applies too; the lower limit wins.
- `run_as` (`user`, `user:group` or numeric ids) runs the key's sessions as that OS user, with `HOME`, `USER` and `LOGNAME` set for it. The server must run as root; the server-wide `run_as` is the default for keys without one.
- `templates` (list of names) for keys with only `templates:run`.

### Other protections

- Default listen address `127.0.0.1:2112`. Listening on another address prints a warning unless TLS is configured (`--tls-cert/--tls-key`, optional client-certificate auth).
- WebSocket `Origin` header checked against `--allowed-origins` (empty by default, so browsers on other sites cannot open connections to the server: cross-site WebSocket hijacking).
- Failed-auth rate limiting per client IP: after `auth_failure_limit` (default 20) failures within a minute, the IP gets 429 for the rest of that minute, even with a valid key.
- Client certificates: with `client_ca`, the TLS server requires and verifies a client certificate signed by that CA, in addition to the bearer key.
- Audit log (`slog`, JSON) for key management, session start (key, argv, cwd), signals and exits. Stdin and output are never logged.

## Jobs, queues and templates

### Jobs

```jsonc
POST /v2/jobs
{
  "name": "deploy",
  "queue": "default",               // optional; omitted = start immediately
  "steps": [
    {"name": "fetch", "spec": {"argv": ["git", "pull"]}},
    {"name": "build", "spec": {"argv": ["make"]}, "depends_on": ["fetch"]},
    {"name": "lint",  "spec": {"argv": ["make", "lint"]}, "depends_on": ["fetch"], "allow_failure": true},
    {"name": "ship",  "spec": {"argv": ["./ship.sh"]}, "depends_on": ["build", "lint"]}
  ],
  "env": {"STAGE": "prod"},          // merged into every step
  "on_restart": "fail"               // fail | resume (re-run only the interrupted step and what follows)
}
```

- Without `depends_on`, steps run in order (v1 behaviour). With it, independent steps run in parallel (v1 roadmap item). Unknown dependencies and cycles are rejected at submission.
- Every step is checked against the submitting key's policy at submission, and runs with the key's policy as it is when the step starts. A key revoked or expired meanwhile makes its next step fail.
- When a step fails (and is not `allow_failure`), no further steps start; steps already running finish; the rest are `skipped` and the job is `failed`.
- Each step runs as a session labelled `shhttp.job=<id>` and `shhttp.step=<name>`. Its session ID is in the job record, so its output (and stdin) go through the normal session endpoints.
- Job states: `queued → running → succeeded | failed | cancelled | lost`. Step states: `pending, running, succeeded, failed, skipped, cancelled`.
- `GET /v2/jobs/{id}/events` streams `job_started`, `step_started`, `step_finished` and `job_finished` events in the same formats as session events.
- Cancelling kills the running steps (marked `cancelled`). Deleting a job also deletes its step sessions.
- On a clean shutdown jobs stop starting steps before sessions are killed, so the interrupted steps are left for `on_restart`: `fail` marks the job `lost`, `resume` re-runs the interrupted steps and continues.

### Queues

Named, persistent, with `concurrency` (default `1`, matching v1's serial queue). Jobs in a queue start in submission order. `default` always exists and cannot be deleted; other queues can be deleted when empty. `GET /v2/queues` shows each queue's running and queued counts.

### Templates

```jsonc
PUT /v2/templates/deploy
{
  "params": {
    "branch": {"type": "string", "pattern": "^[a-zA-Z0-9._/-]+$", "default": "main"},
    "dry_run": {"type": "bool", "default": true}
  },
  "job": { "steps": [ {"spec": {"argv": ["./deploy.sh", "--branch", "{{branch}}"]}} ] }
}
POST /v2/templates/deploy/run  {"params": {"branch": "release/2.0"}}
```

- `{{name}}` placeholders are replaced inside `argv` elements (for example `"--branch={{branch}}"`), `env` values, `cwd` and the job's `env`. They are **never substituted into `shell` strings**, and a template that tries is rejected; every rendered session gets each parameter as an environment variable (`$SHHTTP_PARAM_BRANCH`) instead, which removes shell injection through parameters. Placeholders that name no declared parameter are rejected when the template is saved.
- Parameters are validated (type, pattern, enum) before anything runs.
- A template holds either a single session spec or a job. Running it returns `{"session": …}` or `{"job": …}`; sessions and jobs started from a template carry the label `shhttp.template=<name>`.
- Templates are shared by all keys. Each run happens under the calling key and its policy.

## Storage

- **SQLite** (`modernc.org/sqlite`, pure Go so cross-compiling stays easy) in `<data-dir>/shhttp.db` for keys, sessions, jobs, queues and templates. Versioned migrations run at startup. WAL mode.
- **Event logs**: one append-only file per session, `<data-dir>/sessions/<id>.log`, length-prefixed records plus a sparse seq→offset index for fast `?from=` lookups.
- A retention sweeper deletes expired session records and logs, and finished jobs past their retention.
- All files are created `0600`, directories `0700`.

## Process execution

- Process group per session (`Setpgid`), so signals and kills reach the whole tree. `Pdeathsig: SIGKILL` on Linux.
- TTY sessions use `github.com/creack/pty`. TTY output is a single stream (`stdout`).
- Exit info: `exit_code`, or `signal` when killed by one; `duration_ms`.
- Timeouts send `SIGTERM`, then `SIGKILL` after a grace period (`--kill-grace`, default 10s). Server shutdown does the same for all sessions and marks unfinished jobs according to `on_restart`.
- PATH lookup for `argv[0]` uses the server's `PATH`, not one set in the session's `env`. The resolved absolute path is what policies match and what runs.
- When the session's process exits, anything left in its process group is killed, and the server waits at most 2 seconds for leftover processes holding stdout or stderr open.
- Platforms: Linux and macOS fully supported. Windows: no TTY in v2.0, Job Objects instead of process groups, `cmd /C` for `shell`.

## Server configuration

Flags, each with an env var equivalent (`SHHTTP_<FLAG>`), plus an optional `--config` file (YAML). Main options: `--listen`, `--data-dir`, `--master-key-file`, `--tls-cert`, `--tls-key`, `--client-ca`, `--allowed-origins`, `--max-sessions`, `--default-retention`, `--kill-grace`, `--log-format`, `--log-level`.

## Clients

- **Go client library** (`pkg/client`): every HTTP endpoint, events as a Go iterator (`iter.Seq2`), stdin from any `io.Reader`, and WebSocket `Exec`/`Attach` connections. Server errors are `*api.Problem` values, WebSocket rejections `*client.ProtocolError`.
- **CLI** (`cmd/shhttp`), built on the library, using `SHHTTP_URL`, `SHHTTP_KEY` and, for key commands, `SHHTTP_MASTER_KEY`:
  - `shhttp run [flags] program args…` or `shhttp run -c 'command line'` runs over a WebSocket, streams local stdin (closing the remote stdin at local EOF; `-n` sends none), forwards Ctrl-C, SIGTERM and SIGHUP, and exits with the remote exit code (128 + signal number when a signal ended it). `-d` starts it in the background and prints the id. The session is killed if the client disconnects, unless `-on-disconnect` says otherwise.
  - `shhttp attach [-from N] [-readonly] <id>`, `logs [-f] <id>`, `ps [-a]`, `get`, `kill`, `signal <id> INT`, `rm`
  - `shhttp key create|ls|get|rotate|revoke`, `whoami`, `version`
  - Later: `shhttp job …` and `shhttp template …` with phase 3; `-t` for TTY sessions with phase 4.
- **API description**: the OpenAPI 3.1 document generated by huma (served at `/v2/openapi.json`, printed by `shhttpd openapi`, committed as `docs/openapi.yaml`) and a written spec for the WebSocket protocol, so clients in other languages can be generated or written quickly.
- **Agents**: `?wait=true` with output caps covers simple tool calls. An MCP server mode (`shhttp mcp`) that exposes run/stdin/read/kill as tools is planned after v2.0.

## Code layout

```
cmd/shhttpd/          server entry point
cmd/shhttp/           CLI client
internal/server/      huma operations, auth middleware, error mapping, streaming
internal/cli/         the CLI's commands (cmd/shhttp is a thin wrapper)
internal/testserver/  an in-process server for tests
internal/auth/        master key, API keys, scopes, policy enforcement
internal/session/     session manager, process runner (pipes + pty), lifecycle
internal/eventlog/    append-only log, index, in-memory tail, subscribers
internal/job/         job graph runner, queues
internal/template/    parameter validation and substitution
internal/store/       SQLite access and migrations
pkg/api/              public request/response/event types (shared by server, SDK, CLI)
pkg/client/           Go SDK
docs/                 this design, protocol spec, openapi.yaml (generated)
```

Dependencies kept small: `github.com/danielgtaylor/huma/v2`, `github.com/coder/websocket`, `modernc.org/sqlite`, `github.com/creack/pty`, `github.com/prometheus/client_golang`. Standard library for the router underneath huma (`net/http` patterns), logging (`log/slog`) and flags. Go 1.26+.

## Testing and release

- Unit tests with `-race`, all using `t.TempDir()`.
- A test fails when `docs/openapi.yaml` no longer matches the generated document.
- Integration tests that start the server in-process (`httptest`) and drive real sessions over HTTP and WebSocket: stdin round trips, stdin EOF, reconnect with `from`, slow subscribers, kill/timeout, policy denials, key rotation.
- `golangci-lint`, GitHub Actions on Linux and macOS, goreleaser for multi-arch binaries, distroless non-root container image.

## Implementation phases

1. **Core**: store and migrations, auth (master key, keys API, scopes), session engine (runner, event log, subscribers), session HTTP endpoints including NDJSON/SSE streaming, stdin and `wait=true`, problem errors, tests.
2. **Interactive**: WebSocket `exec`/`attach`, Go SDK, CLI (`run`, `attach`, `logs`, `ps`, `kill`, `key`).
3. **Jobs**: job graph runner, queues, templates, restart behaviour.
4. **Hardening**: TTY, policies, limits, audit log, metrics, retention sweeper, TLS and client certificates, rate limiting.
5. **Ship**: protocol spec, README rewrite, Dockerfile, CI and releases. Then the MCP mode.

## Known limitations

- **Without `run_as`, sessions run as the server's OS user.** A key that may run arbitrary programs can then read everything that user can: the data directory (including `master.key` and other keys' session logs) and, through `/proc`, the server's original environment. Policies limit which programs run, not which files they read. Run the server as root with `run_as` set to an unprivileged user (server-wide or per key), or treat any key without a strict `commands` policy and `allow_shell: false` as equivalent to the master key. The server removes `SHHTTP_MASTER_KEY` from the environment sessions inherit.
- **Processes can outlive a server crash until it restarts.** After `kill -9` or a crash, Linux kills each session's own process (`Pdeathsig`), and on restart the server kills every remaining process whose environment carries the `SHHTTP_SESSION_ID` of a lost session. A process that clears its environment, or runs as a user the server cannot signal, escapes this. On macOS only the restart cleanup is missing; a cgroup-based approach could replace both on Linux later.
- TTY sessions on Windows are not supported (no ConPTY yet); nor are `run_as` and process-tree signals there.
- A stdin write that the process never reads blocks other stdin writes to the same session until the process reads, exits or is killed.

## Implementation status

Phases 1 to 4 are implemented.

Phase 1: config (YAML, env, flags), SQLite store, master key and the keys API (create, list, get, update, rotate with grace, revoke with optional `kill_sessions`), scopes and key policies (enforced already, ahead of phase 4: `allow_shell`, `commands`, `cwd_roots`, `env_allow`, `max_timeout`, `max_concurrent_sessions`), the session engine, all session HTTP endpoints with NDJSON, SSE and raw streaming, `wait=true`, the retention sweeper, an audit log and graceful shutdown. The HTTP API runs on huma and publishes its OpenAPI document.

Phase 4: TTY sessions (pseudo-terminal, resize over HTTP and WebSocket, `shhttp run -t` in raw mode), policies `allow_tty`, `run_as` and `max_output_bytes` with server-wide defaults, orphan cleanup after a crash, Prometheus metrics at `/metrics` (scope `admin:read`), client certificates, failed-auth rate limiting, and `?lines=true`.

Phase 3: jobs (sequential or dependency graph, `allow_failure`, cancel, `on_restart`), job events, queues, templates with typed parameters, and their client and CLI commands.

Phase 2: the WebSocket `exec` and `attach` endpoints with `on_disconnect`, browser authentication through the subprotocol and origin checks; the Go client library; the CLI.

Not yet implemented from this document: the MCP mode (phase 5) and a binary WebSocket subprotocol.

## Decisions

1. The master key cannot run commands. It manages API keys only.
2. Revoking a key leaves its running sessions running; only `admin:read` keys can still see them. `DELETE /v2/keys/{id}?kill_sessions=true` revokes and kills them in one call.
3. The config file is YAML.
