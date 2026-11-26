# inkling-chat

A chat agent with a web interface on a model that runs on this computer: Inkling 2 Mini on the local Strata
server with the patched engine. The agent loop is [govengers/aikido](https://github.com/govengers/aikido); the
service follows the [govengers standards](https://github.com/govengers/standards), `service` profile.

- **Plain chat:** answers start at once and stream as fast as the model writes.
- **File tools (switch in the message box):** the model works in the chat's directory (the folder control next to
  the switch; empty means `workspace/`). It gets aikido's file tools (list, read, write, edit, move, delete, grep,
  glob), `bash`, aikido's virtual shell over the same directory (its own GNU-style commands, not the host's shell),
  and in that shell the host's `go` and `bun`, which run in the shell's current directory. Symbolic links are refused
  and hidden paths (`.env`, `.git`, `_private`) are hidden from the file tools and the shell; `go` and `bun` are host
  programs and see the whole disk. A directory must exist and lie below one of `WORKSPACE_ROOTS` (symbolic links
  resolved); each conversation keeps its shell (working directory, `/tmp`) while it stays in one directory, at most
  32 conversations at a time. A message may take up to 60 model calls (read, edit, build, test, fix). The system
  prompt names no directory, so every directory shares the cached prompt start; each message with tools begins with
  a `Working directory:` line instead. With tools offered the model server sends a reply only once it is complete,
  so the answer does not stream; that is why the switch is off by default. The thinking still streams with tools.
- **Time limits and background processes:** every tool call ends after `TOOL_TIMEOUT` (30 s). A `bash` call may ask
  for more with `timeout_seconds`, up to `TOOL_TIMEOUT_MAXIMUM` (10 min); one that runs out of time returns its output
  so far with exit code 124. Servers and watchers run in the background instead: `start_process` starts `go` or `bun`
  (program and arguments, no shell syntax, the same refusals as in the shell), or a program built inside the chat's
  directory (`./server`), with extra variables in `env` (not `PATH`, `HOME`, `TMPDIR`), in its own process group and answers
  after about 2 s with its first output; `process_output` shows the newest 64 KiB of output and the exit code,
  `stop_process` ends the process and what it started. `http_request` checks such a server (http to `127.0.0.1`/`localhost`, ports 20000 to 65535 only, so the model
  cannot call this app or your other local apps). At most 8 run per conversation; they end when the
  conversation's tools are dropped or the server stops.
- **Host commands:** `go` and `bun` are found at startup (`GO_BINARY`, `BUN_BINARY`, else `PATH`); a missing one is
  left out and the startup event says so (`command.go.missing`). They run with a minimal environment: `HOME`, `PATH`
  (the binary's directory and `/usr/bin:/bin:/usr/sbin:/sbin`), `TMPDIR`, for go the host's `GOPATH`, `GOCACHE`,
  `GOMODCACHE` and `GOPROXY` (so builds reuse its caches), for bun the host's `BUN_INSTALL`. They refuse what changes
  the user's setup: `go install`, `clean`, `telemetry`, `work`, `env -w`/`-u`, flags before the subcommand (`-C`);
  `bun` with `-g`/`--global`, `link`, `unlink`, `upgrade`, `pm cache rm`. One run of either may take 10 minutes, one
  shell call 15. This is no sandbox: `go run`, `go test` and `bun run` execute the directory's code on this computer.
- **Personas (select next to the thinking):** fixed system prompts defined in the server (`internal/service`):
  Assistant (the default), Go developer, Code reviewer, Editor. A message sends only the persona's id; the browser
  keeps the choice per conversation and starts a new chat with the last one. Changing it inside a conversation
  changes the system prompt, so the model reads the whole conversation again once; the warm-up pins the default.
- **Thinking (Off, Low, Medium, High, next to the file tools):** how long the model reasons before it answers, per
  message; the browser remembers the last choice. Off sends `reasoning_effort` `none` and answers at once. The
  thinking shows above the answer as a collapsed "Thinking" disclosure and is kept with the message in the history.
  It needs the locally patched Strata dialect (`~/models/strata-src/inkling-thinking.patch`): stock Strata 0.2.20
  forces `none` for Inkling. The model server ends a thinking after 8,192 tokens (`reasoning_budget_tokens` in
  the launcher) and lets the model answer; the patch also wraps up a thinking that repeats itself and ends an
  answer that repeats itself (as `length`), at any level. The model's template writes the level near the start of the prompt,
  so a conversation that changes it reads its whole history again once (a 200K-token conversation: about five
  minutes); keep one level per conversation.
- **Context:** the model server runs with Inkling's whole window of 1,048,576 tokens. A message may have up to 4 MiB
  (about a million tokens), a request body 16 MiB, and a turn two hours: the model server reads a prompt that fills
  the window in about 80 minutes (708 tokens a second at 232K tokens, 2,000 at a few thousand); a follow-up reads
  only its new tokens. The browser history in `localStorage` holds only a few MiB, so very long conversations stop
  being saved there.
- Conversations live in memory: a restart forgets them. There is no database yet. The browser keeps the chat
  history in `localStorage`; a conversation reopened after a server restart shows its messages, but the model no
  longer remembers them.
- **Warm-up:** at startup the server sends the model server the start of a turn with tools (default persona, system prompt,
  tool definitions and thinking off, the defaults) once, in the background, asking for one token. The patched Strata engine pins that prefix in its
  prompt cache, so the first file-tools message of every new conversation reads only its own tokens (about 2,100
  cached of 2,110 input tokens). The thinking level is part of that prefix: the first file-tools conversation with
  another level reads it once more, and the engine then pins that one instead. A failed warm-up is logged as a
  `warm_up` wide event and changes nothing else.
- **Expert details** (header switch): prompt processing, input and cached input tokens, output tokens and the rate
  per answer and per model call, live while the answer streams. The prompt processing is the model server's own
  measurement (the `timings` Strata and llama.cpp send at the end of a stream, which aikido does not read; the
  model client's transport reads it): tokens a second, its time and the tokens it read without the cached ones.
  The generation rate and the share of kept drafts come from there too. A server without timings shows the time
  to the first token instead. With file tools on, the Strata server sends the reply only when it is complete, so
  there are no live answer numbers in that mode.

## Run

```sh
just model-start        # the local model server (Strata, pack sq-20; `just model-start sq-10` for the smaller pack)
just serve              # builds the frontend into the binary and serves http://127.0.0.1:8080
just model-stop
```

Development: `just dev` runs the Go server and the frontend's dev server; open http://localhost:5173.

Needs Go 1.26 or later, Bun, just, golangci-lint v2, and the Strata install of this machine
(`~/models/strata-0.2.20/inkling-fast`, or `INKLING_FAST=<path>`).

## Check

```sh
just check              # lint, unit tests (race, 80 % coverage gate), end-to-end suite, frontend checks, govulncheck
```

The end-to-end suite starts the built binary against a scripted model server (`internal/modeltest`); it needs no
model.

## Configuration

Environment variables, read once in `cmd/server/main.go` (a `.env` in the working directory is read too).

| Variable | Default | |
|---|---|---|
| `LISTEN_ADDRESS` | `127.0.0.1:8080` | the API and the frontend; loopback, because the API has no authentication |
| `METRICS_ADDRESS` | `127.0.0.1:9090` | internal listener of `/metrics` |
| `MODEL_BASE_URL` | `http://127.0.0.1:18081/v1` | API root of an OpenAI-compatible model server |
| `MODEL_API_KEY` | `local` | sent as bearer token; a local server ignores it |
| `MODEL_NAME` | empty | the model asked for; empty asks the server which one it serves, per message |
| `WORKSPACE_PATH` | `workspace` | the folder of the tools of a chat without a directory; created when missing |
| `TOOL_TIMEOUT` | `30s` | how long one tool call may run |
| `TOOL_TIMEOUT_MAXIMUM` | `10m` | the longest a bash call may ask for with `timeout_seconds` (go test, builds) |
| `WORKSPACE_ROOTS` | the user's home directory | comma-separated absolute paths; a chat's directory must lie below one (none: only `WORKSPACE_PATH`) |
| `GO_BINARY` | empty | the go the shell offers; empty: the `go` in `PATH`, none when missing |
| `BUN_BINARY` | empty | the bun the shell offers; empty: the `bun` in `PATH`, none when missing |
| `MODEL_WARM_UP` | `true` | send the start of a file-tools turn to the model server at startup (see above) |
| `MODEL_PROMPT_CACHING` | `false` | mark requests with prompt-cache breakpoints: needed for Anthropic's models on OpenRouter, which cache only what is marked; a local OpenAI-compatible server caches on its own |
| `MODEL_TEMPERATURE` | `-1` | sampling temperature of every model call; negative leaves the server default, which on Strata is greedy decoding (a small model on greedy decoding tends to repeat itself in long tool loops) |
| `DEV_MODE` | `false` | readable wide events on stderr instead of JSON |
| `READINESS_DELAY` | `5s` | how long readiness is withdrawn before the drain on SIGTERM |
| `OTEL_*` | | tracing; off without `OTEL_EXPORTER_OTLP_ENDPOINT` |

## API

`POST /api/v1/conversations/{uuid}/messages` with `{"text": "...", "workspace": false, "directory": "/abs/path",
"thinking": "off", "persona": "assistant"}` (`directory`: the tools' directory, the workspace when left out;
`thinking`: `off`, `low`, `medium` or `high`, `off` when left out; `persona`: an id of `GET /api/v1/personas`, which
answers `{"items": [{"id", "label", "description"}]}`) answers with server-sent events: `text`,
`thinking`, `tool_call`, `tool_result`, `usage` (tokens, cached input tokens, time to first token and rate of one
model call, and the model server's own `promptMs`, `promptTokens`, `promptPerSecond`, `predictedPerSecond`,
`draftTokens`, `draftAccepted`, 0 when it sends none), `end`, and `error` (the error body as data) when a turn fails
after the stream began. Errors before the stream use `{"code", "message"}`; an invalid directory or persona says
what is wrong in `message`.

For scripts (steering the chat, checking its answers): send `Accept: application/json` and the same route answers
once the turn ended with the whole turn in one body, `{"text", "thinking", "toolCalls": [{"callId", "tool",
"target", "ok", "error"}], "usage": [...], "reason"}`; a turn that failed is an error response `{"code",
"message"}` with its status code. The conversation ID is the client's: reuse it for a follow-up message.

```sh
id=$(uuidgen)
curl -s -H 'Accept: application/json' -H 'Content-Type: application/json' \
  -d '{"text": "Which Go version does go.mod name?", "workspace": true, "directory": "'"$PWD"'"}' \
  http://127.0.0.1:8080/api/v1/conversations/$id/messages | jq -r .text
```

**Request guard** (the API has no authentication, and its tools run programs): on a loopback `LISTEN_ADDRESS` a
request must name a loopback `Host` (`localhost`, `127.0.0.1`, `[::1]`), so a DNS name rebound to this computer
reaches nothing (403 `forbidden`); an API request with an `Origin` must come from the server's own pages or the dev
server (`http://localhost:5173`, `http://127.0.0.1:5173`; 403 `forbidden`); a `POST` must be `application/json` (415
`unsupported_media_type`), which a cross-site form cannot send without a CORS preflight the server never answers. Probes: `GET /api/health`, `GET /api/ready` (the
model server must answer its model list).

## Layout

| Path | |
|---|---|
| `cmd/server` | configuration and bootstrap in order, nothing else |
| `internal/controller` | the route, payloads, the event stream, the one error mapping |
| `internal/service` | what a message must be, which model and prompt a turn gets, the turn's wide-event fields |
| `internal/client/agent` | the model server: aikido's client pointed at it, the runtime of a turn, the model list |
| `internal/server` | router and middleware order, the listener that drains on shutdown |
| `internal/web` | the embedded frontend (`ui/` builds into `html/dist`) |
| `internal/modeltest` | scripted model server for tests |
| `test/e2e` | end-to-end suite against the built binary |
| `ui/` | the frontend: Vite, Vue 3, TypeScript, Bun; orbs from `vue-agent-fx` |

## Deviations from the standards

Declared in `.govengers.yml`: no authentication (AUTH-002), the stream's TypeScript types are written by hand
(API-011), no browser journeys yet (TST-011). The Dockerfile and the CI workflow come from the service template
and have not run yet.
