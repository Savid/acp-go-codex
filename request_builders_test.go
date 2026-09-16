package codexacp

import (
	"testing"

	"github.com/coder/acp-go-sdk"
	"github.com/stretchr/testify/require"

	"github.com/savid/acp-go-core/wire"
)

func TestSessionCodexOptionsCloneTypedEnvironment(t *testing.T) {
	t.Parallel()

	env := map[string]string{"SESSION_KEY": "original"}
	option := WithSessionCodexOptions(NewCodexOptions(WithCodexEnv(env)))
	first := wire.NewSessionRequest(t.TempDir(), option)
	env["SESSION_KEY"] = "caller changed"
	second := wire.NewSessionRequest(t.TempDir(), option)

	for _, request := range []map[string]any{first.Meta, second.Meta} {
		parsed, err := parseSessionMeta(request)
		require.Nil(t, err)
		require.Equal(t, "original", parsed.options.Env["SESSION_KEY"], "a caller mutation reached an environment the builder had already captured")
	}
}

func TestVendorOptionsRideTheOwnedNamespace(t *testing.T) {
	t.Parallel()

	request := wire.NewSessionRequest(t.TempDir(),
		WithSessionCodexOptions(NewCodexOptions(WithCodexModel("gpt-x"))),
		WithSessionOutputSchema(map[string]any{"type": "object"}),
		WithSessionRawEvents(true),
	)

	require.Equal(t, []acp.McpServer{}, request.McpServers)

	parsed, err := parseSessionMeta(request.Meta)
	require.Nil(t, err)
	require.Equal(t, "gpt-x", parsed.options.Model)
	require.Equal(t, map[string]any{"type": "object"}, parsed.options.OutputSchema)
	require.True(t, parsed.rawEvents)
}

func TestSetModelRequestNamesTheModelSelector(t *testing.T) {
	t.Parallel()

	request := SetModelRequest("s", "gpt-x")
	require.NotNil(t, request.ValueId)
	require.Equal(t, configModel, request.ValueId.ConfigId)
	require.Equal(t, acp.SessionConfigValueId("gpt-x"), request.ValueId.Value)
}
