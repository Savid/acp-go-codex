# AGENTS.md

Shared instructions for automated coding agents working in this repository.

## Purpose

This project is a Go implementation of an ACP agent for Codex CLI. It builds on
`github.com/coder/acp-go-sdk` and wraps the local `codex app-server` protocol.

## Project Map

- `cmd/acp-go-codex`: process entrypoint for ACP stdio mode.
- Root package: ACP agent surface, options, session lifecycle, prompt handling,
  session import/load, auth, MCP bridge, and extension methods.
- `internal/codex`: Codex provider boundary. Keep Codex CLI/app-server details
  here instead of leaking them into ACP handlers.

## Commands

```sh
go test ./...
go build ./...
go test -tags=integration ./integration/...
make test-integration-smoke
make test-integration
```

Integration tests are opt-in. `make test-integration-smoke` requires a local
`codex` CLI and runs live app-server checks that do not spend model tokens.
`make test-integration` also sets `ACP_GO_CODEX_LIVE_TURN=1` and may spend model
tokens. Use `ACP_GO_CODEX_CODEX_PATH`, `ACP_GO_CODEX_HOME`,
`ACP_GO_CODEX_MODEL`, and `ACP_GO_CODEX_AGENT_BINARY` to point tests at a
specific CLI, source Codex home, model, or compiled agent binary. Integration
tests always launch Codex with an isolated temp `CODEX_HOME`. When `OPENAI_API_KEY`
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
