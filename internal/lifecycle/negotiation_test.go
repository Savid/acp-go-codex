package lifecycle

import (
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestDecodeOfferReadsWhatTheHostAsked(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name     string
		meta     map[string]any
		present  bool
		versions []int
		field    string
	}{
		{name: "absent offer asks for nothing"},
		{
			name:     "wire float versions",
			meta:     map[string]any{MetaKey: map[string]any{"versions": []any{float64(1)}}},
			present:  true,
			versions: []int{1},
		},
		{
			name:     "embedded int versions",
			meta:     map[string]any{MetaKey: map[string]any{"versions": []any{1}}},
			present:  true,
			versions: []int{1},
		},
		{
			name:     "json number versions",
			meta:     map[string]any{MetaKey: map[string]any{"versions": []any{json.Number("1")}}},
			present:  true,
			versions: []int{1},
		},
		{
			name:     "an unordered offer still intersects",
			meta:     map[string]any{MetaKey: map[string]any{"versions": []any{float64(2), float64(1)}}},
			present:  true,
			versions: []int{2, 1},
		},
		{
			name:  "a non-object offer",
			meta:  map[string]any{MetaKey: []any{1}},
			field: MetaPath,
		},
		{
			name:  "an unknown member beside versions",
			meta:  map[string]any{MetaKey: map[string]any{"versions": []any{float64(1)}, "extra": true}},
			field: MetaPath + ".extra",
		},
		{
			name:  "an empty version array",
			meta:  map[string]any{MetaKey: map[string]any{"versions": []any{}}},
			field: MetaPath + ".versions",
		},
		{
			name:  "versions is not an array",
			meta:  map[string]any{MetaKey: map[string]any{"versions": 1}},
			field: MetaPath + ".versions",
		},
		{
			name:  "a fractional version",
			meta:  map[string]any{MetaKey: map[string]any{"versions": []any{1.5}}},
			field: MetaPath + ".versions",
		},
		{
			name:  "a non-numeric version",
			meta:  map[string]any{MetaKey: map[string]any{"versions": []any{"1"}}},
			field: MetaPath + ".versions",
		},
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
			require.Equal(t, tc.versions, offer.Versions)
		})
	}
}

func TestAnswerIntersectsTheOffer(t *testing.T) {
	t.Parallel()

	proven := Negotiated{ActivityKinds: []ActivityKind{}}

	answer, ok := Offer{Versions: []int{2, 1, 1}}.Answer(proven)
	require.True(t, ok)
	require.Equal(t, []int{1}, answer.Versions)
	require.Equal(t, 1, answer.NegotiatedVersion())

	_, ok = Offer{Versions: []int{2}}.Answer(proven)
	require.False(t, ok)

	require.Equal(t, 0, Negotiated{}.NegotiatedVersion())
	require.False(t, Negotiated{}.Present())
}

func TestAdvertisementRendersOnlyProvenFacts(t *testing.T) {
	t.Parallel()

	degenerate := Negotiated{Versions: []int{1}, ActivityKinds: []ActivityKind{}}.Advertisement()
	require.Equal(t, map[string]any{
		"versions":                []int{1},
		"updatesOutsidePrompt":    false,
		"authoritativeQuiescence": false,
		"activityKinds":           []string{},
	}, degenerate)

	proven := Negotiated{
		Versions:                []int{1},
		UpdatesOutsidePrompt:    true,
		AuthoritativeQuiescence: true,
		QuiescenceSource:        ProofClassProcessContainment,
		ActivityKinds:           []ActivityKind{ActivityTask},
	}
	require.Equal(t, "process-containment", proven.Advertisement()["quiescenceSource"])
	require.Equal(t, []string{"task"}, proven.Advertisement()["activityKinds"])
	require.True(t, proven.DeclaresActivityKind(ActivityTask))
	require.False(t, proven.DeclaresActivityKind(ActivityGoal))
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

	negotiated := Negotiated{Versions: []int{1}}
	submission := func(members map[string]any) map[string]any {
		return map[string]any{MetaKey: map[string]any{"version": float64(1), "submission": members}}
	}

	for _, tc := range []struct {
		name       string
		negotiated Negotiated
		meta       map[string]any
		field      string
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
		{name: "a negotiated connection requires the key", negotiated: negotiated, field: MetaPath},
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
				require.NotNil(t, refusal)
				require.Equal(t, tc.field, refusal.Field)

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
