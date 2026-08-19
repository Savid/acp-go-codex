package lifecycle

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

func provenNegotiation() Negotiated {
	return Negotiated{
		Versions:                []int{1},
		AuthoritativeQuiescence: true,
		QuiescenceSource:        ProofClassProcessContainment,
		ActivityKinds:           []ActivityKind{ActivityTask},
	}
}

func TestStreamClaimsASequenceBeforeDelivery(t *testing.T) {
	t.Parallel()

	stream := NewStream("strm-1", Negotiated{Versions: []int{1}})
	require.Equal(t, "strm-1", stream.ID())

	opening, err := stream.Emit(SnapshotEvent("cycle-1", QuiescenceFact{}))
	require.NoError(t, err)
	require.Equal(t, uint64(1), opening["sequence"])
	require.Equal(t, 1, opening["version"])
	require.Equal(t, "strm-1", opening["streamId"])

	// A refused event consumes its sequence, so the loss is a detectable gap
	// rather than a silently contiguous stream.
	_, err = stream.Emit(SnapshotEvent("cycle-1", QuiescenceFact{}))
	require.ErrorIs(t, err, &ViolationError{Kind: ViolationStreamCycle})

	require.Equal(t, uint64(1), stream.State().ReducedThrough)
}

func TestStreamRefusesAnEventItCannotState(t *testing.T) {
	t.Parallel()

	stream := NewStream("strm-1", Negotiated{Versions: []int{1}})
	_, err := stream.Emit(IdleEvent("cycle-1", "turn-1", StopReasonEndTurn, OutcomeSuccess))
	require.ErrorIs(t, err, &ViolationError{Kind: ViolationDeltaBeforeSnapshot})
}

func TestFencedStreamEmitsNothingFurther(t *testing.T) {
	t.Parallel()

	stream := NewStream("strm-1", Negotiated{Versions: []int{1}})
	_, err := stream.Emit(SnapshotEvent("cycle-1", QuiescenceFact{}))
	require.NoError(t, err)

	require.False(t, stream.Fenced())
	stream.Fence()
	require.True(t, stream.Fenced())

	_, err = stream.Emit(AcceptedEvent(Submission{SubmissionID: "s", ClientNonce: "n"}, "turn-1"))
	require.ErrorIs(t, err, &ViolationError{Kind: ViolationStaleStream})
}

// TestEmittedIncarnationReducesAsItsOwnConsumerWould drives one whole incarnation
// through the emitter and then re-reduces the rendered envelopes from the wire, so
// the encoder and the decoder are held to one contract rather than two.
func TestEmittedIncarnationReducesAsItsOwnConsumerWould(t *testing.T) {
	t.Parallel()

	negotiated := provenNegotiation()
	stream := NewStream("strm-1", negotiated)
	blocks := true

	events := []Event{
		SnapshotEvent("cycle-1", QuiescenceFact{}),
		AcceptedEvent(Submission{SubmissionID: "sub-1", ClientNonce: "non-1", RunID: "run-1"}, "turn-1"),
		TransitionEvent(ForegroundRunning, "cycle-1", "turn-1"),
		ActionEvent(ActionUpdate{
			ActionID:         "act-1",
			Kind:             ActionPermission,
			State:            ActionPending,
			Owner:            Owner{Type: OwnerTurn, ID: "turn-1"},
			RunID:            "run-1",
			BlocksForeground: &blocks,
		}),
		TransitionEvent(ForegroundRequiresAction, "cycle-1", "turn-1"),
		ActionEvent(ResolvedAction("act-1", ActionAccepted)),
		TransitionEvent(ForegroundRunning, "cycle-1", "turn-1"),
		{Type: EventActivityUpdate, Activity: &ActivityUpdate{
			ActivityID:   "act-task",
			Kind:         ActivityTask,
			State:        ActivityRunning,
			Cause:        CauseSubmission,
			OriginTurnID: "turn-1",
			ToolCallID:   "tool-1",
			Progress:     json.RawMessage(`{"done":1}`),
		}},
		{Type: EventActivityUpdate, Activity: &ActivityUpdate{ActivityID: "act-task", State: ActivityCompleted}},
		IdleEvent("cycle-1", "turn-1", StopReasonEndTurn, OutcomeSuccess),
		QuiescenceEvent(QuiescenceFact{
			Quiescent: true,
			Source:    ProofClassProcessContainment,
			Watermark: 10,
			Barrier:   "barrier-1",
		}),
	}

	consumer := NewReducer(Options{Negotiated: negotiated})

	for index, event := range events {
		envelope, err := stream.Emit(event)
		require.NoError(t, err, "event %d", index)

		notification, err := json.Marshal(map[string]any{
			"sessionId": "sess-1",
			"update":    map[string]any{"sessionUpdate": "session_info_update"},
			"_meta":     map[string]any{MetaKey: envelope},
		})
		require.NoError(t, err)
		require.NoError(t, consumer.ReduceSessionUpdate(notification), "event %d", index)
	}

	require.Equal(t, stream.State(), consumer.State())
	require.True(t, consumer.State().Quiescence.Certified)
	require.Equal(t, uint64(10), consumer.State().Quiescence.Watermark)

	turn, ok := consumer.State().Turn("turn-1")
	require.True(t, ok)
	require.True(t, turn.Terminal)
	require.Equal(t, OutcomeSuccess, turn.Outcome)
}

// TestEmittedEndingIdleRecordsHowItSettled holds the emitter to the same ending-idle
// rule the decoder enforces: the outcome is always required, and a failure carries no
// stop reason because no ACP v1 stop reason names one.
func TestEmittedEndingIdleRecordsHowItSettled(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name       string
		stopReason string
		outcome    Outcome
		refused    bool
	}{
		{name: "a settled turn records both", stopReason: StopReasonEndTurn, outcome: OutcomeSuccess},
		{name: "a failure records the outcome alone", outcome: OutcomeFailed},
		{name: "a failure with a stop reason", stopReason: StopReasonEndTurn, outcome: OutcomeFailed, refused: true},
		{name: "a settled turn with no outcome", stopReason: StopReasonEndTurn, refused: true},
		{name: "a settled turn with no stop reason", outcome: OutcomeSuccess, refused: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			stream := NewStream("strm-1", Negotiated{Versions: []int{1}})
			_, err := stream.Emit(SnapshotEvent("cycle-1", QuiescenceFact{}))
			require.NoError(t, err)
			_, err = stream.Emit(AcceptedEvent(Submission{SubmissionID: "s", ClientNonce: "n"}, "turn-1"))
			require.NoError(t, err)
			_, err = stream.Emit(TransitionEvent(ForegroundRunning, "cycle-1", "turn-1"))
			require.NoError(t, err)

			_, err = stream.Emit(IdleEvent("cycle-1", "turn-1", tc.stopReason, tc.outcome))

			if tc.refused {
				require.ErrorIs(t, err, &ViolationError{Kind: ViolationMalformedEnvelope})

				return
			}

			require.NoError(t, err)
		})
	}
}

// TestEmitValidatesTheRenderedNotification pins where the emitter's
// self-validation runs. "Emitted envelopes are well formed" is a claim about
// bytes, so the emitter renders the notification, decodes it, and reduces the
// decoded value — the exact path a consumer takes. Each event below reduces
// perfectly well as an in-process value and is refused the moment it is read
// back the way a host would read it, which is precisely the class of encoder
// infidelity struct-basis validation cannot see.
func TestEmitValidatesTheRenderedNotification(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name  string
		event Event
	}{
		{
			name: "an identifier past the bound",
			event: AcceptedEvent(Submission{
				SubmissionID: strings.Repeat("s", IdentifierBound+1),
				ClientNonce:  "non-1",
			}, "turn-1"),
		},
		{
			name:  "a required identifier rendered empty",
			event: AcceptedEvent(Submission{ClientNonce: "non-1"}, "turn-1"),
		},
		{
			name:  "a positive quiescence fact with no proof class",
			event: QuiescenceEvent(QuiescenceFact{Quiescent: true, Watermark: 1}),
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			stream := NewStream("strm-1", provenNegotiation())
			_, err := stream.Emit(SnapshotEvent("cycle-1", QuiescenceFact{}))
			require.NoError(t, err)

			_, err = stream.Emit(tc.event)
			require.ErrorIs(t, err, &ViolationError{Kind: ViolationMalformedEnvelope})

			// The refusal happens at emission, so nothing reaches the projection
			// and the sequence it claimed stays consumed as a detectable gap.
			require.Equal(t, uint64(1), stream.State().ReducedThrough)
		})
	}
}

func TestEncoderOmitsAnOptionalMemberRatherThanEmptyingIt(t *testing.T) {
	t.Parallel()

	accepted := encodeEvent(AcceptedEvent(Submission{SubmissionID: "s", ClientNonce: "n"}, "turn-1"))
	require.NotContains(t, accepted, "runId")

	patch := encodeEvent(ActionEvent(ResolvedAction("act-1", ActionCancelled)))
	action, _ := patch["action"].(map[string]any)
	require.Equal(t, map[string]any{"actionId": "act-1", "state": "cancelled"}, action)

	first := encodeEvent(ActionEvent(PendingAction("act-1", ActionElicitation, Owner{Type: OwnerActivity, ID: "a-1"}, false)))
	action, _ = first["action"].(map[string]any)
	require.Equal(t, false, action["blocksForeground"])
	require.Equal(t, map[string]any{"type": "activity", "id": "a-1"}, action["owner"])

	require.Equal(t, map[string]any{"quiescent": false}, encodeQuiescence(QuiescenceFact{}))
}

// TestEncodedSnapshotResumesMidTurn pins the one snapshot shape this adapter never
// opens with but must still render correctly: a foreground naming a turn names that
// turn's origin with it.
func TestEncodedSnapshotResumesMidTurn(t *testing.T) {
	t.Parallel()

	blocks := true
	encoded := encodeSnapshot(Snapshot{
		Foreground: Foreground{
			State:   ForegroundRequiresAction,
			CycleID: "cycle-1",
			TurnID:  "turn-1",
			Origin:  CauseSubmission,
		},
		Activities: []ActivityUpdate{{
			ActivityID:   "act-1",
			Kind:         ActivityTask,
			State:        ActivityRunning,
			Cause:        CauseSubmission,
			OriginTurnID: "turn-1",
			ParentID:     "",
			RunID:        "run-1",
		}},
		Actions: []ActionUpdate{{
			ActionID:         "action-1",
			Kind:             ActionPermission,
			State:            ActionPending,
			Owner:            Owner{Type: OwnerTurn, ID: "turn-1"},
			BlocksForeground: &blocks,
		}},
		Quiescence: QuiescenceFact{},
	})

	foreground, _ := encoded["foreground"].(map[string]any)
	require.Equal(t, "submission", foreground["origin"])
	require.Equal(t, "turn-1", foreground["turnId"])
	require.Len(t, encoded["activities"], 1)
	require.Len(t, encoded["actions"], 1)
}
