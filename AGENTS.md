# AGENTS.md

Instructions for automated coding agents working in this repository.

## Purpose

This Go ACP agent wraps the local `codex app-server` protocol using
`github.com/coder/acp-go-sdk`. One Agent owns a shared app-server; logical
sessions retain their own configuration, callbacks, routes, and store state.

## Project Map

- `cmd/acp-go-codex`: process entrypoint for ACP stdio mode.
- Root package: ACP agent surface, options, session lifecycle, prompt handling,
  session load, auth, MCP bridge, and extension methods.
- `internal/codex`: Codex provider boundary. Keep Codex CLI/app-server details
  here instead of leaking them into ACP handlers.
- `internal/lifecycle`: lifecycle negotiation, reducer, and event vocabulary.
- `internal/observer`: OpenTelemetry instrumentation.
- `docs/` and `examples/`: public behavior and embedding examples.

## Commands

```sh
make audit
make test
make lint
make test-integration-smoke
make test-integration-live
make test-integration-attended
make test-integration-keystore
```

`make audit` runs local formatting, lint, build, race tests with statement
coverage reporting, platform cross-compilation, module, vulnerability,
modernization, and documentation checks. Inspect targets before running them.

Integration runs require explicit operator intent and
`ACP_GO_CODEX_RUN_INTEGRATION=1`. The targets set this gate. Smoke runs use the
real CLI without model tokens. Live, attended auth, and keystore targets also
set `ACP_GO_CODEX_RUN_LIVE_TOKENS=1`, `ACP_GO_CODEX_RUN_ATTENDED=1`, or
`ACP_GO_CODEX_RUN_KEYSTORE=1`, respectively; none joins `make audit`. Attended
runs fail when nobody approves. Keystore runs require a container runtime for
the Linux present/absent matrix and a macOS host for its third configuration.

Use `ACP_GO_CODEX_HARNESS_PATH`, `ACP_GO_CODEX_HOME`, `ACP_GO_CODEX_MODEL`, and
`ACP_GO_CODEX_AGENT_BINARY` for the CLI, source home, model, and compiled adapter.

## Coding Rules

- Keep public API small and ACP-oriented.
- Keep Codex protocol details inside `internal/codex`.
- Prefer structured request/response types over ad hoc JSON maps.
- Return explicit method-not-found or unsupported errors for ACP methods that are
  outside the Codex adapter contract.
- Keep public code and documentation self-contained and describe current behavior.

## Testing Rules

- Use `testify/require` for assertions.
- Prefer table-driven tests for Codex app-server mapping and event-decoding
  cases in `internal/codex`.
- Run `go test ./...` for ordinary changes; use `make test` for session, MCP,
  concurrency, or cancellation changes, and run `make lint` before completion.
- Report statement coverage and preserve canonical lifecycle fixtures. Do not add
  tests or production seams solely to increase coverage.
- Unit tests may use in-memory transports and the placeholder Codex client.
- Native compatibility claims require the real CLI. Deterministic transports and
  compiled initialize/EOF tests prove only adapter behavior and startup.
- Keep live prompts deterministic with exact sentinel replies, and assert the
  ACP stop reason plus streamed updates where practical.

## Security And Boundaries

- Do not silently bypass permission prompts. Mapping Codex
  `item/permissions/requestApproval` onto the ACP `session/request_permission`
  flow is load-bearing for user trust in this agent.
- Do not alter borrowed Codex CLI authentication state. ACP `logout`
  closes adapter sessions and calls native account logout, and is refused unless
  `WithCodexAllowAccountLogout` is set for an adapter-owned `CODEX_HOME`.
- Do not log auth material (`auth.json` contents, refresh tokens, ChatGPT
  tokens), user secrets, prompts, tool input, tool output, or raw Codex
  app-server event bodies. Account metadata surfaced over ACP is redacted.
- Managed native launches and prepared-tree transitions use `HostAuthority`
  without ordinary fallback. A prepared tree stays opaque until successful
  reclaim; failed preparation leaves its cleanup with the host.
- Integration tests must launch Codex with a hermetic temp `CODEX_HOME`, clear
  copied refresh tokens, and fail rather than launch without isolated auth.
  `OPENAI_API_KEY` with no explicit source home uses a fresh home; otherwise
  tests copy the configured source home and require env auth or copied `auth.json`.
