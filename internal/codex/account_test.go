package codex

import (
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/require"
)

// The native answer carries the per-limit map beside a bare snapshot and
// credit members the adapter never reads.
func TestAccountUsageDecodesAndOrdersLimits(t *testing.T) {
	t.Parallel()

	var usage AccountUsage

	require.NoError(t, json.Unmarshal([]byte(`{
		"ordinaryUsageAllowed": true,
		"rateLimits": {"limitId": "codex", "limitName": null, "primary": {"usedPercent": 23, "windowDurationMins": 10080, "resetsAt": 1789960938}, "secondary": null, "credits": {"hasCredits": false}, "planType": "pro"},
		"rateLimitsByLimitId": {
			"codex_spark": {"limitId": "", "limitName": "Spark", "primary": {"usedPercent": 0, "windowDurationMins": 300, "resetsAt": 1789616611}, "secondary": {"usedPercent": 0.5, "windowDurationMins": 10080, "resetsAt": 1790203411}, "planType": "pro"},
			"codex": {"limitId": "codex", "limitName": null, "primary": {"usedPercent": 23, "windowDurationMins": 10080, "resetsAt": 1789960938}, "secondary": null, "planType": "pro"}
		},
		"rateLimitResetCredits": {"availableCount": 1},
		"accountId": "acct"
	}`), &usage))

	require.Equal(t, new(true), usage.OrdinaryUsageAllowed)

	limits := usage.Sorted()
	require.Len(t, limits, 2)
	require.Equal(t, "codex", limits[0].ID)
	require.Equal(t, "", limits[0].Name)
	require.Equal(t, &UsageWindow{UsedPercent: 23, WindowDurationMins: 10080, ResetsAt: 1789960938}, limits[0].Primary)
	require.Nil(t, limits[0].Secondary)
	require.Equal(t, "codex_spark", limits[1].ID, "the map key is the limit id")
	require.Equal(t, "Spark", limits[1].Name)
	require.InDelta(t, 0.5, limits[1].Secondary.UsedPercent, 0)
	require.Empty(t, AccountUsage{}.Sorted())

	var unstated AccountUsage

	require.NoError(t, json.Unmarshal([]byte(`{"ordinaryUsageAllowed": null, "rateLimitsByLimitId": {}}`), &unstated))
	require.Nil(t, unstated.OrdinaryUsageAllowed)

	var account Account

	require.NoError(t, json.Unmarshal([]byte(`{"account": null, "requiresOpenaiAuth": true}`), &account))
	require.Nil(t, account.Account)
	require.NoError(t, json.Unmarshal([]byte(`{"account": {"type": "chatgpt", "email": "x", "planType": "pro"}, "requiresOpenaiAuth": true}`), &account))
	require.Equal(t, &AccountIdentity{Type: "chatgpt", PlanType: "pro"}, account.Account)
}
