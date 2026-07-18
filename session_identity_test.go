package codexacp

import (
	"context"
	"errors"
	"testing"
)

func TestRolloutTerminalIdentityUsesFinalAssistantAndReplaysEmptyAssistant(t *testing.T) {
	entries := []SessionStoreEntry{
		SessionStoreEntry(`{"type":"event_msg","payload":{"type":"task_started","turn_id":"turn-1"}}`),
		SessionStoreEntry(`{"type":"response_item","payload":{"type":"message","role":"assistant","id":"message-before","content":[{"type":"output_text","text":"before"}]}}`),
		SessionStoreEntry(`{"type":"response_item","payload":{"type":"message","role":"assistant","id":"message-empty","content":[]}}`),
		SessionStoreEntry(`{"type":"event_msg","payload":{"type":"task_complete","turn_id":"turn-1"}}`),
	}
	want := nativeTurnIdentity{turnID: "turn-1", messageID: "message-empty"}
	if got := rolloutNativeTerminalIdentity(entries); got != want {
		t.Fatalf("terminal identity = %#v, want %#v", got, want)
	}
	trailingTurnID := []SessionStoreEntry{
		SessionStoreEntry(`{"type":"response_item","payload":{"type":"message","role":"assistant","id":"message-trailing"}}`),
		SessionStoreEntry(`{"type":"event_msg","payload":{"type":"task_complete","turn_id":"turn-trailing"}}`),
	}
	if got := rolloutNativeTerminalIdentity(trailingTurnID); got != (nativeTurnIdentity{turnID: "turn-trailing", messageID: "message-trailing"}) {
		t.Fatalf("trailing turn identity = %#v", got)
	}
	completionOnlyAfterMessage := append(cloneStoreEntries(entries),
		SessionStoreEntry(`{"type":"event_msg","payload":{"type":"task_started","turn_id":"turn-2"}}`),
		SessionStoreEntry(`{"type":"event_msg","payload":{"type":"task_complete","turn_id":"turn-2"}}`),
	)
	if got := rolloutNativeTerminalIdentity(completionOnlyAfterMessage); got != (nativeTurnIdentity{turnID: "turn-2"}) {
		t.Fatalf("completion-only identity = %#v", got)
	}

	agent := NewAgent()
	conn := newRecordingAgentClient()
	agent.setAgentClient(conn)
	s := &session{agent: agent, id: "replay"}
	if err := s.replayRollout(context.Background(), entries); err != nil {
		t.Fatalf("replayRollout returned error: %v", err)
	}
	if got := lastNotificationNativeIdentity(conn.updates); got != want {
		t.Fatalf("replayed terminal identity = %#v, want %#v; updates=%#v", got, want, conn.updates)
	}

	errorAgent := NewAgent()
	updateErr := errors.New("identity update failed")
	errorAgent.setAgentClient(&errorAgentClient{recordingAgentClient: newRecordingAgentClient(), updateErr: updateErr})
	errorSession := &session{agent: errorAgent, id: "replay-error"}
	errorEntries := []SessionStoreEntry{
		SessionStoreEntry(`bad`),
		SessionStoreEntry(`{"type":"response_item","payload":{"type":"message","role":"assistant","id":"empty","content":[]}}`),
	}
	if got := rolloutNativeTerminalIdentity(errorEntries); got.messageID != "empty" {
		t.Fatalf("terminal inspection after invalid row = %#v", got)
	}
	if err := errorSession.replayRollout(context.Background(), errorEntries); err == nil {
		// The invalid first row exercises tolerant terminal inspection, while
		// strict replay decoding still rejects the source before any update.
		t.Fatal("replayRollout accepted invalid source")
	}
	if err := errorSession.replayRollout(context.Background(), errorEntries[1:]); !errors.Is(err, updateErr) {
		t.Fatalf("identity replay update error = %v, want %v", err, updateErr)
	}
}

func TestNativeIdentityMetaPreservesStructuredOutput(t *testing.T) {
	meta := mergePromptResponseMeta(
		structuredOutputMeta(`{"ok":true}`, map[string]any{"type": "object"}),
		nativeTurnIdentity{turnID: "turn", messageID: "message"},
	)
	codexMeta, _ := meta[codexMetaKey].(map[string]any)
	structured, _ := codexMeta[structuredOutputMetaKey].(map[string]any)
	if structured["ok"] != true || codexMeta[codexTurnIDMetaKey] != "turn" || codexMeta[codexMessageIDMetaKey] != "message" {
		t.Fatalf("merged prompt response meta = %#v", meta)
	}
}
