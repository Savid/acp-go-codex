# AGENTS.md

Shared instructions for automated coding agents working in this repository.

## Purpose

This project is a Go implementation of an ACP agent for Codex CLI. It builds on
`github.com/coder/acp-go-sdk` and wraps the local `codex app-server` protocol.

## Project Map

- `cmd/acp-go-codex`: process entrypoint for ACP stdio mode.
- Root package: ACP agent surface, options, session lifecycle, prompt handling,
  session load, auth, MCP bridge, and extension methods.
- `internal/codex`: Codex provider boundary. Keep Codex CLI/app-server details
  here instead of leaking them into ACP handlers.

## Commands

```sh
go test ./...
go build ./...
go test -tags=integration ./integration/...
make test-integration-smoke
make test-integration-live
make test-integration-attended
make test-integration-keystore
```

Integration tests are opt-in. `make test-integration-smoke` requires a local
`codex` CLI and runs live app-server checks that do not spend model tokens.
`make test-integration-live` also sets `ACP_GO_CODEX_RUN_LIVE_TOKENS=1` and may spend model
tokens. Use `ACP_GO_CODEX_HARNESS_PATH`, `ACP_GO_CODEX_HOME`,
`ACP_GO_CODEX_MODEL`, and `ACP_GO_CODEX_AGENT_BINARY` to point tests at a
specific CLI, source Codex home, model, or compiled agent binary. Integration
tests always launch Codex with an isolated temp `CODEX_HOME`.
`make test-integration-attended` sets `ACP_GO_CODEX_RUN_ATTENDED=1` and runs the
provider-auth flows a human must approve at the provider; it fails rather than
skips when nobody answers. `make test-integration-keystore` sets
`ACP_GO_CODEX_RUN_KEYSTORE=1` and runs the credential-residence matrix in all
three configurations: the keystore-present and keystore-absent Linux halves
inside the container fixture in `integration/keystore`, and the macOS third on a
macOS host. The same fixture runs the account-command browser legs on Linux,
where `xdg-open` and the other launchers a macOS host never execs are the ones
that matter. It fails rather than skips when no container runtime is available.
Neither joins `make audit`. When `OPENAI_API_KEY`
is set and `ACP_GO_CODEX_HOME` is unset, tests use a fresh temp home. Otherwise
they copy the source home into the temp home and clear copied auth refresh
tokens. If neither env auth nor copied `auth.json` is available, tests fail
instead of launching without isolated auth.

## Coding Rules

- Keep public API small and ACP-oriented.
- Keep Codex protocol details inside `internal/codex`.
- Prefer structured request/response types over ad hoc JSON maps.
- Return explicit method-not-found or unsupported errors for ACP methods that are
  outside the Codex adapter contract.

## Testing Rules

- Use `testify/require` for assertions.
- Prefer table-driven tests for Codex app-server mapping and event-decoding
  cases in `internal/codex`.
- Run `go test ./...` for ordinary changes.
- Run `go test -race ./...` or `make test` for session, MCP bridge, concurrency,
  or cancellation changes.
- Run `golangci-lint run ./...` before considering work complete.
- Integration tests are double-gated: `ACP_GO_CODEX_RUN_INTEGRATION=1` opts into
  the live suite, and `ACP_GO_CODEX_RUN_LIVE_TOKENS=1`,
  `ACP_GO_CODEX_RUN_ATTENDED=1`, or `ACP_GO_CODEX_RUN_KEYSTORE=1` each select one
  further tier. `make test-integration-smoke` runs live
  app-server checks that do not spend tokens.
- Live integration tests launch the actual `codex` binary from `PATH` (or
  `ACP_GO_CODEX_HARNESS_PATH`). Use `ACP_GO_CODEX_AGENT_BINARY` to exercise a
  compiled agent and `ACP_GO_CODEX_MODEL` to override the model.
- Unit tests may use in-memory transports and the placeholder Codex client.
- Local helper processes in integration tests are deterministic MCP stdio
  servers, not real Codex.
- Keep live prompts deterministic with exact sentinel replies, and assert the
  ACP stop reason plus streamed updates where practical.

## Security And Boundaries

- **IMPORTANT**: Do not silently bypass permission prompts. Mapping Codex
  `item/permissions/requestApproval` onto the ACP `session/request_permission`
  flow is load-bearing for user trust in this agent.
- **IMPORTANT**: Do not manage Codex CLI authentication state. ACP `logout`
  only clears adapter-owned session state, and is refused unless
  `WithCodexAllowAccountLogout` is set for an adapter-owned `CODEX_HOME`.
- Do not log auth material (`auth.json` contents, refresh tokens, ChatGPT
  tokens), user secrets, prompts, tool input, tool output, or raw Codex
  app-server event bodies. Account metadata surfaced over ACP is redacted.
- Keep Codex CLI/app-server protocol details inside `internal/codex`; return
  explicit method-not-found or unsupported errors for ACP methods outside the
  adapter contract.
- Integration tests must launch Codex with a hermetic temp `CODEX_HOME`, clear
  copied refresh tokens, and fail rather than launch without isolated auth.
