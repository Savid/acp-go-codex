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

func TestRateLimitsContextFromConfig(t *testing.T) {
	for _, tc := range []struct {
		name     string
		config   map[string]any
		provider string
		custom   bool
	}{
		{name: "default", config: map[string]any{}, provider: "openai"},
		{name: "selected", config: map[string]any{"model_provider": "openai"}, provider: "openai"},
		{name: "resolved ChatGPT default", config: map[string]any{"chatgpt_base_url": "https://chatgpt.com/backend-api/"}, provider: "openai"},
		{name: "ChatGPT default without trailing slash", config: map[string]any{"chatgpt_base_url": "https://chatgpt.com/backend-api"}, provider: "openai"},
		{name: "other provider", config: map[string]any{"model_provider": "other"}, provider: "other", custom: true},
		{name: "base URL", config: map[string]any{"openai_base_url": "https://example.test"}, provider: "openai", custom: true},
		{name: "ChatGPT custom host", config: map[string]any{"chatgpt_base_url": "https://example.test/backend-api/"}, provider: "openai", custom: true},
		{name: "ChatGPT insecure URL", config: map[string]any{"chatgpt_base_url": "http://chatgpt.com/backend-api/"}, provider: "openai", custom: true},
		{name: "ChatGPT invalid URL", config: map[string]any{"chatgpt_base_url": "/"}, provider: "openai", custom: true},
		{name: "ChatGPT custom path", config: map[string]any{"chatgpt_base_url": "https://chatgpt.com/backend-api/custom"}, provider: "openai", custom: true},
		{name: "ChatGPT custom query", config: map[string]any{"chatgpt_base_url": "https://chatgpt.com/backend-api/?account=other"}, provider: "openai", custom: true},
		{name: "ChatGPT user info", config: map[string]any{"chatgpt_base_url": "https://other@chatgpt.com/backend-api/"}, provider: "openai", custom: true},
		{name: "nested base URL", config: map[string]any{"model_providers": map[string]any{"openai": map[string]any{"base_url": "https://example.test"}}}, provider: "openai", custom: true},
		{name: "provider auth override", config: map[string]any{"model_providers": map[string]any{"openai": map[string]any{"env_key": "CUSTOM_OPENAI_KEY"}}}, provider: "openai", custom: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			require.Equal(t, RateLimitsContext{ProviderID: tc.provider, Custom: tc.custom}, rateLimitsContextFromConfig(tc.config))
		})
	}
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

func TestReadRateLimitsContext(t *testing.T) {
	for _, tc := range []struct {
		name        string
		config      any
		environment map[string]string
		implicit    map[string]string
		custom      bool
		fail        bool
	}{
		{name: "native default", config: map[string]any{"model_provider": "openai"}},
		{name: "resolved default with ambient API key", config: map[string]any{"model_provider": nil, "chatgpt_base_url": "https://chatgpt.com/backend-api/", "model_providers": map[string]any{"openai": map[string]any{}}}, implicit: map[string]string{"OPENAI_API_KEY": "test-key"}},
		{name: "OpenAI API key override", config: map[string]any{"model_provider": "openai"}, environment: map[string]string{"OPENAI_API_KEY": "test-key"}},
		{name: "Codex API key override", config: map[string]any{"model_provider": "openai"}, environment: map[string]string{"CODEX_API_KEY": "test-key"}},
		{name: "native custom", config: map[string]any{"model_provider": "openai"}, environment: map[string]string{"OPENAI_BASE_URL": "https://example.test"}, custom: true},
		{name: "ambient custom endpoint", config: map[string]any{"model_provider": "openai"}, implicit: map[string]string{"OPENAI_BASE_URL": "https://example.test"}, custom: true},
		{name: "missing config", fail: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			transport := &responseTransport{responses: map[string]any{"config/read": map[string]any{"config": tc.config}}}
			implicit := tc.implicit
			if implicit == nil {
				implicit = map[string]string{}
			}
			client := &AppServerClient{rpc: newRPCConn(transport, nil), options: Options{ImplicitEnvironment: implicit, Env: tc.environment}}
			defer client.Close(context.Background())
			configuration, err := client.ReadRateLimitsContext(context.Background(), "/workspace")
			if tc.fail {
				require.Error(t, err)

				return
			}
			require.NoError(t, err)
			require.Equal(t, RateLimitsContext{ProviderID: "openai", Custom: tc.custom}, configuration)
			transport.mu.Lock()
			sent := transport.sent[0]
			transport.mu.Unlock()
			require.JSONEq(t, `{"includeLayers":false,"cwd":"/workspace"}`, string(sent.Params))
		})
	}
}
