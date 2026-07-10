# Resume From File

This example reads a Codex rollout JSONL transcript from `session.jsonl` in this
directory into a `SessionStore`, loads the session through ACP so previous
interactions are replayed, then sends one no-tools smoke-test prompt in-process.
It denies tool permissions by default so a copied session cannot silently run
commands while you are checking resume behavior.

Use it with a real Codex rollout:

```sh
cd examples/resume-from-file
cp ~/.codex/sessions/<...>/<session-id>.jsonl ./session.jsonl
go run . -session <session-id> -cwd /absolute/path/to/project
```

If the JSONL includes a `session_meta` row (or top-level `session_id`/
`sessionId`), `-session` can be omitted and the id is inferred; `-cwd` likewise
defaults to the `session_meta` cwd or the current directory. Loading uses normal
ACP `session/load`, and the prompt uses normal ACP `session/prompt`.

Pass `-prompt "..."` to change the smoke-test turn, `-path` to point at a
specific `codex` CLI, and `-home` to choose the parent root for isolated Codex
session state.
