# Minimal Client

Launches `acp-go-codex` as a subprocess, creates one session in the current
directory, sends one prompt, and prints the streamed answer. Every permission
request is allowed once.

```sh
go run ./examples/minimal-client "Reply with a short hello from ACP"
```

A local `codex` must be installed and authenticated; the session inherits your
environment and Codex's home exactly as running `codex` in this directory would.
