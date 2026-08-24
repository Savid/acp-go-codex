package codex

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestRateLimitSnapshotFromMap(t *testing.T) {
	resetRFC3339 := time.Unix(1_000_000, 0).UTC().Format(time.RFC3339)

	tests := []struct {
		name     string
		snapshot map[string]any
		want     RateLimitSnapshot
	}{
		{
			name: "full snapshot with both windows",
			snapshot: map[string]any{
				"planType":  "pro",
				"primary":   map[string]any{"usedPercent": float64(12), "windowDurationMins": float64(300), "resetsAt": float64(1_000_000)},
				"secondary": map[string]any{"usedPercent": float64(80)},
			},
			want: RateLimitSnapshot{
				PlanType: "pro",
				Windows: []RateLimitWindow{
					{ID: "primary", UsedPercent: 12, ResetsAt: resetRFC3339},
					{ID: "secondary", UsedPercent: 80},
				},
			},
		},
		{
			name:     "only secondary present",
			snapshot: map[string]any{"secondary": map[string]any{"usedPercent": float64(5)}},
			want: RateLimitSnapshot{
				Windows: []RateLimitWindow{{ID: "secondary", UsedPercent: 5}},
			},
		},
		{
			name:     "plan type only, no windows",
			snapshot: map[string]any{"planType": "plus"},
			want:     RateLimitSnapshot{PlanType: "plus", Windows: []RateLimitWindow{}},
		},
		{
			name:     "empty snapshot",
			snapshot: map[string]any{},
			want:     RateLimitSnapshot{Windows: []RateLimitWindow{}},
		},
		{
			name:     "nil snapshot",
			snapshot: nil,
			want:     RateLimitSnapshot{Windows: []RateLimitWindow{}},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := rateLimitSnapshotFromMap(tc.snapshot)
			require.Equal(t, tc.want, got)
		})
	}
}

func TestFloat64Value(t *testing.T) {
	require.Equal(t, float64(0), float64Value(nil, "x"))
	require.Equal(t, float64(3.5), float64Value(map[string]any{"x": float64(3.5)}, "x"))
	require.Equal(t, float64(4), float64Value(map[string]any{"x": int64(4)}, "x"))
	require.Equal(t, float64(5), float64Value(map[string]any{"x": int(5)}, "x"))
	require.Equal(t, float64(0), float64Value(map[string]any{"x": "nope"}, "x"))
}

func TestReadRateLimits(t *testing.T) {
	transport := newScriptTransport()
	client := &AppServerClient{rpc: newRPCConn(transport, nil)}
	defer client.Close(context.Background())

	snapshot, err := client.ReadRateLimits(context.Background())
	require.NoError(t, err)
	require.Equal(t, "pro", snapshot.PlanType)
	require.Len(t, snapshot.Windows, 2)
	require.Equal(t, "primary", snapshot.Windows[0].ID)
	require.Equal(t, float64(12), snapshot.Windows[0].UsedPercent)
	require.NotEmpty(t, snapshot.Windows[0].ResetsAt)
	require.Equal(t, "secondary", snapshot.Windows[1].ID)
	require.Empty(t, snapshot.Windows[1].ResetsAt)
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
	event := eventFromRPC(rpcEvent{
		Method: notifyRateLimitsUpdated,
		Params: mustRaw(map[string]any{"rateLimits": map[string]any{
			"planType": "team",
			"primary":  map[string]any{"usedPercent": float64(42)},
		}}),
	})

	require.Equal(t, EventRateLimitsUpdated, event.Kind)
	require.NotNil(t, event.RateLimits)
	require.Equal(t, "team", event.RateLimits.PlanType)
	require.Len(t, event.RateLimits.Windows, 1)
	require.Equal(t, float64(42), event.RateLimits.Windows[0].UsedPercent)
}

func TestDispatchRateLimitsUpdatedToHandler(t *testing.T) {
	var got *RateLimitSnapshot

	client := &AppServerClient{options: Options{EventHandler: func(_ context.Context, event Event) {
		got = event.RateLimits
	}}}

	client.dispatchEvent(Event{Kind: EventRateLimitsUpdated, RateLimits: &RateLimitSnapshot{PlanType: "pro"}})
	require.NotNil(t, got)
	require.Equal(t, "pro", got.PlanType)
}

func TestDispatchRateLimitsUpdatedWithoutHandler(t *testing.T) {
	client := &AppServerClient{}
	require.NotPanics(t, func() {
		client.dispatchEvent(Event{Kind: EventRateLimitsUpdated, RateLimits: &RateLimitSnapshot{PlanType: "pro"}})
	})
}

func TestPlaceholderRateLimits(t *testing.T) {
	client := NewPlaceholderClient(Options{})

	snapshot, err := client.ReadRateLimits(context.Background())
	require.NoError(t, err)
	require.Empty(t, snapshot.PlanType)
	require.Empty(t, snapshot.Windows)
}
