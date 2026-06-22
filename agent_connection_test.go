package codexacp

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"testing"
	"time"

	"github.com/coder/acp-go-sdk"
	"github.com/savid/acp-go-codex/internal/codex"
)

func TestScopedElicitationParams(t *testing.T) {
	requestID := acp.RequestId{Str: acp.Ptr(acp.RequestIdStr("request-1"))}
	raw, err := scopedElicitationParams(acp.UnstableCreateElicitationRequest{
		Url: &acp.UnstableCreateElicitationUrl{
			ElicitationId: "elicit-1",
			Message:       "Open URL",
			Mode:          "url",
			Url:           "https://example.com",
			Meta:          map[string]any{"source": "test"},
		},
	}, elicitationScope{RequestID: &requestID})
	if err != nil {
		t.Fatalf("scoped URL elicitation params returned error: %v", err)
	}
	var payload map[string]any
	if err := json.Unmarshal(raw, &payload); err != nil {
		t.Fatalf("unmarshal scoped URL payload: %v", err)
	}
	if payload["requestId"] != "request-1" || payload["mode"] != "url" || payload["url"] != "https://example.com" || payload["_meta"] == nil {
		t.Fatalf("scoped URL payload = %#v", payload)
	}

	raw, err = scopedElicitationParams(acp.UnstableCreateElicitationRequest{
		Form: &acp.UnstableCreateElicitationForm{Message: "Answer?", Mode: "form"},
	}, elicitationScope{SessionID: "session-1", ToolCallID: "tool-1"})
	if err != nil {
		t.Fatalf("scoped form elicitation params returned error: %v", err)
	}
	if err := json.Unmarshal(raw, &payload); err != nil {
		t.Fatalf("unmarshal scoped form payload: %v", err)
	}
	if payload["sessionId"] != "session-1" || payload["toolCallId"] != "tool-1" || payload["mode"] != "form" {
		t.Fatalf("scoped form payload = %#v", payload)
	}

	if _, err := scopedElicitationParams(acp.UnstableCreateElicitationRequest{}, elicitationScope{}); err == nil {
		t.Fatal("empty elicitation request was accepted")
	}
	if _, err := scopedElicitationParams(acp.UnstableCreateElicitationRequest{
		Form: &acp.UnstableCreateElicitationForm{Message: "Bad", Mode: "form", Meta: map[string]any{"bad": func() {}}},
	}, elicitationScope{}); err == nil {
		t.Fatal("unmarshalable elicitation request was accepted")
	}
}

func TestLocalAgentConnectionClientMethods(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	c2aR, c2aW := io.Pipe()
	a2cR, a2cW := io.Pipe()
	t.Cleanup(func() {
		_ = c2aR.Close()
		_ = c2aW.Close()
		_ = a2cR.Close()
		_ = a2cW.Close()
	})

	agent := NewAgent()
	conn := newLocalAgentConnection(agent, a2cW, c2aR)
	agent.setAgentClient(conn)

	seen := make(chan string, 16)
	clientConn := acp.NewConnection(func(_ context.Context, method string, params json.RawMessage) (any, *acp.RequestError) {
		seen <- method
		switch method {
		case acp.ClientMethodElicitationCreate:
			return acp.NewUnstableCreateElicitationResponseCancel(), nil
		case acp.ClientMethodMcpConnect:
			return acp.UnstableConnectMcpResponse{ConnectionId: "mcp-conn"}, nil
		case acp.ClientMethodMcpDisconnect:
			return acp.UnstableDisconnectMcpResponse{}, nil
		case acp.ClientMethodMcpMessage:
			if bytes.Contains(params, []byte("notifications/")) {
				return nil, nil
			}
			return map[string]any{"ok": true}, nil
		case acp.ClientMethodSessionRequestPermission:
			return acp.RequestPermissionResponse{Outcome: acp.NewRequestPermissionOutcomeSelected(permissionAccept)}, nil
		case acp.ClientMethodSessionUpdate:
			return nil, nil
		case rawCodexSDKMessageMethod:
			return nil, nil
		default:
			return nil, acp.NewMethodNotFound(method)
		}
	}, c2aW, a2cR)
	_ = clientConn

	if _, err := conn.UnstableCreateElicitation(ctx, acp.UnstableCreateElicitationRequest{Form: &acp.UnstableCreateElicitationForm{Message: "m", Mode: "form"}}); err != nil {
		t.Fatalf("UnstableCreateElicitation returned error: %v", err)
	}
	if _, err := conn.CreateElicitation(ctx, acp.UnstableCreateElicitationRequest{Form: &acp.UnstableCreateElicitationForm{Message: "bad", Mode: "form", Meta: map[string]any{"bad": func() {}}}}, elicitationScope{}); err == nil {
		t.Fatal("CreateElicitation accepted unmarshalable params")
	}
	if _, err := conn.UnstableConnectMcp(ctx, acp.UnstableConnectMcpRequest{AcpId: "server"}); err != nil {
		t.Fatalf("UnstableConnectMcp returned error: %v", err)
	}
	if _, err := conn.UnstableDisconnectMcp(ctx, acp.UnstableDisconnectMcpRequest{ConnectionId: "mcp-conn"}); err != nil {
		t.Fatalf("UnstableDisconnectMcp returned error: %v", err)
	}
	if _, err := conn.UnstableMessageMcp(ctx, acp.UnstableMessageMcpRequest{ConnectionId: "mcp-conn", Method: "tools/list", Params: map[string]any{}}); err != nil {
		t.Fatalf("UnstableMessageMcp returned error: %v", err)
	}
	if err := conn.UnstableNotifyMcp(ctx, acp.UnstableMessageMcpNotification{ConnectionId: "mcp-conn", Method: "notifications/initialized", Params: map[string]any{}}); err != nil {
		t.Fatalf("UnstableNotifyMcp returned error: %v", err)
	}
	if _, err := conn.RequestPermission(ctx, acp.RequestPermissionRequest{SessionId: "s"}); err != nil {
		t.Fatalf("RequestPermission returned error: %v", err)
	}
	if err := conn.SessionUpdate(ctx, acp.SessionNotification{SessionId: "s", Update: acp.UpdateAgentMessageText("hi")}); err != nil {
		t.Fatalf("SessionUpdate returned error: %v", err)
	}
	if err := conn.NotifyExtension(ctx, rawCodexSDKMessageMethod, map[string]any{"ok": true}); err != nil {
		t.Fatalf("NotifyExtension returned error: %v", err)
	}
	if err := conn.NotifyExtension(ctx, "bad", nil); err == nil {
		t.Fatal("NotifyExtension accepted non-extension method")
	}

	for i := 0; i < 8; i++ {
		select {
		case <-seen:
		case <-ctx.Done():
			t.Fatalf("timed out waiting for client method %d", i)
		}
	}

	if requestError(nil) != nil {
		t.Fatal("requestError(nil) returned non-nil")
	}
	if requestError(context.Canceled).Code != -32800 {
		t.Fatal("context cancellation did not map to ACP cancellation")
	}
	reqErr := acp.NewInvalidParams(map[string]any{"x": true})
	if requestError(reqErr) != reqErr {
		t.Fatal("requestError did not preserve ACP request error")
	}
	if requestError(errors.New("boom")).Code != -32603 {
		t.Fatal("plain error did not map to internal error")
	}
}

func TestLocalAgentConnectionHandleBranches(t *testing.T) {
	agent := newPlaceholderAgent()
	conn := &localAgentConnection{agent: agent}
	if _, reqErr := conn.handle(context.Background(), acp.AgentMethodSessionList, nil); reqErr == nil {
		t.Fatal("method before initialize succeeded")
	}
	if _, reqErr := conn.handle(context.Background(), acp.AgentMethodInitialize, json.RawMessage(`{bad}`)); reqErr == nil {
		t.Fatal("bad initialize params succeeded")
	}
	if _, reqErr := conn.handle(context.Background(), acp.AgentMethodInitialize, json.RawMessage(`{}`)); reqErr != nil {
		t.Fatalf("initialize returned error: %v", reqErr)
	}
	if _, reqErr := conn.handle(context.Background(), "_missing", json.RawMessage(`{}`)); reqErr == nil {
		t.Fatal("missing extension succeeded")
	}
	if _, reqErr := conn.handle(context.Background(), "missing/method", json.RawMessage(`{}`)); reqErr == nil {
		t.Fatal("missing method succeeded")
	}
	if _, reqErr := conn.handle(context.Background(), acp.AgentMethodSessionSetMode, json.RawMessage(`{"sessionId":"missing","modeId":"plan"}`)); reqErr == nil {
		t.Fatal("legacy session/set_mode succeeded")
	}
	if _, reqErr := conn.handle(context.Background(), acp.AgentMethodSessionCancel, json.RawMessage(`{"sessionId":"missing"}`)); reqErr == nil {
		t.Fatal("cancel missing session succeeded")
	}
	resp, err := agent.NewSession(context.Background(), NewSessionRequest("/tmp/project"))
	if err != nil {
		t.Fatalf("NewSession for cancel returned error: %v", err)
	}
	cancelParams, err := json.Marshal(acp.CancelNotification{SessionId: resp.SessionId})
	if err != nil {
		t.Fatalf("marshal cancel: %v", err)
	}
	if _, reqErr := conn.handle(context.Background(), acp.AgentMethodSessionCancel, cancelParams); reqErr != nil {
		t.Fatalf("cancel existing session returned error: %v", reqErr)
	}
}

func TestLocalAgentConnectionErrorBranches(t *testing.T) {
	ctx := context.Background()
	agent := newPlaceholderAgent()
	conn := &localAgentConnection{agent: agent}
	conn.initialized.Store(true)
	if _, reqErr := conn.handle(ctx, acp.AgentMethodMcpMessage, mustJSONRaw(acp.UnstableMessageMcpRequest{ConnectionId: "missing", Method: "tools/list"})); reqErr == nil {
		t.Fatal("local MCP message accepted missing connection")
	}
	if _, reqErr := conn.handle(ctx, acp.AgentMethodSessionNew, json.RawMessage(`{"cwd":"relative","mcpServers":[],"additionalDirectories":[]}`)); reqErr == nil {
		t.Fatal("local response accepted agent error")
	}
	if _, reqErr := conn.handle(ctx, acp.AgentMethodSessionNew, json.RawMessage(`{}`)); reqErr == nil {
		t.Fatal("local response accepted invalid params")
	}
	if _, reqErr := conn.handle(ctx, acp.AgentMethodSessionCancel, json.RawMessage(`{bad}`)); reqErr == nil {
		t.Fatal("local notification accepted bad JSON")
	}
	cancelAgent := NewAgent(withClientFactory(func(context.Context, codex.Options) (codex.Client, error) {
		return &cancelErrorClient{spyCodexClient: newSpyCodexClient()}, nil
	}))
	cancelResp, err := cancelAgent.NewSession(ctx, NewSessionRequest("/tmp/project"))
	if err != nil {
		t.Fatalf("cancel session new: %v", err)
	}
	cancelConn := &localAgentConnection{agent: cancelAgent}
	cancelConn.initialized.Store(true)
	if _, reqErr := cancelConn.handle(ctx, acp.AgentMethodSessionCancel, json.RawMessage(`{"sessionId":"`+string(cancelResp.SessionId)+`"}`)); reqErr == nil {
		t.Fatal("local notification accepted cancel provider error")
	}
}
