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
	promptSession := &session{agent: agent, id: "s", cwd: "/tmp/project", codexThreadID: "thread", client: &runEventsClient{}}
	held := promptSession.turnQueue()
	held <- struct{}{}
	if resp, err := promptSession.Prompt(canceledContext(), TextPromptRequest("s", "test-turn", "hi")); err != nil || resp.StopReason != acp.StopReasonCancelled {
		t.Fatalf("canceled acquire resp=%#v err=%v", resp, err)
	}
	<-held

	promptSession.client = &runEventsClient{runErr: errors.New("not logged in")}
	if _, err := promptSession.Prompt(ctx, TextPromptRequest("s", "test-turn", "hi")); err == nil {
		t.Fatal("Prompt accepted RunTurn error")
	}

	agent.setAgentClient(&errorAgentClient{recordingAgentClient: newRecordingAgentClient(), updateErr: errors.New("update failed")})
	promptSession.client = &runEventsClient{events: []codex.Event{{Kind: codex.EventAgentMessageDelta, ThreadID: "thread", TurnID: "turn", Text: "hi"}}}
	if _, err := promptSession.Prompt(ctx, TextPromptRequest("s", "test-turn", "hi")); err == nil {
		t.Fatal("Prompt ignored update error")
	}
	promptSession.client = &runEventsClient{events: []codex.Event{{
		Kind:     codex.EventUsageUpdated,
		ThreadID: "thread",
		TurnID:   "turn",
		TokenUsage: codex.TokenUsage{
			Last: codex.Usage{InputTokens: 1, OutputTokens: 2},
		},
	}}}
	if _, err := promptSession.Prompt(ctx, TextPromptRequest("s", "test-turn", "hi")); err == nil {
		t.Fatal("Prompt ignored usage update error")
	}

	agent.setAgentClient(newRecordingAgentClient())
	promptSession.rawMessages = rawMessageConfig{enabled: true}
	promptSession.client = &runEventsClient{events: []codex.Event{{Kind: codex.EventRaw, ThreadID: "thread", TurnID: "turn", RawMethod: "raw", RawParams: json.RawMessage(`{"type":"event_msg"}`)}}}
	agent.setAgentClient(&extensionErrorClient{recordingAgentClient: newRecordingAgentClient()})
	if _, err := promptSession.Prompt(ctx, TextPromptRequest("s", "test-turn", "hi")); err == nil {
		t.Fatal("Prompt ignored raw extension error")
	}

	agent.setAgentClient(newRecordingAgentClient())
	promptSession.rawMessages = rawMessageConfig{}
	promptSession.clientDead = false
	promptSession.client = &runEventsClient{events: []codex.Event{{Kind: codex.EventError, ThreadID: "thread", TurnID: "turn", Err: errors.New("boom")}}}
	if _, err := promptSession.Prompt(ctx, TextPromptRequest("s", "test-turn", "hi")); err == nil {
		t.Fatal("Prompt ignored event error")
	}

	promptSession.clientDead = false
	promptSession.client = &runEventsClient{}
	if _, err := promptSession.Prompt(ctx, TextPromptRequest("s", "test-turn", "hi")); !isTurnFailure(err, codex.CauseTransport) {
		t.Fatalf("Prompt with closed event stream err=%v, want codex_turn_failed transport", err)
	}

	promptSession.rawMessages = rawMessageConfig{enabled: true}
	promptSession.clientDead = false
	promptSession.agent = NewAgent(WithSessionStore(NewInMemorySessionStore()))
	promptSession.agent.setAgentClient(newRecordingAgentClient())
	promptSession.client = &runEventsClient{events: []codex.Event{{Kind: codex.EventCompleted, ThreadID: "thread", TurnID: "turn"}}}
	promptSession.rolloutPath = filepath.Join(t.TempDir(), "missing.jsonl")
	if _, err := promptSession.Prompt(ctx, TextPromptRequest("s", "test-turn", "hi")); err == nil {
		t.Fatal("Prompt ignored final rollout mirror error")
	}

	empty := filepath.Join(t.TempDir(), "empty.jsonl")
	if err := os.WriteFile(empty, nil, 0o600); err != nil {
		t.Fatalf("write empty rollout: %v", err)
	}
	promptSession.rolloutPath = empty
	if err := promptSession.mirrorAndEmitRollout(ctx); err != nil {
		t.Fatalf("empty mirror returned error: %v", err)
	}

	huge := filepath.Join(t.TempDir(), "huge.jsonl")
	if err := os.WriteFile(huge, []byte(strings.Repeat("x", maxSessionImportLineBytes+1)), 0o600); err != nil {
		t.Fatalf("write huge rollout: %v", err)
	}
	promptSession.rolloutPath = huge
	if err := promptSession.mirrorAndEmitRollout(ctx); err == nil {
		t.Fatal("mirror accepted scanner error")
	}

	invalid := filepath.Join(t.TempDir(), "invalid.jsonl")
	if err := os.WriteFile(invalid, []byte("[]\n"), 0o600); err != nil {
		t.Fatalf("write invalid rollout: %v", err)
	}
	promptSession.rolloutPath = invalid
	if err := promptSession.mirrorAndEmitRollout(ctx); err == nil {
		t.Fatal("mirror accepted invalid rollout entry")
	}

	valid := filepath.Join(t.TempDir(), "valid.jsonl")
	if err := os.WriteFile(valid, []byte(`{"type":"event_msg"}`+"\n"), 0o600); err != nil {
		t.Fatalf("write valid rollout: %v", err)
	}
	promptSession.rolloutPath = valid
	promptSession.cwd = "/tmp/project"
	promptSession.agent = NewAgent(WithSessionStore(appendErrorStore{}))
	withRolloutAppendSettings(t, time.Second, []time.Duration{0})
	if err := promptSession.mirrorAndEmitRollout(ctx); err == nil {
		t.Fatal("mirror ignored store append error")
	}
	promptSession.agent = NewAgent()
	promptSession.agent.setAgentClient(&extensionErrorClient{recordingAgentClient: newRecordingAgentClient()})
	promptSession.rawMessages = rawMessageConfig{enabled: true}
	if err := promptSession.mirrorAndEmitRollout(ctx); err != nil {
		t.Fatalf("mirror returned error: %v", err)
	}
}

func TestSessionPromptCancelAndUpdateEdges(t *testing.T) {
	promptSession := &session{agent: NewAgent(), id: "s"}
	promptSession.setTurnID("")
	promptSession.cancelTurn()
	if promptSession.wasTurnCancelled() {
		t.Fatal("cancel without active turn marked canceled")
	}
	promptSession.setAccount(nil)
	if len(promptSession.accountMeta) != 0 {
		t.Fatal("empty account meta was stored")
	}
	if err := (&session{materializedPath: filepath.Join(t.TempDir(), "missing")}).Close(context.Background()); err != nil {
		t.Fatalf("close missing materialized path returned error: %v", err)
	}
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "child"), []byte("x"), 0o600); err != nil {
		t.Fatalf("write child: %v", err)
	}
	if err := (&session{materializedPath: dir}).Close(context.Background()); err == nil {
		t.Fatal("close ignored materialized remove error")
	}
	if update := eventUpdates(codex.Event{Kind: codex.EventWarning, Text: "warn"}); len(update) != 1 || update[0].AgentThoughtChunk == nil {
		t.Fatalf("warning update = %#v", update)
	}
	if update := completeToolUpdate(codex.ToolEvent{ID: "tool", Title: "Title", Content: "done"}); update.ToolCallUpdate == nil {
		t.Fatalf("complete update with title/content = %#v", update)
	}
}

func TestSessionPromptCancelAndAccountUpdate(t *testing.T) {
	agent := NewAgent()
	agent.setAgentClient(newRecordingAgentClient())
	cancelSession := &session{agent: agent, id: "s", cwd: "/tmp/project", codexThreadID: "thread"}
	cancelSession.client = &cancelDuringRunClient{session: cancelSession}
	resp, err := cancelSession.Prompt(context.Background(), TextPromptRequest("s", "test-turn", "hi"))
	if err != nil || resp.StopReason != acp.StopReasonCancelled {
		t.Fatalf("canceled event prompt resp=%#v err=%v", resp, err)
	}

	interactionSession := &session{agent: agent, id: "interaction"}
	interactionSession.beginTurn(context.Background(), "test-turn")
	interactionCtx, finishInteraction := interactionSession.beginInteraction(context.Background(), "input")
	interactionSession.mu.Lock()
	cancelInteractionTurn := interactionSession.cancel
	interactionSession.mu.Unlock()
	cancelInteractionTurn()
	select {
	case <-interactionCtx.Done():
	case <-time.After(time.Second):
		t.Fatal("interaction was not canceled when turn context ended")
	}
	finishInteraction()
	interactionSession.finishTurn()

	accountSession := &session{
		agent:         agent,
		id:            "acct",
		cwd:           "/tmp/project",
		codexThreadID: "thread",
		client: &runEventsClient{events: []codex.Event{
			{Kind: codex.EventAccountUpdated, Account: codex.Account{ID: "acct", Email: "new@example.com", PlanType: "pro", Raw: map[string]any{"accessToken": "secret"}}},
			{Kind: codex.EventCompleted, ThreadID: "thread", TurnID: "turn"},
		}},
	}
	if _, acctErr := accountSession.Prompt(context.Background(), TextPromptRequest("acct", "test-turn", "hi")); acctErr != nil {
		t.Fatalf("account update prompt returned error: %v", acctErr)
	}
	if accountSession.accountMeta["email"] != "new@example.com" || accountSession.accountMeta["accessToken"] != nil {
		t.Fatalf("account update meta = %#v", accountSession.accountMeta)
	}
}

func TestSessionPromptDedupesDeltas(t *testing.T) {
	dedupeAgent := NewAgent()
	dedupeConn := newRecordingAgentClient()
	dedupeAgent.setAgentClient(dedupeConn)
	dedupeSession := &session{
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
	if _, err := dedupeSession.Prompt(context.Background(), TextPromptRequest("dedupe", "test-turn", "hi")); err != nil {
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
}

func TestSessionPromptUsageUpdates(t *testing.T) {
	usageAgent := NewAgent()
	usageConn := newRecordingAgentClient()
	usageAgent.setAgentClient(usageConn)
	usageSession := &session{
		agent:         usageAgent,
		id:            "usage",
		cwd:           "/tmp/project",
		codexThreadID: "thread",
		client: &runEventsClient{events: []codex.Event{
			{
				Kind:     codex.EventUsageUpdated,
				ThreadID: "thread",
				TurnID:   "turn",
				Usage:    codex.Usage{InputTokens: 22143, CachedReadTokens: 6528, OutputTokens: 322, ReasoningOutputTokens: 157, TotalTokens: 22465},
				TokenUsage: codex.TokenUsage{
					Last:               codex.Usage{InputTokens: 22143, CachedReadTokens: 6528, OutputTokens: 322, ReasoningOutputTokens: 157, TotalTokens: 22465},
					Total:              codex.Usage{InputTokens: 23000, CachedReadTokens: 6528, OutputTokens: 400, ReasoningOutputTokens: 200, TotalTokens: 23400},
					ModelContextWindow: 258400,
				},
			},
			{Kind: codex.EventCompleted, ThreadID: "thread", TurnID: "turn"},
		}},
	}
	usageResp, err := usageSession.Prompt(context.Background(), TextPromptRequest("usage", "test-turn", "hi"))
	if err != nil {
		t.Fatalf("usage prompt returned error: %v", err)
	}
	if usageResp.Usage == nil ||
		usageResp.Usage.InputTokens != 22143 ||
		usageResp.Usage.OutputTokens != 322 ||
		usageResp.Usage.TotalTokens != 22465 ||
		usageResp.Usage.CachedReadTokens == nil ||
		*usageResp.Usage.CachedReadTokens != 6528 ||
		usageResp.Usage.ThoughtTokens == nil ||
		*usageResp.Usage.ThoughtTokens != 157 {
		t.Fatalf("prompt usage = %#v", usageResp.Usage)
	}
	if len(usageConn.updates) != 1 || usageConn.updates[0].Update.UsageUpdate == nil {
		t.Fatalf("usage updates = %#v", usageConn.updates)
	}
	usageUpdate := usageConn.updates[0].Update.UsageUpdate
	codexMeta, _ := usageUpdate.Meta[codexMetaKey].(map[string]any)
	usageMeta, _ := codexMeta[codexUsageMetaKey].(map[string]any)
	if usageUpdate.Used != 23400 ||
		usageUpdate.Size != 258400 ||
		usageMeta[usageInputTokensKey] != 22143 ||
		usageMeta[usageCachedReadTokensKey] != 6528 ||
		usageMeta[usageOutputTokensKey] != 322 ||
		usageMeta[usageReasoningOutputKey] != 157 ||
		usageMeta[usageTotalTokensKey] != 22465 {
		t.Fatalf("usage update=%#v meta=%#v", usageUpdate, usageMeta)
	}
	threadUsageMeta, _ := codexMeta[codexThreadUsageMetaKey].(map[string]any)
	if usageUpdate.Used != 23400 || threadUsageMeta[usageTotalTokensKey] != 23400 {
		t.Fatalf("thread usage update=%#v meta=%#v", usageUpdate, threadUsageMeta)
	}

	completedUsageConn := newRecordingAgentClient()
	usageAgent.setAgentClient(completedUsageConn)
	usageSession.client = &runEventsClient{events: []codex.Event{{
		Kind:     codex.EventCompleted,
		ThreadID: "thread",
		TurnID:   "turn",
		Usage:    codex.Usage{InputTokens: 1, OutputTokens: 2},
	}}}
	completedUsageResp, err := usageSession.Prompt(context.Background(), TextPromptRequest("usage", "test-turn", "hi"))
	if err != nil {
		t.Fatalf("completed usage prompt returned error: %v", err)
	}
	if completedUsageResp.Usage == nil || completedUsageResp.Usage.TotalTokens != 3 {
		t.Fatalf("completed usage response = %#v", completedUsageResp.Usage)
	}
	if len(completedUsageConn.updates) != 1 || completedUsageConn.updates[0].Update.UsageUpdate == nil {
		t.Fatalf("completed usage updates = %#v", completedUsageConn.updates)
	}
}

func TestSessionPromptRawRolloutTail(t *testing.T) {
	rollout := filepath.Join(t.TempDir(), "rollout.jsonl")
	if err := os.WriteFile(rollout, []byte("\n"+`{"type":"event_msg"}`+"\n"), 0o600); err != nil {
		t.Fatalf("write rollout: %v", err)
	}
	rawSession := &session{agent: NewAgent(), id: "raw", cwd: "/tmp/project", rolloutPath: rollout, rawMessages: rawMessageConfig{enabled: true}}
	if err := rawSession.mirrorAndEmitRollout(context.Background()); err != nil {
		t.Fatalf("mirror blank+valid rollout returned error: %v", err)
	}
	stop, done := rawSession.startRolloutTail(context.Background(), nil, nil)
	time.Sleep(150 * time.Millisecond)
	stop()
	<-done
}

func TestPromptUsesRolloutTaskCompleteFallback(t *testing.T) {
	withRolloutCompletionFallback(t, time.Millisecond)

	agent := NewAgent()
	conn := newRecordingAgentClient()
	agent.setAgentClient(conn)

	rollout := filepath.Join(t.TempDir(), "rollout.jsonl")
	if err := os.WriteFile(rollout, nil, 0o600); err != nil {
		t.Fatalf("write empty rollout: %v", err)
	}
	writeErr := make(chan error, 1)
	time.AfterFunc(20*time.Millisecond, func() {
		writeErr <- os.WriteFile(rollout, []byte(
			`{"type":"event_msg","payload":{"type":"agent_message","message":"1 + 1 = 2"}}`+"\n"+
				`{"type":"event_msg","payload":{"type":"task_complete"}}`+"\n",
		), 0o600)
	})
	defer func() {
		if err := <-writeErr; err != nil {
			t.Fatalf("write rollout rows: %v", err)
		}
	}()
	session := &session{
		agent:         agent,
		id:            "fallback",
		cwd:           "/tmp/project",
		codexThreadID: "thread",
		rolloutPath:   rollout,
		client:        &openRunEventsClient{},
	}

	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	resp, err := session.Prompt(ctx, TextPromptRequest("fallback", "test-turn", "prove it"))
	if err != nil || resp.StopReason != acp.StopReasonEndTurn {
		t.Fatalf("fallback prompt resp=%#v err=%v", resp, err)
	}
	if len(conn.updates) != 1 || conn.updates[0].Update.AgentMessageChunk == nil {
		t.Fatalf("fallback updates = %#v", conn.updates)
	}
	if session.completionRows != 2 || session.visibleRows != 2 {
		t.Fatalf("rollout cursors completion=%d visible=%d", session.completionRows, session.visibleRows)
	}
}

func TestPromptUsesImmediateRolloutTaskCompleteFallback(t *testing.T) {
	withRolloutCompletionFallback(t, 0)

	rollout := filepath.Join(t.TempDir(), "rollout.jsonl")
	if err := os.WriteFile(rollout, nil, 0o600); err != nil {
		t.Fatalf("write empty rollout: %v", err)
	}
	writeErr := make(chan error, 1)
	time.AfterFunc(20*time.Millisecond, func() {
		writeErr <- os.WriteFile(rollout, []byte(`{"type":"event_msg","payload":{"type":"task_complete"}}`+"\n"), 0o600)
	})
	defer func() {
		if err := <-writeErr; err != nil {
			t.Fatalf("write rollout rows: %v", err)
		}
	}()
	session := &session{
		agent:         NewAgent(),
		id:            "fallback",
		cwd:           "/tmp/project",
		codexThreadID: "thread",
		rolloutPath:   rollout,
		client:        &openRunEventsClient{},
	}

	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	resp, err := session.Prompt(ctx, TextPromptRequest("fallback", "test-turn", "prove it"))
	if err != nil || resp.StopReason != acp.StopReasonEndTurn {
		t.Fatalf("immediate fallback prompt resp=%#v err=%v", resp, err)
	}
	if session.completionRows != 1 {
		t.Fatalf("completion cursor = %d", session.completionRows)
	}
}

func TestPromptReturnsRolloutEventUpdateError(t *testing.T) {
	agent := NewAgent()
	agent.setAgentClient(&errorAgentClient{recordingAgentClient: newRecordingAgentClient(), updateErr: errors.New("update failed")})

	rollout := filepath.Join(t.TempDir(), "rollout.jsonl")
	if err := os.WriteFile(rollout, nil, 0o600); err != nil {
		t.Fatalf("write empty rollout: %v", err)
	}
	writeErr := make(chan error, 1)
	time.AfterFunc(20*time.Millisecond, func() {
		writeErr <- os.WriteFile(rollout, []byte(
			`{"type":"event_msg","payload":{"type":"agent_message","message":"visible"}}`+"\n",
		), 0o600)
	})
	defer func() {
		if err := <-writeErr; err != nil {
			t.Fatalf("write rollout rows: %v", err)
		}
	}()

	session := &session{
		agent:         agent,
		id:            "fallback",
		cwd:           "/tmp/project",
		codexThreadID: "thread",
		rolloutPath:   rollout,
		client:        &openRunEventsClient{},
	}

	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if _, err := session.Prompt(ctx, TextPromptRequest("fallback", "test-turn", "show it")); err == nil {
		t.Fatal("rollout event update error was ignored")
	}
}

func TestSandboxPolicyHelpers(t *testing.T) {
	if sandboxMode("workspace-write") != "workspace-write" {
		t.Fatal("string sandbox mode changed")
	}
	for _, tc := range []struct {
		policy map[string]any
		want   string
	}{
		{map[string]any{"type": "dangerFullAccess"}, "danger-full-access"},
		{map[string]any{"type": "readOnly"}, "read-only"},
		{map[string]any{"type": "workspaceWrite"}, "workspace-write"},
	} {
		if got := sandboxMode(tc.policy); got != tc.want {
			t.Fatalf("sandboxMode(%#v) = %#v", tc.policy, got)
		}
	}
	if sandboxMode(map[string]any{"type": "unknown"}) != nil || sandboxMode(123) != nil {
		t.Fatal("sandboxMode accepted unknown policy")
	}

	danger, _ := sandboxPolicy("danger-full-access").(map[string]any)
	if danger["type"] != "dangerFullAccess" {
		t.Fatalf("danger policy = %#v", danger)
	}
	readOnly, _ := sandboxPolicy("read-only").(map[string]any)
	if readOnly["type"] != "readOnly" || readOnly["networkAccess"] != false {
		t.Fatalf("read-only policy = %#v", readOnly)
	}
	workspace, _ := sandboxPolicy("workspace-write").(map[string]any)
	if workspace["type"] != "workspaceWrite" || workspace["writableRoots"] == nil {
		t.Fatalf("workspace policy = %#v", workspace)
	}
	if sandboxPolicy("custom") != "custom" || sandboxPolicy(123) != 123 {
		t.Fatal("sandboxPolicy did not preserve custom policies")
	}
	nilMap, _ := sandboxPolicy(map[string]any(nil)).(map[string]any)
	if nilMap != nil {
		t.Fatal("nil map policy was not preserved")
	}
	defaulted, _ := sandboxPolicy(map[string]any{"type": "workspaceWrite"}).(map[string]any)
	if defaulted["writableRoots"] == nil ||
		defaulted["networkAccess"] != false ||
		defaulted["excludeTmpdirEnvVar"] != false ||
		defaulted["excludeSlashTmp"] != false {
		t.Fatalf("workspace defaults = %#v", defaulted)
	}
	alreadyComplete := map[string]any{
		"type":                "workspace-write",
		"writableRoots":       []string{"/repo"},
		"networkAccess":       true,
		"excludeTmpdirEnvVar": true,
		"excludeSlashTmp":     true,
	}
	normalized, _ := sandboxPolicy(alreadyComplete).(map[string]any)
	if normalized["type"] != "workspaceWrite" || normalized["networkAccess"] != true {
		t.Fatalf("normalized policy = %#v", normalized)
	}
	otherMap := map[string]any{"type": "custom"}
	if got, _ := sandboxPolicy(otherMap).(map[string]any); got["type"] != "custom" {
		t.Fatalf("custom map policy = %#v", got)
	}
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
	usage := usageFromCodex(codex.Usage{InputTokens: 1, CachedReadTokens: 2, CachedWriteTokens: 3, OutputTokens: 4, ReasoningOutputTokens: 5})
	if usage.TotalTokens != 5 ||
		usage.CachedReadTokens == nil ||
		*usage.CachedReadTokens != 2 ||
		usage.CachedWriteTokens == nil ||
		*usage.CachedWriteTokens != 3 ||
		usage.ThoughtTokens == nil ||
		*usage.ThoughtTokens != 5 {
		t.Fatal("usage mapping failed")
	}
	if updates := usageUpdateFromCodex(codex.Usage{}); updates != nil {
		t.Fatalf("zero usage update = %#v", updates)
	}
	updates := usageUpdateFromCodex(codex.Usage{InputTokens: 1, CachedWriteTokens: 2, OutputTokens: 3})
	if len(updates) != 1 {
		t.Fatalf("usage update = %#v", updates)
	}
	updateMeta, _ := updates[0].UsageUpdate.Meta[codexMetaKey].(map[string]any)
	updateUsage, _ := updateMeta[codexUsageMetaKey].(map[string]any)
	if updateUsage[usageCachedWriteTokensKey] != 2 {
		t.Fatalf("cached write usage meta = %#v", updateUsage)
	}
}

func TestTokenUsageAndObserverBranches(t *testing.T) {
	usage := usageFromCodex(codex.Usage{InputTokens: 1, CachedReadTokens: 2, CachedWriteTokens: 3, OutputTokens: 4, ReasoningOutputTokens: 5})
	tokenUpdates := tokenUsageUpdateFromCodex(codex.TokenUsage{
		Last:               codex.Usage{InputTokens: 1, OutputTokens: 2},
		Total:              codex.Usage{InputTokens: 3, OutputTokens: 4},
		ModelContextWindow: 100,
	})
	if len(tokenUpdates) != 1 || tokenUpdates[0].UsageUpdate.Used != 7 || tokenUpdates[0].UsageUpdate.Size != 100 {
		t.Fatalf("token usage update = %#v", tokenUpdates)
	}
	var streamedUsage codex.Usage
	var streamedThreadUsage codex.Usage
	var streamedWindow int64
	firstUsageUpdates := usageUpdatesForEvent(codex.Event{Kind: codex.EventUsageUpdated, TokenUsage: codex.TokenUsage{Last: codex.Usage{InputTokens: 1, OutputTokens: 2}}}, &streamedUsage, &streamedThreadUsage, &streamedWindow)
	duplicateUsageUpdates := usageUpdatesForEvent(codex.Event{Kind: codex.EventUsageUpdated, TokenUsage: codex.TokenUsage{Last: codex.Usage{InputTokens: 1, OutputTokens: 2}}}, &streamedUsage, &streamedThreadUsage, &streamedWindow)
	if len(firstUsageUpdates) != 1 || duplicateUsageUpdates != nil {
		t.Fatalf("usage update dedupe first=%#v duplicate=%#v", firstUsageUpdates, duplicateUsageUpdates)
	}
	observerResult := promptResultForObserver(acp.PromptResponse{Usage: usage}, nil, "gpt")
	if observerResult.CachedReadTokens != 2 ||
		observerResult.CachedWriteTokens != 3 ||
		observerResult.ThoughtTokens != 5 ||
		observerResult.InputTokens != 1 ||
		observerResult.OutputTokens != 4 ||
		observerResult.TotalTokens != 5 {
		t.Fatalf("observer result = %#v", observerResult)
	}
	if structuredOutputMeta("", map[string]any{"type": "object"}) != nil || structuredOutputMeta(`{"ok":true}`, nil) != nil {
		t.Fatal("structured output emitted without schema/text")
	}
}

func TestPromptValueHelpers(t *testing.T) {
	if nullableString("") != nil || nullableString("x") != "x" {
		t.Fatal("nullableString failed")
	}
	if toolKind(codex.ToolEvent{Kind: "mcpToolCall"}) != acp.ToolKindOther || toolKind(codex.ToolEvent{Kind: "unknown"}) != acp.ToolKindOther {
		t.Fatal("toolKind special cases failed")
	}
	if stopReasonFromCodex(codex.StopReasonCancelled) != acp.StopReasonCancelled || stopReasonFromCodex(codex.StopReasonError) != acp.StopReasonEndTurn {
		t.Fatal("stopReasonFromCodex special cases failed")
	}
}

func TestRecordRawEmitFailure(t *testing.T) {
	recordSession := &session{agent: NewAgent(), id: "record"}
	recordSession.recordRawEmitFailure(context.Background(), nil)
	if recordSession.rawEmitFailures != 0 {
		t.Fatal("nil raw emit error advanced the counter")
	}

	recordSession.recordRawEmitFailure(context.Background(), errors.New("emit failed"))
	if recordSession.rawEmitFailures != 1 {
		t.Fatalf("raw emit failure counter = %d, want 1", recordSession.rawEmitFailures)
	}
}

// TestPromptContentFailsClosed pins the fail-closed prompt-content contract:
// empty prompts, audio blocks, and unknown or empty content blocks reject with
// the uniform unsupported/prompt shape, and images without data or a URI
// reject with the uniform prompt.image shape. Nothing is silently dropped.
func TestPromptContentFailsClosed(t *testing.T) {
	ctx := context.Background()
	promptSession := &session{agent: NewAgent(), id: "s", cwd: "/tmp/project", codexThreadID: "thread", client: &runEventsClient{}}

	for _, tt := range []struct {
		name      string
		prompt    []acp.ContentBlock
		wantField string
		wantError string
	}{
		{name: "audio block", prompt: []acp.ContentBlock{acp.AudioBlock("x", "audio/wav")}, wantField: "prompt", wantError: "unsupported"},
		{name: "empty prompt", prompt: nil, wantField: "prompt", wantError: "unsupported"},
		{name: "unknown block", prompt: []acp.ContentBlock{{}}, wantField: "prompt", wantError: "unsupported"},
		{name: "data-less image", prompt: []acp.ContentBlock{acp.ImageBlock("", "image/png")}, wantField: "prompt.image", wantError: "missing image data or uri"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			_, err := promptSession.Prompt(ctx, acp.PromptRequest{SessionId: "s", Prompt: tt.prompt, Meta: inboundRouteMeta("turn-1")})

			var reqErr *acp.RequestError
			if !errors.As(err, &reqErr) || reqErr.Code != -32602 {
				t.Fatalf("prompt error = %#v, want -32602 invalid params", err)
			}
			if data, ok := reqErr.Data.(map[string]any); !ok || data["error"] != tt.wantError || data["field"] != tt.wantField {
				t.Fatalf("prompt data = %#v, want %s/%s", reqErr.Data, tt.wantError, tt.wantField)
			}
		})
	}
}
