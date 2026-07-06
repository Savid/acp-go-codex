package codexacp

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"testing"
	"time"

	"github.com/coder/acp-go-sdk"
)

func TestLocalAgentConnectionHandleBranches(t *testing.T) {
	agent := newPlaceholderAgent()
	conn := &localAgentConnection{agent: agent}
	ctx := context.Background()

	if _, reqErr := conn.handle(ctx, acp.AgentMethodSessionList, json.RawMessage(`{}`)); reqErr == nil {
		t.Fatal("uninitialized non-initialize request succeeded")
	}

	if _, reqErr := conn.handle(ctx, acp.AgentMethodInitialize, json.RawMessage(`{`)); reqErr == nil {
		t.Fatal("invalid JSON initialize request succeeded")
	}
	if _, reqErr := conn.handle(ctx, acp.AgentMethodInitialize, json.RawMessage(`{}`)); reqErr != nil {
		t.Fatalf("initialize returned request error: %v", reqErr)
	}
	if _, reqErr := conn.handle(ctx, "missing/method", json.RawMessage(`{}`)); reqErr == nil {
		t.Fatal("unknown method succeeded")
	}
	if _, reqErr := conn.handle(ctx, ForkSessionMethod, json.RawMessage(`{`)); reqErr == nil {
		t.Fatal("invalid extension payload succeeded")
	}

	cancelCtx, cancel := context.WithCancel(ctx)
	cancel()
	if got := requestError(cancelCtx.Err()); got == nil || got.Code != -32800 || got.Message != "Request cancelled" {
		t.Fatalf("requestError(context canceled) = %#v", got)
	}
}

func TestLocalAgentConnectionOutboundClientCalls(t *testing.T) {
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

	client := &recordingClient{}
	clientConn := acp.NewClientSideConnection(client, c2aW, a2cR)
	t.Cleanup(func() {
		_ = clientConn
	})

	agent := NewAgent()
	agentConn := newLocalAgentConnection(agent, a2cW, c2aR)
	agent.setAgentClient(agentConn)

	if err := agentConn.SessionUpdate(ctx, acp.SessionNotification{
		SessionId: "session-1",
		Update:    acp.UpdateAgentMessageText("hello"),
	}); err != nil {
		t.Fatalf("SessionUpdate returned error: %v", err)
	}
	eventually(t, func() bool { return len(client.Updates()) == 1 })
	if len(client.Updates()) != 1 {
		t.Fatalf("client updates = %d, want 1", len(client.Updates()))
	}

	permission, err := agentConn.RequestPermission(ctx, testPermissionRequest())
	if err != nil {
		t.Fatalf("RequestPermission returned error: %v", err)
	}
	if permission.Outcome.Cancelled == nil {
		t.Fatalf("permission response = %#v", permission)
	}

	elicitation, err := agentConn.UnstableCreateElicitation(ctx, acp.UnstableCreateElicitationRequest{
		Form: &acp.UnstableCreateElicitationForm{
			Message: "Need a value",
			Mode:    "form",
			RequestedSchema: acp.UnstableElicitationSchema{
				Type: acp.UnstableElicitationSchemaTypeObject,
			},
			Meta: map[string]any{"source": "test"},
		},
	})
	if err != nil {
		t.Fatalf("UnstableCreateElicitation returned error: %v", err)
	}
	if elicitation.Accept == nil || elicitation.Accept.Content["ok"] != true {
		t.Fatalf("elicitation response = %#v", elicitation)
	}
	if got := client.Elicitations(); len(got) != 1 || got[0].Form == nil || got[0].Form.Meta["source"] != "test" {
		t.Fatalf("client elicitations = %#v", got)
	}

	if err := agentConn.NotifyExtension(ctx, "_codex.test/event", map[string]any{"ok": true}); err != nil {
		t.Fatalf("NotifyExtension returned error: %v", err)
	}
	eventually(t, func() bool { return len(client.Extensions()) == 1 })
	if got := client.Extensions(); len(got) != 1 || got[0].method != "_codex.test/event" {
		t.Fatalf("client extensions = %#v", got)
	}
}

func TestLocalAgentConnectionClientCallBackpressure(t *testing.T) {
	agent := NewAgent(WithConcurrencyLimits(ConcurrencyLimits{MaxConcurrentClientCalls: 1}))
	conn := &localAgentConnection{agent: agent}
	agent.clientCalls <- struct{}{}

	if _, err := conn.RequestPermission(context.Background(), testPermissionRequest()); err == nil {
		t.Fatal("RequestPermission ignored client-call backpressure")
	}
	if err := conn.SessionUpdate(context.Background(), acp.SessionNotification{
		SessionId: "session-1",
		Update:    acp.UpdateAgentMessageText("hello"),
	}); err == nil {
		t.Fatal("SessionUpdate ignored client-call backpressure")
	}
	if err := conn.NotifyExtension(context.Background(), "_codex.test/event", nil); err == nil {
		t.Fatal("NotifyExtension ignored client-call backpressure")
	}
	if _, err := conn.CreateElicitation(context.Background(), acp.UnstableCreateElicitationRequest{
		Form: &acp.UnstableCreateElicitationForm{Message: "m", Mode: "form"},
	}, elicitationScope{}); err == nil {
		t.Fatal("CreateElicitation ignored client-call backpressure")
	}
}

func TestLocalAgentConnectionHelpers(t *testing.T) {
	if got := requestError(nil); got != nil {
		t.Fatalf("requestError(nil) = %#v", got)
	}
	if got := requestError(errors.New("plain failure")); got == nil || got.Code != -32603 {
		t.Fatalf("requestError(internal) = %#v", got)
	}

	agent := NewAgent()
	if _, err := (&localAgentConnection{agent: agent}).CreateElicitation(context.Background(), acp.UnstableCreateElicitationRequest{}, elicitationScope{}); err == nil {
		t.Fatal("CreateElicitation accepted empty request")
	}

	if _, err := scopedElicitationParams(acp.UnstableCreateElicitationRequest{}, elicitationScope{}); err == nil {
		t.Fatal("empty elicitation params succeeded")
	}
	requestIDValue := acp.RequestIdStr("request-1")
	requestID := acp.RequestId{Str: &requestIDValue}
	raw, err := scopedElicitationParams(acp.UnstableCreateElicitationRequest{
		Url: &acp.UnstableCreateElicitationUrl{
			ElicitationId: "elicitation-1",
			Message:       "Open",
			Mode:          "url",
			Url:           "https://example.test",
			Meta:          map[string]any{"m": true},
		},
	}, elicitationScope{SessionID: "session-1", ToolCallID: "tool-1", RequestID: &requestID})
	if err != nil {
		t.Fatalf("url elicitation params returned error: %v", err)
	}
	got := mapFromRaw(raw)
	if got["sessionId"] != "session-1" || got["toolCallId"] != "tool-1" || got["url"] != "https://example.test" {
		t.Fatalf("scoped elicitation payload = %#v", got)
	}

	if err := (&localAgentConnection{}).NotifyExtension(context.Background(), "bad", nil); err == nil {
		t.Fatal("NotifyExtension accepted non-extension method")
	}
}

func TestLocalResponseAndNotificationHelpers(t *testing.T) {
	ctx := context.Background()
	agent := NewAgent()

	response := localResponse(func(*Agent, context.Context, acp.ListSessionsRequest) (acp.ListSessionsResponse, error) {
		return acp.ListSessionsResponse{Sessions: []acp.SessionInfo{{SessionId: "session-1"}}}, nil
	})
	if _, reqErr := response(ctx, agent, json.RawMessage(`{"cursor":`)); reqErr == nil {
		t.Fatal("localResponse accepted invalid JSON")
	}
	newSessionResponse := localResponse(func(*Agent, context.Context, acp.NewSessionRequest) (acp.NewSessionResponse, error) {
		return acp.NewSessionResponse{}, nil
	})
	if _, reqErr := newSessionResponse(ctx, agent, json.RawMessage(`{}`)); reqErr == nil {
		t.Fatal("localResponse accepted invalid NewSession params")
	}
	result, reqErr := response(ctx, agent, json.RawMessage(`{}`))
	if reqErr != nil {
		t.Fatalf("localResponse returned request error: %v", reqErr)
	}
	listResult, ok := result.(acp.ListSessionsResponse)
	if !ok {
		t.Fatalf("localResponse result type = %T", result)
	}
	if len(listResult.Sessions) != 1 {
		t.Fatalf("localResponse result = %#v", result)
	}
	responseErr := localResponse(func(*Agent, context.Context, acp.ListSessionsRequest) (acp.ListSessionsResponse, error) {
		return acp.ListSessionsResponse{}, acp.NewInvalidParams(map[string]any{"bad": true})
	})
	if _, reqErr := responseErr(ctx, agent, json.RawMessage(`{}`)); reqErr == nil || reqErr.Code != -32602 {
		t.Fatalf("localResponse error branch = %#v", reqErr)
	}

	notification := localNotification(func(*Agent, context.Context, acp.CancelNotification) error { return nil })
	if _, reqErr := notification(ctx, agent, json.RawMessage(`{"sessionId":`)); reqErr == nil {
		t.Fatal("localNotification accepted invalid JSON")
	}
	if result, reqErr := notification(ctx, agent, json.RawMessage(`{"sessionId":"session-1"}`)); reqErr != nil || result != nil {
		t.Fatalf("localNotification success result=%#v reqErr=%v", result, reqErr)
	}
	notificationErr := localNotification(func(*Agent, context.Context, acp.CancelNotification) error {
		return errors.New("cancel failed")
	})
	if _, reqErr := notificationErr(ctx, agent, json.RawMessage(`{"sessionId":"session-1"}`)); reqErr == nil || reqErr.Code != -32603 {
		t.Fatalf("localNotification error branch = %#v", reqErr)
	}
}

func testPermissionRequest() acp.RequestPermissionRequest {
	return acp.RequestPermissionRequest{
		SessionId: "session-1",
		ToolCall: acp.ToolCallUpdate{
			ToolCallId: "tool-1",
			Title:      acp.Ptr("Run"),
			Kind:       acp.Ptr(acp.ToolKindExecute),
			Status:     acp.Ptr(acp.ToolCallStatusPending),
			Content:    []acp.ToolCallContent{acp.ToolContent(acp.TextBlock("cmd"))},
		},
		Options: []acp.PermissionOption{
			{Kind: acp.PermissionOptionKindAllowOnce, Name: "Allow", OptionId: "allow"},
			{Kind: acp.PermissionOptionKindRejectOnce, Name: "Reject", OptionId: "reject"},
		},
	}
}

func eventually(t *testing.T, done func() bool) {
	t.Helper()

	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if done() {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
}
