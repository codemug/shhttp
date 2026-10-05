# shhttp v2 streaming protocol

This document specifies the parts of the API that the OpenAPI document ([openapi.yaml](openapi.yaml)) cannot fully describe: the event model, the streaming formats of the events endpoints, and the WebSocket protocol. Everything else (request and response bodies, status codes) is in the OpenAPI document, which the server also serves at `/v2/openapi.json`.

## Events

A session's history is a log of events. Every event has:

| Field | Type | Meaning |
|---|---|---|
| `seq` | integer | Position in the log, starting at 1, with no gaps. |
| `time` | RFC 3339 timestamp | When the server recorded the event. |
| `type` | string | One of the types below. |

Session event types and their extra fields:

| `type` | Fields | Meaning |
|---|---|---|
| `started` | `pid` | The process started. Always seq 1 unless the process failed to start. |
| `stdout`, `stderr` | `data` or `data_b64` | A chunk of output. `data` is used when the bytes are valid UTF-8, `data_b64` (standard base64) otherwise. A UTF-8 character is never split between two chunks. |
| `stdin_closed` | | Standard input was closed. |
| `signal` | `signal` | A signal was sent, by a client or by the server (SIGTERM, then SIGKILL, when stopping a session). |
| `exit` | `state`, `exit_code` or `signal`, `duration_ms`, `error` | The process ended. Always the last event. `state` is the final session state: `exited`, `killed` or `timed_out`. `exit_code` is set when the process exited; `signal` when a signal ended it. `error` explains server-side stops such as an output limit. |
| `error` | `error` | The process failed to start. The last and only event. |

Job event types (from `GET /v2/jobs/{id}/events`):

| `type` | Fields |
|---|---|
| `job_started` | |
| `step_started` | `step`, `session_id` |
| `step_finished` | `step`, `session_id`, `state` (step state), `exit_code`, `signal`, `error` |
| `job_finished` | `state` (job state), `error` |

Clients must ignore fields and event types they do not know; new ones may be added.

## HTTP event streams

`GET /v2/sessions/{id}/events` and `GET /v2/jobs/{id}/events` take:

| Parameter | Meaning |
|---|---|
| `from` | First sequence number to return (default 1). |
| `follow` | `true` keeps the response open and sends events as they happen, until the session or job ends. |
| `format` | `ndjson` (default), `sse` or, for sessions, `raw`. Overrides the `Accept` header (`application/x-ndjson`, `text/event-stream`, `application/octet-stream`). |
| `stream` | With `raw`: `stdout` (default) or `stderr`. |
| `lines` | Sessions only: re-chunk output into one event per complete line. A final partial line is sent when the process ends. Each line event carries the `seq` of the chunk that completed it, so resuming from a line's `seq + 1` can skip the start of a line that spanned chunks. |
| `Last-Event-ID` header | Server-sent events: resume after this sequence number. Browsers' `EventSource` sends it on reconnect. |

Formats:

- **ndjson**: one event object per line.
- **sse**: one message per event: `id: <seq>` and `data: <event JSON>`. While following, the server sends a comment line (`: keep-alive`) every 15 seconds of silence.
- **raw**: the bytes of one output stream, nothing else.

A response that ends without the last event (`exit`, `error` or `job_finished`) was cut short: reconnect with `from` set to the last `seq` you received plus one.

## WebSocket

### Connecting

- `GET /v2/exec` starts a session. Needs the `sessions:run` scope.
- `GET /v2/sessions/{id}/attach?from=<seq>&readonly=<bool>` connects to an existing session, replaying its events from `from` (default 1). Read-only connections need `sessions:read`; interactive ones need `sessions:run` and must belong to the key that started the session.

The client offers the subprotocol `shhttp.v2.json`. It authenticates with an `Authorization: Bearer <key>` header, or, where it cannot set headers (browsers), by also offering the subprotocol `shhttp.v2.auth.<key>`; the server selects `shhttp.v2.json` and never echoes the key. Browser pages from another origin than the server are refused unless the server lists them in `allowed_origins`.

Before the upgrade, failures are ordinary HTTP responses with problem+json bodies (401, 403, 404, 429). After it, every message is a JSON text frame.

### Client messages

| Message | When |
|---|---|
| `{"type": "start", "spec": {…}, "on_disconnect": "keep"}` | First message on `/v2/exec` only, within 10 seconds of connecting. `spec` is a session spec as in `POST /v2/sessions`. |
| `{"type": "stdin", "data": "text"}` or `{"type": "stdin", "data_b64": "…"}` | Write to standard input. |
| `{"type": "stdin_close"}` | Close standard input (end of file). On a TTY session this sends Ctrl-D. |
| `{"type": "signal", "signal": "INT"}` | Send a signal to the session's process group. The `SIG` prefix is optional. |
| `{"type": "resize", "cols": 120, "rows": 40}` | Resize a TTY session. |

`on_disconnect` decides what happens to a session started on this connection when the connection closes: `keep` (default) leaves it running; `kill` stops it; a duration such as `"1m"` stops it after that long unless another interactive client is attached by then.

Closing the WebSocket does not close standard input; send `stdin_close` for that.

### Server messages

1. `{"type": "session", "session": {…}}`: always first. The session as in `GET /v2/sessions/{id}`, including its `id`, which a client needs to attach again later.
2. The session's events, exactly as described above, starting at seq 1 for `/v2/exec` and at `from` for attach.
3. `{"type": "protocol_error", "status": 400, "error": "…"}`: a client message was rejected (unknown type, unknown field, binary frame, input to a read-only connection, stdin after it was closed, a signal to a finished process). `status` is the HTTP status the same failure gets over HTTP. The connection stays open.

### Closing

- After the `exit` (or `error`) event, the server closes with status **1000**. Attaching to a finished session replays its events and closes the same way.
- If the start message is missing or the session cannot be started, the server sends a `protocol_error` (with status 400, 403 or 429, as over HTTP) and closes with status **1008**.
- The server pings every 30 seconds and drops connections that do not answer.
- During server shutdown sessions are stopped, so connections end with their `exit` event and status 1000.

### Example

```
→ {"type":"start","spec":{"argv":["python3","-i","-q"]},"on_disconnect":"kill"}
← {"type":"session","session":{"id":"ses_01…","state":"running","stdin_open":true,…}}
← {"seq":1,"time":"…","type":"started","pid":4242}
← {"seq":2,"time":"…","type":"stderr","data":">>> "}
→ {"type":"stdin","data":"print(6 * 7)\n"}
← {"seq":3,"time":"…","type":"stdout","data":"42\n"}
← {"seq":4,"time":"…","type":"stderr","data":">>> "}
→ {"type":"stdin_close"}
← {"seq":5,"time":"…","type":"stdin_closed"}
← {"seq":6,"time":"…","type":"exit","state":"exited","exit_code":0,"duration_ms":812}
← close 1000 "session ended"
```

### Flow control

The server sends events as fast as the connection accepts them; WebSocket backpressure is the only flow control. Output is stored on disk as it is produced, so a slow client never slows the process down: it falls behind and catches up from the log.
