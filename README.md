# acp-go-codex

Go ACP agent that exposes the local Codex CLI as an [Agent Client Protocol](https://agentclientprotocol.com/) agent.

[![Go Reference](https://pkg.go.dev/badge/github.com/savid/acp-go-codex.svg)](https://pkg.go.dev/github.com/savid/acp-go-codex)
[![CI](https://github.com/savid/acp-go-codex/actions/workflows/go-test.yml/badge.svg)](https://github.com/savid/acp-go-codex/actions/workflows/go-test.yml)
[![License: GPL v3](https://img.shields.io/badge/License-GPLv3-blue.svg)](LICENSE)

It wraps `codex app-server`, speaks ACP over JSON-RPC streams, and builds on
[`github.com/coder/acp-go-sdk`](https://github.com/coder/acp-go-sdk).

Use it as either:

- a standalone ACP subprocess: `acp-go-codex`
- an embedded Go adapter through `codexacp.Serve`

## Install

Library:

```sh
go get github.com/savid/acp-go-codex
```

CLI:

```sh
go install github.com/savid/acp-go-codex/cmd/acp-go-codex@latest
```

The `acp-go-codex` binary speaks ACP over stdin/stdout; an editor or ACP host
launches it as a subprocess rather than a human-facing chat UI.

## Quickstart

The example programs run from a checkout of this repo, so clone it first:

```sh
git clone https://github.com/savid/acp-go-codex && cd acp-go-codex
```

Run a tiny local client against the agent:

```sh
go run ./examples/minimal-client "Reply with a short hello from ACP."
```

Start an interactive session against the agent:

```sh
go run ./examples/interactive-chat
```

Load and resume a stored session transcript:

```sh
go run ./examples/resume-from-file -file ./examples/resume-from-file/session.jsonl
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

See the [Go API reference](https://pkg.go.dev/github.com/savid/acp-go-codex)
for options such as the Codex executable path, `CODEX_HOME`, default model,
session storage, external ChatGPT token refresh, guarded logout, and
OpenTelemetry providers.

## What It Provides

- ACP session lifecycle: create, prompt, cancel, close, list, load, resume, and
  fork.
- Codex app-server subprocess management and JSON-RPC request routing.
- Prompt streaming for messages, reasoning, plans, tool calls, diffs, usage, and
  session metadata.
- Structured output through session-level JSON Schema on `turn/start`.
- Command, file, and generic permission prompts, tool user input, and MCP
  elicitation bridging.
- MCP stdio and streamable HTTP configuration; other MCP transports are rejected.
- Codex account status, terminal login passthrough, external ChatGPT token
  login/refresh, and guarded logout for adapter-owned `CODEX_HOME` directories.
- Durable mirroring through a host-provided `SessionStore`; stored rows are Codex
  rollout JSONL keyed by `{SessionID, Subpath}`.
- Optional raw Codex rollout extension notifications through `_codex/rawEvent`.
- OpenTelemetry adapter telemetry plus native Codex app-server OTLP mapping
  without recording prompt or tool secrets by default.

## Slash Commands

Codex app-server exposes no documented native command-discovery surface, so the
adapter advertises no ACP `AvailableCommand` entries. Slash-prefixed text such as
`/review`, `/plan`, or `/compact` is forwarded to Codex as ordinary `turn/start`
input. Codex skills (`skills/list`, `$skill`, `type:"skill"` items) are not
commands and are never projected as `AvailableCommand` entries.

## Docs

- [Overview](docs/overview.mdx)
- [Run modes](docs/get-started/run-modes.mdx)
- [Go API](docs/reference/go-api.mdx)
- [ACP methods](docs/reference/acp-methods.mdx)
- [Observability](docs/operations/observability.mdx)

Full Go API reference:
[pkg.go.dev/github.com/savid/acp-go-codex](https://pkg.go.dev/github.com/savid/acp-go-codex).

## Development

```sh
make audit
make test-integration-smoke
make test-integration-live
make test-integration-cover
```

`make audit` runs the full local gate: format, lint, build, unit tests,
coverage, cross-compile, vuln, and docs checks. Live integration tests require a
local authenticated `codex` CLI. `make test-integration-smoke` sets
`ACP_GO_CODEX_RUN_INTEGRATION=1` and avoids model spend; `make test-integration-live`
sets both `ACP_GO_CODEX_RUN_INTEGRATION=1` and `ACP_GO_CODEX_RUN_LIVE_TOKENS=1`
and may spend model tokens; `make test-integration-cover` runs the token-free
live suite against a coverage-instrumented binary. Live tests always launch
Codex with an isolated temp `CODEX_HOME`. When `OPENAI_API_KEY` is set and `ACP_GO_CODEX_HOME`
is unset, tests use a fresh temp home; otherwise they copy the source home and
clear copied auth refresh tokens so live tests cannot rotate the source home's
refresh token. If neither env auth nor copied `auth.json` is available, tests
fail rather than launch without isolated auth.

## License

Distributed under the GNU General Public License v3.0. See [LICENSE](LICENSE).
