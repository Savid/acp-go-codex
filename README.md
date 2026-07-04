# acp-go-codex

Go ACP agent for the local Codex CLI. It wraps `codex app-server`, speaks
[Agent Client Protocol](https://agentclientprotocol.com/) over JSON-RPC
streams, and is built on
[`github.com/coder/acp-go-sdk`](https://github.com/coder/acp-go-sdk).

Use it as either:

- a standalone ACP subprocess: `acp-go-codex`
- an embedded Go adapter through `codexacp.Serve`

## Install

```sh
go install github.com/savid/acp-go-codex/cmd/acp-go-codex@latest
```

For local development:

```sh
go run ./cmd/acp-go-codex
```

The process speaks ACP over stdin/stdout. In normal use, an editor or ACP host
launches it as a subprocess rather than a human-facing chat UI.

## Quickstart

Run a tiny local client against the agent:

```sh
go run ./examples/minimal-client "Reply with hello from ACP"
```

Or try the interactive example:

```sh
go run ./examples/interactive-chat
```

Load and resume a stored Codex rollout JSONL file:

```sh
go run ./examples/resume-from-file -session-file ./examples/resume-from-file/session.jsonl
```

## Embedded Go

```go
package main

import (
	"context"
	"log"
	"os"

	codexacp "github.com/savid/acp-go-codex"
)

func main() {
	err := codexacp.Serve(context.Background(), os.Stdin, os.Stdout,
		codexacp.WithDefaultModel("gpt-5.5"),
	)
	if err != nil {
		log.Fatal(err)
	}
}
```

See [Go API docs](docs/reference/go-api.mdx) for options such as Codex path,
`CODEX_HOME`, default model, session storage, external ChatGPT token refresh,
guarded logout, and OpenTelemetry providers.

## What It Provides

- ACP session lifecycle: create, prompt, cancel, close, list, load, resume, and
  fork.
- Codex app-server subprocess management and JSON-RPC request routing.
- Prompt streaming for messages, reasoning, plans, tool calls, diffs, usage, and
  session metadata.
- Codex structured output through session-level JSON Schema on `turn/start`.
- No ACP slash-command advertisement. `/review`, `/plan`, `/compact`, and other
  slash-prefixed text is sent to Codex as ordinary `turn/start` input.
- Codex command/file/generic permission prompts, tool user input, and MCP
  elicitation bridging.
- MCP stdio and streamable HTTP configuration. Other MCP transports are
  rejected because Codex does not expose supported paths for them.
- Codex account status, terminal login passthrough, external ChatGPT token
  login/refresh, and guarded logout for adapter-owned `CODEX_HOME` directories.
- Durable mirroring through a host-provided `SessionStore`; stored rows are
  Codex rollout JSONL keyed by `{SessionID, Subpath}`.
- Optional raw Codex rollout extension notifications through `_codex/rawEvent`.
- OpenTelemetry adapter telemetry plus native Codex app-server OTLP mapping
  without recording prompt/tool secrets by default.

## Slash Commands

Codex app-server does not expose a documented native command discovery and
execution surface for this adapter to project into ACP `AvailableCommand`
entries. Skills surfaces (`skills/list`, `$skill`, `type:"skill"` items) are NOT
commands and must not be projected as `AvailableCommand` entries absent a
documented native command projection. Re-entry criteria: documented
`commands/list`+execute, or documented server-side `/x` parsing in `turn/start`,
or documented skill→command projection.

## Docs

- [Overview](docs/overview.mdx)
- [Run modes](docs/get-started/run-modes.mdx)
- [Go API](docs/reference/go-api.mdx)
- [ACP methods](docs/reference/acp-methods.mdx)
- [Observability](docs/operations/observability.mdx)

## Development

```sh
make audit
make test-integration-smoke
make test-integration
make test-integration-cover
```

Live integration tests require a local authenticated `codex` CLI. The full
integration target sets `ACP_GO_CODEX_LIVE_TURN=1` and may spend model tokens.
Live tests always launch Codex with an isolated temp `CODEX_HOME`. When
`OPENAI_API_KEY` is set and `ACP_GO_CODEX_HOME` is unset, tests use a fresh temp
home. Otherwise they copy the source home into the temp home and clear copied
auth refresh tokens so live tests cannot rotate the source home's refresh token.
If neither env auth nor copied `auth.json` is available, tests fail instead of
launching without isolated auth.
