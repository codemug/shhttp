# shhttp

shhttp runs commands on the machine it is installed on and streams their output to any HTTP client. Clients can send stdin while the command runs, send signals, disconnect, and come back later to replay the output from any point.

> **Status:** v2 is being rebuilt from scratch on the `v2` branch. The core (sessions, streaming, stdin, API keys) works today; WebSocket, jobs, templates, a CLI and terminal support are next. See [docs/v2-design.md](docs/v2-design.md) for the full design and its implementation status. The v1 code in `pkg/` and `cmd/shhttp/` is obsolete and will be removed.

## Concepts

- A **session** is one process plus an ordered log of everything that happens to it: start, output chunks, stdin closed, signals, exit. Every event has a sequence number.
- Output is saved to disk as it is produced, so a process never waits for a slow or absent client, and any client can read the log from any sequence number, live or after the fact.
- The **master key** only manages **API keys**. API keys have **scopes** (what endpoints they may use) and an optional **policy** (which commands, working directories and environment variables they may use, and how many sessions at once).

## Quick start

Requires Go 1.26 or newer.

```sh
git clone -b v2 https://github.com/codemug/shhttp && cd shhttp
go build -o shhttpd ./cmd/shhttpd
./shhttpd                       # listens on 127.0.0.1:2112, stores data in ./shhttp-data
```

On first start the server generates a master key and writes it to `shhttp-data/master.key` (readable only by you). Use it to create an API key:

```sh
URL=http://127.0.0.1:2112
MASTER=$(cat shhttp-data/master.key)

curl -s -H "Authorization: Bearer $MASTER" $URL/v2/keys \
  -d '{"name": "me", "scopes": ["sessions:run", "sessions:read"]}'
# → {"id": "key_…", …, "key": "shh_…"}   the key is shown only once

KEY="shh_…"
AUTH="Authorization: Bearer $KEY"
```

### Run a command and wait for the result

```sh
curl -s -H "$AUTH" "$URL/v2/sessions?wait=true" -d '{"argv": ["uname", "-a"]}'
```

```json
{"session": {"id": "ses_…", "state": "exited", "exit_code": 0, …}, "stdout": "Linux …\n", "stderr": ""}
```

Use `"shell": "ls -l | wc -l"` instead of `argv` to run a command line with `sh -c`.

### Stream output while it runs

```sh
ID=$(curl -s -H "$AUTH" $URL/v2/sessions -d '{"shell": "for i in 1 2 3; do echo $i; sleep 1; done"}' | jq -r .id)

curl -sN -H "$AUTH" "$URL/v2/sessions/$ID/events?follow=true"              # one JSON event per line
curl -sN -H "$AUTH" "$URL/v2/sessions/$ID/events?follow=true&format=raw"   # just the stdout bytes
curl -sN -H "$AUTH" "$URL/v2/sessions/$ID/events?follow=true&format=sse"   # server-sent events
```

Add `from=<seq>` to resume after a disconnect. SSE clients resume automatically with `Last-Event-ID`.

### Send stdin to a running process

```sh
ID=$(curl -s -H "$AUTH" $URL/v2/sessions -d '{"argv": ["python3", "-u", "-i"], "merge_stderr": true}' | jq -r .id)
curl -sN -H "$AUTH" "$URL/v2/sessions/$ID/events?follow=true&format=raw" &

curl -s -H "$AUTH" "$URL/v2/sessions/$ID/stdin" --data-binary $'print(6 * 7)\n'
curl -s -H "$AUTH" "$URL/v2/sessions/$ID/stdin?close=true" --data-binary $'print("bye")\n'
```

`close=true` closes stdin after writing, which is how a program reading its input learns it has ended.

## API

All endpoints except `/healthz` and `/v2/version` need `Authorization: Bearer <key>`. Errors are [RFC 9457](https://www.rfc-editor.org/rfc/rfc9457) `application/problem+json`.

| Endpoint | Scope | Purpose |
|---|---|---|
| `POST /v2/sessions` | `sessions:run` | Start a session. `?wait=true` waits and returns the output (`wait_timeout`, `max_output`, `keep=head\|tail`). |
| `GET /v2/sessions` | `sessions:read` | List your sessions, newest first (`state`, `label=k=v`, `limit`, `cursor`). |
| `GET /v2/sessions/{id}` | `sessions:read` | Session state and exit information. |
| `GET /v2/sessions/{id}/events` | `sessions:read` | Events (`from`, `follow`, `format=ndjson\|sse\|raw`, `stream=stdout\|stderr` for raw). |
| `POST /v2/sessions/{id}/stdin` | `sessions:run` | Write the request body to stdin (`close=true` to close it afterwards). |
| `POST /v2/sessions/{id}/signal` | `sessions:run` | `{"signal": "SIGINT"}`, sent to the session's process group. |
| `POST /v2/sessions/{id}/kill` | `sessions:run` | SIGTERM, then SIGKILL after the grace period. |
| `DELETE /v2/sessions/{id}` | `sessions:run` | Kill if needed, then delete the session and its output. |
| `GET /v2/whoami` | any key | The calling key, its scopes and policy. |
| `POST /v2/keys`, `GET /v2/keys`, `GET/PATCH/DELETE /v2/keys/{id}`, `POST /v2/keys/{id}/rotate` | master key | Manage API keys. `DELETE` revokes; add `?kill_sessions=true` to also kill the key's running sessions. `rotate` accepts `{"grace": "1h"}` to keep the old secret working for a while. |

A key sees only its own sessions. The `admin:read` scope allows reading every key's sessions, but never changing them.

### Session spec

```jsonc
{
  "argv": ["python3", "script.py"],   // or "shell": "a | b"
  "env": {"FOO": "bar"},
  "inherit_env": true,                // start from the server's environment (default)
  "cwd": "/srv/app",
  "stdin": "initial input",           // or "stdin_b64" for binary input
  "stdin_close": false,               // close stdin after the initial input
  "merge_stderr": false,              // one stream, original ordering preserved
  "timeout": "10m",
  "retention": "24h",                 // keep the record and output this long after exit
  "labels": {"team": "infra"}
}
```

Output events carry `data` when the bytes are valid UTF-8 and `data_b64` otherwise.

### Key policies

```jsonc
POST /v2/keys
{
  "name": "ci",
  "scopes": ["sessions:run", "sessions:read"],
  "expires_in": "720h",
  "policy": {
    "allow_shell": false,                   // required for "commands" to mean anything
    "commands": ["/usr/bin/(git|make)"],    // matched against the absolute program path
    "cwd_roots": ["/srv/build"],            // the first is the default working directory
    "env_allow": ["CI_.*"],
    "max_timeout": "30m",
    "max_concurrent_sessions": 4
  }
}
```

## Security

shhttp exists to run commands, so treat access to it like shell access.

- Keep the default loopback address, or use TLS when listening on a network.
- **Sessions run as the same OS user as the server.** A key that can run any program can read the server's data directory, including the master key file and other keys' output. Until per-session users (`run_as`) arrive, give untrusted clients only keys with `"allow_shell": false` and a strict `commands` list, and run the server as a dedicated, unprivileged user.
- Prefer `SHHTTP_MASTER_KEY` or a `master_key_file` outside the data directory, so the key is not stored next to the data.

## Configuration

Settings come from defaults, then an optional YAML file (`--config` or `SHHTTP_CONFIG`), then environment variables, then flags. Every flag has an environment variable: `--data-dir` is `SHHTTP_DATA_DIR`, and so on.

```yaml
listen: 127.0.0.1:2112
data_dir: /var/lib/shhttp
master_key_file: /etc/shhttp/master.key   # default <data_dir>/master.key
tls_cert: /etc/shhttp/tls.crt
tls_key: /etc/shhttp/tls.key
default_retention: 24h
kill_grace: 10s
max_sessions: 100                          # 0 = unlimited
sweep_interval: 1m
log_format: json                           # text or json
log_level: info
```

The master key can also be given with `SHHTTP_MASTER_KEY`. `shhttpd keygen` prints a new one. The server warns when it listens on a non-loopback address without TLS.

## Docker

```sh
docker build -t shhttp .
docker run -p 2112:2112 -v shhttp-data:/data shhttp
docker run --rm -v shhttp-data:/data alpine cat /data/master.key
```

## Development

```sh
go test -race ./internal/... ./pkg/api/...
```

## License

Apache 2.0, see [LICENSE](LICENSE).
