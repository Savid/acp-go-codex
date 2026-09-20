package codexacp

import (
	"context"
	"encoding/json"
	"os"
	"strings"
	"testing"

	"github.com/coder/acp-go-sdk"
	"github.com/savid/acp-go-codex/internal/codex"
	"github.com/savid/acp-go-core/lifecycle"
	"github.com/savid/acp-go-core/wire"
	"github.com/stretchr/testify/require"
)

// reduceAll runs every recorded notification through the core reducer and
// returns the state the adapter's stream reduced to.
func reduceAll(t *testing.T, sessionID acp.SessionId, updates []acp.SessionNotification) lifecycle.State {
	t.Helper()

	reducer := lifecycle.NewReducer(lifecycle.Options{Negotiated: lifecycle.Negotiated{Version: 1, UpdatesOutsidePrompt: true, ActivityKinds: []lifecycle.ActivityKind{}}})

	for _, update := range updates {
		if update.SessionId != sessionID {
			continue
		}

		params, err := json.Marshal(update)
		require.NoError(t, err)

		if err := reducer.ReduceSessionUpdate(params); err != nil {
			require.ErrorIs(t, err, lifecycle.ErrNoEnvelope, "reducer refused a notification")
		}
	}

	return reducer.State()
}

func TestLifecyclePromptCycle(t *testing.T) {
	t.Parallel()

	h := newHarness(t)
	h.initialize(withLifecycle())
	session := h.newSession()

	h.rec.waitFor(t, func(updates []acp.SessionNotification) bool { return len(lifecycleEvents(updates)) >= 1 })
	require.Equal(t, []string{"lifecycle_snapshot"}, eventTypes(lifecycleEvents(h.rec.snapshot())))

	resp, err := h.prompt(session.SessionId, "HELLO", promptMeta(1))
	require.NoError(t, err)
	require.Equal(t, acp.StopReasonEndTurn, resp.StopReason)

	events := lifecycleEvents(h.rec.snapshot())
	require.Equal(t, []string{"lifecycle_snapshot", "prompt_accepted", "state_update:running", "state_update:idle"}, eventTypes(events))
	require.Equal(t, "sub-1", events[1]["submissionId"])
	require.Equal(t, "non-1", events[1]["clientNonce"])
	require.Equal(t, "end_turn", events[3]["stopReason"])
	require.Equal(t, "success", events[3]["outcome"])

	state := reduceAll(t, session.SessionId, h.rec.snapshot())
	require.Equal(t, lifecycle.ForegroundIdle, state.Foreground.State)
	require.Len(t, state.Turns, 1)

	for _, update := range h.rec.snapshot() {
		if _, ok := update.Meta[wire.LifecycleKey]; ok {
			require.NotNil(t, update.Update.SessionInfoUpdate, "envelopes ride the identity-only carrier")
			require.Nil(t, update.Update.SessionInfoUpdate.Title)
			require.Nil(t, update.Update.SessionInfoUpdate.UpdatedAt)
		}
	}
}

func TestLifecycleBlockingActionReduces(t *testing.T) {
	t.Parallel()

	h := newHarness(t)
	h.initialize(withLifecycle())
	session := h.newSession()

	_, err := h.prompt(session.SessionId, "TOOL", promptMeta(1))
	require.NoError(t, err)

	require.Equal(t, []string{
		"lifecycle_snapshot", "prompt_accepted", "state_update:running",
		"action_update:pending", "state_update:requires_action", "action_update:accepted", "state_update:running",
		"state_update:idle",
	}, eventTypes(lifecycleEvents(h.rec.snapshot())))

	state := reduceAll(t, session.SessionId, h.rec.snapshot())
	require.Equal(t, lifecycle.ForegroundIdle, state.Foreground.State)

	for _, action := range state.Actions {
		require.Equal(t, lifecycle.ActionAccepted, action.State)
	}
}

func TestCloseFencesStream(t *testing.T) {
	t.Parallel()

	h := newHarness(t)
	h.initialize(withLifecycle())
	session := h.newSession()

	_, err := h.prompt(session.SessionId, "HELLO", promptMeta(1))
	require.NoError(t, err)

	before := len(lifecycleEvents(h.rec.snapshot()))

	_, err = h.conn.CloseSession(h.ctx(), acp.CloseSessionRequest{SessionId: session.SessionId})
	require.NoError(t, err)
	require.Len(t, lifecycleEvents(h.rec.snapshot()), before, "close emits nothing on an idle stream")

	state := reduceAll(t, session.SessionId, h.rec.snapshot())
	require.Len(t, state.Turns, 1)
}

func TestNativeTailAfterCompletionOpensNoCycle(t *testing.T) {
	t.Parallel()

	h := newHarness(t)
	h.initialize(withLifecycle())
	session := h.newSession(WithSessionRawEvents(true))

	_, err := h.prompt(session.SessionId, "TAIL", promptMeta(1))
	require.NoError(t, err)

	// Raw events prove the pump processed the tail record before the next
	// prompt is admitted.
	h.rec.waitFor(t, func([]acp.SessionNotification) bool { return rawEventItem(h, "tail-1") })

	before := len(lifecycleEvents(h.rec.snapshot()))

	resp, err := h.prompt(session.SessionId, "HELLO", promptMeta(2))
	require.NoError(t, err, "a record naming the completed turn opened a cycle that refuses every later prompt")
	require.Equal(t, acp.StopReasonEndTurn, resp.StopReason)

	types := eventTypes(lifecycleEvents(h.rec.snapshot()))[before:]
	require.Equal(t, []string{"prompt_accepted", "state_update:running", "state_update:idle"}, types)
}

func TestNativeTailDoesNotStampTheNextTurn(t *testing.T) {
	t.Parallel()

	rt := &runtime{}
	next := &turn{
		cycle:    cycle{state: cycleState{tools: make(map[string]*toolState)}},
		settled:  make(chan struct{}),
		finished: make(chan struct{}),
	}
	s := &session{agent: NewAgent(), rt: rt, turn: next, lastTerminalTurn: "turn-1"}

	s.handleEvent(t.Context(), rt, nil, codex.Event{Kind: codex.EventAgentMessageDelta, TurnID: "turn-1", ItemID: "tail-1", Text: "tail"})

	s.mu.Lock()
	adopted := next.nativeTurnID
	s.mu.Unlock()
	require.Empty(t, adopted, "the tail of a terminalized turn became the live turn's native id")

	settled, err := s.projectEvent(t.Context(), &next.cycle, codex.Event{Kind: codex.EventTurnCompleted, TurnID: "turn-2"})
	require.NoError(t, err)
	require.True(t, settled, "the new turn's own turn/completed was filtered, so its prompt never settles")
}

// rawNotifyRecorder records the raw-event extension notifications the agent
// sends through a directly attached client.
type rawNotifyRecorder struct{ *recorder }

func (r *rawNotifyRecorder) NotifyExtension(_ context.Context, method string, params any) error {
	if method == RawEventMethod {
		data, err := json.Marshal(params)
		if err != nil {
			return err
		}

		r.mu.Lock()
		r.raw = append(r.raw, data)
		r.mu.Unlock()
	}

	return nil
}

// TestCapturedNativeBetweenPromptRecords replays the captured app-server
// notifications under testdata/native that reach a thread with no turn in
// flight, through the real decoder and the session's own event handling:
// every record is delivered as a raw event outside any prompt, and none
// bears work, so no agent-origin cycle opens and no lifecycle transition is
// published.
func TestCapturedNativeBetweenPromptRecords(t *testing.T) {
	t.Parallel()

	a := NewAgent(testOptions(t)...)
	t.Cleanup(func() { _ = a.Close() })
	rec := newRecorder()
	a.attach(&rawNotifyRecorder{recorder: rec}, nil)
	_, err := a.Initialize(t.Context(), acp.InitializeRequest{ProtocolVersion: acp.ProtocolVersionNumber, Meta: map[string]any{wire.LifecycleKey: map[string]any{"version": 1}}})
	require.NoError(t, err)
	created, err := a.NewSession(t.Context(), wire.NewSessionRequest(t.TempDir(), WithSessionRawEvents(true)))
	require.NoError(t, err)
	s, err := a.session(t.Context(), created.SessionId)
	require.NoError(t, err)
	s.mu.Lock()
	rt := s.rt
	require.Nil(t, s.turn)
	s.mu.Unlock()

	data, err := os.ReadFile("testdata/native/between-prompt.json")
	require.NoError(t, err)
	data = []byte(strings.ReplaceAll(string(data), "fixture-thread", s.nativeID))
	var frames []json.RawMessage
	require.NoError(t, json.Unmarshal(data, &frames))
	require.NotEmpty(t, frames)

	rec.mu.Lock()
	rawBefore := len(rec.raw)
	rec.mu.Unlock()
	before := len(lifecycleEvents(rec.snapshot()))

	for _, frame := range frames {
		var notification codex.Notification
		require.NoError(t, json.Unmarshal(frame, &notification))
		require.True(t, s.handleEvent(t.Context(), rt, nil, codex.DecodeEvent(notification)))
	}

	rec.mu.Lock()
	rawAfter := len(rec.raw)
	rec.mu.Unlock()
	require.Equal(t, len(frames), rawAfter-rawBefore, "every between-prompt record is delivered as a raw event with no prompt in flight")
	require.Empty(t, lifecycleEvents(rec.snapshot())[before:], "between-prompt records bear no work and open nothing")
}

// negotiatedAnswer decodes the lifecycle answer the initialize response carries.
func negotiatedAnswer(t *testing.T, response acp.InitializeResponse) lifecycle.Negotiated {
	t.Helper()

	raw, err := json.Marshal(response.Meta[wire.LifecycleKey])
	require.NoError(t, err)

	var negotiated lifecycle.Negotiated
	require.NoError(t, json.Unmarshal(raw, &negotiated))

	return negotiated
}

// sessionFrames renders one session's recorded notifications as the payloads
// the host received, in delivery order.
func sessionFrames(t *testing.T, updates []acp.SessionNotification, sessionID acp.SessionId) []json.RawMessage {
	t.Helper()

	var frames []json.RawMessage

	for _, update := range updates {
		if update.SessionId != sessionID {
			continue
		}

		encoded, err := json.Marshal(update)
		require.NoError(t, err)
		frames = append(frames, encoded)
	}

	return frames
}

func idleTransitions(updates []acp.SessionNotification, sessionID acp.SessionId) int {
	count := 0

	for _, update := range updates {
		if update.SessionId != sessionID {
			continue
		}

		envelope, _ := update.Meta[wire.LifecycleKey].(map[string]any)
		event, _ := envelope["event"].(map[string]any)

		if event["type"] == "state_update" && event["state"] == "idle" {
			count++
		}
	}

	return count
}

// TestOrdinaryContentAttributesToTheForeground proves the contract's
// attribution rule over a recorded stream: every content update arrives
// while the foreground is live, and no vendor namespace hints a turn or
// message.
func TestOrdinaryContentAttributesToTheForeground(t *testing.T) {
	t.Parallel()

	h := newHarness(t)
	negotiated := negotiatedAnswer(t, h.initialize(withLifecycle()))
	session := h.newSession()

	for n, text := range []string{"TOOL", "HELLO"} {
		_, err := h.prompt(session.SessionId, text, promptMeta(n+1))
		require.NoError(t, err)
	}

	h.rec.waitFor(t, func(updates []acp.SessionNotification) bool {
		return idleTransitions(updates, session.SessionId) >= 2
	})

	updates := h.rec.snapshot()
	require.NotZero(t, contentUpdates(updates, session.SessionId), "the stream carried content to attribute")
	require.NoError(t, lifecycle.CheckAttribution(negotiated, sessionFrames(t, updates, session.SessionId)))
}

func contentUpdates(updates []acp.SessionNotification, sessionID acp.SessionId) int {
	count := 0

	for _, update := range updates {
		if update.SessionId == sessionID && (update.Update.AgentMessageChunk != nil || update.Update.ToolCall != nil) {
			count++
		}
	}

	return count
}
