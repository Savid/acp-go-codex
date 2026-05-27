package codexacp

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/coder/acp-go-sdk"
	"github.com/savid/acp-go-codex/internal/codex"
)

func TestPromptRolloutRawAndPermissionEdges(t *testing.T) {
	ctx := context.Background()
	agent := NewAgent()
	session := &Session{agent: agent, id: "s", cwd: "/tmp/project", codexThreadID: "thread", client: &runEventsClient{}}
	held := session.turnQueue()
	held <- struct{}{}
	if resp, err := session.Prompt(canceledContext(), TextPromptRequest("s", "hi")); err != nil || resp.StopReason != acp.StopReasonCancelled {
		t.Fatalf("canceled acquire resp=%#v err=%v", resp, err)
	}
	<-held
	if _, err := session.Prompt(ctx, acp.PromptRequest{SessionId: "s", Prompt: []acp.ContentBlock{acp.AudioBlock("x", "audio/wav")}}); err == nil {
		t.Fatal("Prompt accepted audio block")
	}

	session.client = &runEventsClient{runErr: errors.New("not logged in")}
	if _, err := session.Prompt(ctx, TextPromptRequest("s", "hi")); err == nil {
		t.Fatal("Prompt accepted RunTurn error")
	}

	agent.setAgentClient(&errorAgentClient{recordingAgentClient: newRecordingAgentClient(), updateErr: errors.New("update failed")})
	session.client = &runEventsClient{events: []codex.Event{{Kind: codex.EventAgentMessageDelta, ThreadID: "thread", TurnID: "turn", Text: "hi"}}}
	if _, err := session.Prompt(ctx, TextPromptRequest("s", "hi")); err == nil {
		t.Fatal("Prompt ignored update error")
	}

	agent.setAgentClient(newRecordingAgentClient())
	session.rawMessages = rawMessageConfig{All: true}
	session.client = &runEventsClient{events: []codex.Event{{Kind: codex.EventRaw, ThreadID: "thread", TurnID: "turn", RawMethod: "raw", RawParams: json.RawMessage(`{"type":"event_msg"}`)}}}
	agent.setAgentClient(&extensionErrorClient{recordingAgentClient: newRecordingAgentClient()})
	if _, err := session.Prompt(ctx, TextPromptRequest("s", "hi")); err == nil {
		t.Fatal("Prompt ignored raw extension error")
	}

	agent.setAgentClient(newRecordingAgentClient())
	session.rawMessages = rawMessageConfig{}
	session.client = &runEventsClient{events: []codex.Event{{Kind: codex.EventError, ThreadID: "thread", TurnID: "turn", Err: errors.New("boom")}}}
	if _, err := session.Prompt(ctx, TextPromptRequest("s", "hi")); err == nil {
		t.Fatal("Prompt ignored event error")
	}

	session.rawMessages = rawMessageConfig{All: true}
	session.client = &runEventsClient{events: []codex.Event{{Kind: codex.EventCompleted, ThreadID: "thread", TurnID: "turn"}}}
	session.rolloutPath = filepath.Join(t.TempDir(), "missing.jsonl")
	if _, err := session.Prompt(ctx, TextPromptRequest("s", "hi")); err == nil {
		t.Fatal("Prompt ignored final rollout mirror error")
	}

	empty := filepath.Join(t.TempDir(), "empty.jsonl")
	if err := os.WriteFile(empty, nil, 0o600); err != nil {
		t.Fatalf("write empty rollout: %v", err)
	}
	session.rolloutPath = empty
	if err := session.mirrorAndEmitRollout(ctx); err != nil {
		t.Fatalf("empty mirror returned error: %v", err)
	}

	huge := filepath.Join(t.TempDir(), "huge.jsonl")
	if err := os.WriteFile(huge, []byte(strings.Repeat("x", maxSessionImportLineBytes+1)), 0o600); err != nil {
		t.Fatalf("write huge rollout: %v", err)
	}
	session.rolloutPath = huge
	if err := session.mirrorAndEmitRollout(ctx); err == nil {
		t.Fatal("mirror accepted scanner error")
	}

	invalid := filepath.Join(t.TempDir(), "invalid.jsonl")
	if err := os.WriteFile(invalid, []byte("[]\n"), 0o600); err != nil {
		t.Fatalf("write invalid rollout: %v", err)
	}
	session.rolloutPath = invalid
	if err := session.mirrorAndEmitRollout(ctx); err == nil {
		t.Fatal("mirror accepted invalid rollout entry")
	}

	valid := filepath.Join(t.TempDir(), "valid.jsonl")
	if err := os.WriteFile(valid, []byte(`{"type":"event_msg"}`+"\n"), 0o600); err != nil {
		t.Fatalf("write valid rollout: %v", err)
	}
	session.rolloutPath = valid
	session.cwd = "relative"
	session.agent = NewAgent(WithSessionStore(NewInMemorySessionStore()))
	if err := session.mirrorAndEmitRollout(ctx); err == nil {
		t.Fatal("mirror accepted relative cwd for store")
	}
	session.cwd = "/tmp/project"
	session.agent = NewAgent(WithSessionStore(appendErrorStore{}))
	withRolloutAppendSettings(t, time.Second, []time.Duration{0})
	if err := session.mirrorAndEmitRollout(ctx); err == nil {
		t.Fatal("mirror ignored store append error")
	}
	session.agent = NewAgent()
	session.agent.setAgentClient(&extensionErrorClient{recordingAgentClient: newRecordingAgentClient()})
	session.rawMessages = rawMessageConfig{All: true}
	if err := session.mirrorAndEmitRollout(ctx); err != nil {
		t.Fatalf("raw rollout extension error was fatal: %v", err)
	}
	if session.emittedRawRows != 0 {
		t.Fatal("failed raw rollout emission advanced cursor")
	}

	session.rawMessages = rawMessageConfig{Filters: []rawMessageFilter{{Type: "other"}}}
	if err := session.emitRawRolloutRow(ctx, SessionStoreEntry(`{"type":"event_msg"}`)); err != nil {
		t.Fatalf("filtered raw rollout row returned error: %v", err)
	}
}

func TestSessionPromptCancelAndUpdateEdges(t *testing.T) {
	session := &Session{agent: NewAgent(), id: "s"}
	session.setTurnID("")
	session.cancelTurn()
	if session.wasTurnCancelled() {
		t.Fatal("cancel without active turn marked canceled")
	}
	session.setAccount(nil)
	if len(session.accountMeta) != 0 {
		t.Fatal("empty account meta was stored")
	}
	if err := (&Session{materializedPath: filepath.Join(t.TempDir(), "missing")}).Close(context.Background()); err != nil {
		t.Fatalf("close missing materialized path returned error: %v", err)
	}
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "child"), []byte("x"), 0o600); err != nil {
		t.Fatalf("write child: %v", err)
	}
	if err := (&Session{materializedPath: dir}).Close(context.Background()); err == nil {
		t.Fatal("close ignored materialized remove error")
	}
	bridgeDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(bridgeDir, "child"), []byte("x"), 0o600); err != nil {
		t.Fatalf("write bridge child: %v", err)
	}
	if err := (&Session{client: newSpyCodexClient(), mcpBridge: &mcpSessionBridge{done: make(chan struct{}), tokenFile: bridgeDir, conns: make(map[*mcpBridgeConn]struct{})}}).Close(context.Background()); err == nil {
		t.Fatal("close with client ignored MCP bridge error")
	}
	bridgeDir = t.TempDir()
	if err := os.WriteFile(filepath.Join(bridgeDir, "child"), []byte("x"), 0o600); err != nil {
		t.Fatalf("write bridge child: %v", err)
	}
	if err := (&Session{mcpBridge: &mcpSessionBridge{done: make(chan struct{}), tokenFile: bridgeDir, conns: make(map[*mcpBridgeConn]struct{})}}).Close(context.Background()); err == nil {
		t.Fatal("close without client ignored MCP bridge error")
	}
	if update := eventUpdates(codex.Event{Kind: codex.EventWarning, Text: "warn"}); len(update) != 1 || update[0].AgentThoughtChunk == nil {
		t.Fatalf("warning update = %#v", update)
	}
	if update := completeToolUpdate(codex.ToolEvent{ID: "tool", Title: "Title", Content: "done"}); update.ToolCallUpdate == nil {
		t.Fatalf("complete update with title/content = %#v", update)
	}

	agent := NewAgent()
	agent.setAgentClient(newRecordingAgentClient())
	cancelSession := &Session{agent: agent, id: "s", cwd: "/tmp/project", codexThreadID: "thread"}
	cancelSession.client = &cancelDuringRunClient{session: cancelSession}
	resp, err := cancelSession.Prompt(context.Background(), TextPromptRequest("s", "hi"))
	if err != nil || resp.StopReason != acp.StopReasonCancelled {
		t.Fatalf("canceled event prompt resp=%#v err=%v", resp, err)
	}

	accountSession := &Session{
		agent:         agent,
		id:            "acct",
		cwd:           "/tmp/project",
		codexThreadID: "thread",
		client: &runEventsClient{events: []codex.Event{
			{Kind: codex.EventAccountUpdated, Account: codex.Account{ID: "acct", Email: "new@example.com", PlanType: "pro", Raw: map[string]any{"accessToken": "secret"}}},
			{Kind: codex.EventCompleted, ThreadID: "thread", TurnID: "turn"},
		}},
	}
	if _, err := accountSession.Prompt(context.Background(), TextPromptRequest("acct", "hi")); err != nil {
		t.Fatalf("account update prompt returned error: %v", err)
	}
	if accountSession.accountMeta["email"] != "new@example.com" || accountSession.accountMeta["accessToken"] != nil {
		t.Fatalf("account update meta = %#v", accountSession.accountMeta)
	}

	dedupeAgent := NewAgent()
	dedupeConn := newRecordingAgentClient()
	dedupeAgent.setAgentClient(dedupeConn)
	dedupeSession := &Session{
		agent:         dedupeAgent,
		id:            "dedupe",
		cwd:           "/tmp/project",
		codexThreadID: "thread",
		client: &runEventsClient{events: []codex.Event{
			{Kind: codex.EventAgentMessageDelta, ItemID: "msg", ThreadID: "thread", TurnID: "turn", Text: "hello"},
			{Kind: codex.EventAgentMessageDelta, ItemID: "msg", ThreadID: "thread", TurnID: "turn", Text: "hello", Completed: true},
			{Kind: codex.EventAgentMessageDelta, ThreadID: "thread", TurnID: "turn", Text: "world"},
			{Kind: codex.EventAgentMessageDelta, ThreadID: "thread", TurnID: "turn", Text: "helloworld", Completed: true},
			{Kind: codex.EventReasoningDelta, ItemID: "why", ThreadID: "thread", TurnID: "turn", Text: "thinking"},
			{Kind: codex.EventReasoningDelta, ItemID: "why", ThreadID: "thread", TurnID: "turn", Text: "thinking", Completed: true},
			{Kind: codex.EventCompleted, ThreadID: "thread", TurnID: "turn"},
		}},
	}
	if _, err := dedupeSession.Prompt(context.Background(), TextPromptRequest("dedupe", "hi")); err != nil {
		t.Fatalf("dedupe prompt returned error: %v", err)
	}
	messageUpdates := 0
	thoughtUpdates := 0
	for _, update := range dedupeConn.updates {
		if update.Update.AgentMessageChunk != nil {
			messageUpdates++
		}
		if update.Update.AgentThoughtChunk != nil {
			thoughtUpdates++
		}
	}
	if messageUpdates != 2 || thoughtUpdates != 1 {
		t.Fatalf("deduped updates message=%d thought=%d all=%#v", messageUpdates, thoughtUpdates, dedupeConn.updates)
	}
	if event := dedupeCompletedAggregateTextEvent(codex.Event{Kind: codex.EventAgentMessageDelta, Text: "same", Completed: true}, "same"); event.Text != "" {
		t.Fatalf("aggregate duplicate was not suppressed: %#v", event)
	}

	rollout := filepath.Join(t.TempDir(), "rollout.jsonl")
	if err := os.WriteFile(rollout, []byte("\n"+`{"type":"event_msg"}`+"\n"), 0o600); err != nil {
		t.Fatalf("write rollout: %v", err)
	}
	rawSession := &Session{agent: NewAgent(), id: "raw", cwd: "/tmp/project", rolloutPath: rollout, rawMessages: rawMessageConfig{All: true}}
	if err := rawSession.mirrorAndEmitRollout(context.Background()); err != nil {
		t.Fatalf("mirror blank+valid rollout returned error: %v", err)
	}
	if err := rawSession.emitRawRolloutRow(context.Background(), SessionStoreEntry(`{"type":"event_msg"}`)); err != nil {
		t.Fatalf("raw rollout row without conn returned error: %v", err)
	}
	stop, done := rawSession.startRolloutTail(context.Background())
	time.Sleep(150 * time.Millisecond)
	stop()
	<-done
}

func TestEventUpdateHelpers(t *testing.T) {
	updates := eventUpdates(codex.Event{Kind: codex.EventToolStarted, Tool: codex.ToolEvent{ID: "tool", Kind: "commandExecution", Title: "Run"}})
	if len(updates) != 1 || updates[0].ToolCall == nil {
		t.Fatalf("tool start updates = %#v", updates)
	}
	updates = eventUpdates(codex.Event{Kind: codex.EventToolDelta, Tool: codex.ToolEvent{ID: "tool"}, Text: "out"})
	if len(updates) != 1 || updates[0].ToolCallUpdate == nil {
		t.Fatalf("tool delta updates = %#v", updates)
	}
	updates = eventUpdates(codex.Event{Kind: codex.EventToolCompleted, Tool: codex.ToolEvent{ID: "tool", Kind: "fileChange", Content: "done"}})
	if len(updates) != 1 || updates[0].ToolCallUpdate == nil {
		t.Fatalf("tool complete updates = %#v", updates)
	}
	updates = eventUpdates(codex.Event{Kind: codex.EventDiffUpdated, Diff: "diff"})
	if len(updates) != 1 || updates[0].ToolCallUpdate == nil {
		t.Fatalf("diff updates = %#v", updates)
	}
	if toolKind(codex.ToolEvent{Kind: "shell"}) != acp.ToolKindExecute || toolKind(codex.ToolEvent{Kind: "patch"}) != acp.ToolKindEdit {
		t.Fatal("tool kind mapping failed")
	}
	meta := structuredOutputMeta(`{"ok":true}`, map[string]any{"type": "object"})
	if meta[codexMetaKey] == nil || meta["claude"] != nil {
		t.Fatalf("structured meta = %#v", meta)
	}
	if structuredOutputMeta("not-json", map[string]any{"type": "object"}) != nil {
		t.Fatal("invalid structured output emitted meta")
	}
}

func TestEventUpdateEmptyAndFallbackBranches(t *testing.T) {
	if eventUpdates(codex.Event{Kind: codex.EventAgentMessageDelta}) != nil {
		t.Fatal("empty agent delta emitted update")
	}
	if eventUpdates(codex.Event{Kind: codex.EventReasoningDelta}) != nil {
		t.Fatal("empty reasoning delta emitted update")
	}
	if eventUpdates(codex.Event{Kind: codex.EventDiffUpdated}) != nil {
		t.Fatal("empty diff emitted update")
	}
	if eventUpdates(codex.Event{Kind: codex.EventWarning}) != nil {
		t.Fatal("empty warning emitted update")
	}
	if eventUpdates(codex.Event{Kind: codex.EventError}) != nil {
		t.Fatal("error event emitted direct update")
	}
	if planUpdate(nil) != nil {
		t.Fatal("empty plan emitted update")
	}
	if updates := toolDeltaUpdate(codex.ToolEvent{ID: "tool", Content: "fallback"}, ""); len(updates) != 1 {
		t.Fatalf("tool content fallback updates = %#v", updates)
	}
	if toolDeltaUpdate(codex.ToolEvent{}, "") != nil {
		t.Fatal("empty tool delta emitted update")
	}
	if update := completeToolUpdate(codex.ToolEvent{ID: "tool"}); update.ToolCallUpdate == nil {
		t.Fatalf("complete tool update = %#v", update)
	}
	if planStatus(codex.PlanStepPending) != acp.PlanEntryStatusPending {
		t.Fatal("pending plan status did not map")
	}
	if stopReasonFromCodex(codex.StopReasonCancelled) != acp.StopReasonCancelled || stopReasonFromCodex(codex.StopReasonError) != acp.StopReasonEndTurn {
		t.Fatal("stop reason mapping changed")
	}
	if usageFromCodex(codex.Usage{}) != nil {
		t.Fatal("zero usage emitted usage")
	}
	if usageFromCodex(codex.Usage{InputTokens: 1, OutputTokens: 2, TotalTokens: 3}).TotalTokens != 3 {
		t.Fatal("usage mapping failed")
	}
	if structuredOutputMeta("", map[string]any{"type": "object"}) != nil || structuredOutputMeta(`{"ok":true}`, nil) != nil {
		t.Fatal("structured output emitted without schema/text")
	}
}
