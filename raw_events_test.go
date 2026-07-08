package codexacp

import (
	"context"
	"encoding/json"
	"strings"
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
	if decodedRawEvent(nil) != nil {
		t.Fatal("nil raw event decoded to non-nil map")
	}
	payload := map[string]any{"sessionId": "s", "sequence": int64(1), "source": "codex", "event": strings.Repeat("x", rawEventMaxBytes)}
	capped := capRawEventPayload(payload)
	event := asType[map[string]any](t, capped["event"])
	if event[rawMarkerTruncated] != true || event[rawMarkerReason] != rawMarkerReasonOversize ||
		event[rawMarkerMaxBytes] != rawEventMaxBytes {
		t.Fatalf("oversize marker = %#v", capped)
	}
	if size, ok := event[rawMarkerSizeBytes].(int); !ok || size <= rawEventMaxBytes {
		t.Fatalf("oversize marker sizeBytes = %#v", event[rawMarkerSizeBytes])
	}
	if capped["sessionId"] != "s" || capped["sequence"] != int64(1) || capped["source"] != codexMetaKey {
		t.Fatalf("oversize marker envelope = %#v", capped)
	}

	unserializable := map[string]any{"sessionId": "s", "sequence": int64(2), "source": codexMetaKey, "event": map[string]any{"bad": make(chan int)}}
	badCapped := capRawEventPayload(unserializable)
	badEvent := asType[map[string]any](t, badCapped["event"])
	if badEvent[rawMarkerTruncated] != true || badEvent[rawMarkerReason] != rawMarkerReasonUnserializable {
		t.Fatalf("unserializable marker = %#v", badCapped)
	}
	if _, ok := badEvent[rawMarkerSizeBytes]; ok {
		t.Fatalf("unserializable marker must omit sizeBytes: %#v", badEvent)
	}
}

func TestEmitRawCodexEventBranches(t *testing.T) {
	agent := NewAgent()
	liveSession := &session{agent: agent, id: "s", rawMessages: rawMessageConfig{enabled: true}}
	event := codex.Event{RawMethod: "raw", RawParams: json.RawMessage(`{"type":"response_item"}`), RawJSON: `{"raw":true}`}
	disabled := &session{agent: agent, id: "disabled"}
	if err := disabled.emitRawCodexEvent(context.Background(), event); err != nil {
		t.Fatalf("disabled raw event returned error: %v", err)
	}
	if err := liveSession.emitRawCodexEvent(context.Background(), codex.Event{RawMethod: "raw", RawParams: json.RawMessage(`bad`)}); err != nil {
		t.Fatalf("malformed raw event returned error: %v", err)
	}
	if err := liveSession.emitRawCodexEvent(context.Background(), event); err != nil {
		t.Fatalf("emit without connection returned error: %v", err)
	}
	conn := newRecordingAgentClient()
	agent.setAgentClient(conn)
	if err := liveSession.emitRawCodexEvent(context.Background(), event); err != nil {
		t.Fatalf("emit raw event returned error: %v", err)
	}
	if len(conn.extensions) != 1 || conn.extensions[0].method != RawEventMethod {
		t.Fatalf("extensions = %#v", conn.extensions)
	}
	liveSession.rolloutPath = "/tmp/rollout"
	if err := liveSession.emitRawCodexEvent(context.Background(), event); err != nil {
		t.Fatalf("rollout raw event returned error: %v", err)
	}
	if len(conn.extensions) != 2 {
		t.Fatalf("rollout-backed session must still emit live raw events, extensions=%#v", conn.extensions)
	}
}
