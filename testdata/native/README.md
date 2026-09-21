Captured from Codex CLI 0.155.1 (`codex app-server`) on 2026-09-20.

The fixture holds the app-server notifications a thread receives while no
turn is in flight. An installed `codex app-server --listen stdio:// --disable
plugins` ran in a temporary working directory with the operator's own login;
after `initialize`, `thread/resume` of a thread whose one turn (`1+1`) had
completed in an earlier app-server produced, with no `turn/start`, the
thread-scoped `mcpServer/startupStatus/updated`, `thread/status/changed`,
`thread/tokenUsage/updated`, and `thread/goal/cleared` records kept here.
Agent-level `remoteControl/status/changed` and `deprecationNotice` records
carry no thread id and are left out.

No supported path started thread work outside a client turn: nothing arrived
in the 30 s after `turn/completed`; a native `codex exec resume <thread> 1+1`
against the app-server's own thread was refused (`thread-store conflict:
thread already has an active writer`); and a fresh app-server idle for 180 s
after `thread/resume` emitted nothing further. The between-prompt records are
session-scoped: the test proves they are delivered as raw events with no
prompt in flight and open nothing.

The live thread id is normalised to `fixture-thread`; the working directory is
normalised to `/workspace`; payloads, turn ids, and usage values are otherwise
unchanged.
