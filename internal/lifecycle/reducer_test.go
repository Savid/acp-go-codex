package lifecycle

import (
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/require"
)

// deliver frames one event as a delivery on the one legal carrier. Building the
// delivery directly is what reaches the reducer's own precedence rules, including
// the payload-shape defences the decoder normally satisfies for it.
func deliver(sequence uint64, event Event) Delivery {
	return Delivery{StreamID: "strm-1", Sequence: sequence, Carrier: CarrierSessionInfo, Event: event}
}

func blocking(value bool) *bool { return &value }

func openStream(t *testing.T, negotiated Negotiated) *Reducer {
	t.Helper()

	reducer := NewReducer(Options{Negotiated: negotiated})
	require.NoError(t, reducer.Reduce(deliver(1, SnapshotEvent("cycle-1", QuiescenceFact{}))))

	return reducer
}

func TestReducerReportsWhatItValidatesAgainst(t *testing.T) {
	t.Parallel()

	negotiated := negotiatedForDecode()
	reducer := NewReducer(Options{Negotiated: negotiated})
	require.Equal(t, negotiated, reducer.Negotiated())
	require.Nil(t, reducer.Failed())

	require.Error(t, reducer.Reduce(deliver(1, Event{Type: EventStateUpdate, State: &StateTransition{}})))
	require.NotNil(t, reducer.Failed())

	// The latch holds whichever entry point a later frame arrives through.
	require.Equal(t, reducer.Failed(), reducer.Reduce(deliver(2, SnapshotEvent("cycle-1", QuiescenceFact{}))))
	require.Equal(t, reducer.Failed(), reducer.ReduceSessionUpdate(notification(envelopeAround(`{"type":"prompt_accepted"}`))))
}

func TestReducerRefusesAnIneligibleCarrierBeforeOrdering(t *testing.T) {
	t.Parallel()

	reducer := NewReducer(Options{Negotiated: Negotiated{Versions: []int{1}}})
	delivery := deliver(9, SnapshotEvent("cycle-1", QuiescenceFact{}))
	delivery.Carrier = CarrierIneligible

	require.ErrorIs(t, reducer.Reduce(delivery), &ViolationError{Kind: ViolationIllegalCarrier})
}

func TestReducerRefusesEveryPayloadItCannotRead(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name  string
		event Event
		first bool
	}{
		{name: "a snapshot with no payload", event: Event{Type: EventSnapshot}, first: true},
		{name: "an acceptance with no payload", event: Event{Type: EventPromptAccepted}},
		{name: "a transition with no payload", event: Event{Type: EventStateUpdate}},
		{name: "an activity update with no payload", event: Event{Type: EventActivityUpdate}},
		{name: "an action update with no payload", event: Event{Type: EventActionUpdate}},
		{name: "a quiescence fact with no payload", event: Event{Type: EventQuiescenceUpdate}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			reducer := NewReducer(Options{Negotiated: negotiatedForDecode()})
			sequence := uint64(1)

			if !tc.first {
				require.NoError(t, reducer.Reduce(deliver(1, SnapshotEvent("cycle-1", QuiescenceFact{}))))

				sequence = 2
			}

			require.ErrorIs(t, reducer.Reduce(deliver(sequence, tc.event)),
				&ViolationError{Kind: ViolationMalformedEnvelope})
		})
	}
}

func TestSnapshotIsJudgedWholeBeforeItIsProjected(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name     string
		snapshot Snapshot
		kind     ViolationKind
	}{
		{
			name:     "an incomplete foreground",
			snapshot: Snapshot{Foreground: Foreground{State: ForegroundIdle}},
			kind:     ViolationMalformedEnvelope,
		},
		{
			name:     "an idle foreground naming a turn",
			snapshot: Snapshot{Foreground: Foreground{State: ForegroundIdle, CycleID: "c", TurnID: "t"}},
			kind:     ViolationMalformedEnvelope,
		},
		{
			name:     "a foreground turn with no origin",
			snapshot: Snapshot{Foreground: Foreground{State: ForegroundRunning, CycleID: "c", TurnID: "t"}},
			kind:     ViolationMalformedEnvelope,
		},
		{
			name:     "a foreground origin outside the two causes",
			snapshot: Snapshot{Foreground: Foreground{State: ForegroundRunning, CycleID: "c", TurnID: "t", Origin: "bogus"}},
			kind:     ViolationMalformedEnvelope,
		},
		{
			name: "a terminal action in the nonterminal set",
			snapshot: Snapshot{
				Foreground: Foreground{State: ForegroundRunning, CycleID: "c", TurnID: "t", Origin: CauseSubmission},
				Actions: []ActionUpdate{{
					ActionID: "act", Kind: ActionPermission, State: ActionAccepted,
					Owner: Owner{Type: OwnerTurn, ID: "t"}, BlocksForeground: blocking(false),
				}},
			},
			kind: ViolationMalformedEnvelope,
		},
		{
			name: "an owner the snapshot does not introduce",
			snapshot: Snapshot{
				Foreground: Foreground{State: ForegroundRunning, CycleID: "c", TurnID: "t", Origin: CauseSubmission},
				Actions: []ActionUpdate{{
					ActionID: "act", Kind: ActionPermission, State: ActionPending,
					Owner: Owner{Type: OwnerActivity, ID: "ghost"}, BlocksForeground: blocking(false),
				}},
			},
			kind: ViolationUnknownEntity,
		},
		{
			name: "an action first sight with no blocking claim",
			snapshot: Snapshot{
				Foreground: Foreground{State: ForegroundRunning, CycleID: "c", TurnID: "t", Origin: CauseSubmission},
				Actions: []ActionUpdate{{
					ActionID: "act", Kind: ActionPermission, State: ActionPending,
					Owner: Owner{Type: OwnerTurn, ID: "t"},
				}},
			},
			kind: ViolationMalformedEnvelope,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			reducer := NewReducer(Options{Negotiated: negotiatedForDecode()})
			err := reducer.Reduce(deliver(1, Event{Type: EventSnapshot, Snapshot: &tc.snapshot}))

			require.ErrorIs(t, err, &ViolationError{Kind: tc.kind})
			require.Nil(t, reducer.State().Foreground, "a refused snapshot projects nothing")
			require.Equal(t, uint64(0), reducer.State().ReducedThrough)
		})
	}
}

func TestForeignStreamIsStaleUnlessItOpensWithASnapshot(t *testing.T) {
	t.Parallel()

	reducer := openStream(t, Negotiated{Versions: []int{1}})

	foreign := Delivery{
		StreamID: "strm-2",
		Sequence: 5,
		Carrier:  CarrierSessionInfo,
		Event:    AcceptedEvent(Submission{SubmissionID: "s", ClientNonce: "n"}, "turn-1"),
	}
	require.ErrorIs(t, reducer.Reduce(foreign), &ViolationError{Kind: ViolationStaleStream})
	require.Equal(t, "strm-1", reducer.State().StreamID)
}

func TestNextIncarnationAdoptsNothingFromTheOneItSupersedes(t *testing.T) {
	t.Parallel()

	reducer := openStream(t, Negotiated{Versions: []int{1}})
	require.NoError(t, reducer.Reduce(deliver(2, AcceptedEvent(Submission{SubmissionID: "s", ClientNonce: "n"}, "turn-1"))))

	next := Delivery{
		StreamID: "strm-2",
		Sequence: 40,
		Carrier:  CarrierSessionInfo,
		Event:    SnapshotEvent("cycle-2", QuiescenceFact{}),
	}
	require.NoError(t, reducer.Reduce(next))

	state := reducer.State()
	require.Equal(t, "strm-2", state.StreamID)
	require.Equal(t, uint64(40), state.ReducedThrough)
	require.Empty(t, state.Turns)
}

// TestSupersededIncarnationNeverResurrects pins the retired-identity memory the
// family battery's snapshot-on-superseded-stream vector states: once a later
// incarnation opens, the identity it replaced is fenced for the rest of the
// session, so a snapshot bearing it is stale rather than the opening of a fresh
// stream — and the refusal leaves the standing projection alone.
func TestSupersededIncarnationNeverResurrects(t *testing.T) {
	t.Parallel()

	reducer := openStream(t, Negotiated{Versions: []int{1}})

	next := Delivery{
		StreamID: "strm-2",
		Sequence: 1,
		Carrier:  CarrierSessionInfo,
		Event:    SnapshotEvent("cycle-2", QuiescenceFact{}),
	}
	require.NoError(t, reducer.Reduce(next))
	require.NoError(t, reducer.Reduce(Delivery{
		StreamID: "strm-2",
		Sequence: 2,
		Carrier:  CarrierSessionInfo,
		Event:    AcceptedEvent(Submission{SubmissionID: "s", ClientNonce: "n"}, "turn-1"),
	}))

	resurrection := Delivery{
		StreamID: "strm-1",
		Sequence: 9,
		Carrier:  CarrierSessionInfo,
		Event:    SnapshotEvent("cycle-3", QuiescenceFact{}),
	}
	require.ErrorIs(t, reducer.Reduce(resurrection), &ViolationError{Kind: ViolationStaleStream})

	state := reducer.State()
	require.Equal(t, "strm-2", state.StreamID)
	require.Equal(t, uint64(2), state.ReducedThrough)
	require.Len(t, state.Turns, 1)
}

func TestTerminalTurnNeverReopens(t *testing.T) {
	t.Parallel()

	reducer := openStream(t, Negotiated{Versions: []int{1}})
	submission := Submission{SubmissionID: "s", ClientNonce: "n"}

	require.NoError(t, reducer.Reduce(deliver(2, AcceptedEvent(submission, "turn-1"))))
	require.NoError(t, reducer.Reduce(deliver(3, TransitionEvent(ForegroundRunning, "cycle-1", "turn-1"))))
	require.NoError(t, reducer.Reduce(deliver(4, IdleEvent("cycle-1", "turn-1", StopReasonEndTurn, OutcomeSuccess))))

	for _, tc := range []struct {
		name  string
		event Event
	}{
		{name: "a second acceptance", event: AcceptedEvent(submission, "turn-1")},
		{name: "a later running transition", event: TransitionEvent(ForegroundRunning, "cycle-1", "turn-1")},
		{name: "a later ending idle", event: IdleEvent("cycle-1", "turn-1", StopReasonEndTurn, OutcomeSuccess)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			replay := openStream(t, Negotiated{Versions: []int{1}})
			require.NoError(t, replay.Reduce(deliver(2, AcceptedEvent(submission, "turn-1"))))
			require.NoError(t, replay.Reduce(deliver(3, TransitionEvent(ForegroundRunning, "cycle-1", "turn-1"))))
			require.NoError(t, replay.Reduce(deliver(4, IdleEvent("cycle-1", "turn-1", StopReasonEndTurn, OutcomeSuccess))))

			require.ErrorIs(t, replay.Reduce(deliver(5, tc.event)),
				&ViolationError{Kind: ViolationPostTerminalMutation})
		})
	}
}

func TestSessionCausedIdleEndsNoTurn(t *testing.T) {
	t.Parallel()

	reducer := openStream(t, Negotiated{Versions: []int{1}})
	idle := Event{Type: EventStateUpdate, State: &StateTransition{
		State:   ForegroundIdle,
		CycleID: "cycle-2",
		Cause:   CauseSession,
	}}

	require.NoError(t, reducer.Reduce(deliver(2, idle)))
	require.Equal(t, "cycle-2", reducer.State().Foreground.CycleID)
	require.Empty(t, reducer.State().Turns)
}

func TestActivityIdentityIsFixedOnFirstSight(t *testing.T) {
	t.Parallel()

	firstSight := ActivityUpdate{
		ActivityID:   "act-1",
		Kind:         ActivityTask,
		State:        ActivityRunning,
		Cause:        CauseSubmission,
		OriginTurnID: "turn-1",
		ToolCallID:   "tool-1",
		ParentID:     "",
		RunID:        "run-1",
	}

	for _, tc := range []struct {
		name  string
		patch ActivityUpdate
	}{
		{name: "kind", patch: ActivityUpdate{ActivityID: "act-1", State: ActivityRunning, Kind: ActivityMonitor}},
		{name: "parent", patch: ActivityUpdate{ActivityID: "act-1", State: ActivityRunning, ParentID: "other"}},
		{name: "tool link", patch: ActivityUpdate{ActivityID: "act-1", State: ActivityRunning, ToolCallID: "tool-2"}},
		{name: "cause", patch: ActivityUpdate{ActivityID: "act-1", State: ActivityRunning, Cause: CauseActivity}},
		{name: "origin turn", patch: ActivityUpdate{ActivityID: "act-1", State: ActivityRunning, OriginTurnID: "turn-2"}},
		{name: "ownership root", patch: ActivityUpdate{ActivityID: "act-1", State: ActivityRunning, RunID: "run-2"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			reducer := openStream(t, negotiatedForDecode())
			require.NoError(t, reducer.Reduce(deliver(2, AcceptedEvent(Submission{SubmissionID: "s", ClientNonce: "n"}, "turn-1"))))
			require.NoError(t, reducer.Reduce(deliver(3, TransitionEvent(ForegroundRunning, "cycle-1", "turn-1"))))
			require.NoError(t, reducer.Reduce(deliver(4, Event{Type: EventActivityUpdate, Activity: &firstSight})))

			patch := tc.patch
			require.ErrorIs(t, reducer.Reduce(deliver(5, Event{Type: EventActivityUpdate, Activity: &patch})),
				&ViolationError{Kind: ViolationImmutableIdentityChange})
		})
	}
}

func TestActivityProgressIsRenderedRatherThanReduced(t *testing.T) {
	t.Parallel()

	reducer := openStream(t, negotiatedForDecode())
	require.NoError(t, reducer.Reduce(deliver(2, AcceptedEvent(Submission{SubmissionID: "s", ClientNonce: "n"}, "turn-1"))))
	require.NoError(t, reducer.Reduce(deliver(3, TransitionEvent(ForegroundRunning, "cycle-1", "turn-1"))))

	first := ActivityUpdate{
		ActivityID: "act-1", Kind: ActivityTask, State: ActivityRunning,
		Cause: CauseSubmission, OriginTurnID: "turn-1",
	}
	require.NoError(t, reducer.Reduce(deliver(4, Event{Type: EventActivityUpdate, Activity: &first})))

	patch := ActivityUpdate{ActivityID: "act-1", State: ActivityRunning, Progress: json.RawMessage(`{"done":2}`)}
	require.NoError(t, reducer.Reduce(deliver(5, Event{Type: EventActivityUpdate, Activity: &patch})))

	activity, ok := reducer.State().Activity("act-1")
	require.True(t, ok)
	require.Equal(t, json.RawMessage(`{"done":2}`), activity.Progress)

	_, ok = reducer.State().Activity("missing")
	require.False(t, ok)
}

func TestParentTerminalizesAfterEveryActionItOwns(t *testing.T) {
	t.Parallel()

	reducer := openStream(t, negotiatedForDecode())
	require.NoError(t, reducer.Reduce(deliver(2, AcceptedEvent(Submission{SubmissionID: "s", ClientNonce: "n"}, "turn-1"))))
	require.NoError(t, reducer.Reduce(deliver(3, TransitionEvent(ForegroundRunning, "cycle-1", "turn-1"))))

	parent := ActivityUpdate{
		ActivityID: "act-1", Kind: ActivityTask, State: ActivityRunning,
		Cause: CauseSubmission, OriginTurnID: "turn-1",
	}
	require.NoError(t, reducer.Reduce(deliver(4, Event{Type: EventActivityUpdate, Activity: &parent})))
	require.NoError(t, reducer.Reduce(deliver(5, ActionEvent(
		PendingAction("action-1", ActionElicitation, Owner{Type: OwnerActivity, ID: "act-1"}, false)))))

	done := ActivityUpdate{ActivityID: "act-1", State: ActivityCompleted}
	require.ErrorIs(t, reducer.Reduce(deliver(6, Event{Type: EventActivityUpdate, Activity: &done})),
		&ViolationError{Kind: ViolationParentTerminalBeforeChild})
}

func TestActionIdentityIsFixedOnFirstSight(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name  string
		patch ActionUpdate
	}{
		{name: "kind", patch: ActionUpdate{ActionID: "act-1", State: ActionAccepted, Kind: ActionElicitation}},
		{
			name:  "owner",
			patch: ActionUpdate{ActionID: "act-1", State: ActionAccepted, Owner: Owner{Type: OwnerTurn, ID: "other"}},
		},
		{name: "ownership root", patch: ActionUpdate{ActionID: "act-1", State: ActionAccepted, RunID: "run-2"}},
		{
			name:  "what it blocks",
			patch: ActionUpdate{ActionID: "act-1", State: ActionAccepted, BlocksForeground: blocking(true)},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			reducer := openStream(t, negotiatedForDecode())
			require.NoError(t, reducer.Reduce(deliver(2, AcceptedEvent(Submission{SubmissionID: "s", ClientNonce: "n"}, "turn-1"))))
			require.NoError(t, reducer.Reduce(deliver(3, TransitionEvent(ForegroundRunning, "cycle-1", "turn-1"))))
			require.NoError(t, reducer.Reduce(deliver(4, ActionEvent(ActionUpdate{
				ActionID: "act-1", Kind: ActionPermission, State: ActionPending,
				Owner: Owner{Type: OwnerTurn, ID: "turn-1"}, RunID: "run-1", BlocksForeground: blocking(false),
			}))))

			patch := tc.patch
			require.ErrorIs(t, reducer.Reduce(deliver(5, ActionEvent(patch))),
				&ViolationError{Kind: ViolationImmutableIdentityChange})
		})
	}
}

func TestAnActionThatArrivesResolvedBlocksNothing(t *testing.T) {
	t.Parallel()

	reducer := openStream(t, negotiatedForDecode())
	require.NoError(t, reducer.Reduce(deliver(2, AcceptedEvent(Submission{SubmissionID: "s", ClientNonce: "n"}, "turn-1"))))
	require.NoError(t, reducer.Reduce(deliver(3, TransitionEvent(ForegroundRunning, "cycle-1", "turn-1"))))
	require.NoError(t, reducer.Reduce(deliver(4, ActionEvent(ActionUpdate{
		ActionID: "act-1", Kind: ActionPermission, State: ActionCancelled,
		Owner: Owner{Type: OwnerTurn, ID: "turn-1"}, BlocksForeground: blocking(true),
	}))))
	require.NoError(t, reducer.Reduce(deliver(5, IdleEvent("cycle-1", "turn-1", StopReasonCancelled, OutcomeCancelled))))

	action, ok := reducer.State().Action("act-1")
	require.True(t, ok)
	require.Equal(t, ActionCancelled, action.State)

	_, ok = reducer.State().Action("missing")
	require.False(t, ok)

	_, ok = reducer.State().Turn("missing")
	require.False(t, ok)
}

func TestQuiescenceIsRefusedWhenTheAnswerNamedAnotherClass(t *testing.T) {
	t.Parallel()

	negotiated := Negotiated{
		Versions:                []int{1},
		AuthoritativeQuiescence: true,
		QuiescenceSource:        ProofClassProcessContainment,
	}
	reducer := openStream(t, negotiated)

	fact := QuiescenceFact{Quiescent: true, Source: ProofClassNativeSettledBarrier, Watermark: 1}
	require.ErrorIs(t, reducer.Reduce(deliver(2, QuiescenceEvent(fact))),
		&ViolationError{Kind: ViolationUnnegotiatedFact})
}

func TestVacancyIsNotQuiescence(t *testing.T) {
	t.Parallel()

	require.True(t, State{}.Vacant())
	require.False(t, State{Foreground: &Foreground{State: ForegroundRunning}}.Vacant())
	require.False(t, State{Activities: []ActivityRecord{{State: ActivityRunning}}}.Vacant())
	require.False(t, State{Actions: []ActionRecord{{State: ActionPending}}}.Vacant())
	require.True(t, State{
		Foreground: &Foreground{State: ForegroundIdle},
		Activities: []ActivityRecord{{State: ActivityCompleted}},
		Actions:    []ActionRecord{{State: ActionAccepted}},
	}.Vacant())
}

func TestSnapshotActivityStatesACompleteIdentity(t *testing.T) {
	t.Parallel()

	snapshot := Snapshot{
		Foreground: Foreground{State: ForegroundRunning, CycleID: "c", TurnID: "t", Origin: CauseSubmission},
		Activities: []ActivityUpdate{{ActivityID: "act-1", State: ActivityRunning, OriginTurnID: "t"}},
	}

	reducer := NewReducer(Options{Negotiated: negotiatedForDecode()})
	require.ErrorIs(t, reducer.Reduce(deliver(1, Event{Type: EventSnapshot, Snapshot: &snapshot})),
		&ViolationError{Kind: ViolationImmutableIdentityChange})
}

func TestEndingIdleNamesATurnTheStreamOpened(t *testing.T) {
	t.Parallel()

	reducer := openStream(t, Negotiated{Versions: []int{1}})
	require.ErrorIs(t, reducer.Reduce(deliver(2, IdleEvent("cycle-1", "ghost", StopReasonEndTurn, OutcomeSuccess))),
		&ViolationError{Kind: ViolationUnknownEntity})
}

func TestResolvedActionResolvesExactlyOnce(t *testing.T) {
	t.Parallel()

	reducer := openStream(t, negotiatedForDecode())
	require.NoError(t, reducer.Reduce(deliver(2, AcceptedEvent(Submission{SubmissionID: "s", ClientNonce: "n"}, "turn-1"))))
	require.NoError(t, reducer.Reduce(deliver(3, TransitionEvent(ForegroundRunning, "cycle-1", "turn-1"))))
	require.NoError(t, reducer.Reduce(deliver(4, ActionEvent(
		PendingAction("act-1", ActionPermission, Owner{Type: OwnerTurn, ID: "turn-1"}, false)))))

	// A patch that leaves the action pending records the same nonterminal state and
	// keeps its owner incomplete.
	require.NoError(t, reducer.Reduce(deliver(5, ActionEvent(ResolvedAction("act-1", ActionPending)))))
	require.NoError(t, reducer.Reduce(deliver(6, ActionEvent(ResolvedAction("act-1", ActionAccepted)))))

	require.ErrorIs(t, reducer.Reduce(deliver(7, ActionEvent(ResolvedAction("act-1", ActionDeclined)))),
		&ViolationError{Kind: ViolationPostTerminalMutation})
}

// TestTerminalActionAdmitsOnlyNoOpRestatement pins the member-wise basis on the
// entity the shared battery states it for only through activities. A restatement
// that carries no difference is suppressed and consumes its sequence; one that
// carries any difference is refused, and a carried immutable difference is
// refused under the terminal token because a token naming a terminal entity
// always wins.
func TestTerminalActionAdmitsOnlyNoOpRestatement(t *testing.T) {
	t.Parallel()

	settled := func(t *testing.T) *Reducer {
		t.Helper()

		reducer := openStream(t, negotiatedForDecode())
		require.NoError(t, reducer.Reduce(deliver(2, AcceptedEvent(Submission{SubmissionID: "s", ClientNonce: "n"}, "turn-1"))))
		require.NoError(t, reducer.Reduce(deliver(3, TransitionEvent(ForegroundRunning, "cycle-1", "turn-1"))))
		require.NoError(t, reducer.Reduce(deliver(4, ActionEvent(ActionUpdate{
			ActionID: "act-1", Kind: ActionPermission, State: ActionPending,
			Owner: Owner{Type: OwnerTurn, ID: "turn-1"}, BlocksForeground: blocking(false),
		}))))
		require.NoError(t, reducer.Reduce(deliver(5, ActionEvent(ResolvedAction("act-1", ActionAccepted)))))

		return reducer
	}

	t.Run("a restatement carrying no difference is suppressed", func(t *testing.T) {
		t.Parallel()

		reducer := settled(t)
		require.NoError(t, reducer.Reduce(deliver(6, ActionEvent(ActionUpdate{
			ActionID: "act-1", Kind: ActionPermission, State: ActionAccepted,
			Owner: Owner{Type: OwnerTurn, ID: "turn-1"}, BlocksForeground: blocking(false),
		}))))

		state := reducer.State()
		require.Equal(t, uint64(6), state.ReducedThrough)
		require.Zero(t, state.SuppressedRetransmissions)

		action, known := state.Action("act-1")
		require.True(t, known)
		require.Equal(t, ActionAccepted, action.State)
	})

	t.Run("a restatement changing an immutable reports the terminal token", func(t *testing.T) {
		t.Parallel()

		reducer := settled(t)
		require.ErrorIs(t, reducer.Reduce(deliver(6, ActionEvent(ActionUpdate{
			ActionID: "act-1", Kind: ActionElicitation, State: ActionAccepted,
		}))), &ViolationError{Kind: ViolationPostTerminalMutation})
	})
}
