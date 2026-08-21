package codexacp

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/coder/acp-go-sdk"
	"github.com/savid/acp-go-codex/internal/codex"
	"github.com/stretchr/testify/require"
)

type failThenRecordRawClient struct {
	*recordingAgentClient
	failures int
}

type rawFailureContainmentClient struct {
	*runEventsClient
	cancelStarted chan [2]string
	cancelRelease chan struct{}
}

func (c *rawFailureContainmentClient) CancelTurn(_ context.Context, threadID, turnID string) error {
	c.cancelStarted <- [2]string{threadID, turnID}
	<-c.cancelRelease

	return nil
}

func (c *rawFailureContainmentClient) ListBackgroundTerminals(context.Context, codex.BackgroundTerminalListRequest) (codex.BackgroundTerminalListResponse, error) {
	return codex.BackgroundTerminalListResponse{}, nil
}

func (c *failThenRecordRawClient) NotifyExtension(ctx context.Context, method string, params any) error {
	if c.failures > 0 {
		c.failures--

		return errors.New("raw notification failed")
	}

	return c.recordingAgentClient.NotifyExtension(ctx, method, params)
}

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
	capped, err := capRawEventPayload(payload)
	if err != nil {
		t.Fatalf("cap oversized payload: %v", err)
	}
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
	badCapped, err := capRawEventPayload(unserializable)
	if err != nil {
		t.Fatalf("cap unserializable payload: %v", err)
	}
	badEvent := asType[map[string]any](t, badCapped["event"])
	if badEvent[rawMarkerTruncated] != true || badEvent[rawMarkerReason] != rawMarkerReasonUnserializable {
		t.Fatalf("unserializable marker = %#v", badCapped)
	}
	if _, ok := badEvent[rawMarkerSizeBytes]; ok {
		t.Fatalf("unserializable marker must omit sizeBytes: %#v", badEvent)
	}
}

func TestRawEventFinalPayloadBoundaryIncludesRouteMeta(t *testing.T) {
	payload := map[string]any{
		jsonFieldSessionID: "session-1",
		jsonFieldSequence:  int64(1),
		jsonFieldSource:    rawEventSource,
		jsonFieldEvent:     map[string]any{"data": ""},
		jsonFieldMeta:      inboundRouteMeta(strings.Repeat("n", routeTurnNonceMaxBytes)),
	}
	empty, err := json.Marshal(payload)
	if err != nil {
		t.Fatalf("marshal empty payload: %v", err)
	}
	padding := rawEventMaxBytes - len(empty)
	if padding <= 0 {
		t.Fatalf("empty routed payload is %d bytes", len(empty))
	}
	payload[jsonFieldEvent] = map[string]any{"data": strings.Repeat("x", padding)}

	capped, err := capRawEventPayload(payload)
	if err != nil {
		t.Fatalf("cap boundary payload: %v", err)
	}
	encoded, err := json.Marshal(capped)
	if err != nil {
		t.Fatalf("marshal boundary payload: %v", err)
	}
	if len(encoded) != rawEventMaxBytes {
		t.Fatalf("boundary payload = %d bytes, want %d", len(encoded), rawEventMaxBytes)
	}
	if event := asType[map[string]any](t, capped[jsonFieldEvent]); event[rawMarkerTruncated] == true {
		t.Fatalf("exact-boundary event was replaced: %#v", event)
	}

	payload[jsonFieldEvent] = map[string]any{"data": strings.Repeat("x", padding+1)}
	capped, err = capRawEventPayload(payload)
	if err != nil {
		t.Fatalf("cap over-boundary payload: %v", err)
	}
	encoded, err = json.Marshal(capped)
	if err != nil {
		t.Fatalf("marshal capped payload: %v", err)
	}
	if len(encoded) > rawEventMaxBytes {
		t.Fatalf("capped payload = %d bytes, exceeds %d", len(encoded), rawEventMaxBytes)
	}
	marker := asType[map[string]any](t, capped[jsonFieldEvent])
	if marker[rawMarkerReason] != rawMarkerReasonOversize || marker[rawMarkerSizeBytes] != rawEventMaxBytes+1 {
		t.Fatalf("over-boundary marker = %#v", marker)
	}
}

func TestRawEventFinalPayloadRejectsUnboundedInternalRoute(t *testing.T) {
	payload := map[string]any{
		jsonFieldSessionID: "session-1",
		jsonFieldSequence:  int64(1),
		jsonFieldSource:    rawEventSource,
		jsonFieldEvent:     map[string]any{"type": "event"},
		jsonFieldMeta:      inboundRouteMeta(strings.Repeat("n", rawEventMaxBytes)),
	}

	if _, err := capRawEventPayload(payload); err == nil || !strings.Contains(err.Error(), "exceeds") {
		t.Fatalf("unbounded route error = %v", err)
	}

	payload[jsonFieldMeta] = map[string]any{"bad": make(chan int)}
	if _, err := capRawEventPayload(payload); err == nil || !strings.Contains(err.Error(), "marshal capped") {
		t.Fatalf("unserializable route error = %v", err)
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
	if err := liveSession.emitRawCodexEvent(withTurnRoute(context.Background(), strings.Repeat("n", rawEventMaxBytes)), event); err == nil {
		t.Fatal("unbounded raw-event route was accepted")
	}
	if err := liveSession.emitRawCodexEvent(context.Background(), event); err != nil {
		t.Fatalf("emit raw event returned error: %v", err)
	}
	if len(conn.extensions) != 1 || conn.extensions[0].method != RawEventMethod {
		t.Fatalf("extensions = %#v", conn.extensions)
	}
	payload := asType[map[string]any](t, conn.extensions[0].params)
	if len(payload) != 4 {
		t.Fatalf("raw event params must be exactly {sessionId, sequence, source, event}, got %#v", payload)
	}
	for _, key := range []string{"sessionId", "sequence", "source", "event"} {
		if _, ok := payload[key]; !ok {
			t.Fatalf("raw event params missing %q: %#v", key, payload)
		}
	}
	liveSession.rolloutPath = "/tmp/rollout"
	if err := liveSession.emitRawCodexEvent(context.Background(), event); err != nil {
		t.Fatalf("rollout raw event returned error: %v", err)
	}
	if len(conn.extensions) != 2 {
		t.Fatalf("rollout-backed session must still emit live raw events, extensions=%#v", conn.extensions)
	}
}

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

func TestRawEventDeliveryFailureDoesNotConsumeSequence(t *testing.T) {
	agent := NewAgent()
	conn := &failThenRecordRawClient{recordingAgentClient: newRecordingAgentClient(), failures: 1}
	agent.setAgentClient(conn)
	session := &session{agent: agent, id: "seq", rawMessages: rawMessageConfig{enabled: true}}

	if err := session.emitRawCodexEvent(context.Background(), rawEventFor("raw", false)); err == nil {
		t.Fatal("failed raw delivery succeeded")
	}
	if session.rawEventSequence != 0 {
		t.Fatalf("failed delivery consumed sequence %d", session.rawEventSequence)
	}
	for range 2 {
		if err := session.emitRawCodexEvent(context.Background(), rawEventFor("raw", false)); err != nil {
			t.Fatalf("successful raw delivery returned error: %v", err)
		}
	}

	for index, note := range conn.extensions {
		if sequence := rawEventPayload(t, note)[jsonFieldSequence]; sequence != int64(index+1) {
			t.Fatalf("sequence[%d] = %v, want %d", index, sequence, index+1)
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

// Case 5 — raw delivery is mandatory after native acceptance and therefore
// contains the exact turn before the failed prompt settles.
func TestRawEventsEmitFailureContainsExactTurn(t *testing.T) {
	agent := NewAgent()
	agent.setAgentClient(&extensionErrorClient{recordingAgentClient: newRecordingAgentClient()})
	client := &rawFailureContainmentClient{
		runEventsClient: &runEventsClient{spyCodexClient: newSpyCodexClient(), events: []codex.Event{
			{Kind: codex.EventRaw, ThreadID: "thread", TurnID: "turn", RawMethod: "raw", RawParams: json.RawMessage(`{"type":"response_item"}`)},
			{Kind: codex.EventCompleted, ThreadID: "thread", TurnID: "turn", StopReason: codex.StopReasonEndTurn},
		}},
		cancelStarted: make(chan [2]string, 1), cancelRelease: make(chan struct{}),
	}
	session := &session{
		agent:         agent,
		id:            "emit-fail",
		cwd:           "/tmp/project",
		codexThreadID: "thread",
		rawMessages:   rawMessageConfig{enabled: true},
		client:        client,
	}

	done := make(chan error, 1)
	go func() {
		_, err := session.Prompt(t.Context(), TextPromptRequest("emit-fail", "test-turn", "hi"))
		done <- err
	}()
	require.Equal(t, [2]string{"thread", "turn"}, <-client.cancelStarted)
	select {
	case err := <-done:
		t.Fatalf("raw delivery failure returned before containment: %v", err)
	default:
	}
	close(client.cancelRelease)
	require.Error(t, <-done)
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
