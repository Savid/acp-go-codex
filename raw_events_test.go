package codexacp

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/savid/acp-go-codex/internal/codex"
)

func TestRawMessageConfigFilters(t *testing.T) {
	meta := map[string]any{codexMetaKey: map[string]any{
		emitRawSDKMessagesKey: []any{
			map[string]any{"type": "response_item", "payloadType": "message", "payloadRole": "assistant"},
			map[string]any{"type": ""},
		},
	}}
	config := rawMessageConfigFromMeta(meta)
	if !config.Enabled() {
		t.Fatal("raw config disabled")
	}
	if !config.ShouldEmit(map[string]any{"type": "response_item", "payload": map[string]any{"type": "message", "role": "assistant"}}) {
		t.Fatal("expected filter match")
	}
	if config.ShouldEmit(map[string]any{"type": "response_item", "payload": map[string]any{"type": "message", "role": "user"}}) {
		t.Fatal("unexpected filter match")
	}
	all, ok := rawMessageConfigFromValue(true)
	if !ok || !all.ShouldEmit(map[string]any{"type": "anything"}) {
		t.Fatalf("bool raw config = %#v ok=%v", all, ok)
	}
	if decodedRawEvent(json.RawMessage(`{"type":"x"}`))["type"] != "x" {
		t.Fatal("decoded raw event failed")
	}
}

func TestEmitRawCodexEventBranches(t *testing.T) {
	agent := NewAgent()
	session := &Session{agent: agent, id: "s", rawMessages: rawMessageConfig{All: true}}
	event := codex.Event{RawMethod: "raw", RawParams: json.RawMessage(`{"type":"response_item","payload":{"type":"message","role":"assistant"}}`), RawJSON: `{"raw":true}`}
	if err := session.emitRawCodexEvent(context.Background(), event); err != nil {
		t.Fatalf("emit without connection returned error: %v", err)
	}
	conn := newRecordingAgentClient()
	agent.setAgentClient(conn)
	if err := session.emitRawCodexEvent(context.Background(), event); err != nil {
		t.Fatalf("emit raw event returned error: %v", err)
	}
	if len(conn.extensions) != 1 || conn.extensions[0].method != rawCodexSDKMessageMethod {
		t.Fatalf("extensions = %#v", conn.extensions)
	}
	session.rolloutPath = "/tmp/rollout"
	if err := session.emitRawCodexEvent(context.Background(), event); err != nil {
		t.Fatalf("rollout raw event returned error: %v", err)
	}
	if len(conn.extensions) != 1 {
		t.Fatal("rollout-backed raw event should not emit live duplicate")
	}
	session.rolloutPath = ""
	session.rawMessages = rawMessageConfig{Filters: []rawMessageFilter{{Type: "other"}}}
	if err := session.emitRawCodexEvent(context.Background(), event); err != nil {
		t.Fatalf("filtered raw event returned error: %v", err)
	}
	if len(conn.extensions) != 1 {
		t.Fatal("filtered raw event emitted unexpectedly")
	}
}

func TestRawMessageConfigInvalidBranches(t *testing.T) {
	disabled, ok := rawMessageConfigFromValue(false)
	if !ok || disabled.Enabled() {
		t.Fatalf("false raw config = %#v ok=%v", disabled, ok)
	}
	invalid, ok := rawMessageConfigFromValue("bad")
	if ok || invalid.Enabled() {
		t.Fatalf("invalid raw config = %#v ok=%v", invalid, ok)
	}
	if rawMessageConfigFromMeta(nil).Enabled() {
		t.Fatal("nil meta enabled raw messages")
	}
	if (rawMessageConfig{All: true}).ShouldEmit(nil) {
		t.Fatal("nil raw event was emitted")
	}
	filter := rawMessageFilter{Type: "response_item", PayloadType: "message", PayloadRole: "assistant"}
	if filter.Matches(map[string]any{"type": "response_item"}) {
		t.Fatal("filter matched missing payload")
	}
	if _, ok := rawMessageFilterFromMap(nil); ok {
		t.Fatal("nil raw filter parsed")
	}
}
