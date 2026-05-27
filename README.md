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

Import and resume a stored Codex rollout JSONL file:

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
MCP proxy override, guarded logout, and OpenTelemetry providers.

## What It Provides

- ACP session lifecycle: create, prompt, cancel, close, list, load, resume, and
  fork.
- Codex app-server subprocess management and JSON-RPC request routing.
- Prompt streaming for messages, reasoning, plans, tool calls, diffs, usage, and
  session metadata.
- Codex structured output through session-level JSON Schema on `turn/start`.
- Codex command/file/generic permission prompts, tool user input, and MCP
  elicitation bridging.
- MCP stdio, streamable HTTP, and ACP-transport bridging. SSE MCP is rejected
  because Codex does not expose a supported SSE path.
- Codex account status, terminal login passthrough, external ChatGPT token
  login/refresh, and guarded logout for adapter-owned `CODEX_HOME` directories.
- Session import and optional durable mirroring through a host-provided
  `SessionStore`; stored rows are Codex rollout JSONL.
- Optional raw Codex rollout extension notifications through `_codex/sdkMessage`.
- OpenTelemetry spans, metrics, trace propagation, and structured logs without
  recording prompt/tool secrets by default.

## Docs

- [Overview](docs/overview.mdx)
- [Run modes](docs/get-started/run-modes.mdx)
- [Go API](docs/reference/go-api.mdx)
- [ACP methods](docs/reference/acp-methods.mdx)
- [Session import](docs/reference/session-import.mdx)
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
By default it uses the normal Codex home, matching `acp-go-claude`; set
`ACP_GO_CODEX_HOME` only when you want to run against an isolated authenticated
Codex home.
