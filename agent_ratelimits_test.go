package codexacp

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	"github.com/coder/acp-go-sdk"
	"github.com/stretchr/testify/require"

	"github.com/savid/acp-go-codex/internal/codex"
)

func storeRateLimitsSession(t *testing.T, agent *Agent, id string, client codex.Client) {
	t.Helper()

	session := newSession(agent, acp.SessionId(id), "/tmp/project", nil, codex.Thread{ID: id}, client, sessionMeta{}, nil)
	require.NoError(t, agent.storeStartedSession(session))
}

func decodeRateLimits(t *testing.T, result any) RateLimitsResponse {
	t.Helper()

	raw, err := json.Marshal(result)
	require.NoError(t, err)

	var resp RateLimitsResponse
	require.NoError(t, json.Unmarshal(raw, &resp))

	return resp
}

func TestRateLimitsFreshQuery(t *testing.T) {
	agent := NewAgent()
	client := newSpyCodexClient()
	client.rateLimitsSupported = true
	client.rateLimits = codex.RateLimitSnapshot{
		PlanType: "pro",
		Windows: []codex.RateLimitWindow{
			{ID: "primary", UsedPercent: 25, ResetsAt: "1970-01-12T13:46:40Z"},
			{ID: "secondary", UsedPercent: 60},
		},
	}
	storeRateLimitsSession(t, agent, "thread-1", client)

	result, err := agent.HandleExtensionMethod(context.Background(), RateLimitsMethod, json.RawMessage(`{}`))
	require.NoError(t, err)

	resp := decodeRateLimits(t, result)
	require.Equal(t, "pro", resp.PlanType)
	require.Len(t, resp.Windows, 2)
	require.Equal(t, "primary", resp.Windows[0].ID)
	require.Equal(t, float64(25), resp.Windows[0].UsedPercent)
	require.Equal(t, "1970-01-12T13:46:40Z", resp.Windows[0].ResetsAt)
	require.Equal(t, 1, client.rateLimitsReads)

	// The fresh snapshot is cached: a later call with no supporting client still
	// returns it.
	cached, ok := agent.cachedRateLimits()
	require.True(t, ok)
	require.Equal(t, "pro", cached.PlanType)
}

func TestRateLimitsFallsBackToCacheOnError(t *testing.T) {
	agent := NewAgent()
	agent.cacheRateLimits(codex.RateLimitSnapshot{PlanType: "plus", Windows: []codex.RateLimitWindow{{ID: "primary", UsedPercent: 5}}})

	client := newSpyCodexClient()
	client.rateLimitsSupported = true
	client.rateLimitsErr = errors.New("boom")
	storeRateLimitsSession(t, agent, "thread-1", client)

	resp := decodeRateLimits(t, agent.rateLimits(context.Background()))
	require.Equal(t, "plus", resp.PlanType)
	require.Len(t, resp.Windows, 1)
	require.Equal(t, 1, client.rateLimitsReads)
}

func TestRateLimitsFallsBackWhenFreshQueryEmpty(t *testing.T) {
	agent := NewAgent()
	agent.cacheRateLimits(codex.RateLimitSnapshot{PlanType: "team"})

	client := newSpyCodexClient()
	client.rateLimitsSupported = true // returns empty snapshot
	storeRateLimitsSession(t, agent, "thread-1", client)

	resp := decodeRateLimits(t, agent.rateLimits(context.Background()))
	require.Equal(t, "team", resp.PlanType)
}

func TestRateLimitsSkipsUnsupportedVersion(t *testing.T) {
	agent := NewAgent()

	client := newSpyCodexClient() // rateLimitsSupported defaults false
	storeRateLimitsSession(t, agent, "thread-1", client)

	resp := decodeRateLimits(t, agent.rateLimits(context.Background()))
	require.Empty(t, resp.Windows)
	require.NotNil(t, resp.Windows)
	require.Equal(t, 0, client.rateLimitsReads)
}

func TestRateLimitsEmptyWhenNothingKnown(t *testing.T) {
	agent := NewAgent()

	resp := decodeRateLimits(t, agent.rateLimits(context.Background()))
	require.NotNil(t, resp.Windows)
	require.Empty(t, resp.Windows)
	require.Empty(t, resp.PlanType)
}

func TestRateLimitsCacheLatestWins(t *testing.T) {
	agent := NewAgent()

	agent.cacheRateLimits(codex.RateLimitSnapshot{PlanType: "plus"})
	agent.cacheRateLimits(codex.RateLimitSnapshot{PlanType: "pro"})
	agent.cacheRateLimits(codex.RateLimitSnapshot{}) // no data: ignored

	cached, ok := agent.cachedRateLimits()
	require.True(t, ok)
	require.Equal(t, "pro", cached.PlanType)
}

func TestRateLimitsCacheEmptyBeforeAnyData(t *testing.T) {
	agent := NewAgent()
	_, ok := agent.cachedRateLimits()
	require.False(t, ok)
}

func TestApplyCodexClientEventCachesRateLimits(t *testing.T) {
	agent := NewAgent()
	client := newSpyCodexClient()

	agent.applyCodexClientEvent(context.Background(), client, codex.Event{
		Kind:       codex.EventRateLimitsUpdated,
		RateLimits: &codex.RateLimitSnapshot{PlanType: "pro"},
	})

	cached, ok := agent.cachedRateLimits()
	require.True(t, ok)
	require.Equal(t, "pro", cached.PlanType)

	// A rate-limits event without a payload is a no-op.
	agent.applyCodexClientEvent(context.Background(), client, codex.Event{Kind: codex.EventRateLimitsUpdated})
	cached, ok = agent.cachedRateLimits()
	require.True(t, ok)
	require.Equal(t, "pro", cached.PlanType)
}

func TestRateLimitsLiveClientSkipsClosedAgent(t *testing.T) {
	agent := NewAgent()
	require.NoError(t, agent.Close())

	_, ok := agent.liveRateLimitsClient()
	require.False(t, ok)
}

func TestDecodeRateLimitsParams(t *testing.T) {
	require.NoError(t, decodeRateLimitsParams(nil))
	require.NoError(t, decodeRateLimitsParams(json.RawMessage(``)))
	require.NoError(t, decodeRateLimitsParams(json.RawMessage(`null`)))
	require.NoError(t, decodeRateLimitsParams(json.RawMessage(`{}`)))
	require.Error(t, decodeRateLimitsParams(json.RawMessage(`{"unexpected":1}`)))

	require.Error(t, decodeRateLimitsParams(json.RawMessage(`[1,2]`)))
	require.Error(t, decodeRateLimitsParams(json.RawMessage(`"string"`)))
	require.Error(t, decodeRateLimitsParams(json.RawMessage(`not-json`)))
}

func TestHandleExtensionMethodRateLimitsMalformed(t *testing.T) {
	agent := NewAgent()
	_, err := agent.HandleExtensionMethod(context.Background(), RateLimitsMethod, json.RawMessage(`[1,2]`))
	require.Error(t, err)
}

func TestHandleExtensionMethodRateLimitsClosedAgent(t *testing.T) {
	agent := NewAgent()
	require.NoError(t, agent.Close())

	_, err := agent.HandleExtensionMethod(context.Background(), RateLimitsMethod, json.RawMessage(`{}`))
	require.Error(t, err)
}

func TestRateLimitsRequestValidate(t *testing.T) {
	require.NoError(t, RateLimitsRequest{}.Validate())
}
