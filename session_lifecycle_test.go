package codexacp

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/coder/acp-go-sdk"
	"github.com/savid/acp-go-codex/internal/codex"
	acpcore "github.com/savid/acp-go-core"
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

func TestAgentOriginCycleRunsWithNoPromptInFlight(t *testing.T) {
	t.Parallel()

	store := acpcore.NewInMemorySessionStore()
	h := newHarness(t, WithSessionStore(store))
	h.initialize(withLifecycle())
	session := h.newSession()

	_, err := h.prompt(session.SessionId, "AGENT", promptMeta(1))
	require.NoError(t, err)

	h.rec.waitFor(t, func(updates []acp.SessionNotification) bool { return len(lifecycleEvents(updates)) >= 6 })

	events := lifecycleEvents(h.rec.snapshot())
	require.Equal(t, []string{
		"lifecycle_snapshot", "prompt_accepted", "state_update:running", "state_update:idle",
		"state_update:running", "state_update:idle",
	}, eventTypes(events))
	require.Equal(t, "activity", events[4]["cause"])
	require.Equal(t, "activity", events[5]["cause"])
	require.Equal(t, "success", events[5]["outcome"])

	state := reduceAll(t, session.SessionId, h.rec.snapshot())
	require.Equal(t, lifecycle.ForegroundIdle, state.Foreground.State)
	require.Len(t, state.Turns, 2)

	// The agent-origin cycle commits its own rows before its terminal idle.
	rows, err := loadEntries(t.Context(), store, acpcore.SessionKey{SessionID: string(session.SessionId)})
	require.NoError(t, err)
	require.Len(t, rows, 4)

	resp, err := h.prompt(session.SessionId, "HELLO", promptMeta(2))
	require.NoError(t, err)
	require.Equal(t, acp.StopReasonEndTurn, resp.StopReason)
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

	s.handleEvent(t.Context(), rt, codex.Event{Kind: codex.EventAgentMessageDelta, TurnID: "turn-1", ItemID: "tail-1", Text: "tail"})

	s.mu.Lock()
	adopted := next.nativeTurnID
	s.mu.Unlock()
	require.Empty(t, adopted, "the tail of a terminalized turn became the live turn's native id")

	settled, err := s.projectEvent(t.Context(), &next.cycle, codex.Event{Kind: codex.EventTurnCompleted, TurnID: "turn-2"})
	require.NoError(t, err)
	require.True(t, settled, "the new turn's own turn/completed was filtered, so its prompt never settles")
}

func TestCloseDoesNotRepeatAnAgentCycleTerminal(t *testing.T) {
	t.Parallel()

	rollout := filepath.Join(t.TempDir(), "rollout.jsonl")
	require.NoError(t, os.WriteFile(rollout, nil, 0o600))

	s := &session{agent: NewAgent(), id: "sess-1", cwd: t.TempDir(), rolloutPath: rollout, gate: make(chan struct{}, 1)}

	var delivered []map[string]any

	negotiated := lifecycle.Negotiated{Version: 1, UpdatesOutsidePrompt: true, ActivityKinds: []lifecycle.ActivityKind{}}
	require.NoError(t, s.lc.Open(t.Context(), "sess-1:1", negotiated, func(_ context.Context, envelope map[string]any) error {
		event, _ := envelope["event"].(map[string]any)
		delivered = append(delivered, event)

		return nil
	}))

	c := &cycle{}
	c.Cycle = s.lc.NewAgentCycle()
	require.NoError(t, s.lc.OpenAgentCycle(t.Context(), c.Cycle))

	s.mu.Lock()
	s.cycle = c
	s.mu.Unlock()

	// The pump reaches the cycle's terminal idle first; close then finds the
	// cycle still installed.
	require.True(t, s.claimTerminal(c))
	require.NoError(t, s.lcIdle(t.Context(), c, cycleVerdict{outcome: lifecycle.OutcomeSuccess, stopReason: lifecycle.StopReasonEndTurn}))

	require.NoError(t, s.close(t.Context()),
		"close published a second terminal event for a cycle the pump had already ended")
	require.Equal(t, []string{"lifecycle_snapshot", "state_update:running", "state_update:idle"}, eventTypes(delivered))
}

func TestCloseBackgroundCycleRequiresCommit(t *testing.T) {
	for _, fail := range []bool{false, true} {
		t.Run(map[bool]string{false: "committed", true: "failed"}[fail], func(t *testing.T) {
			store := &recoveryFaultStore{SessionStore: acpcore.NewInMemorySessionStore()}
			h := newHarness(t, WithSessionStore(store))
			h.initialize(withLifecycle())
			created := h.newSession()
			_, err := h.prompt(created.SessionId, "AGENTHANG", promptMeta(1))
			require.NoError(t, err)
			h.rec.waitFor(t, func(updates []acp.SessionNotification) bool {
				events := lifecycleEvents(updates)

				return len(events) >= 5 && events[4]["state"] == "running"
			})
			before := len(lifecycleEvents(h.rec.snapshot()))
			store.fail.Store(fail)
			_, err = h.conn.CloseSession(h.ctx(), acp.CloseSessionRequest{SessionId: created.SessionId})
			store.fail.Store(false)
			if fail {
				require.Error(t, err)
			} else {
				require.NoError(t, err)
			}
			terminal := 0
			for _, event := range lifecycleEvents(h.rec.snapshot())[before:] {
				if event["state"] == "idle" {
					terminal++
					require.Equal(t, "cancelled", event["outcome"])
				}
			}
			if fail {
				require.Zero(t, terminal)
			} else {
				require.Equal(t, 1, terminal)
			}
		})
	}
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

	data, err := os.ReadFile("testdata/native/agent-origin.json")
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
		require.True(t, s.handleEvent(t.Context(), rt, codex.DecodeEvent(notification)))
	}

	rec.mu.Lock()
	rawAfter := len(rec.raw)
	rec.mu.Unlock()
	require.Equal(t, len(frames), rawAfter-rawBefore, "every between-prompt record is delivered as a raw event with no prompt in flight")
	require.Empty(t, lifecycleEvents(rec.snapshot())[before:], "between-prompt records bear no work and open no cycle")
	s.mu.Lock()
	require.Nil(t, s.cycle)
	s.mu.Unlock()
}
