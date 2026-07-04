package codexacp

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/savid/acp-go-codex/internal/codex"
)

func TestRawMessageConfigFromMeta(t *testing.T) {
	meta := map[string]any{codexMetaKey: map[string]any{
		rawEventKey: map[string]any{rawEventEnabledKey: true},
	}}
	config := rawMessageConfigFromMeta(meta)
	if !config.Enabled() || !config.ShouldEmit(map[string]any{"type": "event"}) {
		t.Fatalf("raw event config = %#v", config)
	}
	if rawMessageConfigFromMeta(nil).Enabled() {
		t.Fatal("nil meta enabled raw events")
	}
	if config.ShouldEmit(nil) {
		t.Fatal("nil raw event was emitted")
	}
	if decodedRawEvent(json.RawMessage(`{"type":"x"}`))["type"] != "x" {
		t.Fatal("decoded raw event failed")
	}
}

func TestEmitRawCodexEventBranches(t *testing.T) {
	agent := NewAgent()
	session := &session{agent: agent, id: "s", rawMessages: rawMessageConfig{enabled: true}}
	event := codex.Event{RawMethod: "raw", RawParams: json.RawMessage(`{"type":"response_item"}`), RawJSON: `{"raw":true}`}
	if err := session.emitRawCodexEvent(context.Background(), event); err != nil {
		t.Fatalf("emit without connection returned error: %v", err)
	}
	conn := newRecordingAgentClient()
	agent.setAgentClient(conn)
	if err := session.emitRawCodexEvent(context.Background(), event); err != nil {
		t.Fatalf("emit raw event returned error: %v", err)
	}
	if len(conn.extensions) != 1 || conn.extensions[0].method != RawEventMethod {
		t.Fatalf("extensions = %#v", conn.extensions)
	}
	session.rolloutPath = "/tmp/rollout"
	if err := session.emitRawCodexEvent(context.Background(), event); err != nil {
		t.Fatalf("rollout raw event returned error: %v", err)
	}
	if len(conn.extensions) != 1 {
		t.Fatal("rollout-backed raw event should not emit live duplicate")
	}
}
