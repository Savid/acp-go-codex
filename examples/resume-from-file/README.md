# Resume From File

This example imports Codex rollout JSONL through `_codex/session/import`, loads
the session through ACP, sends one prompt, then closes the session.

The bundled `session.jsonl` is a sanitized rollout captured from a real
`codex exec` turn, trimmed to the rows Codex needs for native path resume.

```sh
go run ./examples/resume-from-file -session-file ./examples/resume-from-file/session.jsonl
```
