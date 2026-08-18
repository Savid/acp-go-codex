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

// docs/03 fixes the negotiated answer as a fact about the **active
// configuration** — sibling x platform x isolation/containment mode — resolved
// from the same code path that enforces containment and never from a
// compiled-in constant. The answer is therefore looked up in the per-mode table
// by the agent's own containment mode. Every row is identical because no codex
// containment mode moves any of these three facts, and the lookup is what keeps
// a mode that ever proves more from being answered for by a mode that does not.
func TestLifecycleFactsResolveThroughTheContainmentModeAccessor(t *testing.T) {
	modes := []RuntimeContainmentMode{
		RuntimeContainmentAuthoritative,
		RuntimeContainmentBestEffort,
		RuntimeContainmentSharedIdentity,
		RuntimeContainmentUnavailable,
	}

	require.Len(t, lifecycleFactsByContainment, len(modes),
		"every containment mode this adapter can select owns exactly one row")

	for _, mode := range modes {
		require.Contains(t, lifecycleFactsByContainment, mode)

		facts := provenLifecycleFacts(mode)
		require.False(t, facts.UpdatesOutsidePrompt, mode)
		require.False(t, facts.AuthoritativeQuiescence, mode)
		require.Empty(t, facts.QuiescenceSource, mode)
		require.Equal(t, []lifecycle.ActivityKind{}, facts.ActivityKinds, mode)
	}

	// A mode with no row proves nothing rather than inheriting a neighbour's.
	require.Equal(t, provenFacts(), provenLifecycleFacts(RuntimeContainmentMode("future-mode")))

	// The answer this connection settles on is the row for the mode the agent
	// actually enforces, read through the same accessor the runtime reports.
	agent := NewAgent()
	require.Equal(t, RuntimeContainmentSharedIdentity, agent.ContainmentMode())

	answer, err := agent.negotiateLifecycle(map[string]any{lifecycle.MetaKey: map[string]any{"versions": []any{1.0}}})
	require.NoError(t, err)
	require.True(t, answer.Present())

	expected := provenLifecycleFacts(agent.ContainmentMode())
	require.Equal(t, expected.UpdatesOutsidePrompt, answer.UpdatesOutsidePrompt)
	require.Equal(t, expected.AuthoritativeQuiescence, answer.AuthoritativeQuiescence)
	require.Equal(t, expected.QuiescenceSource, answer.QuiescenceSource)
	require.Equal(t, expected.ActivityKinds, answer.ActivityKinds)

	// A quiescence source is present exactly when a proof class was proven, so
	// the negative answer omits it from the wire advertisement entirely.
	require.NotContains(t, answer.Advertisement(), "quiescenceSource")
}
