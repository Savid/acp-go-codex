package codexacp

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/coder/acp-go-sdk"
	"github.com/savid/acp-go-codex/internal/lifecycle"
	"github.com/stretchr/testify/require"
)

func TestLifecycleNegotiationAndReservedExtensionDispatch(t *testing.T) {
	agent := NewAgent()
	answer, err := agent.negotiateLifecycle(map[string]any{lifecycle.MetaKey: map[string]any{"versions": []any{1.0}}})
	require.NoError(t, err)
	require.True(t, answer.Present())
	require.NotNil(t, lifecycleResponseMeta(answer))
	require.Nil(t, lifecycleResponseMeta(lifecycle.Negotiated{}))

	answer, err = agent.negotiateLifecycle(nil)
	require.NoError(t, err)
	require.False(t, answer.Present())
	answer, err = agent.negotiateLifecycle(map[string]any{lifecycle.MetaKey: map[string]any{"versions": []any{2.0}}})
	require.NoError(t, err)
	require.False(t, answer.Present())
	_, err = agent.negotiateLifecycle(map[string]any{lifecycle.MetaKey: "bad"})
	require.Error(t, err)
	require.NoError(t, rejectLifecycleKey(nil))
	require.Error(t, rejectLifecycleKey(map[string]any{lifecycle.MetaKey: nil}))
	require.NoError(t, rejectLifecycleKeyInParams(json.RawMessage(`{`)))
	require.NoError(t, rejectLifecycleKeyInParams(json.RawMessage(`{"_meta":{}}`)))
	require.Error(t, rejectLifecycleKeyInParams(json.RawMessage(`{"_meta":{"acp-go.dev/lifecycle":{}}}`)))

	_, err = agent.HandleExtensionMethod(context.Background(), "unknown", json.RawMessage(`{"_meta":{"acp-go.dev/lifecycle":{}}}`))
	require.Error(t, err)
	_, err = agent.HandleExtensionMethod(context.Background(), "unknown", json.RawMessage(`{}`))
	require.Error(t, err)
}

func TestLifecycleReservedKeyRejectedAcrossAgentSurfaces(t *testing.T) {
	ctx := context.Background()
	agent := NewAgent()
	meta := map[string]any{lifecycle.MetaKey: map[string]any{}}

	_, err := agent.Initialize(ctx, acp.InitializeRequest{Meta: meta})
	require.Error(t, err)
	_, err = agent.NewSession(ctx, acp.NewSessionRequest{Meta: meta})
	require.Error(t, err)
	_, err = agent.CloseSession(ctx, acp.CloseSessionRequest{Meta: meta})
	require.Error(t, err)
	_, err = agent.UnstableDeleteSession(ctx, acp.UnstableDeleteSessionRequest{Meta: meta})
	require.Error(t, err)
	_, err = agent.ListSessions(ctx, acp.ListSessionsRequest{Meta: meta})
	require.Error(t, err)
	_, err = agent.ResumeSession(ctx, acp.ResumeSessionRequest{Meta: meta})
	require.Error(t, err)
	_, err = agent.LoadSession(ctx, acp.LoadSessionRequest{Meta: meta})
	require.Error(t, err)
	_, err = agent.SetSessionMode(ctx, acp.SetSessionModeRequest{Meta: meta})
	require.Error(t, err)
	_, err = agent.Authenticate(ctx, acp.AuthenticateRequest{Meta: meta})
	require.Error(t, err)
	_, err = agent.Logout(ctx, acp.LogoutRequest{Meta: meta})
	require.Error(t, err)
	_, err = agent.SetSessionConfigOption(ctx, acp.SetSessionConfigOptionRequest{
		Boolean: &acp.SetSessionConfigOptionBoolean{Meta: meta},
	})
	require.Error(t, err)
	_, err = agent.SetSessionConfigOption(ctx, acp.SetSessionConfigOptionRequest{
		ValueId: &acp.SetSessionConfigOptionValueId{Meta: meta},
	})
	require.Error(t, err)
}

// The negotiated answer is a fact about the active configuration — this adapter
// on this platform under this containment mode — resolved on the agent that
// enforces containment rather than from a compiled-in constant. Every mode this
// adapter can select answers the same three negatives, so the answer is stated
// once instead of tabulated; this pins that the statement really is mode
// independent, so a mode that ever proves more cannot be answered for silently
// by one that does not.
func TestLifecycleFactsAreNegativeUnderEveryContainmentMode(t *testing.T) {
	modes := []RuntimeContainmentMode{
		RuntimeContainmentAuthoritative,
		RuntimeContainmentBestEffort,
		RuntimeContainmentSharedIdentity,
		RuntimeContainmentUnavailable,
		RuntimeContainmentMode("future-mode"),
	}

	for _, mode := range modes {
		agent := NewAgent()
		agent.containmentMode = mode
		require.Equal(t, mode, agent.ContainmentMode())

		facts := agent.provenLifecycleFacts()
		require.False(t, facts.UpdatesOutsidePrompt, mode)
		require.False(t, facts.AuthoritativeQuiescence, mode)
		require.Empty(t, facts.QuiescenceSource, mode)
		require.Equal(t, []lifecycle.ActivityKind{}, facts.ActivityKinds, mode)
	}

	// The answer this connection settles on is the one the agent proves, and the
	// mode it enforces is the one the runtime reports.
	agent := NewAgent()
	require.Equal(t, RuntimeContainmentSharedIdentity, agent.ContainmentMode())

	answer, err := agent.negotiateLifecycle(map[string]any{lifecycle.MetaKey: map[string]any{"versions": []any{1.0}}})
	require.NoError(t, err)
	require.True(t, answer.Present())

	expected := agent.provenLifecycleFacts()
	require.Equal(t, expected.UpdatesOutsidePrompt, answer.UpdatesOutsidePrompt)
	require.Equal(t, expected.AuthoritativeQuiescence, answer.AuthoritativeQuiescence)
	require.Equal(t, expected.QuiescenceSource, answer.QuiescenceSource)
	require.Equal(t, expected.ActivityKinds, answer.ActivityKinds)

	// A quiescence source is present exactly when a proof class was proven, so
	// the negative answer omits it from the wire advertisement entirely.
	require.NotContains(t, answer.Advertisement(), "quiescenceSource")
}
