# AGENTS.md

## Purpose

This Go module exposes the local `codex` CLI as an Agent Client Protocol
agent. One `codex app-server` serves the agent; each ACP session is one Codex
thread on it. Codex inherits the adapter's environment and keeps its rollouts
in its own home, so a session started over ACP can be continued natively with
`codex resume` afterwards.

## Project Map

- `cmd/acp-go-codex`: stdio entrypoint, OpenTelemetry setup, signals, flags.
- Root `agent*.go`, `options.go`, `request_builders.go`: the public ACP
  surface, option validation, the shared app-server generation and its event
  pump, and the transport wrapper that orders session publication behind the
  establishing response.
- Root `session*.go`, `image_output.go`: one session's thread binding, prompt
  turns, approvals and elicitation, lifecycle stream, store mirror, replay,
  config options, and image output.
- `internal/codex`: the app-server JSON-RPC client, launch arguments, event
  decoding, server request mapping, thread configuration, seed files, and the
  rollout file layout.
- `internal/observer`: OpenTelemetry spans and metrics.
- `integration`: gated tests against the installed codex.
- `examples`: runnable ACP clients.

## Commands

```sh
make build
make test
make lint
make audit
make test-integration-smoke
make test-integration-live
```

`make test` runs with race detection and shuffled order. `make audit` is the
full local gate. Integration targets need an installed `codex`; the live
target spends model tokens and requires explicit operator intent.

## Coding Rules

- Follow Go idioms: `ctx` first, `%w` for wrapped errors, small interfaces at
  the consumer. Keep native protocol details in `internal/codex` and ACP glue
  beside its handler.
- Shared family behavior comes from `github.com/savid/acp-go-core`; never copy
  it here.
- The adapter does no isolation: the app-server inherits the process
  environment and the agent overlay; each thread carries its own session env
  and `PATH` through its shell environment policy.
- Native state is never deleted. The session store is the durability
  boundary; the rollout in Codex's home is the native copy.
- Unit tests never require an installed codex: the test binary doubles as a
  scripted fake app-server. Keep the fake's protocol in step with
  `internal/codex`.
- A comment states what the code does or why a constraint exists.

## Verification

Run `go test ./...` for ordinary changes and `make lint` for Go edits. Run
`make audit` once changes settle. Run the integration smoke target after
changing anything app-server-facing.

## Boundaries

- Approval requests are the session permission system. Never bypass the host
  or fail open on a denied or cancelled answer.
- Do not log prompts, tool input or output, or raw native event bodies by
  default.
- Reject every ACP extension method; the only extension surface is the
  outbound raw-event notification.
