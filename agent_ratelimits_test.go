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
}

func TestRateLimitsFreshQueryReturnsReadError(t *testing.T) {
	agent := NewAgent()
	client := newSpyCodexClient()
	readErr := errors.New("rate-limit read failed")
	client.rateLimitsErr = readErr
	storeRateLimitsSession(t, agent, "thread-1", client)

	_, err := agent.rateLimits(context.Background())
	require.ErrorIs(t, err, readErr)
	require.Equal(t, 1, client.rateLimitsReads)
}

func TestRateLimitsReturnsFreshEmptySnapshot(t *testing.T) {
	agent := NewAgent()
	client := newSpyCodexClient()
	storeRateLimitsSession(t, agent, "thread-1", client)

	response, err := agent.rateLimits(context.Background())
	require.NoError(t, err)
	resp := decodeRateLimits(t, response)
	require.Empty(t, resp.Windows)
	require.NotNil(t, resp.Windows)
	require.Equal(t, 1, client.rateLimitsReads)
}

func TestRateLimitsEmptyWhenNothingKnown(t *testing.T) {
	agent := NewAgent()

	response, err := agent.rateLimits(context.Background())
	require.NoError(t, err)
	resp := decodeRateLimits(t, response)
	require.NotNil(t, resp.Windows)
	require.Empty(t, resp.Windows)
	require.Empty(t, resp.PlanType)
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
