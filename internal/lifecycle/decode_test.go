package lifecycle

import (
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/require"
)

func negotiatedForDecode() Negotiated {
	return Negotiated{
		Versions:                []int{1},
		AuthoritativeQuiescence: true,
		QuiescenceSource:        ProofClassProcessContainment,
		ActivityKinds:           []ActivityKind{ActivityTask},
	}
}

// notification frames one envelope on the one legal carrier so a decoder table row
// states only the part it is about.
func notification(envelope string) json.RawMessage {
	return json.RawMessage(`{"sessionId":"sess-1","update":{"sessionUpdate":"session_info_update"},` +
		`"_meta":{"` + MetaKey + `":` + envelope + `}}`)
}

func envelopeAround(event string) string {
	return `{"version":1,"streamId":"strm-1","sequence":1,"event":` + event + `}`
}

func TestDecodeReportsNoEnvelopeForOrdinaryContent(t *testing.T) {
	t.Parallel()

	for _, params := range []string{
		`{"sessionId":"sess-1","update":{"sessionUpdate":"agent_message_chunk"}}`,
		`{"sessionId":"sess-1","update":{"sessionUpdate":"session_info_update"},"_meta":{"other":1}}`,
		`{"sessionId":"sess-1","update":{"sessionUpdate":"session_info_update"},"_meta":"scalar"}`,
	} {
		_, err := DecodeSessionUpdate(json.RawMessage(params), negotiatedForDecode())
		require.ErrorIs(t, err, ErrNoEnvelope)
	}
}

func TestDecodeRefusesAFrameItCannotRead(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name   string
		params string
	}{
		{name: "not decodable JSON", params: `{"sessionId":`},
		{name: "not an object", params: `["session/update"]`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			_, err := DecodeSessionUpdate(json.RawMessage(tc.params), negotiatedForDecode())
			require.ErrorIs(t, err, &ViolationError{Kind: ViolationMalformedEnvelope})
		})
	}
}

func TestCarrierClassification(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name   string
		update string
		want   CarrierClass
	}{
		{name: "the identity-only carrier", update: `{"sessionUpdate":"session_info_update"}`, want: CarrierSessionInfo},
		{name: "a titled carrier mutates state", update: `{"sessionUpdate":"session_info_update","title":"t"}`, want: CarrierIneligible},
		{name: "a stamped carrier mutates state", update: `{"sessionUpdate":"session_info_update","updatedAt":"t"}`, want: CarrierIneligible},
		{name: "an entity patch", update: `{"sessionUpdate":"tool_call_update"}`, want: CarrierIneligible},
		{name: "no discriminant", update: `{"other":1}`, want: CarrierUnknown},
		{name: "a non-string discriminant", update: `{"sessionUpdate":1}`, want: CarrierUnknown},
		{name: "not an object", update: `"session_info_update"`, want: CarrierUnknown},
		{name: "absent", update: ``, want: CarrierUnknown},
		{name: "json null", update: `null`, want: CarrierUnknown},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			require.Equal(t, tc.want, CarrierClassForSessionUpdate(json.RawMessage(tc.update)))
		})
	}
}

func TestDecodeEnvelopeStrictness(t *testing.T) {
	t.Parallel()

	accepted := `{"type":"prompt_accepted","submissionId":"s","clientNonce":"n","turnId":"t"}`

	for _, tc := range []struct {
		name     string
		envelope string
		kind     ViolationKind
	}{
		{name: "not an object", envelope: `[1]`, kind: ViolationIllegalCarrier},
		{
			name:     "an unknown envelope member",
			envelope: `{"version":1,"streamId":"strm-1","sequence":1,"event":` + accepted + `,"extra":1}`,
			kind:     ViolationUnknownField,
		},
		{
			name:     "a missing version",
			envelope: `{"streamId":"strm-1","sequence":1,"event":` + accepted + `}`,
			kind:     ViolationMalformedEnvelope,
		},
		{
			name:     "a non-integer version",
			envelope: `{"version":"1","streamId":"strm-1","sequence":1,"event":` + accepted + `}`,
			kind:     ViolationMalformedEnvelope,
		},
		{
			name:     "an unnegotiated version",
			envelope: `{"version":2,"streamId":"strm-1","sequence":1,"event":` + accepted + `}`,
			kind:     ViolationUnsupportedVersion,
		},
		{
			name:     "a missing sequence",
			envelope: `{"version":1,"streamId":"strm-1","event":` + accepted + `}`,
			kind:     ViolationMalformedEnvelope,
		},
		{
			name:     "a non-integer sequence",
			envelope: `{"version":1,"streamId":"strm-1","sequence":"1","event":` + accepted + `}`,
			kind:     ViolationMalformedEnvelope,
		},
		{
			name:     "a missing event",
			envelope: `{"version":1,"streamId":"strm-1","sequence":1}`,
			kind:     ViolationMalformedEnvelope,
		},
		{
			name:     "a missing event type",
			envelope: envelopeAround(`{"turnId":"t"}`),
			kind:     ViolationMalformedEnvelope,
		},
		{
			name:     "a non-string stream identity",
			envelope: `{"version":1,"streamId":1,"sequence":1,"event":` + accepted + `}`,
			kind:     ViolationMalformedEnvelope,
		},
		// The integer-typed members are lexically integral, not merely
		// integer-valued: the number equality that reads 1 and 1.0 as one value
		// inside an opaque progress object stops at the members this contract
		// types as integers, because an ordering identity with a fraction part
		// has no integral spelling to be contiguous against.
		{
			name:     "a fractional version",
			envelope: `{"version":1.0,"streamId":"strm-1","sequence":1,"event":` + accepted + `}`,
			kind:     ViolationMalformedEnvelope,
		},
		{
			name:     "an exponent version",
			envelope: `{"version":1e0,"streamId":"strm-1","sequence":1,"event":` + accepted + `}`,
			kind:     ViolationMalformedEnvelope,
		},
		{
			name:     "a fractional sequence",
			envelope: `{"version":1,"streamId":"strm-1","sequence":2.0,"event":` + accepted + `}`,
			kind:     ViolationMalformedEnvelope,
		},
		{
			name: "a fractional watermark",
			envelope: `{"version":1,"streamId":"strm-1","sequence":3,"event":` +
				`{"type":"quiescence_update","quiescent":true,"source":"process-containment","watermark":2.0}}`,
			kind: ViolationMalformedEnvelope,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			_, err := DecodeSessionUpdate(notification(tc.envelope), negotiatedForDecode())
			require.ErrorIs(t, err, &ViolationError{Kind: tc.kind})
		})
	}
}

func TestDecodeEventStrictness(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name  string
		event string
		kind  ViolationKind
	}{
		{
			name:  "a snapshot with no foreground",
			event: `{"type":"lifecycle_snapshot","activities":[],"actions":[],"quiescence":{"quiescent":false}}`,
			kind:  ViolationMalformedEnvelope,
		},
		{
			name: "a snapshot with no quiescence",
			event: `{"type":"lifecycle_snapshot","foreground":{"state":"idle","cycleId":"c"},` +
				`"activities":[],"actions":[]}`,
			kind: ViolationMalformedEnvelope,
		},
		{
			name: "a snapshot with no activity set",
			event: `{"type":"lifecycle_snapshot","foreground":{"state":"idle","cycleId":"c"},` +
				`"actions":[],"quiescence":{"quiescent":false}}`,
			kind: ViolationMalformedEnvelope,
		},
		{
			name: "a snapshot whose activity set is not an array",
			event: `{"type":"lifecycle_snapshot","foreground":{"state":"idle","cycleId":"c"},` +
				`"activities":1,"actions":[],"quiescence":{"quiescent":false}}`,
			kind: ViolationMalformedEnvelope,
		},
		{
			name: "an unknown foreground member",
			event: `{"type":"lifecycle_snapshot","foreground":{"state":"idle","cycleId":"c","extra":1},` +
				`"activities":[],"actions":[],"quiescence":{"quiescent":false}}`,
			kind: ViolationUnknownField,
		},
		{
			name: "an invalid foreground state",
			event: `{"type":"lifecycle_snapshot","foreground":{"state":"waiting","cycleId":"c"},` +
				`"activities":[],"actions":[],"quiescence":{"quiescent":false}}`,
			kind: ViolationMalformedEnvelope,
		},
		{
			name: "a foreground origin naming a session cause",
			event: `{"type":"lifecycle_snapshot","foreground":{"state":"running","cycleId":"c","turnId":"t","origin":"session"},` +
				`"activities":[],"actions":[],"quiescence":{"quiescent":false}}`,
			kind: ViolationMalformedEnvelope,
		},
		{
			name: "a snapshot quiescence with an unknown member",
			event: `{"type":"lifecycle_snapshot","foreground":{"state":"idle","cycleId":"c"},` +
				`"activities":[],"actions":[],"quiescence":{"quiescent":false,"extra":1}}`,
			kind: ViolationUnknownField,
		},
		{
			name:  "a transition with an invalid state",
			event: `{"type":"state_update","state":"waiting","cycleId":"c","turnId":"t","cause":"submission"}`,
			kind:  ViolationMalformedEnvelope,
		},
		{
			name:  "a transition with an invalid cause",
			event: `{"type":"state_update","state":"running","cycleId":"c","turnId":"t","cause":"other"}`,
			kind:  ViolationMalformedEnvelope,
		},
		{
			name:  "a submission transition naming no turn",
			event: `{"type":"state_update","state":"running","cycleId":"c","cause":"submission"}`,
			kind:  ViolationMalformedEnvelope,
		},
		{
			name:  "a running transition carrying an outcome",
			event: `{"type":"state_update","state":"running","cycleId":"c","turnId":"t","cause":"submission","outcome":"success"}`,
			kind:  ViolationMalformedEnvelope,
		},
		{
			name: "an invalid stop reason",
			event: `{"type":"state_update","state":"idle","cycleId":"c","turnId":"t","cause":"submission",` +
				`"stopReason":"stopped","outcome":"success"}`,
			kind: ViolationMalformedEnvelope,
		},
		{
			name: "an invalid outcome",
			event: `{"type":"state_update","state":"idle","cycleId":"c","turnId":"t","cause":"submission",` +
				`"stopReason":"end_turn","outcome":"done"}`,
			kind: ViolationMalformedEnvelope,
		},
		{
			name:  "an activity update carrying no activity",
			event: `{"type":"activity_update"}`,
			kind:  ViolationMalformedEnvelope,
		},
		{
			name:  "an activity with an invalid kind",
			event: `{"type":"activity_update","activity":{"activityId":"a","kind":"widget","state":"running"}}`,
			kind:  ViolationMalformedEnvelope,
		},
		{
			name:  "an activity with an invalid state",
			event: `{"type":"activity_update","activity":{"activityId":"a","state":"halted"}}`,
			kind:  ViolationMalformedEnvelope,
		},
		{
			name:  "an activity with an invalid cause",
			event: `{"type":"activity_update","activity":{"activityId":"a","state":"running","cause":"other"}}`,
			kind:  ViolationMalformedEnvelope,
		},
		{
			name:  "an activity whose progress is not an object",
			event: `{"type":"activity_update","activity":{"activityId":"a","state":"running","progress":1}}`,
			kind:  ViolationMalformedEnvelope,
		},
		{
			name:  "an action update carrying no action",
			event: `{"type":"action_update"}`,
			kind:  ViolationMalformedEnvelope,
		},
		{
			name:  "an action with an invalid kind",
			event: `{"type":"action_update","action":{"actionId":"a","kind":"prompt","state":"pending"}}`,
			kind:  ViolationMalformedEnvelope,
		},
		{
			name:  "an action with an invalid state",
			event: `{"type":"action_update","action":{"actionId":"a","state":"answered"}}`,
			kind:  ViolationMalformedEnvelope,
		},
		{
			name:  "an action whose owner is not an object",
			event: `{"type":"action_update","action":{"actionId":"a","state":"pending","owner":1}}`,
			kind:  ViolationMalformedEnvelope,
		},
		{
			name:  "an action owner with an unknown member",
			event: `{"type":"action_update","action":{"actionId":"a","state":"pending","owner":{"type":"turn","id":"t","extra":1}}}`,
			kind:  ViolationUnknownField,
		},
		{
			name:  "an action owner with an invalid type",
			event: `{"type":"action_update","action":{"actionId":"a","state":"pending","owner":{"type":"cycle","id":"t"}}}`,
			kind:  ViolationMalformedEnvelope,
		},
		{
			name:  "a non-boolean blocking claim",
			event: `{"type":"action_update","action":{"actionId":"a","state":"pending","blocksForeground":"yes"}}`,
			kind:  ViolationMalformedEnvelope,
		},
		{
			name:  "a quiescence fact with no polarity",
			event: `{"type":"quiescence_update","source":"process-containment","watermark":0}`,
			kind:  ViolationMalformedEnvelope,
		},
		{
			name:  "a non-boolean polarity",
			event: `{"type":"quiescence_update","quiescent":"true"}`,
			kind:  ViolationMalformedEnvelope,
		},
		{
			name:  "a positive fact with no watermark",
			event: `{"type":"quiescence_update","quiescent":true,"source":"process-containment"}`,
			kind:  ViolationMalformedEnvelope,
		},
		{
			name:  "an unknown proof class",
			event: `{"type":"quiescence_update","quiescent":true,"source":"drain-window","watermark":0}`,
			kind:  ViolationMalformedEnvelope,
		},
		{
			name:  "a non-integer watermark",
			event: `{"type":"quiescence_update","quiescent":true,"source":"process-containment","watermark":"0"}`,
			kind:  ViolationMalformedEnvelope,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			_, err := DecodeSessionUpdate(notification(envelopeAround(tc.event)), negotiatedForDecode())
			require.ErrorIs(t, err, &ViolationError{Kind: tc.kind})
		})
	}
}

func TestDecodeReadsAWholeSnapshot(t *testing.T) {
	t.Parallel()

	event := `{"type":"lifecycle_snapshot","foreground":{"state":"requires_action","cycleId":"c","turnId":"t","origin":"activity"},` +
		`"activities":[{"activityId":"a","kind":"task","state":"running","cause":"activity","originTurnId":"t",` +
		`"toolCallId":"tool","runId":"run","progress":{"done":1}}],` +
		`"actions":[{"actionId":"act","kind":"elicitation","state":"pending","owner":{"type":"turn","id":"t"},` +
		`"runId":"run","blocksForeground":true}],` +
		`"quiescence":{"quiescent":false}}`

	delivery, err := DecodeSessionUpdate(notification(envelopeAround(event)), negotiatedForDecode())
	require.NoError(t, err)
	require.Equal(t, CarrierSessionInfo, delivery.Carrier)
	require.Equal(t, uint64(1), delivery.Sequence)
	require.Equal(t, CauseActivity, delivery.Event.Snapshot.Foreground.Origin)
	require.Equal(t, json.RawMessage(`{"done":1}`), delivery.Event.Snapshot.Activities[0].Progress)
	require.Equal(t, ActionElicitation, delivery.Event.Snapshot.Actions[0].Kind)
	require.True(t, *delivery.Event.Snapshot.Actions[0].BlocksForeground)
}

func TestDecodeRefusesAnOverBoundProgressObject(t *testing.T) {
	t.Parallel()

	filler := make([]byte, IdentifierBound)
	for index := range filler {
		filler[index] = 'a'
	}

	event := `{"type":"activity_update","activity":{"activityId":"a","state":"running","progress":{"note":"` +
		string(filler) + `"}}}`

	_, err := DecodeSessionUpdate(notification(envelopeAround(event)), negotiatedForDecode())
	require.ErrorIs(t, err, &ViolationError{Kind: ViolationMalformedEnvelope})
}

func TestViolationErrorNamesTheFrameItRefused(t *testing.T) {
	t.Parallel()

	refusal := violation(ViolationSequenceGap, "strm-1", 7, "expected 6")
	require.Equal(t, "lifecycle violation sequence_gap at strm-1#7: expected 6", refusal.Error())

	bare := violation(ViolationStaleStream, "strm-1", 7, "")
	require.Equal(t, "lifecycle violation stale_stream at strm-1#7", bare.Error())

	require.False(t, refusal.Is(ErrNoEnvelope))
	require.True(t, refusal.Is(&ViolationError{Kind: ViolationSequenceGap}))
}

func TestClosedVocabulariesRefuseAnUnlistedMember(t *testing.T) {
	t.Parallel()

	require.False(t, ForegroundState("waiting").Valid())
	require.False(t, Cause("other").Valid())
	require.False(t, Outcome("done").Valid())
	require.False(t, ValidStopReason("stopped"))
	require.False(t, ActivityKind("widget").Valid())
	require.False(t, ActivityState("halted").Valid())
	require.False(t, ActionKind("prompt").Valid())
	require.False(t, ActionState("answered").Valid())
	require.False(t, OwnerType("cycle").Valid())
	require.False(t, ProofClass("drain-window").Valid())
	require.True(t, ProofClassNativeSettledBarrier.Valid())
}

func TestDecodePresenceRulesInsideNestedObjects(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name  string
		event string
		kind  ViolationKind
	}{
		{
			name: "a foreground with no state",
			event: `{"type":"lifecycle_snapshot","foreground":{"cycleId":"c"},` +
				`"activities":[],"actions":[],"quiescence":{"quiescent":false}}`,
			kind: ViolationMalformedEnvelope,
		},
		{
			name: "an open turn with no origin",
			event: `{"type":"lifecycle_snapshot","foreground":{"state":"running","cycleId":"c","turnId":"t"},` +
				`"activities":[],"actions":[],"quiescence":{"quiescent":false}}`,
			kind: ViolationMalformedEnvelope,
		},
		{
			name: "an activity set entry that is not an object",
			event: `{"type":"lifecycle_snapshot","foreground":{"state":"idle","cycleId":"c"},` +
				`"activities":[1],"actions":[],"quiescence":{"quiescent":false}}`,
			kind: ViolationMalformedEnvelope,
		},
		{
			name: "an action set entry that is not an object",
			event: `{"type":"lifecycle_snapshot","foreground":{"state":"idle","cycleId":"c"},` +
				`"activities":[],"actions":[1],"quiescence":{"quiescent":false}}`,
			kind: ViolationMalformedEnvelope,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			_, err := DecodeSessionUpdate(notification(envelopeAround(tc.event)), negotiatedForDecode())
			require.ErrorIs(t, err, &ViolationError{Kind: tc.kind})
		})
	}
}
