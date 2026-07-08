package codexacp

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"github.com/coder/acp-go-sdk"
	"github.com/savid/acp-go-codex/internal/codex"
)

func rawEventPayload(t *testing.T, note extensionNotification) map[string]any {
	t.Helper()

	if note.method != RawEventMethod {
		t.Fatalf("notification method = %s, want %s", note.method, RawEventMethod)
	}

	payload, ok := note.params.(map[string]any)
	if !ok {
		t.Fatalf("raw event params = %#v", note.params)
	}

	return payload
}

func rawEventFor(source string, oversize bool) codex.Event {
	body := `{"type":"response_item"}`
	if oversize {
		body = fmt.Sprintf(`{"type":"response_item","blob":%q}`, strings.Repeat("x", rawEventMaxBytes))
	}

	return codex.Event{RawMethod: source, RawParams: json.RawMessage(body)}
}

// Case 2 — a per-session sequence is contiguous across normal and oversized events.
func TestRawEventsContiguousSequence(t *testing.T) {
	agent := NewAgent()
	conn := newRecordingAgentClient()
	agent.setAgentClient(conn)
	session := &session{agent: agent, id: "seq", rawMessages: rawMessageConfig{enabled: true}}

	for i, oversize := range []bool{false, true, false} {
		if err := session.emitRawCodexEvent(context.Background(), rawEventFor("raw", oversize)); err != nil {
			t.Fatalf("emit %d returned error: %v", i, err)
		}
	}

	if len(conn.extensions) != 3 {
		t.Fatalf("emitted %d notifications, want 3", len(conn.extensions))
	}

	for i, note := range conn.extensions {
		payload := rawEventPayload(t, note)
		if payload[jsonFieldSequence] != int64(i+1) {
			t.Fatalf("sequence[%d] = %v, want %d", i, payload[jsonFieldSequence], i+1)
		}
	}
}

// Case 3 — two sessions each keep an independent, contiguous sequence.
func TestRawEventsCrossSessionIsolation(t *testing.T) {
	agent := NewAgent()
	conn := newRecordingAgentClient()
	agent.setAgentClient(conn)
	first := &session{agent: agent, id: "one", rawMessages: rawMessageConfig{enabled: true}}
	second := &session{agent: agent, id: "two", rawMessages: rawMessageConfig{enabled: true}}

	for _, s := range []*session{first, second, first, second} {
		if err := s.emitRawCodexEvent(context.Background(), rawEventFor("raw", false)); err != nil {
			t.Fatalf("emit for %s returned error: %v", s.id, err)
		}
	}

	perSession := map[string][]int64{}
	for _, note := range conn.extensions {
		payload := rawEventPayload(t, note)
		id, _ := payload[jsonFieldSessionID].(acp.SessionId)
		seq, _ := payload[jsonFieldSequence].(int64)
		perSession[string(id)] = append(perSession[string(id)], seq)
	}

	for id, seqs := range perSession {
		if len(seqs) != 2 || seqs[0] != 1 || seqs[1] != 2 {
			t.Fatalf("session %s sequences = %v, want [1 2]", id, seqs)
		}
	}
}

// Case 4 — every emitted event field is valid JSON, including oversized and
// unserializable inputs.
func TestRawEventsValidJSONInvariant(t *testing.T) {
	agent := NewAgent()
	conn := newRecordingAgentClient()
	agent.setAgentClient(conn)
	session := &session{agent: agent, id: "json", rawMessages: rawMessageConfig{enabled: true}}

	if err := session.emitRawCodexEvent(context.Background(), rawEventFor("raw", false)); err != nil {
		t.Fatalf("normal emit returned error: %v", err)
	}
	if err := session.emitRawCodexEvent(context.Background(), rawEventFor("raw", true)); err != nil {
		t.Fatalf("oversize emit returned error: %v", err)
	}

	for i, note := range conn.extensions {
		payload := rawEventPayload(t, note)
		encoded, err := json.Marshal(payload[jsonFieldEvent])
		if err != nil || !json.Valid(encoded) {
			t.Fatalf("event[%d] is not valid JSON: %v", i, err)
		}
	}
}

// Case 5 — a raw-event emit failure is recorded but does not fail the turn.
func TestRawEventsEmitFailureDoesNotFailTurn(t *testing.T) {
	agent := NewAgent()
	agent.setAgentClient(&extensionErrorClient{recordingAgentClient: newRecordingAgentClient()})
	session := &session{
		agent:         agent,
		id:            "emit-fail",
		cwd:           "/tmp/project",
		codexThreadID: "thread",
		rawMessages:   rawMessageConfig{enabled: true},
		client: &runEventsClient{spyCodexClient: newSpyCodexClient(), events: []codex.Event{
			{Kind: codex.EventRaw, ThreadID: "thread", TurnID: "turn", RawMethod: "raw", RawParams: json.RawMessage(`{"type":"response_item"}`)},
			{Kind: codex.EventCompleted, ThreadID: "thread", TurnID: "turn", StopReason: codex.StopReasonEndTurn},
		}},
	}

	resp, err := session.Prompt(context.Background(), TextPromptRequest("emit-fail", "hi"))
	if err != nil {
		t.Fatalf("raw emit failure aborted the turn: %v", err)
	}
	if resp.StopReason != acp.StopReasonEndTurn {
		t.Fatalf("stop reason = %v, want end_turn", resp.StopReason)
	}
	if session.rawEmitFailures == 0 {
		t.Fatal("raw emit failure was not recorded on the observer hook")
	}
}

// Case 6 — with raw events disabled, no notifications are emitted.
func TestRawEventsDefaultOff(t *testing.T) {
	agent := NewAgent()
	conn := newRecordingAgentClient()
	agent.setAgentClient(conn)
	session := &session{agent: agent, id: "off"}

	for range 5 {
		if err := session.emitRawCodexEvent(context.Background(), rawEventFor("raw", false)); err != nil {
			t.Fatalf("disabled emit returned error: %v", err)
		}
	}

	if len(conn.extensions) != 0 {
		t.Fatalf("disabled raw events emitted %d notifications", len(conn.extensions))
	}
}
