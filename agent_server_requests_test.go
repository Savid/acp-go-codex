package codexacp

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	"github.com/coder/acp-go-sdk"
	"github.com/savid/acp-go-codex/internal/codex"
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

	return agent, agent.sessionMust(resp.SessionId), ctx
}

func TestClientElicitationCapabilityGating(t *testing.T) {
	t.Parallel()

	for _, tt := range []struct {
		name     string
		caps     *acp.ElicitationCapabilities
		wantForm bool
		wantURL  bool
	}{
		{name: "nil", caps: nil, wantForm: false, wantURL: false},
		{name: "empty object", caps: &acp.ElicitationCapabilities{}, wantForm: true, wantURL: false},
		{name: "url only", caps: &acp.ElicitationCapabilities{Url: &acp.ElicitationUrlCapabilities{}}, wantForm: false, wantURL: true},
		{name: "form explicit", caps: &acp.ElicitationCapabilities{Form: &acp.ElicitationFormCapabilities{}}, wantForm: true, wantURL: false},
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
	cancelResp, err := agent.handleCodexServerRequest(ctx, codex.ServerRequest{
		Method: codex.RequestCommandApproval,
		Params: json.RawMessage(`{"threadId":"missing","command":"ls"}`),
	})
	if err != nil || asType[map[string]any](t, cancelResp)["decision"] != "cancel" {
		t.Fatalf("approval without session = %#v err=%v", cancelResp, err)
	}
	permissions, err := agent.handleCodexServerRequest(ctx, codex.ServerRequest{
		Method: codex.RequestPermissionsApproval,
		Params: json.RawMessage(`{"threadId":"missing","permissions":{"network":true}}`),
	})
	if err != nil || asType[map[string]any](t, permissions)["scope"] != "turn" {
		t.Fatalf("permissions without session = %#v err=%v", permissions, err)
	}
	input, err := agent.handleCodexServerRequest(ctx, codex.ServerRequest{
		Method: codex.RequestToolUserInput,
		Params: json.RawMessage(`{}`),
	})
	if err != nil || asType[map[string]any](t, input)["answers"] == nil {
		t.Fatalf("tool input without client = %#v err=%v", input, err)
	}
	mcp, err := agent.handleCodexServerRequest(ctx, codex.ServerRequest{
		Method: codex.RequestMCPElicitation,
		Params: json.RawMessage(`{}`),
	})
	if err != nil || asType[map[string]any](t, mcp)["action"] != "cancel" {
		t.Fatalf("mcp without client = %#v err=%v", mcp, err)
	}
	mcpApproval, err := agent.handleCodexServerRequest(ctx, codex.ServerRequest{
		Method: codex.RequestMCPElicitation,
		Params: json.RawMessage(`{"threadId":"missing","_meta":{"codex":{"_meta":{"codex_approval_kind":"mcp_tool_call"}}}}`),
	})
	if err != nil || asType[map[string]any](t, mcpApproval)["action"] != "cancel" {
		t.Fatalf("mcp approval without session = %#v err=%v", mcpApproval, err)
	}
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
	errConn := &serverRequestErrorClient{recordingAgentClient: newRecordingAgentClient(), permissionErr: errors.New("permission failed"), elicitationErr: errors.New("elicitation failed")}
	agent.setAgentClient(errConn)
	if _, fileErr := agent.handleCodexServerRequest(ctx, codex.ServerRequest{
		Method: codex.RequestFileChangeApproval,
		Params: json.RawMessage(`{"threadId":"` + session.codexThreadID + `","grantRoot":"/repo"}`),
	}); fileErr == nil {
		t.Fatal("file approval with permission error succeeded")
	}
	if _, permErr := agent.handleCodexServerRequest(ctx, codex.ServerRequest{
		Method: codex.RequestPermissionsApproval,
		Params: json.RawMessage(`{"threadId":"` + session.codexThreadID + `","permissions":{"fs":true}}`),
	}); permErr == nil {
		t.Fatal("permissions approval with permission error succeeded")
	}
	enableClientElicitation(agent, true, false)
	if _, inputErr := agent.handleCodexServerRequest(ctx, codex.ServerRequest{
		Method: codex.RequestToolUserInput,
		Params: json.RawMessage(`{"threadId":"` + session.codexThreadID + `","questions":[{"id":"name"}]}`),
	}); inputErr == nil {
		t.Fatal("tool input with elicitation error succeeded")
	}
	if _, mcpErr := agent.handleCodexServerRequest(ctx, codex.ServerRequest{
		Method: codex.RequestMCPElicitation,
		Params: json.RawMessage(`{"mode":"form"}`),
	}); mcpErr == nil {
		t.Fatal("MCP elicitation with elicitation error succeeded")
	}
	if _, mcpToolErr := agent.handleCodexServerRequest(ctx, codex.ServerRequest{
		Method: codex.RequestMCPElicitation,
		Params: json.RawMessage(`{"threadId":"` + session.codexThreadID + `","_meta":{"codex":{"_meta":{"codex_approval_kind":"mcp_tool_call"}}}}`),
	}); mcpToolErr == nil {
		t.Fatal("MCP tool approval with permission error succeeded")
	}

	declineConn := newRecordingAgentClient()
	declineConn.permission = acp.PermissionOptionId("decline")
	declineConn.elicitation = acp.NewUnstableCreateElicitationResponseCancel()
	agent.setAgentClient(declineConn)
	permissions, err := agent.handleCodexServerRequest(ctx, codex.ServerRequest{
		Method: codex.RequestPermissionsApproval,
		Params: json.RawMessage(`{"threadId":"` + session.codexThreadID + `","permissions":{"fs":true}}`),
	})
	if err != nil || asType[map[string]any](t, permissions)["scope"] != "turn" {
		t.Fatalf("declined permissions = %#v err=%v", permissions, err)
	}
	mcp, err := agent.handleCodexServerRequest(ctx, codex.ServerRequest{
		Method: codex.RequestMCPElicitation,
		Params: json.RawMessage(`{"mode":"form"}`),
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
	blocking := &blockingPermissionAgentClient{
		recordingAgentClient: newRecordingAgentClient(),
		started:              make(chan struct{}),
		release:              make(chan struct{}),
	}
	agent.setAgentClient(blocking)
	_ = session.beginTurn(ctx)
	resultCh := make(chan any, 1)
	errCh := make(chan error, 1)
	go func() {
		result, err := agent.handleCodexServerRequest(ctx, codex.ServerRequest{
			ID:     json.RawMessage("approval-1"),
			Method: codex.RequestCommandApproval,
			Params: json.RawMessage(`{"threadId":"` + session.codexThreadID + `","approvalId":"approval-1","command":"ls"}`),
		})
		resultCh <- result
		errCh <- err
	}()
	<-blocking.started
	session.cancelTurn()
	close(blocking.release)
	if result := <-resultCh; result != nil {
		t.Fatalf("late canceled approval returned result: %#v", result)
	}
	if err := <-errCh; !errors.Is(err, context.Canceled) {
		t.Fatalf("late canceled approval error = %v", err)
	}
	session.finishTurn()
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

	approval, err := agent.handleCodexServerRequest(ctx, codex.ServerRequest{
		Method: codex.RequestCommandApproval,
		Params: json.RawMessage(`{"threadId":"` + session.codexThreadID + `","itemId":"item-1","command":"git status","availableDecisions":[{"acceptWithExecpolicyAmendment":{"execpolicy_amendment":["git status"]}}]}`),
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
		Params: json.RawMessage(`{"threadId":"` + session.codexThreadID + `","itemId":"input-1","questions":[{"id":"name","question":"Name?"}]}`),
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

	permissions, err := agent.handleCodexServerRequest(ctx, codex.ServerRequest{
		Method: codex.RequestPermissionsApproval,
		Params: json.RawMessage(`{"threadId":"` + session.codexThreadID + `","itemId":"perm-1","reason":"need fs","permissions":{"filesystem":{"write":true}}}`),
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
		Params: json.RawMessage(`{"threadId":"` + session.codexThreadID + `","mode":"form","message":"Need input","requestedSchema":{"title":"T","description":"D","required":["token"],"properties":{"token":{"type":"string"}}}}`),
	})
	if err != nil {
		t.Fatalf("MCP form elicitation returned error: %v", err)
	}
	formMap, ok := form.(map[string]any)
	if !ok || formMap["action"] != "accept" {
		t.Fatalf("form elicitation response = %#v", form)
	}
	if len(conn.scopes) == 0 || conn.scopes[len(conn.scopes)-1].SessionID != session.id || conn.scopes[len(conn.scopes)-1].RequestID == nil {
		t.Fatalf("MCP form scope = %#v", conn.scopes)
	}

	conn.elicitation = acp.NewUnstableCreateElicitationResponseDecline()
	url, err := agent.handleCodexServerRequest(ctx, codex.ServerRequest{
		Method: codex.RequestMCPElicitation,
		Params: json.RawMessage(`{"mode":"url","message":"Open","url":"https://example.com","elicitationId":"e1"}`),
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

	result, err := agent.handleCodexServerRequest(ctx, codex.ServerRequest{
		ID:     json.RawMessage(`"mcp-approval-1"`),
		Method: codex.RequestMCPElicitation,
		Params: json.RawMessage(`{"threadId":"` + session.codexThreadID + `","message":"Allow the remote MCP server to run tool \"execute\"?","tool_params":{"code":"api.request({})"},"_meta":{"codex":{"serverName":"remote","_meta":{"codex_approval_kind":"mcp_tool_call","tool_title":"Execute"}}}}`),
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
		Params: json.RawMessage(`{"threadId":"` + session.codexThreadID + `","_meta":{"codex":{"_meta":{"codex_approval_kind":"mcp_tool_call"}}}}`),
	})
	if err != nil || asType[map[string]any](t, declined)["action"] != "decline" {
		t.Fatalf("declined MCP tool approval = %#v err=%v", declined, err)
	}

	nilPerm := &nilPermissionClient{recordingAgentClient: newRecordingAgentClient()}
	agent.setAgentClient(nilPerm)
	canceled, err := agent.handleCodexServerRequest(ctx, codex.ServerRequest{
		Method: codex.RequestMCPElicitation,
		Params: json.RawMessage(`{"threadId":"` + session.codexThreadID + `","_meta":{"codex":{"_meta":{"codex_approval_kind":"mcp_tool_call"}}}}`),
	})
	if err != nil || asType[map[string]any](t, canceled)["action"] != "cancel" {
		t.Fatalf("canceled MCP tool approval = %#v err=%v", canceled, err)
	}
}

func TestServerRequestFallbackAndElicitationBranches(t *testing.T) {
	agent, session, ctx := newServerRequestSession(t)

	nilPerm := &nilPermissionClient{recordingAgentClient: newRecordingAgentClient()}
	agent.setAgentClient(nilPerm)
	if decision, err := agent.handleCodexServerRequest(ctx, codex.ServerRequest{Method: codex.RequestCommandApproval, Params: json.RawMessage(`{"threadId":"` + session.codexThreadID + `"}`)}); err != nil || asType[map[string]any](t, decision)["decision"] != "cancel" {
		t.Fatalf("nil selected approval = %#v err=%v", decision, err)
	}
	if permissions, err := agent.handleCodexServerRequest(ctx, codex.ServerRequest{Method: codex.RequestPermissionsApproval, Params: json.RawMessage(`{"threadId":"` + session.codexThreadID + `","permissions":{"fs":true}}`)}); err != nil || asType[map[string]any](t, permissions)["scope"] != "turn" {
		t.Fatalf("nil selected permissions = %#v err=%v", permissions, err)
	}
	agent.setAgentClient(nil)
	if decision, err := agent.handleCodexServerRequest(ctx, codex.ServerRequest{Method: codex.RequestCommandApproval, Params: json.RawMessage(`{"threadId":"` + session.codexThreadID + `"}`)}); err != nil || asType[map[string]any](t, decision)["decision"] != "cancel" {
		t.Fatalf("no client approval = %#v err=%v", decision, err)
	}
	if permissions, err := agent.handleCodexServerRequest(ctx, codex.ServerRequest{Method: codex.RequestPermissionsApproval, Params: json.RawMessage(`{"threadId":"` + session.codexThreadID + `","permissions":{"fs":true}}`)}); err != nil || asType[map[string]any](t, permissions)["scope"] != "turn" {
		t.Fatalf("no client permissions = %#v err=%v", permissions, err)
	}
	if mcpApproval, err := agent.handleCodexServerRequest(ctx, codex.ServerRequest{Method: codex.RequestMCPElicitation, Params: json.RawMessage(`{"threadId":"` + session.codexThreadID + `","_meta":{"codex":{"_meta":{"codex_approval_kind":"mcp_tool_call"}}}}`)}); err != nil || asType[map[string]any](t, mcpApproval)["action"] != "cancel" {
		t.Fatalf("no client MCP approval = %#v err=%v", mcpApproval, err)
	}
	if input, err := agent.handleCodexServerRequest(ctx, codex.ServerRequest{Method: codex.RequestToolUserInput, Params: json.RawMessage(`{"threadId":"` + session.codexThreadID + `","questions":[{"id":"answer"}]}`)}); err != nil || len(asType[map[string]any](t, asType[map[string]any](t, input)["answers"])) != 0 {
		t.Fatalf("no client tool input = %#v err=%v", input, err)
	}
}

func TestServerRequestElicitationCapabilityBranches(t *testing.T) {
	agent, session, ctx := newServerRequestSession(t)

	noCapConn := newRecordingAgentClient()
	agent.setAgentClient(noCapConn)
	input, err := agent.handleCodexServerRequest(ctx, codex.ServerRequest{Method: codex.RequestToolUserInput, Params: json.RawMessage(`{"threadId":"` + session.codexThreadID + `","questions":[{"id":"answer"}]}`)})
	if err != nil || len(asType[map[string]any](t, asType[map[string]any](t, input)["answers"])) != 0 || len(noCapConn.elicitations) != 0 {
		t.Fatalf("tool input without form capability = %#v err=%v elicitations=%#v", input, err, noCapConn.elicitations)
	}
	mcpNoCap, err := agent.handleCodexServerRequest(ctx, codex.ServerRequest{Method: codex.RequestMCPElicitation, Params: json.RawMessage(`{"threadId":"` + session.codexThreadID + `","mode":"form"}`)})
	if err != nil || asType[map[string]any](t, mcpNoCap)["action"] != "decline" || len(noCapConn.elicitations) != 0 {
		t.Fatalf("MCP form without capability = %#v err=%v elicitations=%#v", mcpNoCap, err, noCapConn.elicitations)
	}
	mcpURLNoCap, err := agent.handleCodexServerRequest(ctx, codex.ServerRequest{Method: codex.RequestMCPElicitation, Params: json.RawMessage(`{"threadId":"` + session.codexThreadID + `","mode":"url","url":"https://example.com"}`)})
	if err != nil || asType[map[string]any](t, mcpURLNoCap)["action"] != "decline" || len(noCapConn.elicitations) != 0 {
		t.Fatalf("MCP URL without capability = %#v err=%v elicitations=%#v", mcpURLNoCap, err, noCapConn.elicitations)
	}

	acceptConn := &acceptElicitationClient{recordingAgentClient: newRecordingAgentClient()}
	agent.setAgentClient(acceptConn)
	enableClientElicitation(agent, true, true)
	input, err = agent.handleCodexServerRequest(ctx, codex.ServerRequest{Method: codex.RequestToolUserInput, Params: json.RawMessage(`{"threadId":"` + session.codexThreadID + `","questions":[{"header":"Name","question":"Your name?"},{"question":"skip"}]}`)})
	if err != nil || asType[map[string]any](t, input)["answers"] == nil {
		t.Fatalf("accepted tool input = %#v err=%v", input, err)
	}
	mcp, err := agent.handleCodexServerRequest(ctx, codex.ServerRequest{Method: codex.RequestMCPElicitation, Params: json.RawMessage(`{"mode":"url","url":"https://example.com","message":"Open"}`)})
	if err != nil || asType[map[string]any](t, mcp)["action"] != "accept" {
		t.Fatalf("accepted MCP URL = %#v err=%v", mcp, err)
	}
	declineConn := newRecordingAgentClient()
	declineConn.elicitation = acp.NewUnstableCreateElicitationResponseDecline()
	agent.setAgentClient(declineConn)
	mcp, err = agent.handleCodexServerRequest(ctx, codex.ServerRequest{Method: codex.RequestMCPElicitation, Params: json.RawMessage(`{"requestedSchema":{"title":"T","description":"D","required":["x"],"properties":{"x":{"type":"string"}}}}`)})
	if err != nil || asType[map[string]any](t, mcp)["action"] != "decline" {
		t.Fatalf("declined MCP form = %#v err=%v", mcp, err)
	}
	cancelInputConn := newRecordingAgentClient()
	agent.setAgentClient(cancelInputConn)
	input, err = agent.handleCodexServerRequest(ctx, codex.ServerRequest{Method: codex.RequestToolUserInput, Params: json.RawMessage(`{"threadId":"` + session.codexThreadID + `","questions":[{"id":"answer"}]}`)})
	if err != nil || len(asType[map[string]any](t, asType[map[string]any](t, input)["answers"])) != 0 {
		t.Fatalf("canceled tool input = %#v err=%v", input, err)
	}
}
