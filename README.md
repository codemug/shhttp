# shhttp

shhttp runs commands on the machine it is installed on and streams their output to any HTTP client. Clients can send stdin while the command runs, send signals, disconnect, and come back later to replay the output from any point.

> **Status:** v2 is being rebuilt from scratch on the `v2` branch. Sessions, streaming over HTTP and WebSocket, stdin, API keys, a CLI and a Go client library work today; jobs, templates and terminal (TTY) support are next. See [docs/v2-design.md](docs/v2-design.md) for the full design and its implementation status. v1 has been removed; its last version is on the `master` branch.

## Concepts

- A **session** is one process plus an ordered log of everything that happens to it: start, output chunks, stdin closed, signals, exit. Every event has a sequence number.
- Output is saved to disk as it is produced, so a process never waits for a slow or absent client, and any client can read the log from any sequence number, live or after the fact.
- The **master key** only manages **API keys**. API keys have **scopes** (what endpoints they may use) and an optional **policy** (which commands, working directories and environment variables they may use, and how many sessions at once).

## Quick start

Requires Go 1.26 or newer.

```sh
git clone -b v2 https://github.com/codemug/shhttp && cd shhttp
go build -o shhttpd ./cmd/shhttpd     # the server
go build -o shhttp ./cmd/shhttp       # the command-line client
./shhttpd                             # listens on 127.0.0.1:2112, stores data in ./shhttp-data
```

On first start the server generates a master key and writes it to `shhttp-data/master.key` (readable only by you). The master key only creates and manages API keys; commands run with an API key.

### With the CLI

```sh
export SHHTTP_URL=http://127.0.0.1:2112
export SHHTTP_MASTER_KEY=$(cat shhttp-data/master.key)
export SHHTTP_KEY=$(./shhttp key create -name me -scope sessions:run,sessions:read -q)

./shhttp run uname -a
printf 'c\na\nb\n' | ./shhttp run sort      # local stdin streams to the remote process
./shhttp run python3 -i                      # interactive; Ctrl-C goes to python
./shhttp run -c 'make test 2>&1 | tail'      # a command line, run with sh -c
id=$(./shhttp run -d ./long-job.sh)          # start in the background, print the session id
./shhttp logs -f "$id"                       # follow its output
./shhttp attach "$id"                        # connect stdin and output to it
./shhttp ps -a                               # list sessions
```

`shhttp run` exits with the remote exit code, or 128 plus the signal number when a signal ended the process, so it works in scripts. The remote process is killed if the client disconnects (`-on-disconnect keep` or a grace period such as `1m` changes that). Ctrl-C, SIGTERM and SIGHUP are forwarded to the remote process; Ctrl-\ quits the client. Run `shhttp -h` for every command.

### With curl

Create an API key with the master key:

```sh
URL=http://127.0.0.1:2112
MASTER=$(cat shhttp-data/master.key)

curl -s -H "Authorization: Bearer $MASTER" $URL/v2/keys \
  --json '{"name": "me", "scopes": ["sessions:run", "sessions:read"]}'
# → {"id": "key_…", …, "key": "shh_…"}   the key is shown only once

KEY="shh_…"
AUTH="Authorization: Bearer $KEY"
```

#### Run a command and wait for the result

```sh
curl -s -H "$AUTH" "$URL/v2/sessions?wait=true" --json '{"argv": ["uname", "-a"]}'
```

```json
{"session": {"id": "ses_…", "state": "exited", "exit_code": 0, …}, "stdout": "Linux …\n", "stderr": ""}
```

Use `"shell": "ls -l | wc -l"` instead of `argv` to run a command line with `sh -c`.

#### Stream output while it runs

```sh
ID=$(curl -s -H "$AUTH" $URL/v2/sessions --json '{"shell": "for i in 1 2 3; do echo $i; sleep 1; done"}' | jq -r .id)

curl -sN -H "$AUTH" "$URL/v2/sessions/$ID/events?follow=true"              # one JSON event per line
curl -sN -H "$AUTH" "$URL/v2/sessions/$ID/events?follow=true&format=raw"   # just the stdout bytes
curl -sN -H "$AUTH" "$URL/v2/sessions/$ID/events?follow=true&format=sse"   # server-sent events
```

Add `from=<seq>` to resume after a disconnect. SSE clients resume automatically with `Last-Event-ID`.

#### Send stdin to a running process

```sh
ID=$(curl -s -H "$AUTH" $URL/v2/sessions --json '{"argv": ["python3", "-u", "-i"], "merge_stderr": true}' | jq -r .id)
curl -sN -H "$AUTH" "$URL/v2/sessions/$ID/events?follow=true&format=raw" &

curl -s -H "$AUTH" "$URL/v2/sessions/$ID/stdin" --data-binary $'print(6 * 7)\n'
curl -s -H "$AUTH" "$URL/v2/sessions/$ID/stdin?close=true" --data-binary $'print("bye")\n'
```

`close=true` closes stdin after writing, which is how a program reading its input learns it has ended.

`--json` (curl 7.82 or newer) sends the body with `Content-Type: application/json`, which JSON endpoints require; with older curl use `-H 'Content-Type: application/json' -d '…'`. The stdin endpoint takes raw bytes with any content type.

## API

The server publishes its own OpenAPI 3.1 description, generated from the code with [huma](https://github.com/danielgtaylor/huma):

- `GET /v2/openapi.json` and `/v2/openapi.yaml`: the document, for generating clients in any language.
- `GET /v2/docs`: interactive API reference in the browser.
- `shhttpd openapi` prints the document without starting a server, and [docs/openapi.yaml](docs/openapi.yaml) is a committed copy.

All endpoints except `/healthz`, `/v2/version` and the API description need `Authorization: Bearer <key>`. JSON request bodies are validated against the schema, and unknown fields are rejected. Errors are [RFC 9457](https://www.rfc-editor.org/rfc/rfc9457) `application/problem+json`:

| Status | Meaning |
|---|---|
| 400 | Malformed JSON, or a request the schema allows but the server rejects (for example both `argv` and `shell`). `detail` says why. |
| 401 / 403 | Missing or invalid key / the key lacks the scope or its policy forbids the request. |
| 404 | No such resource, or it belongs to another key. |
| 409 | The session is not running, or its stdin is closed. |
| 415 | A JSON endpoint received a body without `Content-Type: application/json`. |
| 422 | The request does not match the schema. `errors` lists each problem and where it is, such as `body.argv`. |
| 429 | A concurrency limit was reached. |

```json
{"title": "Unprocessable Entity", "status": 422, "detail": "validation failed",
 "errors": [{"message": "unexpected property", "location": "body.oops", "value": {"argv": ["true"], "oops": 1}}]}
```

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
| `GET /v2/exec` | `sessions:run` | WebSocket: start a session and exchange stdin, signals and events on one connection. |
| `GET /v2/sessions/{id}/attach` | `sessions:read` | WebSocket: replay a session from `from`, then follow it. Sending input needs `sessions:run`; `readonly=true` only watches. |
| `GET /v2/whoami` | any key | The calling key, its scopes and policy. |
| `POST /v2/keys`, `GET /v2/keys`, `GET/PATCH/DELETE /v2/keys/{id}`, `POST /v2/keys/{id}/rotate` | master key | Manage API keys. `DELETE` revokes; add `?kill_sessions=true` to also kill the key's running sessions. `rotate` accepts `{"grace": "1h"}` to keep the old secret working for a while. |

A key sees only its own sessions. The `admin:read` scope allows reading every key's sessions, but never changing them.

### WebSocket

Connect to `/v2/exec` offering the subprotocol `shhttp.v2.json`, then send a start message. Every message is JSON text:

```jsonc
→ {"type": "start", "spec": {"argv": ["python3", "-i"]}, "on_disconnect": "kill"}
← {"type": "session", "session": {"id": "ses_…", "state": "running", …}}
← {"seq": 1, "type": "started", "pid": 4242, …}
→ {"type": "stdin", "data": "print(6 * 7)\n"}
← {"seq": 2, "type": "stdout", "data": "42\n", …}
→ {"type": "signal", "signal": "SIGINT"}
→ {"type": "stdin_close"}
← {"seq": 9, "type": "exit", "state": "exited", "exit_code": 0, …}
   the server closes the connection with status 1000
```

- Events are the same objects as `GET /v2/sessions/{id}/events` returns. A rejected message gets `{"type": "protocol_error", "status": 400, "error": "…"}` and the connection stays open.
- `on_disconnect`: `keep` (default), `kill`, or a duration such as `"1m"` after which the session is killed unless a client has attached again.
- `/v2/sessions/{id}/attach?from=N` sends the same `session` message and then the events from `N`, so a client can reconnect without missing output.
- Browsers cannot set an Authorization header on WebSocket requests: offer the key as a second subprotocol, `shhttp.v2.auth.<key>`. Pages from other origins are refused unless listed in `allowed_origins`.

### Go client

```go
import (
	"github.com/codemug/shhttp/pkg/api"
	"github.com/codemug/shhttp/pkg/client"
)

c := client.New("http://127.0.0.1:2112", os.Getenv("SHHTTP_KEY"))

// Run and wait.
res, err := c.Run(ctx, api.SessionSpec{Argv: []string{"uname", "-a"}}, nil)
fmt.Print(*res.Stdout)

// Interactive, over a WebSocket.
conn, err := c.Exec(ctx, api.SessionSpec{Argv: []string{"cat"}}, nil)
conn.Stdin(ctx, []byte("hello\n"))
conn.CloseStdin(ctx)
for {
	e, err := conn.Recv(ctx) // io.EOF after the exit event
	if err != nil {
		break
	}
	os.Stdout.Write(e.Data)
}

// Follow a session's events over HTTP.
for e, err := range c.Events(ctx, id, &client.EventsOptions{Follow: true}) { … }
```

Errors from the server are `*api.Problem` values (`client.IsStatus(err, 404)`); rejected WebSocket messages are `*client.ProtocolError`.

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
allowed_origins: [app.example.com]         # web pages allowed to open WebSockets
```

The master key can also be given with `SHHTTP_MASTER_KEY`. `shhttpd keygen` prints a new one. The server warns when it listens on a non-loopback address without TLS.

## Docker

```sh
docker build -t shhttp .
docker run -p 2112:2112 -v shhttp-data:/data shhttp
docker run --rm -v shhttp-data:/data alpine cat /data/master.key
```

The image also contains the `shhttp` client.

## Development

```sh
go test -race ./...
```

`docs/openapi.yaml` must match the code; a test fails when it does not. After changing an endpoint or an API type, regenerate it:

```sh
go test ./internal/server -run TestOpenAPIFileIsCurrent -update
```

## License

Apache 2.0, see [LICENSE](LICENSE).
