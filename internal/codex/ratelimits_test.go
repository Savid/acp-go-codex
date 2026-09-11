package codex

import (
	"context"
	"math"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestRateLimitSnapshotFromMap(t *testing.T) {
	snapshot, err := rateLimitSnapshotFromMap(map[string]any{
		"accountId":  "account-1",
		"rateLimits": map[string]any{"primary": map[string]any{"usedPercent": float64(99)}},
		"rateLimitsByLimitId": map[string]any{
			"spark": map[string]any{"limitName": "Spark", "planType": "pro", "primary": map[string]any{"usedPercent": float64(0), "windowDurationMins": float64(300)}},
			"codex": map[string]any{"planType": "pro", "primary": map[string]any{"usedPercent": float64(125), "windowDurationMins": float64(10080), "resetsAt": float64(2_000_000_000)}},
		},
	})
	require.NoError(t, err)
	require.Equal(t, "account-1", snapshot.AccountID)
	require.WithinDuration(t, time.Now(), snapshot.ObservedAt, time.Second)
	require.Len(t, snapshot.Pools, 2)
	require.Equal(t, "codex", snapshot.Pools[0].ID)
	require.Equal(t, float64(125), snapshot.Pools[0].Windows[0].UsedPercent)
	require.Equal(t, int64(604800), *snapshot.Pools[0].Windows[0].DurationSeconds)
	require.Equal(t, time.Unix(2_000_000_000, 0).UTC().Format(time.RFC3339), snapshot.Pools[0].Windows[0].ResetsAt)
	require.Equal(t, float64(0), snapshot.Pools[1].Windows[0].UsedPercent)
	require.Equal(t, int64(18000), *snapshot.Pools[1].Windows[0].DurationSeconds)
}

func TestRateLimitSnapshotRejectsUnmeasuredUtilization(t *testing.T) {
	for _, value := range []any{nil, "0", float64(-1), math.NaN(), math.Inf(1)} {
		_, err := rateLimitSnapshotFromMap(map[string]any{"rateLimits": map[string]any{"primary": map[string]any{"usedPercent": value}}})
		require.Error(t, err)
	}
}

func TestRateLimitSnapshotOmitsUnknownOptionalFacts(t *testing.T) {
	snapshot, err := rateLimitSnapshotFromMap(map[string]any{"rateLimits": map[string]any{
		"primary": map[string]any{"usedPercent": float64(10), "windowDurationMins": float64(-1), "resetsAt": "unknown"},
		"credits": map[string]any{"balance": "100", "unlimited": true},
	}})
	require.NoError(t, err)
	require.Len(t, snapshot.Pools, 1)
	require.Equal(t, "codex", snapshot.Pools[0].ID)
	require.Nil(t, snapshot.Pools[0].Windows[0].DurationSeconds)
	require.Empty(t, snapshot.Pools[0].Windows[0].ResetsAt)
	empty, err := rateLimitSnapshotFromMap(map[string]any{"rateLimits": map[string]any{"planType": "pro"}})
	require.NoError(t, err)
	require.Empty(t, empty.Pools)
}

func TestReadRateLimits(t *testing.T) {
	transport := newScriptTransport()
	client := &AppServerClient{rpc: newRPCConn(transport, nil)}
	defer client.Close(context.Background())
	snapshot, err := client.ReadRateLimits(context.Background())
	require.NoError(t, err)
	require.Len(t, snapshot.Pools, 1)
	require.Equal(t, "pro", snapshot.Pools[0].PlanType)
	require.Len(t, snapshot.Pools[0].Windows, 2)
	require.Equal(t, float64(12), snapshot.Pools[0].Windows[0].UsedPercent)
	require.NotEmpty(t, snapshot.Pools[0].Windows[0].ResetsAt)
}

func TestReadRateLimitsError(t *testing.T) {
	transport := newScriptTransport()
	transport.fail(methodAccountRateLimitsRead, "boom")
	client := &AppServerClient{rpc: newRPCConn(transport, nil)}
	defer client.Close(context.Background())
	_, err := client.ReadRateLimits(context.Background())
	require.Error(t, err)
}

func TestEventFromRPCRateLimitsUpdated(t *testing.T) {
	event := eventFromRPC(rpcEvent{Method: notifyRateLimitsUpdated, Params: mustRaw(map[string]any{"rateLimits": map[string]any{
		"planType": "team", "primary": map[string]any{"usedPercent": float64(42)},
	}})})
	require.Equal(t, EventRateLimitsUpdated, event.Kind)
	require.NotNil(t, event.RateLimits)
	require.Equal(t, "team", event.RateLimits.Pools[0].PlanType)
	require.Equal(t, float64(42), event.RateLimits.Pools[0].Windows[0].UsedPercent)
}

func TestIsRateLimitsAuthError(t *testing.T) {
	require.True(t, IsRateLimitsAuthError(&rpcError{Code: 401}))
	require.True(t, IsRateLimitsAuthError(&rpcError{Code: -32600, Message: "Not authenticated"}))
	require.False(t, IsRateLimitsAuthError(&rpcError{Code: -32603, Message: "backend unavailable"}))
}

func TestFloat64Value(t *testing.T) {
	require.Equal(t, float64(0), float64Value(nil, "x"))
	require.Equal(t, float64(3.5), float64Value(map[string]any{"x": float64(3.5)}, "x"))
	require.Equal(t, float64(4), float64Value(map[string]any{"x": int64(4)}, "x"))
	require.Equal(t, float64(5), float64Value(map[string]any{"x": int(5)}, "x"))
	require.Equal(t, float64(0), float64Value(map[string]any{"x": "nope"}, "x"))
}
