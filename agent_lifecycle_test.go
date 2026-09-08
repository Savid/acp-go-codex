package codexacp

import (
	"context"
	"encoding/json"
	"io"
	"testing"

	"github.com/coder/acp-go-sdk"
	"github.com/savid/acp-go-codex/internal/codex"
	"github.com/savid/acp-go-codex/internal/lifecycle"
	"github.com/stretchr/testify/require"
)

func TestServeRejectsLossyLifecycleMetadata(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	input, write := io.Pipe()
	read, output := io.Pipe()
	done := make(chan error, 1)
	go func() {
		done <- Serve(ctx, input, output,
			WithHome(t.TempDir()), WithScratchDir(t.TempDir()),
			withClientFactory(func(context.Context, codex.Options) (codex.Client, error) { return newRuntimeRecordingClient(), nil }),
		)
	}()
	t.Cleanup(func() {
		cancel()
		_ = write.Close()
		_ = read.Close()
		_ = input.Close()
		_ = output.Close()
		require.ErrorIs(t, <-done, context.Canceled)
	})
	peer := acp.NewConnection(nil, write, read)
	for _, raw := range []string{`{"version":2,"version":1}`, `{"version":1.0000000000000001}`, `{"version":1.0}`, `{"version":1e400}`} {
		_, err := acp.SendRequest[acp.InitializeResponse](peer, ctx, acp.AgentMethodInitialize,
			json.RawMessage(`{"protocolVersion":1,"_meta":{"acp-go.dev/lifecycle":`+raw+`}}`))
		requireLifecycleWireField(t, err, lifecycle.MetaPath+".version")
	}
	for _, raw := range []string{
		`{"protocolVersion":1,"_meta":{"acp-go.dev/lifecycle":{"version":2},"acp-go.dev/lifecycle":{"version":1}}}`,
		`{"protocolVersion":1,"_meta":{"acp-go.dev/lifecycle":{"version":1}},"_meta":{}}`,
		`{"protocolVersion":1,"_meta":{"acp-go.dev/lifecycle":{"version":1}},"_meta":null}`,
		`{"protocolVersion":1,"_meta":{"acp-go.dev/lifecycle":{"version":1}},"_META":null}`,
	} {
		_, err := acp.SendRequest[acp.InitializeResponse](peer, ctx, acp.AgentMethodInitialize, json.RawMessage(raw))
		requireLifecycleWireField(t, err, lifecycle.MetaPath)
	}
	_, err := acp.SendRequest[acp.InitializeResponse](peer, ctx, acp.AgentMethodInitialize,
		json.RawMessage(`{"protocolVersion":1,"_meta":{"foreign":1},"_meta":{"foreign":2}}`))
	require.NoError(t, err)
	_, err = acp.SendRequest[acp.InitializeResponse](peer, ctx, acp.AgentMethodInitialize,
		json.RawMessage(`{"protocolVersion":1,"_meta":{"foreign":{"version":2,"version":1},"acp-go.dev/lifecycle":{"version":1}}}`))
	require.NoError(t, err)
	_, err = acp.SendRequest[acp.ListSessionsResponse](peer, ctx, acp.AgentMethodSessionList,
		json.RawMessage(`{"_meta":{"acp-go.dev/lifecycle":{}},"_meta":null}`))
	requireLifecycleWireField(t, err, lifecycle.MetaPath)
	created, err := acp.SendRequest[acp.NewSessionResponse](peer, ctx, acp.AgentMethodSessionNew, NewSessionRequest(t.TempDir()))
	require.NoError(t, err)
	for _, tc := range []struct{ method, params string }{
		{acp.AgentMethodAuthenticate, `{"methodId":"codex-api-key"`},
		{acp.AgentMethodLogout, `{`},
		{acp.AgentMethodSessionDelete, `{"sessionId":"missing"`},
		{acp.AgentMethodSessionSetConfigOption, `{"sessionId":"` + string(created.SessionId) + `","configId":"model","type":"boolean","value":true`},
		{acp.AgentMethodSessionSetConfigOption, `{"sessionId":"` + string(created.SessionId) + `","configId":"model","value":"example-model"`},
	} {
		separator := ","
		if tc.params == "{" {
			separator = ""
		}
		raw := tc.params + separator + `"_meta":{"acp-go.dev/lifecycle":{}},"_meta":null}`
		_, err = acp.SendRequest[json.RawMessage](peer, ctx, tc.method, json.RawMessage(raw))
		requireLifecycleWireField(t, err, lifecycle.MetaPath)
	}
	for _, tc := range []struct{ raw, field string }{
		{`{"version":2,"version":1}`, ".version"},
		{`{"version":1.0000000000000001}`, ".version"},
		{`{"version":1e400}`, ".version"},
		{`{"version":1,"submission":{"submissionId":"first","submissionId":"last","clientNonce":"nonce"}}`, ".submission.submissionId"},
	} {
		request := TextPromptRequest(created.SessionId, "route-nonce", "hello")
		request.Meta[lifecycle.MetaKey] = json.RawMessage(tc.raw)
		_, err = acp.SendRequest[acp.PromptResponse](peer, ctx, acp.AgentMethodSessionPrompt, request)
		requireLifecycleWireField(t, err, lifecycle.MetaPath+tc.field)
	}
	_, err = acp.SendRequest[acp.InitializeResponse](peer, ctx, acp.AgentMethodInitialize,
		json.RawMessage(`{"protocolVersion":1,"_META":{"acp-go.dev/lifecycle":{"version":2,"version":1}}}`))
	requireLifecycleWireField(t, err, lifecycle.MetaPath+".version")
	for _, method := range []string{acp.AgentMethodSessionPrompt, SteerTurnMethod} {
		raw := `{"sessionId":"` + string(created.SessionId) + `","prompt":[],"_meta":{"acp-go.dev/route":{"version":1.0000000000000001,"turnNonce":"nonce"},"acp-go.dev/lifecycle":{"version":1e400}}}`
		_, err = acp.SendRequest[json.RawMessage](peer, ctx, method, json.RawMessage(raw))
		requireLifecycleWireField(t, err, routeMetaPath+".version")
	}
	request := TextPromptRequest(created.SessionId, "route-nonce", "hello")
	request.Meta[lifecycle.MetaKey] = json.RawMessage(`{"version":1,"submission":{"submissionId":"submission","clientNonce":"nonce"}}`)
	_, err = acp.SendRequest[acp.PromptResponse](peer, ctx, acp.AgentMethodSessionPrompt, request)
	require.NoError(t, err)
}

func requireLifecycleWireField(t *testing.T, err error, field string) {
	t.Helper()
	var requestErr *acp.RequestError
	require.ErrorAs(t, err, &requestErr)
	require.Equal(t, -32602, requestErr.Code)
	require.Equal(t, "Invalid params", requestErr.Message)
	require.Equal(t, map[string]any{"error": "unsupported", "field": field}, requestErr.Data)
}

func TestLifecycleNegotiationAndReservedExtensionDispatch(t *testing.T) {
	agent := NewAgent()
	answer, err := agent.negotiateLifecycle(map[string]any{lifecycle.MetaKey: map[string]any{"version": 1}})
	require.NoError(t, err)
	require.True(t, answer.Present())
	require.NotNil(t, lifecycleResponseMeta(answer))
	require.Nil(t, lifecycleResponseMeta(lifecycle.Negotiated{}))

	answer, err = agent.negotiateLifecycle(nil)
	require.NoError(t, err)
	require.False(t, answer.Present())
	_, err = agent.negotiateLifecycle(map[string]any{lifecycle.MetaKey: map[string]any{"version": 2.0}})
	require.Error(t, err)
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
