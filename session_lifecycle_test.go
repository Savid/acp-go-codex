package codexacp

import (
	"encoding/json"
	"testing"

	"github.com/coder/acp-go-sdk"
	"github.com/stretchr/testify/require"

	"github.com/savid/acp-go-core/lifecycle"
	"github.com/savid/acp-go-core/wire"
)

// reduceAll runs every recorded notification through the core reducer, so
// the stream the adapter emitted is proven against the same validator the
// fixture battery drives.
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
