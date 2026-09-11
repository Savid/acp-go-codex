# acp-go-codex

`acp-go-codex` exposes the [Codex CLI](https://github.com/openai/codex) as an
[Agent Client Protocol](https://agentclientprotocol.com) agent. It runs one
`codex app-server` for the agent, opens one Codex thread per ACP session on
it, maps ACP requests onto the app-server protocol, and streams ACP session
updates back to the client.

Codex inherits the adapter's environment and keeps its rollouts in its own
home. A session started over ACP can be continued natively:

```sh
acp-go-codex           # host runs a session
codex resume <id>      # the ACP session id is the Codex thread id
```

## Install

```sh
go install github.com/savid/acp-go-codex/cmd/acp-go-codex@latest
```

Requires `codex` 0.153.4 or newer on `PATH` or named with `-path`.

## Run

```sh
acp-go-codex [-path codex] [-home DIR] [-scratch-dir DIR] [-model MODEL] [-seed-file rel=host]... [-debug]
```

| Flag | Meaning |
|---|---|
| `-path` | codex executable; a bare name is searched on `PATH` |
| `-home` | Codex home, passed as `CODEX_HOME`; empty inherits Codex's own resolution |
| `-scratch-dir` | parent for ephemeral adapter state; empty means the system temp directory |
| `-model` | default model for new sessions |
| `-seed-file` | `<relpath>=<hostpath>` written into Codex's home before the app-server launches; repeatable |
| `-debug` | debug logs to stderr |
| `-version` | print the adapter version |

OpenTelemetry exporters are configured from the standard `OTEL_*` variables.

## Embed

```go
err := codexacp.Serve(ctx, os.Stdin, os.Stdout,
    codexacp.WithHome("/srv/codex"),
    codexacp.WithSessionStore(store),
)
```

Options: `WithExecutablePath`, `WithHome`, `WithScratchDir`,
`WithInputHandoffRoot`, `WithDefaultModel`, `WithConfiguredModels`, `WithEnv`,
`WithCodexConfigOverrides`, `WithSeedFiles`, `WithSessionStore`,
`WithSessionStoreLoadTimeout`, `WithTurnTimeout`, `WithConcurrencyLimits`,
`WithImageLimits`, `WithLogger`, `WithTracerProvider`, `WithMeterProvider`,
`WithTextMapPropagator`, `WithAgentName`, `WithAgentTitle`,
`WithAgentVersion`.

`WithCodexConfigOverrides` passes `-c key=value` to the app-server; the
`shell_environment_policy` keyspace is reserved for session environments.

### Session options

`_meta.codex.options` on `session/new`, `session/load`, and `session/resume`,
or `WithSessionCodexOptions` from Go:

| Field | Meaning |
|---|---|
| `model` | model for the session |
| `env` | environment overlay for the session's thread |
| `extraPathDirs` | absolute directories prepended to the thread's `PATH`, in order |
| `outputSchema` | JSON schema every turn's final answer must satisfy; the parsed answer rides `_meta.codex.structuredOutput` on the prompt response |
| `effort` | reasoning effort |
| `serviceTier` | service tier |
| `personality` | personality |
| `approvalPolicy` | Codex's own approval policy, forwarded unchanged |
| `sandboxPolicy` | Codex's own sandbox policy, forwarded unchanged |

`_meta.codex.rawEvent.enabled` forwards every native app-server event for
the session's thread on the `_codex/rawEvent` notification.

### Config options

`session/set_config_option` accepts `model`, `mode` (`default`, `plan`),
`effort`, `service_tier`, and `personality`. Values forward to the next turn;
only `mode`, `effort`, and `personality` reject an empty value.

### Session store

`WithSessionStore` mirrors the thread's rollout rows under the main subpath
and the adapter's session record under `config`, format
`codex-rollout-jsonl-v1`. `session/load` and `session/resume` prefer the
rollout in Codex's home when it exists and materialize it from the store
otherwise.

## Development

```sh
make test
make lint
make audit
make test-integration-smoke   # needs codex installed, spends no tokens
make test-integration-live    # spends model tokens
```

Unit tests run the test binary as a scripted fake app-server and need no
installed codex, credentials, or network.
