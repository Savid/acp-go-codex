package codexacp

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"strings"
	"testing"
	"time"

	"github.com/coder/acp-go-sdk"
	"github.com/savid/acp-go-codex/internal/codex"
	"github.com/stretchr/testify/require"
)

func enableClientElicitation(agent *Agent, form bool, url bool) {
	agent.mu.Lock()
	defer agent.mu.Unlock()

	caps := &acp.ElicitationCapabilities{}
	if form {
		caps.Form = &acp.ElicitationFormCapabilities{}
	}
	if url {
		caps.Url = &acp.ElicitationUrlCapabilities{}
	}
	agent.clientCapabilities.Elicitation = caps
}

func newServerRequestSession(t *testing.T) (*Agent, *session, context.Context) {
	t.Helper()

	ctx := context.Background()
	client := newSpyCodexClient()
	agent := NewAgent(withClientFactory(func(context.Context, codex.Options) (codex.Client, error) { return client, nil }))

	resp, err := agent.NewSession(ctx, NewSessionRequest("/tmp/project"))
	if err != nil {
		t.Fatalf("NewSession returned error: %v", err)
	}

	session := agent.sessionMust(resp.SessionId)
	_ = session.beginTurn(ctx, "turn-1")
	session.setTurnID("native-turn-1")

	return agent, session, ctx
}

func TestClientElicitationCapabilityGating(t *testing.T) {
	t.Parallel()

	var bothModesNull acp.ElicitationCapabilities
	if err := json.Unmarshal([]byte(`{"form":null,"url":null}`), &bothModesNull); err != nil {
		t.Fatalf("decode explicit-null elicitation capabilities: %v", err)
	}

	for _, tt := range []struct {
		name     string
		caps     *acp.ElicitationCapabilities
		wantForm bool
		wantURL  bool
	}{
		{name: "nil", caps: nil, wantForm: false, wantURL: false},
		{name: "empty object", caps: &acp.ElicitationCapabilities{}, wantForm: false, wantURL: false},
		{name: "both modes null", caps: &bothModesNull, wantForm: false, wantURL: false},
		{name: "url only", caps: &acp.ElicitationCapabilities{Url: &acp.ElicitationUrlCapabilities{}}, wantForm: false, wantURL: true},
		{name: "form explicit", caps: &acp.ElicitationCapabilities{Form: &acp.ElicitationFormCapabilities{}}, wantForm: true, wantURL: false},
		{name: "form and url", caps: &acp.ElicitationCapabilities{Form: &acp.ElicitationFormCapabilities{}, Url: &acp.ElicitationUrlCapabilities{}}, wantForm: true, wantURL: true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			agent := NewAgent()
			agent.clientCapabilities.Elicitation = tt.caps
			if got := agent.clientSupportsFormElicitation(); got != tt.wantForm {
				t.Fatalf("clientSupportsFormElicitation() = %v, want %v", got, tt.wantForm)
			}
			if got := agent.clientSupportsURLElicitation(); got != tt.wantURL {
				t.Fatalf("clientSupportsURLElicitation() = %v, want %v", got, tt.wantURL)
			}
		})
	}
}

func TestServerRequestsNoSessionOrClientBranches(t *testing.T) {
	agent := NewAgent()
	ctx := context.Background()
	_, err := agent.handleCodexServerRequest(ctx, codex.ServerRequest{
		Method: codex.RequestCommandApproval,
		Params: json.RawMessage(`{"threadId":"missing","command":"ls"}`),
	})
	if err == nil {
		t.Fatal("approval without session succeeded")
	}
	_, err = agent.handleCodexServerRequest(ctx, codex.ServerRequest{
		Method: codex.RequestPermissionsApproval,
		Params: json.RawMessage(`{"threadId":"missing","permissions":{"network":true}}`),
	})
	if err == nil {
		t.Fatal("permissions without session succeeded")
	}
	_, err = agent.handleCodexServerRequest(ctx, codex.ServerRequest{
		Method: codex.RequestToolUserInput,
		Params: json.RawMessage(`{}`),
	})
	if err == nil {
		t.Fatal("tool input without thread succeeded")
	}
	_, err = agent.handleCodexServerRequest(ctx, codex.ServerRequest{
		Method: codex.RequestMCPElicitation,
		Params: json.RawMessage(`{}`),
	})
	if err == nil {
		t.Fatal("MCP elicitation without thread succeeded")
	}
	_, err = agent.handleCodexServerRequest(ctx, codex.ServerRequest{
		Method: codex.RequestMCPElicitation,
		Params: json.RawMessage(`{"threadId":"missing","_meta":{"codex_approval_kind":"mcp_tool_call"}}`),
	})
	if err == nil {
		t.Fatal("MCP approval without session succeeded")
	}
}

func TestServerRequestWaitsForAcceptedNativeTurnBinding(t *testing.T) {
	agent := NewAgent()
	client := newSpyCodexClient()
	session := newSession(agent, "session", "/tmp/project", nil, codex.Thread{ID: "thread"}, client, sessionMeta{}, nil)
	agent.sessions[session.id] = session
	_ = session.beginTurn(context.Background(), "turn-nonce")

	conn := &bindingPermissionClient{
		recordingAgentClient: newRecordingAgentClient(),
		requested:            make(chan struct{}),
	}
	agent.setAgentClient(conn)

	type requestResult struct {
		value any
		err   error
	}
	result := make(chan requestResult, 1)
	go func() {
		value, err := agent.handleCodexServerRequest(context.Background(), codex.ServerRequest{
			Method: codex.RequestCommandApproval,
			Params: json.RawMessage(`{"threadId":"thread","turnId":"native-turn","itemId":"command","command":"echo ok"}`),
		})
		result <- requestResult{value: value, err: err}
	}()

	require.Eventually(t, func() bool {
		session.mu.Lock()
		defer session.mu.Unlock()

		return len(session.interactions) == 1
	}, time.Second, time.Millisecond)

	canceled, cancel := context.WithCancel(context.Background())
	cancel()
	cancelledApproval, err := agent.handleCodexServerRequest(canceled, codex.ServerRequest{
		Method: codex.RequestCommandApproval,
		Params: json.RawMessage(`{"threadId":"thread","turnId":"native-turn","itemId":"cancelled-command","command":"echo no"}`),
	})
	require.NoError(t, err)
	require.Equal(t, "cancel", asType[map[string]any](t, cancelledApproval)["decision"])
	_, err = agent.handleCodexServerRequest(canceled, codex.ServerRequest{
		Method: codex.RequestToolUserInput,
		Params: json.RawMessage(`{"threadId":"thread","turnId":"native-turn","itemId":"cancelled-input","questions":[{"id":"name"}]}`),
	})
	require.ErrorIs(t, err, context.Canceled)

	// The start response can name the turn before lifecycle acceptance is
	// emitted. Staging that ID alone must not release the native request.
	session.stageTurnID("native-turn")
	select {
	case <-conn.requested:
		t.Fatal("server request reached the client before turn acceptance")
	default:
	}

	session.acceptTurnBinding()
	select {
	case <-conn.requested:
	case <-time.After(time.Second):
		t.Fatal("accepted turn did not release its early server request")
	}

	got := <-result
	require.NoError(t, got.err)
	require.Equal(t, "accept", asType[map[string]any](t, got.value)["decision"])
}

type bindingPermissionClient struct {
	*recordingAgentClient
	requested chan struct{}
}

type cancelingElicitationClient struct {
	*recordingAgentClient
	cancel context.CancelFunc
}

func (c *bindingPermissionClient) RequestPermission(
	ctx context.Context,
	request acp.RequestPermissionRequest,
) (acp.RequestPermissionResponse, error) {
	close(c.requested)

	return c.recordingAgentClient.RequestPermission(ctx, request)
}

func (c *cancelingElicitationClient) UnstableCreateElicitation(
	ctx context.Context,
	request acp.UnstableCreateElicitationRequest,
) (acp.UnstableCreateElicitationResponse, error) {
	return c.CreateElicitation(ctx, request, elicitationScope{})
}

func (c *cancelingElicitationClient) CreateElicitation(
	_ context.Context,
	_ acp.UnstableCreateElicitationRequest,
	_ elicitationScope,
) (acp.UnstableCreateElicitationResponse, error) {
	c.cancel()

	return acp.NewUnstableCreateElicitationResponseCancel(), nil
}

func TestServerRequestsCancelWhenPermissionToolIsNoLongerRequestable(t *testing.T) {
	agent, session, _ := newServerRequestSession(t)
	agent.setAgentClient(newRecordingAgentClient())

	for _, tc := range []struct {
		name   string
		method string
		params string
		class  permissionToolClass
		field  string
		want   any
	}{
		{
			name: "command", method: codex.RequestCommandApproval,
			params: `,"command":"echo no"`, class: permissionToolCommand,
			field: "decision", want: "cancel",
		},
		{
			name: "permissions", method: codex.RequestPermissionsApproval,
			params: `,"permissions":{"fs":true}`, class: permissionToolPermissions,
			field: "scope", want: "turn",
		},
		{
			name: "mcp", method: codex.RequestMCPElicitation,
			params: `,"_meta":{"codex_approval_kind":"mcp_tool_call"}`, class: permissionToolMCP,
			field: "action", want: "cancel",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			id := acp.ToolCallId(tc.method)
			session.permissionTools.mu.Lock()
			session.permissionTools.tools = map[acp.ToolCallId]*permissionToolRecord{
				id: {id: id, class: tc.class, terminal: true},
			}
			session.permissionTools.aliases = map[string]acp.ToolCallId{string(id): id}
			session.permissionTools.mu.Unlock()

			response, err := agent.handleCodexServerRequest(context.Background(), codex.ServerRequest{
				Method: tc.method,
				Params: json.RawMessage(`{"threadId":"` + session.codexThreadID + `","turnId":"native-turn-1"` + tc.params + `}`),
			})
			require.NoError(t, err)
			require.Equal(t, tc.want, asType[map[string]any](t, response)[tc.field])
		})
	}
}

func TestServerRequestReportsCancellationThatArrivesDuringNonPermissionInteraction(t *testing.T) {
	agent, session, _ := newServerRequestSession(t)
	enableClientElicitation(agent, true, false)
	ctx, cancel := context.WithCancel(context.Background())
	agent.setAgentClient(&cancelingElicitationClient{
		recordingAgentClient: newRecordingAgentClient(),
		cancel:               cancel,
	})

	_, err := agent.handleCodexServerRequest(ctx, codex.ServerRequest{
		Method: codex.RequestToolUserInput,
		Params: json.RawMessage(`{"threadId":"` + session.codexThreadID + `","turnId":"native-turn-1","itemId":"question","questions":[{"id":"name","question":"Name?"}]}`),
	})
	require.ErrorIs(t, err, context.Canceled)
}

func TestServerRequestErrorAndDecisionBranches(t *testing.T) {
	ctx := context.Background()
	client := newSpyCodexClient()
	agent := NewAgent(withClientFactory(func(context.Context, codex.Options) (codex.Client, error) { return client, nil }))
	resp, err := agent.NewSession(ctx, NewSessionRequest("/tmp/project"))
	if err != nil {
		t.Fatalf("NewSession returned error: %v", err)
	}
	session := agent.sessionMust(resp.SessionId)
	_ = session.beginTurn(ctx, "turn-1")
	session.setTurnID("native-turn-1")
	errConn := &serverRequestErrorClient{recordingAgentClient: newRecordingAgentClient(), permissionErr: errors.New("permission failed"), elicitationErr: errors.New("elicitation failed")}
	agent.setAgentClient(errConn)
	if _, fileErr := agent.handleCodexServerRequest(ctx, codex.ServerRequest{
		Method: codex.RequestFileChangeApproval,
		Params: json.RawMessage(`{"threadId":"` + session.codexThreadID + `","turnId":"native-turn-1","grantRoot":"/repo"}`),
	}); fileErr == nil {
		t.Fatal("file approval with permission error succeeded")
	}
	if _, permErr := agent.handleCodexServerRequest(ctx, codex.ServerRequest{
		Method: codex.RequestPermissionsApproval,
		Params: json.RawMessage(`{"threadId":"` + session.codexThreadID + `","turnId":"native-turn-1","permissions":{"fs":true}}`),
	}); permErr == nil {
		t.Fatal("permissions approval with permission error succeeded")
	}
	enableClientElicitation(agent, true, false)
	originalRand := sessionIDRandReader
	sessionIDRandReader = strings.NewReader("short")
	_, mintErr := agent.handleCodexServerRequest(ctx, codex.ServerRequest{
		Method: codex.RequestMCPElicitation,
		Params: json.RawMessage(`{"threadId":"` + session.codexThreadID + `","turnId":"native-turn-1","mode":"form"}`),
	})
	sessionIDRandReader = originalRand
	if !errors.Is(mintErr, io.ErrUnexpectedEOF) {
		t.Fatalf("MCP elicitation request-ID mint error = %v", mintErr)
	}
	if _, inputErr := agent.handleCodexServerRequest(ctx, codex.ServerRequest{
		Method: codex.RequestToolUserInput,
		Params: json.RawMessage(`{"threadId":"` + session.codexThreadID + `","turnId":"native-turn-1","questions":[{"id":"name"}]}`),
	}); inputErr == nil {
		t.Fatal("tool input with elicitation error succeeded")
	}
	if _, mcpErr := agent.handleCodexServerRequest(ctx, codex.ServerRequest{
		ID:     json.RawMessage(`"mcp-error"`),
		Method: codex.RequestMCPElicitation,
		Params: json.RawMessage(`{"threadId":"` + session.codexThreadID + `","turnId":"native-turn-1","mode":"form"}`),
	}); mcpErr == nil {
		t.Fatal("MCP elicitation with elicitation error succeeded")
	}
	if _, mcpToolErr := agent.handleCodexServerRequest(ctx, codex.ServerRequest{
		Method: codex.RequestMCPElicitation,
		Params: json.RawMessage(`{"threadId":"` + session.codexThreadID + `","turnId":"native-turn-1","_meta":{"codex_approval_kind":"mcp_tool_call"}}`),
	}); mcpToolErr == nil {
		t.Fatal("MCP tool approval with permission error succeeded")
	}

	declineConn := newRecordingAgentClient()
	declineConn.permission = acp.PermissionOptionId("decline")
	declineConn.elicitation = acp.NewUnstableCreateElicitationResponseCancel()
	agent.setAgentClient(declineConn)
	permissions, err := agent.handleCodexServerRequest(ctx, codex.ServerRequest{
		Method: codex.RequestPermissionsApproval,
		Params: json.RawMessage(`{"threadId":"` + session.codexThreadID + `","turnId":"native-turn-1","permissions":{"fs":true}}`),
	})
	if err != nil || asType[map[string]any](t, permissions)["scope"] != "turn" {
		t.Fatalf("declined permissions = %#v err=%v", permissions, err)
	}
	mcp, err := agent.handleCodexServerRequest(ctx, codex.ServerRequest{
		ID:     json.RawMessage(`"mcp-cancel"`),
		Method: codex.RequestMCPElicitation,
		Params: json.RawMessage(`{"threadId":"` + session.codexThreadID + `","turnId":"native-turn-1","mode":"form"}`),
	})
	if err != nil || asType[map[string]any](t, mcp)["action"] != "cancel" {
		t.Fatalf("canceled MCP elicitation = %#v err=%v", mcp, err)
	}

	refreshAgent := NewAgent(WithCodexChatGPTAuthTokenRefresher(func(context.Context) (ChatGPTAuthTokens, error) {
		return ChatGPTAuthTokens{}, errors.New("refresh failed")
	}))
	if _, err := refreshAgent.handleCodexServerRequest(ctx, codex.ServerRequest{Method: codex.RequestAuthTokenRefresh}); err == nil {
		t.Fatal("refresh callback error succeeded")
	}
}

func TestServerRequestLateApprovalIsDetachedOnCancel(t *testing.T) {
	ctx := context.Background()
	client := newSpyCodexClient()
	agent := NewAgent(withClientFactory(func(context.Context, codex.Options) (codex.Client, error) { return client, nil }))
	resp, err := agent.NewSession(ctx, NewSessionRequest("/tmp/project"))
	if err != nil {
		t.Fatalf("NewSession returned error: %v", err)
	}
	session := agent.sessionMust(resp.SessionId)
	_ = session.beginTurn(ctx, "turn-1")
	session.setTurnID("native-turn-1")
	blocking := &blockingPermissionAgentClient{
		recordingAgentClient: newRecordingAgentClient(),
		started:              make(chan struct{}),
		release:              make(chan struct{}),
	}
	agent.setAgentClient(blocking)
	_ = session.beginTurn(ctx, "test-turn")
	session.setTurnID("native-turn-1")
	resultCh := make(chan any, 1)
	errCh := make(chan error, 1)
	go func() {
		result, err := agent.handleCodexServerRequest(ctx, codex.ServerRequest{
			ID:     json.RawMessage("approval-1"),
			Method: codex.RequestCommandApproval,
			Params: json.RawMessage(`{"threadId":"` + session.codexThreadID + `","turnId":"native-turn-1","approvalId":"approval-1","command":"ls"}`),
		})
		resultCh <- result
		errCh <- err
	}()
	<-blocking.started
	session.cancelTurn()
	lateConn := newRecordingAgentClient()
	agent.setAgentClient(lateConn)
	lateResult, lateErr := agent.handleCodexServerRequest(ctx, codex.ServerRequest{
		ID:     json.RawMessage("approval-after-cancel"),
		Method: codex.RequestCommandApproval,
		Params: json.RawMessage(`{"threadId":"` + session.codexThreadID + `","turnId":"native-turn-1","approvalId":"approval-after-cancel","command":"pwd"}`),
	})
	require.NoError(t, lateErr)
	require.Equal(t, "cancel", asType[map[string]any](t, lateResult)["decision"])
	require.Empty(t, lateConn.permissions, "request created after cancellation reached the host")
	close(blocking.release)
	result := <-resultCh
	if decision := asType[map[string]any](t, result)["decision"]; decision != "cancel" {
		t.Fatalf("late canceled approval result = %#v", result)
	}
	if err := <-errCh; err != nil {
		t.Fatalf("late canceled approval error = %v", err)
	}
	session.finishTurn()
}

func TestServerRequestPendingApprovalIsAnsweredCancelledOnShutdown(t *testing.T) {
	ctx := context.Background()
	agent, session, _ := newServerRequestSession(t)
	blocking := &blockingPermissionAgentClient{
		recordingAgentClient: newRecordingAgentClient(),
		started:              make(chan struct{}),
		release:              make(chan struct{}),
	}
	agent.setAgentClient(blocking)
	resultCh := make(chan any, 1)
	errCh := make(chan error, 1)
	go func() {
		result, err := agent.handleCodexServerRequest(ctx, codex.ServerRequest{
			ID:     json.RawMessage("approval-shutdown"),
			Method: codex.RequestCommandApproval,
			Params: json.RawMessage(`{"threadId":"` + session.codexThreadID + `","turnId":"native-turn-1","approvalId":"approval-shutdown","command":"ls"}`),
		})
		resultCh <- result
		errCh <- err
	}()
	<-blocking.started
	if err := agent.Close(); err != nil {
		t.Fatalf("Close returned error: %v", err)
	}
	close(blocking.release)

	result := <-resultCh
	if decision := asType[map[string]any](t, result)["decision"]; decision != "cancel" {
		t.Fatalf("shutdown approval result = %#v", result)
	}
	if err := <-errCh; err != nil {
		t.Fatalf("shutdown approval error = %v", err)
	}
}

func TestCodexPermissionCancellationResponses(t *testing.T) {
	for name, test := range map[string]struct {
		method string
		params map[string]any
		field  string
		want   any
		ok     bool
	}{
		"command":     {method: codex.RequestCommandApproval, field: "decision", want: "cancel", ok: true},
		"file":        {method: codex.RequestFileChangeApproval, field: "decision", want: "cancel", ok: true},
		"permissions": {method: codex.RequestPermissionsApproval, field: "scope", want: "turn", ok: true},
		"mcp tool": {
			method: codex.RequestMCPElicitation,
			params: map[string]any{"_meta": map[string]any{"codex_approval_kind": "mcp_tool_call"}},
			field:  "action",
			want:   "cancel",
			ok:     true,
		},
		"mcp user": {method: codex.RequestMCPElicitation},
		"unknown":  {method: "unknown"},
	} {
		t.Run(name, func(t *testing.T) {
			response, ok := codexPermissionCancellationResponse(test.method, test.params)
			if ok != test.ok {
				t.Fatalf("ok = %t, want %t", ok, test.ok)
			}
			if !ok {
				return
			}

			if got := asType[map[string]any](t, response)[test.field]; got != test.want {
				t.Fatalf("response = %#v, want %s=%v", response, test.field, test.want)
			}
		})
	}

	agent, session, _ := newServerRequestSession(t)
	agent.setAgentClient(newRecordingAgentClient())
	canceled, cancel := context.WithCancel(context.Background())
	cancel()

	for name, request := range map[string]codex.ServerRequest{
		"command": {
			Method: codex.RequestCommandApproval,
			Params: json.RawMessage(`{"threadId":"` + session.codexThreadID + `","turnId":"native-turn-1","itemId":"command"}`),
		},
		"permissions": {
			Method: codex.RequestPermissionsApproval,
			Params: json.RawMessage(`{"threadId":"` + session.codexThreadID + `","turnId":"native-turn-1","itemId":"permissions","permissions":{}}`),
		},
		"mcp": {
			Method: codex.RequestMCPElicitation,
			Params: json.RawMessage(`{"threadId":"` + session.codexThreadID + `","turnId":"native-turn-1","_meta":{"codex_approval_kind":"mcp_tool_call"}}`),
		},
	} {
		t.Run("canceled "+name, func(t *testing.T) {
			response, err := agent.handleCodexServerRequest(canceled, request)
			if err != nil || response == nil {
				t.Fatalf("canceled %s response = %#v err=%v", name, response, err)
			}
		})
	}

	_, err := agent.handleCodexServerRequest(canceled, codex.ServerRequest{
		Method: codex.RequestToolUserInput,
		Params: json.RawMessage(`{"threadId":"` + session.codexThreadID + `","turnId":"native-turn-1","questions":[]}`),
	})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled non-permission request error = %v", err)
	}
}

func TestTurnScopedServerRequestsRejectMissingOrStaleNativeTurn(t *testing.T) {
	for _, test := range []struct {
		name   string
		method string
		params string
		check  func(*testing.T, any)
	}{
		{
			name: "command approval", method: codex.RequestCommandApproval,
			params: `,"itemId":"command-item","command":"pwd"`,
			check: func(t *testing.T, response any) {
				t.Helper()
				if got := asType[map[string]any](t, response)["decision"]; got != "cancel" {
					t.Fatalf("command decision = %#v", got)
				}
			},
		},
		{
			name: "file approval", method: codex.RequestFileChangeApproval,
			params: `,"itemId":"file-item","grantRoot":"/repo"`,
			check: func(t *testing.T, response any) {
				t.Helper()
				if got := asType[map[string]any](t, response)["decision"]; got != "cancel" {
					t.Fatalf("file decision = %#v", got)
				}
			},
		},
		{
			name: "permission profile", method: codex.RequestPermissionsApproval,
			params: `,"itemId":"permissions-item","permissions":{"filesystem":{"write":true}}`,
			check: func(t *testing.T, response any) {
				t.Helper()
				result := asType[map[string]any](t, response)
				if result["scope"] != "turn" || len(asType[map[string]any](t, result["permissions"])) != 0 {
					t.Fatalf("permissions response = %#v", result)
				}
			},
		},
		{
			name: "tool user input", method: codex.RequestToolUserInput,
			params: `,"itemId":"input-item","questions":[{"id":"answer","question":"Continue?"}]`,
			check: func(t *testing.T, response any) {
				t.Helper()
				answers := asType[map[string]any](t, asType[map[string]any](t, response)["answers"])
				if len(answers) != 0 {
					t.Fatalf("tool input answers = %#v", answers)
				}
			},
		},
	} {
		for turnName, turnField := range map[string]string{
			"missing": "",
			"stale":   `,"turnId":"native-turn-stale"`,
		} {
			t.Run(test.name+"/"+turnName, func(t *testing.T) {
				agent, session, ctx := newServerRequestSession(t)
				conn := newRecordingAgentClient()
				conn.elicitation = acp.NewUnstableCreateElicitationResponseAccept()
				agent.setAgentClient(conn)
				enableClientElicitation(agent, true, true)

				response, err := agent.handleCodexServerRequest(ctx, codex.ServerRequest{
					ID:     json.RawMessage(`"turn-mismatch"`),
					Method: test.method,
					Params: json.RawMessage(`{"threadId":"` + session.codexThreadID + `"` + turnField + test.params + `}`),
				})
				if err != nil {
					t.Fatalf("server request returned error: %v", err)
				}

				test.check(t, response)
				if len(conn.permissions) != 0 || len(conn.elicitations) != 0 || len(conn.updates) != 0 {
					t.Fatalf("stale request reached client: permissions=%d elicitations=%d updates=%d", len(conn.permissions), len(conn.elicitations), len(conn.updates))
				}
			})
		}
	}
}

func TestServerRequestsApprovalElicitationAndRefresh(t *testing.T) {
	client := newSpyCodexClient()
	agent := NewAgent(
		WithCodexChatGPTAuthTokenRefresher(func(context.Context) (ChatGPTAuthTokens, error) {
			return ChatGPTAuthTokens{AccessToken: "new", RefreshToken: "refresh-new", AccountID: "acct", PlanType: "pro", ExpiresAtUnixSec: 456}, nil
		}),
		withClientFactory(func(context.Context, codex.Options) (codex.Client, error) { return client, nil }),
	)
	conn := newRecordingAgentClient()
	conn.permission = acp.PermissionOptionId(`{"acceptWithExecpolicyAmendment":{"execpolicy_amendment":["git status"]}}`)
	conn.elicitation = acp.UnstableCreateElicitationResponse{
		Accept: &acp.UnstableCreateElicitationAccept{Action: "accept", Content: map[string]any{
			"name":   "value",
			"multi":  []any{"a", 2},
			"preset": []string{"x"},
			"empty":  nil,
		}},
	}
	agent.setAgentClient(conn)
	enableClientElicitation(agent, true, false)

	ctx := context.Background()
	resp, err := agent.NewSession(ctx, NewSessionRequest("/tmp/project"))
	if err != nil {
		t.Fatalf("NewSession returned error: %v", err)
	}
	session, _ := agent.session(resp.SessionId)
	_ = session.beginTurn(ctx, "turn-1")
	session.setTurnID("native-turn-1")
	defer session.finishTurn()

	approval, err := agent.handleCodexServerRequest(ctx, codex.ServerRequest{
		Method: codex.RequestCommandApproval,
		Params: json.RawMessage(`{"threadId":"` + session.codexThreadID + `","turnId":"native-turn-1","itemId":"item-1","command":"git status","availableDecisions":[{"acceptWithExecpolicyAmendment":{"execpolicy_amendment":["git status"]}}]}`),
	})
	if err != nil {
		t.Fatalf("approval returned error: %v", err)
	}
	approvalMap, ok := approval.(map[string]any)
	if !ok || approvalMap["decision"] == nil {
		t.Fatalf("approval response = %#v", approval)
	}

	elicit, err := agent.handleCodexServerRequest(ctx, codex.ServerRequest{
		Method: codex.RequestToolUserInput,
		Params: json.RawMessage(`{"threadId":"` + session.codexThreadID + `","turnId":"native-turn-1","itemId":"input-1","questions":[{"id":"name","question":"Name?"}]}`),
	})
	if err != nil {
		t.Fatalf("tool input returned error: %v", err)
	}
	elicitMap, ok := elicit.(map[string]any)
	if !ok || elicitMap["answers"] == nil {
		t.Fatalf("elicitation response = %#v", elicit)
	}
	answers := asType[map[string]any](t, elicitMap["answers"])
	if len(asType[[]string](t, asType[map[string]any](t, answers["multi"])["answers"])) != 2 ||
		len(asType[[]string](t, asType[map[string]any](t, answers["preset"])["answers"])) != 1 ||
		len(asType[[]string](t, asType[map[string]any](t, answers["empty"])["answers"])) != 0 {
		t.Fatalf("elicitation answers = %#v", answers)
	}
	if len(conn.scopes) == 0 || conn.scopes[len(conn.scopes)-1].SessionID != session.id || conn.scopes[len(conn.scopes)-1].ToolCallID != "input-1" {
		t.Fatalf("tool input elicitation scope = %#v", conn.scopes)
	}
	if len(conn.elicitations) == 0 || conn.elicitations[len(conn.elicitations)-1].Form == nil || conn.elicitations[len(conn.elicitations)-1].Form.Message != "Name?" {
		t.Fatalf("tool input elicitation request = %#v", conn.elicitations)
	}

	refresh, err := agent.handleCodexServerRequest(ctx, codex.ServerRequest{Method: codex.RequestAuthTokenRefresh})
	if err != nil {
		t.Fatalf("refresh returned error: %v", err)
	}
	refreshMap, ok := refresh.(map[string]any)
	if !ok || refreshMap["accessToken"] != "new" || refreshMap["refreshToken"] != "refresh-new" || refreshMap["expiresAt"] != int64(456) {
		t.Fatalf("refresh response = %#v", refresh)
	}
	if tokens, ok := agent.externalAuthTokens(); !ok || tokens.AccessToken != "new" || tokens.AccountID != "acct" || tokens.PlanType != "pro" {
		t.Fatalf("refresh did not update stored tokens: %#v ok=%v", tokens, ok)
	}
}

func TestServerRequestsPermissionsAndMCPElicitation(t *testing.T) {
	client := newSpyCodexClient()
	agent := NewAgent(withClientFactory(func(context.Context, codex.Options) (codex.Client, error) { return client, nil }))
	conn := newRecordingAgentClient()
	conn.permission = acp.PermissionOptionId("grant-session")
	conn.elicitation = acp.UnstableCreateElicitationResponse{
		Accept: &acp.UnstableCreateElicitationAccept{Action: "accept", Content: map[string]any{"token": "value"}, Meta: map[string]any{"m": true}},
	}
	agent.setAgentClient(conn)
	enableClientElicitation(agent, true, true)

	ctx := context.Background()
	resp, err := agent.NewSession(ctx, NewSessionRequest("/tmp/project"))
	if err != nil {
		t.Fatalf("NewSession returned error: %v", err)
	}
	session := agent.sessionMust(resp.SessionId)
	_ = session.beginTurn(ctx, "turn-1")
	session.setTurnID("native-turn-1")

	permissions, err := agent.handleCodexServerRequest(ctx, codex.ServerRequest{
		Method: codex.RequestPermissionsApproval,
		Params: json.RawMessage(`{"threadId":"` + session.codexThreadID + `","turnId":"native-turn-1","itemId":"perm-1","reason":"need fs","permissions":{"filesystem":{"write":true}}}`),
	})
	if err != nil {
		t.Fatalf("permissions approval returned error: %v", err)
	}
	permissionsMap, ok := permissions.(map[string]any)
	if !ok || permissionsMap["scope"] != "session" {
		t.Fatalf("permissions response = %#v", permissions)
	}

	form, err := agent.handleCodexServerRequest(ctx, codex.ServerRequest{
		ID:     json.RawMessage(`"mcp-form-1"`),
		Method: codex.RequestMCPElicitation,
		Params: json.RawMessage(`{"threadId":"` + session.codexThreadID + `","turnId":"native-turn-1","mode":"form","message":"Need input","requestedSchema":{"title":"T","description":"D","required":["token"],"properties":{"token":{"type":"string"}}}}`),
	})
	if err != nil {
		t.Fatalf("MCP form elicitation returned error: %v", err)
	}
	formMap, ok := form.(map[string]any)
	if !ok || formMap["action"] != "accept" {
		t.Fatalf("form elicitation response = %#v", form)
	}
	if len(conn.scopes) == 0 || conn.scopes[len(conn.scopes)-1].SessionID != session.id || conn.scopes[len(conn.scopes)-1].TurnNonce != "turn-1" || conn.scopes[len(conn.scopes)-1].RequestID == nil || conn.scopes[len(conn.scopes)-1].RequestID.Str == nil || *conn.scopes[len(conn.scopes)-1].RequestID.Str != "jsonrpc:string:bWNwLWZvcm0tMQ" || conn.scopes[len(conn.scopes)-1].ToolCallID != "" {
		t.Fatalf("MCP form scope = %#v", conn.scopes)
	}

	conn.elicitation = acp.NewUnstableCreateElicitationResponseDecline()
	url, err := agent.handleCodexServerRequest(ctx, codex.ServerRequest{
		Method: codex.RequestMCPElicitation,
		Params: json.RawMessage(`{"threadId":"` + session.codexThreadID + `","turnId":"native-turn-1","mode":"url","message":"Open","url":"https://example.com","elicitationId":"e1"}`),
	})
	if err != nil {
		t.Fatalf("MCP URL elicitation returned error: %v", err)
	}
	urlMap, ok := url.(map[string]any)
	if !ok || urlMap["action"] != "decline" {
		t.Fatalf("url elicitation response = %#v", url)
	}

	if _, err := agent.handleCodexServerRequest(ctx, codex.ServerRequest{Method: "missing"}); err == nil {
		t.Fatal("unsupported server request succeeded")
	}
	if _, err := agent.handleCodexServerRequest(ctx, codex.ServerRequest{Method: codex.RequestAuthTokenRefresh}); err == nil {
		t.Fatal("refresh without callback succeeded")
	}
}

func TestServerRequestsMCPToolApprovalUsesPermission(t *testing.T) {
	client := newSpyCodexClient()
	agent := NewAgent(withClientFactory(func(context.Context, codex.Options) (codex.Client, error) { return client, nil }))
	conn := newRecordingAgentClient()
	agent.setAgentClient(conn)

	ctx := context.Background()
	resp, err := agent.NewSession(ctx, NewSessionRequest("/tmp/project"))
	if err != nil {
		t.Fatalf("NewSession returned error: %v", err)
	}
	session := agent.sessionMust(resp.SessionId)
	_ = session.beginTurn(ctx, "mcp-permission-turn")
	session.setTurnID("native-mcp-permission")
	defer session.finishTurn()

	result, err := agent.handleCodexServerRequest(ctx, codex.ServerRequest{
		ID:     json.RawMessage(`"mcp-approval-1"`),
		Method: codex.RequestMCPElicitation,
		Params: json.RawMessage(`{"threadId":"` + session.codexThreadID + `","turnId":"native-mcp-permission","serverName":"remote","message":"Allow the remote MCP server to run tool \"execute\"?","tool_params":{"code":"api.request({})"},"_meta":{"codex_approval_kind":"mcp_tool_call","codex_request_type":"approval","tool_name":"execute","tool_title":"Execute"}}`),
	})
	if err != nil {
		t.Fatalf("MCP tool approval returned error: %v", err)
	}
	resultMap, ok := result.(map[string]any)
	if !ok || resultMap["action"] != "accept" {
		t.Fatalf("MCP tool approval response = %#v", result)
	}
	if len(conn.elicitations) != 0 {
		t.Fatalf("MCP tool approval used elicitation: %#v", conn.elicitations)
	}
	if len(conn.permissions) != 1 {
		t.Fatalf("MCP tool approval permission count = %d", len(conn.permissions))
	}
	permission := conn.permissions[0]
	if permission.SessionId != session.id || permission.ToolCall.Title == nil || *permission.ToolCall.Title != "Execute" {
		t.Fatalf("MCP tool approval permission = %#v", permission)
	}
	if len(permission.Options) != 2 || permission.Options[0].OptionId != "accept" || permission.Options[1].OptionId != "decline" {
		t.Fatalf("MCP tool approval options = %#v", permission.Options)
	}
	if len(permission.ToolCall.Content) == 0 || permission.ToolCall.RawInput == nil || permission.ToolCall.Meta[codexMetaKey] == nil {
		t.Fatalf("MCP tool approval content/meta missing: %#v", permission.ToolCall)
	}

	conn.permission = acp.PermissionOptionId("decline")
	declined, err := agent.handleCodexServerRequest(ctx, codex.ServerRequest{
		Method: codex.RequestMCPElicitation,
		Params: json.RawMessage(`{"threadId":"` + session.codexThreadID + `","turnId":"native-mcp-permission","_meta":{"codex_approval_kind":"mcp_tool_call"}}`),
	})
	if err != nil || asType[map[string]any](t, declined)["action"] != "decline" {
		t.Fatalf("declined MCP tool approval = %#v err=%v", declined, err)
	}

	nilPerm := &nilPermissionClient{recordingAgentClient: newRecordingAgentClient()}
	agent.setAgentClient(nilPerm)
	canceled, err := agent.handleCodexServerRequest(ctx, codex.ServerRequest{
		Method: codex.RequestMCPElicitation,
		Params: json.RawMessage(`{"threadId":"` + session.codexThreadID + `","turnId":"native-mcp-permission","_meta":{"codex_approval_kind":"mcp_tool_call"}}`),
	})
	if err != nil || asType[map[string]any](t, canceled)["action"] != "cancel" {
		t.Fatalf("canceled MCP tool approval = %#v err=%v", canceled, err)
	}
}

func TestServerRequestFallbackAndElicitationBranches(t *testing.T) {
	agent, session, ctx := newServerRequestSession(t)

	nilPerm := &nilPermissionClient{recordingAgentClient: newRecordingAgentClient()}
	agent.setAgentClient(nilPerm)
	if decision, err := agent.handleCodexServerRequest(ctx, codex.ServerRequest{Method: codex.RequestCommandApproval, Params: json.RawMessage(`{"threadId":"` + session.codexThreadID + `","turnId":"native-turn-1"}`)}); err != nil || asType[map[string]any](t, decision)["decision"] != "cancel" {
		t.Fatalf("nil selected approval = %#v err=%v", decision, err)
	}
	if permissions, err := agent.handleCodexServerRequest(ctx, codex.ServerRequest{Method: codex.RequestPermissionsApproval, Params: json.RawMessage(`{"threadId":"` + session.codexThreadID + `","turnId":"native-turn-1","permissions":{"fs":true}}`)}); err != nil || asType[map[string]any](t, permissions)["scope"] != "turn" {
		t.Fatalf("nil selected permissions = %#v err=%v", permissions, err)
	}
	agent.setAgentClient(nil)
	if decision, err := agent.handleCodexServerRequest(ctx, codex.ServerRequest{Method: codex.RequestCommandApproval, Params: json.RawMessage(`{"threadId":"` + session.codexThreadID + `","turnId":"native-turn-1"}`)}); err != nil || asType[map[string]any](t, decision)["decision"] != "cancel" {
		t.Fatalf("no client approval = %#v err=%v", decision, err)
	}
	if permissions, err := agent.handleCodexServerRequest(ctx, codex.ServerRequest{Method: codex.RequestPermissionsApproval, Params: json.RawMessage(`{"threadId":"` + session.codexThreadID + `","turnId":"native-turn-1","permissions":{"fs":true}}`)}); err != nil || asType[map[string]any](t, permissions)["scope"] != "turn" {
		t.Fatalf("no client permissions = %#v err=%v", permissions, err)
	}
	if mcpApproval, err := agent.handleCodexServerRequest(ctx, codex.ServerRequest{Method: codex.RequestMCPElicitation, Params: json.RawMessage(`{"threadId":"` + session.codexThreadID + `","turnId":"native-turn-1","_meta":{"codex_approval_kind":"mcp_tool_call"}}`)}); err != nil || asType[map[string]any](t, mcpApproval)["action"] != "cancel" {
		t.Fatalf("no client MCP approval = %#v err=%v", mcpApproval, err)
	}
	if mcpUser, err := agent.handleCodexServerRequest(ctx, codex.ServerRequest{ID: json.RawMessage(`"mcp-user"`), Method: codex.RequestMCPElicitation, Params: json.RawMessage(`{"threadId":"` + session.codexThreadID + `","turnId":"native-turn-1","mode":"form"}`)}); err != nil || asType[map[string]any](t, mcpUser)["action"] != "cancel" {
		t.Fatalf("no client MCP user elicitation = %#v err=%v", mcpUser, err)
	}
	if input, err := agent.handleCodexServerRequest(ctx, codex.ServerRequest{Method: codex.RequestToolUserInput, Params: json.RawMessage(`{"threadId":"` + session.codexThreadID + `","turnId":"native-turn-1","questions":[{"id":"answer"}]}`)}); err != nil || len(asType[map[string]any](t, asType[map[string]any](t, input)["answers"])) != 0 {
		t.Fatalf("no client tool input = %#v err=%v", input, err)
	}
}

func TestServerRequestElicitationCapabilityBranches(t *testing.T) {
	agent, session, ctx := newServerRequestSession(t)

	noCapConn := newRecordingAgentClient()
	agent.setAgentClient(noCapConn)
	input, err := agent.handleCodexServerRequest(ctx, codex.ServerRequest{Method: codex.RequestToolUserInput, Params: json.RawMessage(`{"threadId":"` + session.codexThreadID + `","turnId":"native-turn-1","questions":[{"id":"answer"}]}`)})
	if err != nil || len(asType[map[string]any](t, asType[map[string]any](t, input)["answers"])) != 0 || len(noCapConn.elicitations) != 0 {
		t.Fatalf("tool input without form capability = %#v err=%v elicitations=%#v", input, err, noCapConn.elicitations)
	}
	mcpNoCap, err := agent.handleCodexServerRequest(ctx, codex.ServerRequest{Method: codex.RequestMCPElicitation, Params: json.RawMessage(`{"threadId":"` + session.codexThreadID + `","turnId":"native-turn-1","mode":"form"}`)})
	if err != nil || asType[map[string]any](t, mcpNoCap)["action"] != "decline" || len(noCapConn.elicitations) != 0 {
		t.Fatalf("MCP form without capability = %#v err=%v elicitations=%#v", mcpNoCap, err, noCapConn.elicitations)
	}
	mcpURLNoCap, err := agent.handleCodexServerRequest(ctx, codex.ServerRequest{Method: codex.RequestMCPElicitation, Params: json.RawMessage(`{"threadId":"` + session.codexThreadID + `","turnId":"native-turn-1","mode":"url","url":"https://example.com"}`)})
	if err != nil || asType[map[string]any](t, mcpURLNoCap)["action"] != "decline" || len(noCapConn.elicitations) != 0 {
		t.Fatalf("MCP URL without capability = %#v err=%v elicitations=%#v", mcpURLNoCap, err, noCapConn.elicitations)
	}

	acceptConn := &acceptElicitationClient{recordingAgentClient: newRecordingAgentClient()}
	agent.setAgentClient(acceptConn)
	enableClientElicitation(agent, true, true)
	input, err = agent.handleCodexServerRequest(ctx, codex.ServerRequest{Method: codex.RequestToolUserInput, Params: json.RawMessage(`{"threadId":"` + session.codexThreadID + `","turnId":"native-turn-1","questions":[{"header":"Name","question":"Your name?"},{"question":"skip"}]}`)})
	if err != nil || asType[map[string]any](t, input)["answers"] == nil {
		t.Fatalf("accepted tool input = %#v err=%v", input, err)
	}
	mcp, err := agent.handleCodexServerRequest(ctx, codex.ServerRequest{ID: json.RawMessage(`"mcp-url-accept"`), Method: codex.RequestMCPElicitation, Params: json.RawMessage(`{"threadId":"` + session.codexThreadID + `","turnId":"native-turn-1","mode":"url","url":"https://example.com","message":"Open"}`)})
	if err != nil || asType[map[string]any](t, mcp)["action"] != "accept" {
		t.Fatalf("accepted MCP URL = %#v err=%v", mcp, err)
	}
	declineConn := newRecordingAgentClient()
	declineConn.elicitation = acp.NewUnstableCreateElicitationResponseDecline()
	agent.setAgentClient(declineConn)
	mcp, err = agent.handleCodexServerRequest(ctx, codex.ServerRequest{ID: json.RawMessage(`"mcp-form-decline"`), Method: codex.RequestMCPElicitation, Params: json.RawMessage(`{"threadId":"` + session.codexThreadID + `","turnId":"native-turn-1","requestedSchema":{"title":"T","description":"D","required":["x"],"properties":{"x":{"type":"string"}}}}`)})
	if err != nil || asType[map[string]any](t, mcp)["action"] != "decline" {
		t.Fatalf("declined MCP form = %#v err=%v", mcp, err)
	}
	cancelInputConn := newRecordingAgentClient()
	agent.setAgentClient(cancelInputConn)
	input, err = agent.handleCodexServerRequest(ctx, codex.ServerRequest{Method: codex.RequestToolUserInput, Params: json.RawMessage(`{"threadId":"` + session.codexThreadID + `","turnId":"native-turn-1","questions":[{"id":"answer"}]}`)})
	if err != nil || len(asType[map[string]any](t, asType[map[string]any](t, input)["answers"])) != 0 {
		t.Fatalf("canceled tool input = %#v err=%v", input, err)
	}
}

// TestElicitationRefusesSecretMarkedStableForms proves both stable-form
// producers fail closed on a secret marker: the native tool question and the
// MCP server's verbatim requestedSchema. Neither reaches the client, so no
// value is ever collected and nothing can be echoed back to Codex, and the
// refusal payload discloses nothing about what was asked for.
func TestElicitationRefusesSecretMarkedStableForms(t *testing.T) {
	for name, secret := range map[string]struct {
		method   string
		params   string
		wantBody map[string]any
	}{
		"tool question marked secret": {
			method: codex.RequestToolUserInput,
			params: `{"turnId":"native-permission-turn","itemId":"ask-1","questions":[` +
				`{"id":"api_token","header":"API token","question":"Paste the production API token","isSecret":true}]}`,
			wantBody: map[string]any{"answers": map[string]any{}},
		},
		"tool question quotes the secret marker": {
			method: codex.RequestToolUserInput,
			params: `{"turnId":"native-permission-turn","itemId":"ask-2","questions":[` +
				`{"id":"api_token","question":"Paste the production API token","isSecret":"true"}]}`,
			wantBody: map[string]any{"answers": map[string]any{}},
		},
		"mcp schema marks a password format": {
			method: codex.RequestMCPElicitation,
			params: `{"turnId":"native-permission-turn","serverName":"vault","mode":"form","message":"Need input",` +
				`"requestedSchema":{"type":"object","required":["api_token"],"properties":{` +
				`"api_token":{"type":"string","title":"API token","format":"password"}}}}`,
			wantBody: map[string]any{"action": "decline"},
		},
		"mcp schema hides a write-only field in a nested object": {
			method: codex.RequestMCPElicitation,
			params: `{"turnId":"native-permission-turn","serverName":"vault","mode":"form","message":"Need input",` +
				`"requestedSchema":{"type":"object","properties":{"credentials":{"type":"object","properties":{` +
				`"api_token":{"type":"string","writeOnly":true}}}}}}`,
			wantBody: map[string]any{"action": "decline"},
		},
		"mcp schema hides a secret field in an array item": {
			method: codex.RequestMCPElicitation,
			params: `{"turnId":"native-permission-turn","serverName":"vault","mode":"form","message":"Need input",` +
				`"requestedSchema":{"type":"object","properties":{"credentials":{"type":"array","items":[` +
				`{"type":"string","isSecret":true}]}}}}`,
			wantBody: map[string]any{"action": "decline"},
		},
	} {
		t.Run(name, func(t *testing.T) {
			agent, session, conn, turnCtx := newStrictPermissionSession(t)
			enableClientElicitation(agent, true, false)

			params := `{"threadId":"` + session.codexThreadID + `",` + strings.TrimPrefix(secret.params, "{")

			response, err := agent.handleCodexServerRequest(turnCtx, codex.ServerRequest{
				ID:     json.RawMessage(`91`),
				Method: secret.method,
				Params: json.RawMessage(params),
			})
			require.NoError(t, err)
			require.Equal(t, secret.wantBody, asType[map[string]any](t, response))

			// Nothing was asked of the client, so nothing was collected and the
			// reverse leg has no answer to echo back to Codex.
			require.Empty(t, conn.elicitations)
			require.Empty(t, conn.scopes)

			encoded, marshalErr := json.Marshal(response)
			require.NoError(t, marshalErr)

			for _, disclosure := range []string{"api_token", "API token", "password", "writeOnly", "isSecret", "production"} {
				require.NotContains(t, string(encoded), disclosure)
			}
		})
	}
}

// TestElicitationAdmitsFormsWithoutSecretMarkers proves the refusal is scoped
// to the marker and not to stable forms in general.
func TestElicitationAdmitsFormsWithoutSecretMarkers(t *testing.T) {
	agent, session, conn, turnCtx := newStrictPermissionSession(t)
	enableClientElicitation(agent, true, false)

	_, err := agent.handleCodexServerRequest(turnCtx, codex.ServerRequest{
		ID:     json.RawMessage(`92`),
		Method: codex.RequestToolUserInput,
		Params: json.RawMessage(`{"threadId":"` + session.codexThreadID + `","turnId":"native-permission-turn",` +
			`"itemId":"ask-3","questions":[{"id":"branch","question":"Which branch?","isSecret":false}]}`),
	})
	require.NoError(t, err)
	require.Len(t, conn.elicitations, 1)
	require.NotNil(t, conn.elicitations[0].Form)
	require.Contains(t, conn.elicitations[0].Form.RequestedSchema.Properties, "branch")
}
