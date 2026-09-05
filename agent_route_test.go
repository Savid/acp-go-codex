package codexacp

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/coder/acp-go-sdk"
	"github.com/savid/acp-go-codex/internal/codex"
	"github.com/stretchr/testify/require"
)

func TestRouteCapabilityScalar(t *testing.T) {
	response, err := NewAgent().Initialize(t.Context(), acp.InitializeRequest{})
	require.NoError(t, err)
	route, ok := response.AgentCapabilities.Meta["acp-go.dev/route"].(map[string]any)
	require.True(t, ok)
	require.Equal(t, 1, route["version"])
}

func TestInitializeAdvertisesExactRouteEnvelopeCapability(t *testing.T) {
	resp, err := NewAgent().Initialize(context.Background(), acp.InitializeRequest{})
	require.NoError(t, err)
	require.Equal(t, map[string]any{routeVersionKey: routeVersion}, resp.AgentCapabilities.Meta[routeMetaKey])
	require.NotContains(t, resp.Meta, routeMetaKey)
}

func TestInboundRouteRequiresExactShapeAndCancelMatchesActiveTurn(t *testing.T) {
	boundaryNonce := strings.Repeat("n", routeTurnNonceMaxBytes)
	boundary, err := parseInboundRoute(inboundRouteMeta(boundaryNonce))
	require.NoError(t, err)
	require.Equal(t, boundaryNonce, boundary.TurnNonce)

	// A host reads two distinct facts off one field path: `missing` means it
	// left the key out, `unsupported` means the value it sent is refused — named
	// down to the member at fault, or on the bare key when the value as a whole
	// is not an object.
	for name, tc := range map[string]struct {
		meta    map[string]any
		verdict string
		field   string
	}{
		"absent": {
			meta: nil, verdict: errValueMissing, field: routeMetaPath,
		},
		"missing key beside another": {
			meta: map[string]any{"other": 1}, verdict: errValueMissing, field: routeMetaPath,
		},
		"non-object": {
			meta: map[string]any{routeMetaKey: "route"}, verdict: errValueUnsupported, field: routeMetaPath,
		},
		"absent version": {
			meta:    map[string]any{routeMetaKey: map[string]any{routeTurnNonceKey: "n"}},
			verdict: errValueUnsupported, field: routeMetaPath + "." + routeVersionKey,
		},
		"wrong version": {
			meta:    map[string]any{routeMetaKey: map[string]any{routeVersionKey: 2, routeTurnNonceKey: "n"}},
			verdict: errValueUnsupported, field: routeMetaPath + "." + routeVersionKey,
		},
		"wrong version type": {
			meta:    map[string]any{routeMetaKey: map[string]any{routeVersionKey: "1", routeTurnNonceKey: "n"}},
			verdict: errValueUnsupported, field: routeMetaPath + "." + routeVersionKey,
		},
		"fractional version": {
			meta:    map[string]any{routeMetaKey: map[string]any{routeVersionKey: 1.5, routeTurnNonceKey: "n"}},
			verdict: errValueUnsupported, field: routeMetaPath + "." + routeVersionKey,
		},
		"absent nonce": {
			meta:    map[string]any{routeMetaKey: map[string]any{routeVersionKey: 1}},
			verdict: errValueUnsupported, field: routeMetaPath + "." + routeTurnNonceKey,
		},
		"empty nonce": {
			meta:    map[string]any{routeMetaKey: map[string]any{routeVersionKey: 1, routeTurnNonceKey: ""}},
			verdict: errValueUnsupported, field: routeMetaPath + "." + routeTurnNonceKey,
		},
		"oversized nonce": {
			meta: map[string]any{routeMetaKey: map[string]any{
				routeVersionKey: 1, routeTurnNonceKey: strings.Repeat("n", routeTurnNonceMaxBytes+1),
			}},
			verdict: errValueUnsupported, field: routeMetaPath + "." + routeTurnNonceKey,
		},
		"unknown member": {
			meta: map[string]any{routeMetaKey: map[string]any{
				routeVersionKey: 1, routeTurnNonceKey: "n", "extra": true,
			}},
			verdict: errValueUnsupported, field: routeMetaPath + ".extra",
		},
	} {
		t.Run(name, func(t *testing.T) {
			_, routeErr := parseInboundRoute(tc.meta)
			require.Error(t, routeErr)

			var requestErr *acp.RequestError

			require.ErrorAs(t, routeInvalidParams(routeErr), &requestErr)
			require.Equal(t, -32602, requestErr.Code)

			data, ok := requestErr.Data.(map[string]any)
			require.True(t, ok)
			require.Equal(t, map[string]any{jsonFieldError: tc.verdict, jsonFieldField: tc.field}, data)
		})
	}

	require.Nil(t, routeInvalidParams(nil))
	require.Equal(t, errValueMissing+" "+routeMetaPath, routeMissing().Error())
	require.Equal(t,
		errValueUnsupported+" "+routeMetaPath+"."+routeVersionKey,
		routeUnsupported(routeVersionKey).Error(),
	)

	// A nonce that does not name the addressed session's current turn is a
	// present, refused value like any other, so it names the member.
	var staleErr *acp.RequestError

	require.ErrorAs(t, routeInvalidParams(errTurnRouteMismatch), &staleErr)
	require.Equal(t, map[string]any{
		jsonFieldError: errValueUnsupported,
		jsonFieldField: routeMetaPath + "." + routeTurnNonceKey,
	}, staleErr.Data)

	client := newSpyCodexClient()
	agent := NewAgent(withClientFactory(func(context.Context, codex.Options) (codex.Client, error) { return client, nil }))
	resp, err := agent.NewSession(context.Background(), NewSessionRequest(absTestPath("work")))
	require.NoError(t, err)
	session := agent.sessionMust(resp.SessionId)
	_ = session.beginTurn(context.Background(), "active")
	session.setTurnID("native-turn-active")

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
	require.Equal(t, "native-request", route[routeRequestIDKey])
	require.NotContains(t, route, routeToolCallIDKey)

	var wirePayload map[string]json.RawMessage
	require.NoError(t, json.Unmarshal(raw, &wirePayload))
	var wireMeta map[string]json.RawMessage
	require.NoError(t, json.Unmarshal(wirePayload[jsonFieldMeta], &wireMeta))
	var wireRoute map[string]json.RawMessage
	require.NoError(t, json.Unmarshal(wireMeta[routeMetaKey], &wireRoute))
	require.Equal(t, `"native-request"`, string(wireRoute[routeRequestIDKey]))

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

	_, err = stampElicitationRoute(nil, elicitationScope{
		SessionID: "session", TurnNonce: strings.Repeat("n", routeTurnNonceMaxBytes+1), ToolCallID: "tool",
	})
	require.ErrorContains(t, err, "maximum size")
}

func TestElicitationRouteRejectsNonStringRequestIDUnions(t *testing.T) {
	numberValue := acp.RequestIdNumber(7)
	nullValue := acp.RequestIdNull{}
	emptyString := acp.RequestIdStr(" ")
	validString := acp.RequestIdStr("request")

	for name, requestID := range map[string]acp.RequestId{
		"numeric":     {Number: &numberValue},
		"null":        {Null: &nullValue},
		"empty union": {},
		"empty string": {
			Str: &emptyString,
		},
		"dual": {Number: &numberValue, Str: &validString},
	} {
		t.Run(name, func(t *testing.T) {
			_, err := stampElicitationRoute(nil, elicitationScope{
				SessionID: "session", TurnNonce: "turn", RequestID: &requestID,
			})
			require.ErrorContains(t, err, "exactly one non-empty string variant")
		})
	}
}

func TestPromptAndCancelBuildersStampExactInboundRoute(t *testing.T) {
	prompt := TextPromptRequest("session", "nonce", "hello")
	require.Equal(t, inboundRouteMeta("nonce"), prompt.Meta)
	cancel := CancelRequest("session", "nonce")
	require.Equal(t, inboundRouteMeta("nonce"), cancel.Meta)

	boundary := strings.Repeat("n", routeTurnNonceMaxBytes)
	require.Equal(t, inboundRouteMeta(boundary), PromptRequest("session", boundary).Meta)
	require.Nil(t, PromptRequest("session", strings.Repeat("n", routeTurnNonceMaxBytes+1)).Meta)
	require.Nil(t, CancelRequest("session", " ").Meta)
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
