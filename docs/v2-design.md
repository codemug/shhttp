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

`lost` means the server stopped while the process was running. Child processes are tied to the server (`Pdeathsig` on Linux, process-group kill on shutdown), so a session cannot keep running unobserved after a restart.

## API overview

Base path `/v2`. JSON everywhere except the streaming formats listed below. Errors use RFC 9457 `application/problem+json`. IDs are prefixed and time-sortable: `ses_…`, `job_…`, `tpl_…`, `key_…`. Lists use cursor pagination (`?limit=&cursor=`).

| Area | Endpoint | Purpose |
|---|---|---|
| Sessions | `POST /v2/sessions` | Create a session. `?wait=true` blocks and returns the full result (v1 `exec` behaviour). |
| | `GET /v2/sessions` | List (filter by `state`, `label`). |
| | `GET /v2/sessions/{id}` | Metadata, state, exit info. |
| | `GET /v2/sessions/{id}/events` | Output and state events. `?from=<seq>&follow=true`. Format chosen by `Accept`. |
| | `POST /v2/sessions/{id}/stdin` | Write the request body (streamed) to stdin. `?close=true` closes stdin afterwards. |
| | `POST /v2/sessions/{id}/signal` | `{"signal":"SIGINT"}` |
| | `POST /v2/sessions/{id}/resize` | `{"cols":120,"rows":40}` (TTY sessions only). |
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
| Ops | `GET /healthz`, `GET /v2/version`, `GET /metrics` | Health (no auth), version, Prometheus metrics. |

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
  "on_disconnect": "keep",                  // keep | kill | kill_after:<duration> (applies to /v2/exec)
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

- Output is sent as **chunks**, not lines: programs print partial lines, progress bars, binary data and terminal escape codes. `?lines=true` makes the server buffer output into complete lines for clients that want them.
- A client resumes with `?from=<last seq + 1>` (or `Last-Event-ID` for SSE).
- A subscriber that falls too far behind the in-memory buffer is served from the disk log transparently; it never misses events.

### Streaming formats for `GET …/events`

| `Accept` | Format | Typical client |
|---|---|---|
| `application/x-ndjson` (default) | One event JSON object per line | curl, scripts, any language |
| `text/event-stream` | Server-sent events, `id:` = seq | Browsers, SSE libraries, auto-reconnect |
| `application/octet-stream` | Raw stdout bytes only (`?stream=stderr` for stderr) | `curl … \| tail`, piping into other tools |

### `?wait=true` (request/response mode)

Blocks until the session ends (or `wait_timeout`), then returns metadata plus collected output. Output is capped (`max_output`, default 1 MiB per stream) with `truncated: true` and the option to keep the head, the tail or both. This is the simplest interface for scripts and for agents that call a tool and read the result.

### WebSocket protocol

Subprotocol `shhttp.v2.json`. Text frames carrying JSON messages.

Client → server:

```jsonc
{"type":"start", "spec":{…session spec…}}        // first message on /v2/exec only
{"type":"stdin", "data":"print(1)\n"}            // or "data_b64"
{"type":"stdin_close"}
{"type":"signal", "signal":"SIGINT"}
{"type":"resize", "cols":120, "rows":40}
{"type":"ack", "seq":42}                          // optional flow control, see below
```

Server → client: the same event objects as above, plus `{"type":"error", …}` for protocol errors.

- `GET /v2/sessions/{id}/attach?from=<seq>` replays from `seq` then follows.
- Several clients can attach to one session. All receive output; all may write stdin (a `readonly=true` attach is available).
- **Closing stdin is an explicit message.** Closing the WebSocket is not the same as EOF on stdin; the session's `on_disconnect` decides what happens to the process.
- Flow control: the server keeps at most a window of unacknowledged events in flight per connection; clients that never send `ack` get the default WebSocket backpressure only. Because output is on disk, a slow client cannot stall the process.
- A binary subprotocol (`shhttp.v2.binary`, one channel byte + payload) can be added later for high-throughput output without base64.

### Plain-HTTP equivalent of the WebSocket flow

For clients that cannot use WebSocket, the same session can be driven entirely over HTTP/1.1:

```sh
id=$(curl -s -H "$AUTH" -d '{"argv":["python3","-i"]}' $URL/v2/sessions | jq -r .id)
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
| `jobs:run`, `jobs:read` | Same split for jobs and queues |
| `templates:run` | Run templates only (a key can be limited to "run these approved templates") |
| `templates:write`, `templates:read` | Manage templates |
| `admin:read` | See all keys' sessions and jobs, read metrics |

By default a key sees only the sessions and jobs it created. `admin:read` lifts that for reading; no key can write to another key's sessions.

### Policies

A policy is enforced when a session starts, after template parameters are filled in:

- `commands` matches against the **resolved absolute path** of `argv[0]` (so `PATH` tricks don't bypass it).
- `allow_shell: false` is needed for `commands` to mean anything: with shell access a client can run any program, so the docs and `whoami` say so explicitly.
- `cwd_roots`, `env_allow`, `tty`, `max_timeout`, `max_concurrent_sessions`, `max_output_bytes`.
- Optional `run_as` (user/group) when the server runs as root.
- `templates` (list of names) for keys with only `templates:run`.

### Other protections

- Default listen address `127.0.0.1:2112`. Listening on another address prints a warning unless TLS is configured (`--tls-cert/--tls-key`, optional client-certificate auth).
- WebSocket `Origin` header checked against `--allowed-origins` (empty by default, so browsers on other sites cannot open connections to the server: cross-site WebSocket hijacking).
- Failed-auth rate limiting per client IP.
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

- Without `depends_on`, steps run in order (v1 behaviour). With it, independent steps run in parallel (v1 roadmap item).
- Each step's session ID is in the job record, so its output is read through the normal session endpoints.
- Job states: `queued → running → succeeded | failed | cancelled | lost`. Step states record which ones were skipped because a dependency failed.
- Stdin can be sent to a running step via its session.

### Queues

Named, persistent, with `concurrency` (default `1`, matching v1's serial queue). Jobs in a queue start in submission order. `default` always exists.

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

- Placeholders may appear only in whole `argv` elements, `env` values and `cwd`. They are **never substituted into `shell` strings**; shell templates receive parameters as environment variables (`$SHHTTP_PARAM_BRANCH`) instead, which removes shell injection through parameters.
- Parameters are validated (type, pattern, enum) before anything runs.
- A template holds either a single session spec or a job.

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
- Platforms: Linux and macOS fully supported. Windows: no TTY in v2.0, Job Objects instead of process groups, `cmd /C` for `shell`.

## Server configuration

Flags, each with an env var equivalent (`SHHTTP_<FLAG>`), plus an optional `--config` file (YAML). Main options: `--listen`, `--data-dir`, `--master-key-file`, `--tls-cert`, `--tls-key`, `--client-ca`, `--allowed-origins`, `--max-sessions`, `--default-retention`, `--kill-grace`, `--log-format`, `--log-level`.

## Clients

- **Go SDK** (`pkg/client`): sessions, events (as a Go iterator), stdin writer, jobs, templates, keys.
- **CLI** (`cmd/shhttp`), built on the SDK, using `SHHTTP_URL` and `SHHTTP_KEY`:
  - `shhttp run [-t] -- cmd args…` connects local stdin/stdout/stderr and the terminal to a remote session; exits with the remote exit code.
  - `shhttp attach <id>`, `logs [-f] <id>`, `ps`, `kill <id>`, `signal <id> INT`
  - `shhttp job submit|ls|get|cancel`, `shhttp template put|ls|run`
  - `shhttp key create|ls|rotate|revoke` (with the master key)
- **API description**: OpenAPI 3.1 document for the HTTP endpoints (served at `/v2/openapi.json`) and a written spec for the WebSocket protocol, so clients in other languages can be generated or written quickly.
- **Agents**: `?wait=true` with output caps covers simple tool calls. An MCP server mode (`shhttp mcp`) that exposes run/stdin/read/kill as tools is planned after v2.0.

## Code layout

```
cmd/shhttpd/          server entry point
cmd/shhttp/           CLI client
internal/server/      HTTP routing, middleware (auth, logging, recovery), problem errors
internal/server/ws/   WebSocket protocol
internal/auth/        master key, API keys, scopes, policy enforcement
internal/session/     session manager, process runner (pipes + pty), lifecycle
internal/eventlog/    append-only log, index, in-memory tail, subscribers
internal/job/         job graph runner, queues
internal/template/    parameter validation and substitution
internal/store/       SQLite access and migrations
pkg/api/              public request/response/event types (shared by server, SDK, CLI)
pkg/client/           Go SDK
docs/                 this design, protocol spec, OpenAPI
```

Dependencies kept small: `github.com/coder/websocket`, `modernc.org/sqlite`, `github.com/creack/pty`, `github.com/prometheus/client_golang`. Standard library for routing (`net/http` patterns), logging (`log/slog`) and flags. Go 1.23+.

## Testing and release

- Unit tests with `-race`, all using `t.TempDir()`.
- Integration tests that start the server in-process (`httptest`) and drive real sessions over HTTP and WebSocket: stdin round trips, stdin EOF, reconnect with `from`, slow subscribers, kill/timeout, policy denials, key rotation.
- `golangci-lint`, GitHub Actions on Linux and macOS, goreleaser for multi-arch binaries, distroless non-root container image.

## Implementation phases

1. **Core**: store and migrations, auth (master key, keys API, scopes), session engine (runner, event log, subscribers), session HTTP endpoints including NDJSON/SSE streaming, stdin and `wait=true`, problem errors, tests.
2. **Interactive**: WebSocket `exec`/`attach`, Go SDK, CLI (`run`, `attach`, `logs`, `ps`, `kill`, `key`).
3. **Jobs**: job graph runner, queues, templates, restart behaviour.
4. **Hardening**: TTY, policies, limits, audit log, metrics, retention sweeper, TLS and client certificates, rate limiting.
5. **Ship**: OpenAPI document, protocol spec, README rewrite, Dockerfile, CI and releases. Then the MCP mode.

## Open questions

1. Should the master key be allowed to run commands as a convenience for single-user setups? (Current answer: no.)
2. Should sessions owned by a revoked key keep running? (Proposed: yes, but nobody but `admin:read` can see them; add `?kill_sessions=true` to revoke.)
3. Config file format: YAML or TOML?
