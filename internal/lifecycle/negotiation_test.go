package lifecycle

import (
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestRawLifecycleObjectsRejectAmbiguityAndMalformedValues(t *testing.T) {
	for _, tc := range []struct{ raw, field string }{
		{`null`, ""},
		{`[]`, ""},
		{`{"version":1,:2}`, ""},
		{`{"version":}`, ".version"},
		{`{"version":1`, ""},
		{`{"version":1} 2`, ""},
		{`{"version":1,"version":1}`, ".version"},
		{`{"version":"1"}`, ".version"},
		{`{"version":1e0}`, ".version"},
	} {
		_, present, refusal := DecodeOffer(map[string]any{MetaKey: json.RawMessage(tc.raw)})
		require.False(t, present)
		require.Equal(t, &ParamError{Field: MetaPath + tc.field, Verdict: VerdictUnsupported}, refusal)
	}
	_, refusal := DecodePromptCorrelation(map[string]any{MetaKey: json.RawMessage(
		`{"version":1,"submission":{"submissionId":42,"clientNonce":"nonce"}}`,
	)}, Negotiated{Version: 1})
	require.Equal(t, paramError(fieldSubmission, fieldSubmissionID), refusal)
}

func TestRetainRequestMetadataPreservesOnlyOwnedWireSemantics(t *testing.T) {
	for _, tc := range []struct {
		params  string
		present bool
		refused bool
	}{
		{params: `null`},
		{params: `{"extra":1,"_meta":{"foreign":{"version":2,"version":1}}}`},
		{params: `{"_meta":{"foreign":1},"_meta":{"foreign":2}}`},
		{params: `{"_meta":{"foreign":true,"acp-go.dev/lifecycle":{"version":1}}}`, present: true},
		{params: `{"_meta":{"acp-go.dev/lifecycle":{"version":2},"acp-go.dev/lifecycle":{"version":1}}}`, refused: true},
		{params: `{"_meta":{"acp-go.dev/lifecycle":{"version":1}},"_meta":null}`, refused: true},
	} {
		var request struct {
			Meta map[string]any `json:"_meta"` //nolint:tagliatelle // ACP wire name.
		}
		require.NoError(t, json.Unmarshal([]byte(tc.params), &request))
		meta := RetainRequestMetadata(request.Meta, json.RawMessage(tc.params))
		_, present, refusal := DecodeOffer(meta)
		require.Equal(t, tc.present, present)
		if tc.refused {
			require.Equal(t, paramError(), refusal)
		} else {
			require.Nil(t, refusal)
		}
	}
}

func TestDecodeOfferReadsWhatTheHostAsked(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name    string
		meta    map[string]any
		present bool
		field   string
	}{
		{name: "absent offer asks for nothing"},
		{name: "wire float version", meta: map[string]any{MetaKey: map[string]any{"version": float64(1)}}, present: true},
		{name: "embedded int version", meta: map[string]any{MetaKey: map[string]any{"version": 1}}, present: true},
		{name: "json number version", meta: map[string]any{MetaKey: map[string]any{"version": json.Number("1")}}, present: true},
		{name: "json number cannot wrap to version one", meta: map[string]any{MetaKey: map[string]any{"version": json.Number("4294967297")}}, field: MetaPath + ".version"},
		{name: "invalid json number", meta: map[string]any{MetaKey: map[string]any{"version": json.Number("1.0")}}, field: MetaPath + ".version"},
		{
			name:  "a non-object offer",
			meta:  map[string]any{MetaKey: []any{1}},
			field: MetaPath,
		},
		{
			name:  "an unknown member beside version",
			meta:  map[string]any{MetaKey: map[string]any{"version": float64(1), "extra": true}},
			field: MetaPath + ".extra",
		},
		{name: "a missing version", meta: map[string]any{MetaKey: map[string]any{}}, field: MetaPath + ".version"},
		{name: "a fractional version", meta: map[string]any{MetaKey: map[string]any{"version": 1.5}}, field: MetaPath + ".version"},
		{name: "an unsupported version", meta: map[string]any{MetaKey: map[string]any{"version": 2}}, field: MetaPath + ".version"},
		{name: "a non-numeric version", meta: map[string]any{MetaKey: map[string]any{"version": "1"}}, field: MetaPath + ".version"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			offer, present, refusal := DecodeOffer(tc.meta)

			if tc.field != "" {
				require.NotNil(t, refusal)
				require.Equal(t, tc.field, refusal.Field)
				require.Equal(t, "unsupported "+tc.field, refusal.Error())
				require.False(t, present)

				return
			}

			require.Nil(t, refusal)
			require.Equal(t, tc.present, present)
			require.Equal(t, Offer{}, offer)
		})
	}
}

func TestAnswerCarriesCurrentVersion(t *testing.T) {
	t.Parallel()

	proven := Negotiated{ActivityKinds: []ActivityKind{}}

	offer, present, refusal := DecodeOffer(map[string]any{MetaKey: map[string]any{"version": Version}})
	require.True(t, present)
	require.Nil(t, refusal)

	answer := offer.Answer(proven)
	require.Equal(t, 1, answer.Version)
	require.Equal(t, 1, answer.NegotiatedVersion())

	require.Equal(t, 0, Negotiated{}.NegotiatedVersion())
	require.False(t, Negotiated{}.Present())
}

func TestAdvertisementRendersOnlyProvenFacts(t *testing.T) {
	t.Parallel()

	degenerate := Negotiated{Version: 1, ActivityKinds: []ActivityKind{}}.Advertisement()
	require.Equal(t, map[string]any{
		"version":                 1,
		"updatesOutsidePrompt":    false,
		"authoritativeQuiescence": false,
		"activityKinds":           []string{},
	}, degenerate)

	proven := Negotiated{
		Version:                 1,
		UpdatesOutsidePrompt:    true,
		AuthoritativeQuiescence: true,
		QuiescenceSource:        ProofClassProcessContainment,
		ActivityKinds:           []ActivityKind{ActivityTask},
	}
	require.Equal(t, "process-containment", proven.Advertisement()["quiescenceSource"])
	require.Equal(t, []string{"task"}, proven.Advertisement()["activityKinds"])
	require.True(t, proven.DeclaresActivityKind(ActivityTask))
	require.False(t, proven.DeclaresActivityKind(ActivitySubagent))
}

func TestRejectKeyRefusesTheLiteralByName(t *testing.T) {
	t.Parallel()

	require.Nil(t, RejectKey(nil))
	require.Nil(t, RejectKey(map[string]any{"other": 1}))

	refusal := RejectKey(map[string]any{MetaKey: map[string]any{}})
	require.NotNil(t, refusal)
	require.Equal(t, MetaPath, refusal.Field)
}

func TestDecodePromptCorrelationStrictness(t *testing.T) {
	t.Parallel()

	negotiated := Negotiated{Version: 1}
	submission := func(members map[string]any) map[string]any {
		return map[string]any{MetaKey: map[string]any{"version": float64(1), "submission": members}}
	}

	for _, tc := range []struct {
		name       string
		negotiated Negotiated
		meta       map[string]any
		field      string
		verdict    string
		want       Submission
	}{
		{
			name:       "an unnegotiated connection carries no key",
			negotiated: Negotiated{},
		},
		{
			name:       "an unnegotiated connection refuses a present key",
			negotiated: Negotiated{},
			meta:       map[string]any{MetaKey: map[string]any{}},
			field:      MetaPath,
		},
		// The host left the key out rather than misspelling it, which is the one
		// case on this path a host fixes by adding the value it omitted.
		{
			name:       "a negotiated connection requires the key",
			negotiated: negotiated,
			field:      MetaPath,
			verdict:    VerdictMissing,
		},
		{
			name:       "a non-object value",
			negotiated: negotiated,
			meta:       map[string]any{MetaKey: "value"},
			field:      MetaPath,
		},
		{
			name:       "an unknown top member",
			negotiated: negotiated,
			meta:       map[string]any{MetaKey: map[string]any{"version": float64(1), "extra": 1}},
			field:      MetaPath + ".extra",
		},
		{
			name:       "an unsupported version",
			negotiated: negotiated,
			meta:       map[string]any{MetaKey: map[string]any{"version": float64(2), "submission": map[string]any{}}},
			field:      MetaPath + ".version",
		},
		// The correlation surface refuses this twice over: the value names no
		// integer, and no integer it could be truncated into is negotiated. The
		// case pins the verdict rather than either rule on its own.
		{
			name:       "a version too large to name an integer exactly",
			negotiated: negotiated,
			meta:       map[string]any{MetaKey: map[string]any{"version": 1e300, "submission": map[string]any{}}},
			field:      MetaPath + ".version",
		},
		{
			name:       "a missing version",
			negotiated: negotiated,
			meta:       map[string]any{MetaKey: map[string]any{"submission": map[string]any{}}},
			field:      MetaPath + ".version",
		},
		{
			name:       "a non-object submission",
			negotiated: negotiated,
			meta:       map[string]any{MetaKey: map[string]any{"version": float64(1), "submission": 1}},
			field:      MetaPath + ".submission",
		},
		{
			name:       "an unknown submission member",
			negotiated: negotiated,
			meta:       submission(map[string]any{"submissionId": "s", "clientNonce": "n", "extra": 1}),
			field:      MetaPath + ".submission.extra",
		},
		{
			name:       "a missing submission id",
			negotiated: negotiated,
			meta:       submission(map[string]any{"clientNonce": "n"}),
			field:      MetaPath + ".submission.submissionId",
		},
		{
			name:       "a missing client nonce",
			negotiated: negotiated,
			meta:       submission(map[string]any{"submissionId": "s"}),
			field:      MetaPath + ".submission.clientNonce",
		},
		{
			name:       "a non-string identifier",
			negotiated: negotiated,
			meta:       submission(map[string]any{"submissionId": 1, "clientNonce": "n"}),
			field:      MetaPath + ".submission.submissionId",
		},
		{
			name:       "an emptied identifier",
			negotiated: negotiated,
			meta:       submission(map[string]any{"submissionId": "", "clientNonce": "n"}),
			field:      MetaPath + ".submission.submissionId",
		},
		{
			name:       "an emptied optional run id",
			negotiated: negotiated,
			meta:       submission(map[string]any{"submissionId": "s", "clientNonce": "n", "runId": ""}),
			field:      MetaPath + ".submission.runId",
		},
		{
			name:       "an over-bound identifier",
			negotiated: negotiated,
			meta:       submission(map[string]any{"submissionId": overBound(), "clientNonce": "n"}),
			field:      MetaPath + ".submission.submissionId",
		},
		{
			name:       "a complete value",
			negotiated: negotiated,
			meta:       submission(map[string]any{"submissionId": "s", "clientNonce": "n", "runId": "r"}),
			want:       Submission{SubmissionID: "s", ClientNonce: "n", RunID: "r"},
		},
		{
			name:       "an omitted optional run id",
			negotiated: negotiated,
			meta:       submission(map[string]any{"submissionId": "s", "clientNonce": "n"}),
			want:       Submission{SubmissionID: "s", ClientNonce: "n"},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			got, refusal := DecodePromptCorrelation(tc.meta, tc.negotiated)

			if tc.field != "" {
				verdict := tc.verdict
				if verdict == "" {
					verdict = VerdictUnsupported
				}

				require.NotNil(t, refusal)
				require.Equal(t, tc.field, refusal.Field)
				require.Equal(t, verdict, refusal.Verdict)
				require.Equal(t, verdict+" "+tc.field, refusal.Error())

				return
			}

			require.Nil(t, refusal)
			require.Equal(t, tc.want, got)
		})
	}
}

func TestActionCorrelationValueNamesTheStreamAndTheAction(t *testing.T) {
	t.Parallel()

	correlation := ActionCorrelation{
		StreamID: "strm-1",
		ActionID: "act-1",
		Owner:    Owner{Type: OwnerTurn, ID: "turn-1"},
	}
	require.Equal(t, map[string]any{
		"version":  1,
		"streamId": "strm-1",
		"action": map[string]any{
			"actionId": "act-1",
			"owner":    map[string]any{"type": "turn", "id": "turn-1"},
		},
	}, correlation.Value())

	correlation.RunID = "run-1"
	action, _ := correlation.Value()["action"].(map[string]any)
	require.Equal(t, "run-1", action["runId"])
}

func overBound() string {
	value := make([]byte, IdentifierBound+1)
	for index := range value {
		value[index] = 'a'
	}

	return string(value)
}
