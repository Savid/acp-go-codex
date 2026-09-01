package codexacp

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/coder/acp-go-sdk"
	"github.com/savid/acp-go-codex/internal/codex"
)

func TestRolloutReplayResponseItemVariants(t *testing.T) {
	entries := []SessionStoreEntry{
		SessionStoreEntry(`{"type":"response_item","payload":{"type":"message","role":"user","content":[{"type":"input_text","text":"user"}]}}`),
		SessionStoreEntry(`{"type":"response_item","payload":{"type":"message","role":"assistant","content":[{"type":"output_text","text":"agent"}]}}`),
		SessionStoreEntry(`{"type":"response_item","payload":{"type":"reasoning","summary":"why"}}`),
		SessionStoreEntry(`{"type":"response_item","payload":{"type":"custom_tool_call","name":"apply_patch","call_id":"patch-1","input":"*** Begin Patch"}}`),
		SessionStoreEntry(`{"type":"response_item","payload":{"type":"custom_tool_call_output","call_id":"patch-1","output":{"ok":true}}}`),
		SessionStoreEntry(`{"type":"response_item","payload":{"type":"local_shell_call","call_id":"shell-1","status":"completed","action":{"exec":{"command":["git","status"]}}}}`),
		SessionStoreEntry(`{"type":"response_item","payload":{"type":"image_generation_call","id":"img-1","status":"failed","revised_prompt":"draw"}}`),
	}
	updates, err := rolloutReplayUpdates(entries)
	if err != nil {
		t.Fatalf("rolloutReplayUpdates returned error: %v", err)
	}
	var users, agents, thoughts, starts, outputs int
	for _, update := range updates {
		if update.UserMessageChunk != nil {
			users++
		}
		if update.AgentMessageChunk != nil {
			agents++
		}
		if update.AgentThoughtChunk != nil {
			thoughts++
		}
		if update.ToolCall != nil {
			starts++
		}
		if update.ToolCallUpdate != nil {
			outputs++
		}
	}
	if users != 1 || agents != 1 || thoughts != 1 || starts != 2 || outputs != 1 {
		t.Fatalf("counts users=%d agents=%d thoughts=%d starts=%d outputs=%d updates=%#v", users, agents, thoughts, starts, outputs, updates)
	}

	if _, err := rolloutReplayUpdates([]SessionStoreEntry{SessionStoreEntry(`{}`)}); err == nil {
		t.Fatal("invalid rollout row succeeded")
	}
	if _, err := rolloutReplayUpdates([]SessionStoreEntry{SessionStoreEntry(`bad`)}); err == nil {
		t.Fatal("bad rollout JSON succeeded")
	}
}

func TestRolloutNativeThreadID(t *testing.T) {
	entries := []SessionStoreEntry{
		SessionStoreEntry(`bad`),
		SessionStoreEntry(`{"type":"event_msg","payload":{"id":"ignored"}}`),
		SessionStoreEntry(`{"type":"session_meta","payload":{}}`),
		SessionStoreEntry(`{"type":"session_meta","payload":{"id":"native-thread"}}`),
	}
	if id := rolloutNativeThreadID(entries); id != "native-thread" {
		t.Fatalf("native thread id = %q", id)
	}
	if id := rolloutNativeThreadID([]SessionStoreEntry{SessionStoreEntry(`{"type":"session_meta","payload":{}}`)}); id != "" {
		t.Fatalf("empty native thread id = %q", id)
	}
}

func TestReplayCommandAndTextHelpers(t *testing.T) {
	if commandText([]any{"a", "b"}) != "a b" || commandText([]string{"c", "d"}) != "c d" || commandText("x") != "x" || commandText(1) != "" {
		t.Fatal("commandText failed")
	}
	if textFromAny(map[string]any{"ok": true}) != `{"ok":true}` {
		t.Fatal("textFromAny map failed")
	}
}

func TestReplayAdditionalBranches(t *testing.T) {
	if _, err := decodeRolloutRow(SessionStoreEntry(" ")); err == nil {
		t.Fatal("decodeRolloutRow accepted empty row")
	}
	if _, err := decodeRolloutRow(SessionStoreEntry(`{"type":"event_msg"}`)); err == nil {
		t.Fatal("decodeRolloutRow accepted a missing payload")
	}
	row, err := decodeRolloutRow(SessionStoreEntry(`{"type":"event_msg","payload":{}}`))
	if err != nil || row.Payload == nil {
		t.Fatalf("decodeRolloutRow valid row=%#v err=%v", row, err)
	}
	for _, invalid := range []SessionStoreEntry{
		SessionStoreEntry(`{"type":"event_msg","type":"event_msg","payload":{}}`),
		SessionStoreEntry(`{"type":"event_msg","payload":{"nested":1,"nested":2}}`),
		SessionStoreEntry(`{"type":"event_msg","payload":{},"unknown":true}`),
		SessionStoreEntry(`{"type":"event_msg","payload":{}} {}`),
		SessionStoreEntry(`{"timestamp":1,"type":"event_msg","payload":{}}`),
	} {
		if _, err := decodeRolloutRow(invalid); err == nil {
			t.Fatalf("decodeRolloutRow accepted %s", invalid)
		}
	}
	if updates, err := rolloutReplayUpdates([]SessionStoreEntry{SessionStoreEntry(`{"payload":{}}`)}); err == nil || updates != nil {
		t.Fatalf("rolloutReplayUpdates invalid row updates=%#v err=%v", updates, err)
	}

	if replayEventMsg(map[string]any{"type": "context_compacted"})[0].AgentThoughtChunk == nil {
		t.Fatal("context_compacted fallback did not emit thought")
	}
	if replayEventMsg(map[string]any{"type": "unknown"}) != nil {
		t.Fatal("unknown event message emitted update")
	}
	if replayResponseItem(map[string]any{"type": "message", "role": "user"}, replayFallbacks{messageUser: true}) != nil {
		t.Fatal("empty message response item emitted update")
	}
	if replayResponseItem(map[string]any{"type": "reasoning"}, replayFallbacks{}) != nil {
		t.Fatal("reasoning without fallback emitted update")
	}
	if replayResponseItem(map[string]any{"type": "unknown"}, replayFallbacks{}) != nil {
		t.Fatal("unknown response item emitted update")
	}
	if replayToolStart(map[string]any{"type": "custom"}, "", acp.ToolKindOther, acp.ToolCallStatusPending, nil).ToolCall == nil {
		t.Fatal("replayToolStart without title failed")
	}
	if replayToolOutput(map[string]any{"output": "missing id"}) != nil {
		t.Fatal("replayToolOutput without id emitted update")
	}
	if replayLocalShellCall(map[string]any{"status": "running"}).ToolCall.Status != acp.ToolCallStatusFailed {
		t.Fatal("replayLocalShellCall running status did not become failed")
	}
	if replayStatus("pending") != acp.ToolCallStatusPending {
		t.Fatal("replayStatus pending failed")
	}
	if responseItemText(map[string]any{"content": []any{"a", map[string]any{"summary_text": "b"}}}) != "ab" {
		t.Fatal("responseItemText content failed")
	}
	if commandText([]string{"go", "test"}) != "go test" || commandText([]any{"go", "test"}) != "go test" || commandText(7) != "" {
		t.Fatal("commandText failed")
	}
	if textFromAny(nil) != "" || textFromAny(func() {}) == "" {
		t.Fatal("textFromAny nil/fallback failed")
	}
	if firstNonNil(nil, nil) != nil {
		t.Fatal("firstNonNil all nil failed")
	}

	replaySession := &session{agent: NewAgent(), id: "s"}
	if err := replaySession.replayRollout(context.Background(), []SessionStoreEntry{SessionStoreEntry(`{"payload":{}}`)}); err == nil {
		t.Fatal("replayRollout invalid row succeeded")
	}
	updateErr := errors.New("update failed")
	errorAgent := NewAgent()
	errorAgent.setAgentClient(&errorAgentClient{recordingAgentClient: newRecordingAgentClient(), updateErr: updateErr})
	errorSession := &session{agent: errorAgent, id: "s"}
	if err := errorSession.replayRollout(context.Background(), []SessionStoreEntry{SessionStoreEntry(`{"type":"event_msg","payload":{"type":"agent_message","message":"hello"}}`)}); !errors.Is(err, updateErr) {
		t.Fatalf("replayRollout update error = %v", err)
	}
}

func TestLoadSessionReplaysRolloutHistory(t *testing.T) {
	store := NewInMemorySessionStore()
	client := newSpyCodexClient()
	agent := NewAgent(
		WithSessionStore(store),
		withClientFactory(func(context.Context, codex.Options) (codex.Client, error) { return client, nil }),
	)
	conn := newRecordingAgentClient()
	agent.setAgentClient(conn)
	ctx := context.Background()
	cwd := t.TempDir()
	entries := []SessionStoreEntry{
		SessionStoreEntry(`{"type":"session_meta","payload":{"id":"stored-session"}}`),
		SessionStoreEntry(`{"type":"event_msg","payload":{"type":"user_message","message":"hi"}}`),
		SessionStoreEntry(`{"type":"event_msg","payload":{"type":"agent_reasoning","text":"thinking"}}`),
		SessionStoreEntry(`{"type":"event_msg","payload":{"type":"agent_message","message":"hello"}}`),
		SessionStoreEntry(`{"type":"response_item","payload":{"type":"function_call","name":"shell_command","call_id":"call-1","arguments":{"command":"pwd"}}}`),
		SessionStoreEntry(`{"type":"response_item","payload":{"type":"function_call_output","call_id":"call-1","output":"done"}}`),
		SessionStoreEntry(`{"type":"response_item","payload":{"type":"web_search_call","id":"search-1","status":"completed"}}`),
		SessionStoreEntry(`{"type":"compacted","payload":{"message":"compacted"}}`),
	}
	if err := store.Append(ctx, SessionKey{SessionID: "stored-session"}, entries); err != nil {
		t.Fatalf("store Append returned error: %v", err)
	}

	if _, err := agent.LoadSession(ctx, LoadSessionRequest("stored-session", cwd)); err != nil {
		t.Fatalf("LoadSession returned error: %v", err)
	}
	if len(conn.updates) < 6 {
		t.Fatalf("updates = %#v", conn.updates)
	}
	var sawUser, sawAgent, sawThought, sawTool, sawToolOutput bool
	for _, notification := range conn.updates {
		update := notification.Update
		if update.UserMessageChunk != nil {
			sawUser = true
		}
		if update.AgentMessageChunk != nil {
			sawAgent = true
		}
		if update.AgentThoughtChunk != nil {
			sawThought = true
		}
		if update.ToolCall != nil {
			sawTool = true
		}
		if update.ToolCallUpdate != nil {
			sawToolOutput = true
		}
	}
	if !sawUser || !sawAgent || !sawThought || !sawTool || !sawToolOutput {
		t.Fatalf("missing replay update types: user=%v agent=%v thought=%v tool=%v output=%v updates=%#v", sawUser, sawAgent, sawThought, sawTool, sawToolOutput, conn.updates)
	}
}

func TestReplayValueHelpers(t *testing.T) {
	if replayStatus("failed") != acp.ToolCallStatusFailed || replayStatus("in_progress") != acp.ToolCallStatusInProgress || replayStatus("done") != acp.ToolCallStatusCompleted {
		t.Fatal("replayStatus failed")
	}
	if !strings.Contains(textFromAny([]any{"a", map[string]any{"text": "b"}}), `"text":"b"`) {
		t.Fatal("textFromAny slice failed")
	}
	if stringFromAny(fmt.Stringer(stringer("x"))) != "x" {
		t.Fatal("stringFromAny did not use Stringer")
	}
	if mapFromAny("bad") != nil {
		t.Fatal("mapFromAny accepted non-map")
	}
}
