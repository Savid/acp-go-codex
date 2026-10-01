Captured from Codex CLI 0.159.3 (`codex app-server`) on 2026-10-02.

`response-ids.jsonl` is the rollout of one thread an installed app-server ran
through an OpenRouter Responses gateway: a turn answered by one response, and
a turn whose first response started a shell command and whose second
answered. Each `token_usage_record` names the `gen-…` id the gateway returned
for its response, the same id the response's `rawResponse/completed` carried.
The session meta, developer and environment context, world state, turn
context, and thread settings rows are left out; the working directory is
normalised to `/workspace`; the rest is unchanged.
