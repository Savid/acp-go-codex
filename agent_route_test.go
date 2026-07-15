package codexacp

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/coder/acp-go-sdk"
	"github.com/savid/acp-go-codex/internal/codex"
	"github.com/stretchr/testify/require"
)

func TestInitializeAdvertisesExactRouteEnvelopeCapability(t *testing.T) {
	resp, err := NewAgent().Initialize(context.Background(), acp.InitializeRequest{})
	require.NoError(t, err)
	require.Equal(t, map[string]any{routeVersionsKey: []int{routeVersion}}, resp.AgentCapabilities.Meta[routeMetaKey])
	require.NotContains(t, resp.Meta, routeMetaKey)
}

func TestInboundRouteRequiresExactShapeAndCancelMatchesActiveTurn(t *testing.T) {
	for name, meta := range map[string]map[string]any{
		"missing":            nil,
		"wrong version":      {routeMetaKey: map[string]any{routeVersionKey: 2, routeTurnNonceKey: "n"}},
		"wrong version type": {routeMetaKey: map[string]any{routeVersionKey: "1", routeTurnNonceKey: "n"}},
		"empty nonce":        {routeMetaKey: map[string]any{routeVersionKey: 1, routeTurnNonceKey: ""}},
		"extra field":        {routeMetaKey: map[string]any{routeVersionKey: 1, routeTurnNonceKey: "n", "extra": true}},
	} {
		t.Run(name, func(t *testing.T) {
			_, err := parseInboundRoute(meta)
			require.Error(t, err)
		})
	}

	client := newSpyCodexClient()
	agent := NewAgent(withClientFactory(func(context.Context, codex.Options) (codex.Client, error) { return client, nil }))
	resp, err := agent.NewSession(context.Background(), NewSessionRequest("/work"))
	require.NoError(t, err)
	session := agent.sessionMust(resp.SessionId)
	_ = session.beginTurn(context.Background(), "active")

	require.Error(t, agent.Cancel(context.Background(), CancelRequest(resp.SessionId, "stale")))
	require.False(t, session.wasTurnCancelled())
	require.NoError(t, agent.Cancel(context.Background(), CancelRequest(resp.SessionId, "active")))
	require.True(t, session.wasTurnCancelled())
}

func TestElicitationRouteStampingPreservesMetadataAndRejectsCollision(t *testing.T) {
	requestIDValue := acp.RequestIdStr("native-request")
	requestID := acp.RequestId{Str: &requestIDValue}
	scope := elicitationScope{SessionID: "session", TurnNonce: "turn", RequestID: &requestID}
	raw, err := scopedElicitationParams(acp.UnstableCreateElicitationRequest{
		Form: &acp.UnstableCreateElicitationForm{
			Mode:    "form",
			Message: "Need input",
			Meta:    map[string]any{"native": map[string]any{"kept": true}},
		},
	}, scope)
	require.NoError(t, err)

	var payload map[string]any
	require.NoError(t, json.Unmarshal(raw, &payload))
	meta, ok := payload[jsonFieldMeta].(map[string]any)
	require.True(t, ok)
	require.Equal(t, map[string]any{"kept": true}, meta["native"])
	route, ok := meta[routeMetaKey].(map[string]any)
	require.True(t, ok)
	require.Equal(t, float64(1), route[routeVersionKey])
	require.Equal(t, "session", route[jsonFieldSessionID])
	require.Equal(t, "turn", route[routeTurnNonceKey])
	require.Contains(t, route, routeRequestIDKey)
	require.NotContains(t, route, routeToolCallIDKey)

	_, err = scopedElicitationParams(acp.UnstableCreateElicitationRequest{
		Url: &acp.UnstableCreateElicitationUrl{
			Mode: "url",
			Meta: map[string]any{routeMetaKey: map[string]any{}},
		},
	}, elicitationScope{SessionID: "session", TurnNonce: "turn", ToolCallID: "tool"})
	require.ErrorContains(t, err, "collision")

	_, err = scopedElicitationParams(acp.UnstableCreateElicitationRequest{
		Url: &acp.UnstableCreateElicitationUrl{Mode: "url"},
	}, elicitationScope{SessionID: "session", TurnNonce: "turn", ToolCallID: "tool", RequestID: &requestID})
	require.ErrorContains(t, err, "exactly one")
}

func TestPromptAndCancelBuildersStampExactInboundRoute(t *testing.T) {
	prompt := TextPromptRequest("session", "nonce", "hello")
	require.Equal(t, inboundRouteMeta("nonce"), prompt.Meta)
	cancel := CancelRequest("session", "nonce")
	require.Equal(t, inboundRouteMeta("nonce"), cancel.Meta)
}

func TestTurnScopedNotificationsCarryExactRoute(t *testing.T) {
	agent := NewAgent()
	client := newRecordingAgentClient()
	agent.setAgentClient(client)
	ctx := withTurnRoute(context.Background(), "turn-old")

	require.NoError(t, agent.emitUpdate(ctx, "session", acp.UpdateAgentMessageText("late")))
	require.Equal(t, inboundRouteMeta("turn-old"), client.updates[0].Meta)

	session := &session{agent: agent, id: "session", rawMessages: rawMessageConfig{enabled: true}}
	require.NoError(t, session.emitRawCodexEvent(ctx, rawEventFor("item/updated", true)))
	payload, ok := client.extensions[0].params.(map[string]any)
	require.True(t, ok)
	require.Equal(t, inboundRouteMeta("turn-old"), payload[jsonFieldMeta])
	var nilContext context.Context
	require.Nil(t, turnRouteMetaFromContext(nilContext))
	require.Nil(t, turnRouteMetaFromContext(context.Background()))
}
