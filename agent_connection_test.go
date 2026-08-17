package codexacp

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"reflect"
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
}

// TestRequestErrorCancelPrecedence pins which signal decides -32800. Only an
// honored $/cancel_request cancels a request context with cause
// context.Canceled, and it outranks whatever error the handler was carrying;
// a connection teardown or an adapter deadline carries a different cause and
// must not be reported as a cancellation even when the error itself wraps
// context.Canceled.
func TestRequestErrorCancelPrecedence(t *testing.T) {
	invalidParams := acp.NewInvalidParams(map[string]any{jsonFieldError: errValueUnsupported, jsonFieldField: jsonFieldPrompt})

	for name, test := range map[string]struct {
		cause    error
		err      error
		wantCode int
		wantSame bool
	}{
		"honored cancel outranks a request error": {
			cause:    context.Canceled,
			err:      invalidParams,
			wantCode: -32800,
		},
		"honored cancel with a plain error": {
			cause:    context.Canceled,
			err:      context.Canceled,
			wantCode: -32800,
		},
		"connection teardown is not a cancellation": {
			cause:    errors.New("connection closed"),
			err:      fmt.Errorf("write update: %w", context.Canceled),
			wantCode: -32603,
		},
		"adapter deadline is an internal failure": {
			cause:    context.DeadlineExceeded,
			err:      context.DeadlineExceeded,
			wantCode: -32603,
		},
		"live request keeps its request error": {
			err:      invalidParams,
			wantCode: -32602,
			wantSame: true,
		},
		"live request wraps an opaque failure": {
			err:      errors.New("boom"),
			wantCode: -32603,
		},
	} {
		t.Run(name, func(t *testing.T) {
			ctx, cancel := context.WithCancelCause(context.Background())
			defer cancel(errors.New("test cleanup"))

			if test.cause != nil {
				cancel(test.cause)
			}

			got := requestError(ctx, test.err)
			if got == nil || got.Code != test.wantCode {
				t.Fatalf("requestError = %#v, want code %d", got, test.wantCode)
			}
			if test.wantSame && got != invalidParams {
				t.Fatalf("requestError = %#v, want the original request error", got)
			}
		})
	}

	if got := requestError(context.Background(), nil); got != nil {
		t.Fatalf("requestError(nil) = %#v", got)
	}
}

func TestLocalAgentConnectionClosedWinsBeforeDispatchAndDecode(t *testing.T) {
	agent := NewAgent()
	if err := agent.Close(); err != nil {
		t.Fatalf("Close returned error: %v", err)
	}

	conn := &localAgentConnection{agent: agent}
	for name, test := range map[string]struct {
		method string
		params json.RawMessage
	}{
		"initialize malformed": {method: acp.AgentMethodInitialize, params: json.RawMessage(`{`)},
		"known malformed":      {method: acp.AgentMethodSessionList, params: json.RawMessage(`{`)},
		"unknown stable":       {method: "missing/method", params: json.RawMessage(`{}`)},
		"unknown extension":    {method: "_codex/missing", params: json.RawMessage(`{`)},
	} {
		t.Run(name, func(t *testing.T) {
			if _, reqErr := conn.handle(context.Background(), test.method, test.params); reqErr == nil || reqErr.Code != -32600 {
				t.Fatalf("closed request error = %#v, want -32600", reqErr)
			}
		})
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

	elicitation, err := agentConn.CreateElicitation(ctx, acp.UnstableCreateElicitationRequest{
		Form: &acp.UnstableCreateElicitationForm{
			Message: "Need a value",
			Mode:    "form",
			RequestedSchema: acp.UnstableElicitationSchema{
				Type: acp.UnstableElicitationSchemaTypeObject,
			},
			Meta: map[string]any{"source": "test"},
		},
	}, elicitationScope{SessionID: "session-1", TurnNonce: "turn-1", ToolCallID: "tool-1"})
	if err != nil {
		t.Fatalf("UnstableCreateElicitation returned error: %v", err)
	}
	if elicitation.Accept == nil || elicitation.Accept.Content["ok"] != true {
		t.Fatalf("elicitation response = %#v", elicitation)
	}
	requestIDValue := acp.RequestIdStr("request-2")
	requestID := acp.RequestId{Str: &requestIDValue}
	urlElicitation, err := agentConn.CreateElicitation(ctx, acp.NewUnstableCreateElicitationRequestUrl(
		"elicitation-2", "https://example.test/open",
	), elicitationScope{SessionID: "session-1", TurnNonce: "turn-1", RequestID: &requestID})
	if err != nil {
		t.Fatalf("URL elicitation returned error: %v", err)
	}
	if urlElicitation.Accept == nil {
		t.Fatalf("URL elicitation response = %#v", urlElicitation)
	}
	if got := client.Elicitations(); len(got) != 2 || got[0].Form == nil || got[0].Form.Meta["source"] != "test" || got[1].Url == nil {
		t.Fatalf("client elicitations = %#v", got)
	}
	gotElicitations := client.Elicitations()
	formRoute := asType[map[string]any](t, gotElicitations[0].Form.Meta[routeMetaKey])
	if !reflect.DeepEqual(formRoute, map[string]any{
		routeVersionKey:    float64(routeVersion),
		jsonFieldSessionID: "session-1",
		routeTurnNonceKey:  "turn-1",
		routeToolCallIDKey: "tool-1",
	}) {
		t.Fatalf("decoded form elicitation route = %#v", formRoute)
	}
	urlRoute := asType[map[string]any](t, gotElicitations[1].Url.Meta[routeMetaKey])
	if !reflect.DeepEqual(urlRoute, map[string]any{
		routeVersionKey:    float64(routeVersion),
		jsonFieldSessionID: "session-1",
		routeTurnNonceKey:  "turn-1",
		routeRequestIDKey:  "request-2",
	}) {
		t.Fatalf("decoded URL elicitation route = %#v", urlRoute)
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
	agent := NewAgent()
	if _, err := (&localAgentConnection{agent: agent}).CreateElicitation(context.Background(), acp.UnstableCreateElicitationRequest{}, elicitationScope{}); err == nil {
		t.Fatal("CreateElicitation accepted empty request")
	}

	if _, err := scopedElicitationParams(acp.UnstableCreateElicitationRequest{}, elicitationScope{}); err == nil {
		t.Fatal("empty elicitation params succeeded")
	}
	raw, err := scopedElicitationParams(acp.UnstableCreateElicitationRequest{
		Url: &acp.UnstableCreateElicitationUrl{
			ElicitationId: "elicitation-1",
			Message:       "Open",
			Mode:          "url",
			Url:           "https://example.test",
			Meta:          map[string]any{"m": true},
		},
	}, elicitationScope{SessionID: "session-1", TurnNonce: "turn-1", ToolCallID: "tool-1"})
	if err != nil {
		t.Fatalf("url elicitation params returned error: %v", err)
	}
	got := mapFromRaw(raw)
	route := asType[map[string]any](t, asType[map[string]any](t, got[jsonFieldMeta])[routeMetaKey])
	if route[jsonFieldSessionID] != "session-1" || route[routeToolCallIDKey] != "tool-1" || got["url"] != "https://example.test" {
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
