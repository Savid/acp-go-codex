package codexacp

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/coder/acp-go-sdk"
	"github.com/savid/acp-go-codex/internal/codex"
)

func TestServerRequestHelperBranches(t *testing.T) {
	if approvalTitle(codexReqFileChangeApproval, map[string]any{"grantRoot": "/repo"}) != "/repo" {
		t.Fatal("file approval title did not use grant root")
	}
	if approvalTitle("other", nil) != "Codex permission request" {
		t.Fatal("default approval title changed")
	}
	if len(approvalContent(codexReqFileChangeApproval, map[string]any{"grantRoot": "/repo"})) != 1 {
		t.Fatal("file approval content missing")
	}
	if len(approvalContent("other", map[string]any{"reason": "because"})) != 1 {
		t.Fatal("reason approval content missing")
	}
	if approvalContent("other", nil) != nil {
		t.Fatal("empty approval content should be nil")
	}

	options := codexApprovalOptions(map[string]any{"proposedExecpolicyAmendment": []any{"git status"}})
	if len(options) != 5 || options[2].OptionId != "acceptWithExecpolicyAmendment" {
		t.Fatalf("amendment options = %#v", options)
	}
	options = codexApprovalOptions(map[string]any{"availableDecisions": []any{"accept", "custom", 7}})
	if len(options) != 2 || options[1].OptionId != "custom" {
		t.Fatalf("available decision options = %#v", options)
	}
	id, name, kind := codexDecisionOption(map[string]any{"applyNetworkPolicyAmendment": map[string]any{"allow": true}})
	if id == "" || name != "Apply network policy" || kind != acp.PermissionOptionKindAllowAlways {
		t.Fatalf("network decision option = %q %q %v", id, name, kind)
	}
	if id, _, _ := codexDecisionOption(func() {}); id != "" {
		t.Fatalf("unmarshalable decision id = %q", id)
	}
	if got := codexDecisionFromOption("acceptWithExecpolicyAmendment", map[string]any{"proposedExecpolicyAmendment": []any{"ls"}}); mapFromAny(got)["acceptWithExecpolicyAmendment"] == nil {
		t.Fatalf("decision from amendment = %#v", got)
	}
	if got := codexDecisionFromOption("unknown", nil); got != permissionCancel {
		t.Fatalf("unknown decision = %#v", got)
	}
	if text := permissionProfileText(func() {}); text == "" {
		t.Fatalf("permission profile text fallback = %q", text)
	}
	props, required := schemaFromToolQuestions(nil)
	if props["answer"] == nil || len(required) != 1 {
		t.Fatalf("default tool schema props=%#v required=%#v", props, required)
	}
	if stringFromAny(fmt.Stringer(stringer("x"))) != "x" {
		t.Fatal("stringFromAny did not use Stringer")
	}
	if mapFromAny("bad") != nil {
		t.Fatal("mapFromAny accepted non-map")
	}
	if sliceOfMaps("bad") != nil {
		t.Fatal("sliceOfMaps accepted non-slice")
	}
	if !slicesEqual([]string{"a", "b"}, []string{"a", "b"}) || slicesEqual([]string{"a"}, []string{"a", "b"}) || slicesEqual([]string{"a"}, []string{"b"}) {
		t.Fatal("slicesEqual failed")
	}
	boolOpt := acp.SessionConfigOption{Boolean: &acp.SessionConfigOptionBoolean{Id: "b", Name: "Bool", Type: "checkbox", CurrentValue: true}}
	if unstableConfigOption(boolOpt).Boolean == nil {
		t.Fatal("unstableConfigOption did not map boolean")
	}
	if unstableConfigOption(acp.SessionConfigOption{}).Select != nil {
		t.Fatal("empty unstableConfigOption produced select")
	}
	if nullableString("") != nil || nullableString("x") != "x" {
		t.Fatal("nullableString failed")
	}
	if toolKind(codex.ToolEvent{Kind: "mcpToolCall"}) != acp.ToolKindOther || toolKind(codex.ToolEvent{Kind: "unknown"}) != acp.ToolKindOther {
		t.Fatal("toolKind special cases failed")
	}
	if stopReasonFromCodex(codex.StopReasonCancelled) != acp.StopReasonCancelled || stopReasonFromCodex(codex.StopReasonError) != acp.StopReasonEndTurn {
		t.Fatal("stopReasonFromCodex special cases failed")
	}
	if replayStatus("failed") != acp.ToolCallStatusFailed || replayStatus("in_progress") != acp.ToolCallStatusInProgress || replayStatus("done") != acp.ToolCallStatusCompleted {
		t.Fatal("replayStatus failed")
	}
	if threadGoalText(map[string]any{"goal": "nested"}) != "Goal updated: nested" {
		t.Fatal("threadGoalText goal failed")
	}
	if !strings.Contains(textFromAny([]any{"a", map[string]any{"text": "b"}}), `"text":"b"`) {
		t.Fatal("textFromAny slice failed")
	}
}

func TestServerRequestsNoSessionOrClientBranches(t *testing.T) {
	agent := NewAgent()
	ctx := context.Background()
	cancelResp, err := agent.handleCodexServerRequest(ctx, codex.ServerRequest{
		Method: codexReqCommandApproval,
		Params: json.RawMessage(`{"threadId":"missing","command":"ls"}`),
	})
	if err != nil || cancelResp.(map[string]any)["decision"] != permissionCancel {
		t.Fatalf("approval without session = %#v err=%v", cancelResp, err)
	}
	permissions, err := agent.handleCodexServerRequest(ctx, codex.ServerRequest{
		Method: codexReqPermissionsApproval,
		Params: json.RawMessage(`{"threadId":"missing","permissions":{"network":true}}`),
	})
	if err != nil || permissions.(map[string]any)["scope"] != "turn" {
		t.Fatalf("permissions without session = %#v err=%v", permissions, err)
	}
	input, err := agent.handleCodexServerRequest(ctx, codex.ServerRequest{
		Method: codexReqToolUserInput,
		Params: json.RawMessage(`{}`),
	})
	if err != nil || input.(map[string]any)["answers"] == nil {
		t.Fatalf("tool input without client = %#v err=%v", input, err)
	}
	mcp, err := agent.handleCodexServerRequest(ctx, codex.ServerRequest{
		Method: codexReqMCPElicitation,
		Params: json.RawMessage(`{}`),
	})
	if err != nil || mcp.(map[string]any)["action"] != "cancel" {
		t.Fatalf("mcp without client = %#v err=%v", mcp, err)
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
	if _, err := agent.handleCodexServerRequest(ctx, codex.ServerRequest{
		Method: codexReqFileChangeApproval,
		Params: json.RawMessage(`{"threadId":"` + session.codexThreadID + `","grantRoot":"/repo"}`),
	}); err == nil {
		t.Fatal("file approval with permission error succeeded")
	}
	if _, err := agent.handleCodexServerRequest(ctx, codex.ServerRequest{
		Method: codexReqPermissionsApproval,
		Params: json.RawMessage(`{"threadId":"` + session.codexThreadID + `","permissions":{"fs":true}}`),
	}); err == nil {
		t.Fatal("permissions approval with permission error succeeded")
	}
	if _, err := agent.handleCodexServerRequest(ctx, codex.ServerRequest{
		Method: codexReqToolUserInput,
		Params: json.RawMessage(`{"questions":[{"id":"name"}]}`),
	}); err == nil {
		t.Fatal("tool input with elicitation error succeeded")
	}
	if _, err := agent.handleCodexServerRequest(ctx, codex.ServerRequest{
		Method: codexReqMCPElicitation,
		Params: json.RawMessage(`{"mode":"form"}`),
	}); err == nil {
		t.Fatal("MCP elicitation with elicitation error succeeded")
	}

	declineConn := newRecordingAgentClient()
	declineConn.permission = acp.PermissionOptionId(permissionDecline)
	declineConn.elicitation = acp.NewUnstableCreateElicitationResponseCancel()
	agent.setAgentClient(declineConn)
	permissions, err := agent.handleCodexServerRequest(ctx, codex.ServerRequest{
		Method: codexReqPermissionsApproval,
		Params: json.RawMessage(`{"threadId":"` + session.codexThreadID + `","permissions":{"fs":true}}`),
	})
	if err != nil || permissions.(map[string]any)["scope"] != "turn" {
		t.Fatalf("declined permissions = %#v err=%v", permissions, err)
	}
	mcp, err := agent.handleCodexServerRequest(ctx, codex.ServerRequest{
		Method: codexReqMCPElicitation,
		Params: json.RawMessage(`{"mode":"form"}`),
	})
	if err != nil || mcp.(map[string]any)["action"] != "cancel" {
		t.Fatalf("canceled MCP elicitation = %#v err=%v", mcp, err)
	}

	refreshAgent := NewAgent(WithChatGPTAuthTokenRefresher(func(context.Context) (codex.ChatGPTAuthTokens, error) {
		return codex.ChatGPTAuthTokens{}, errors.New("refresh failed")
	}))
	if _, err := refreshAgent.handleCodexServerRequest(ctx, codex.ServerRequest{Method: codexReqAuthTokenRefresh}); err == nil {
		t.Fatal("refresh callback error succeeded")
	}
	if approvalTitle(codexReqCommandApproval, map[string]any{"command": "ls"}) != "ls" {
		t.Fatal("command approval title failed")
	}
	for _, decision := range []string{permissionAcceptForSession, permissionDecline, permissionCancel} {
		id, _, _ := codexDecisionOption(decision)
		if id != decision {
			t.Fatalf("decision option %q -> %q", decision, id)
		}
	}
	id, name, kind := codexDecisionOption(map[string]any{"acceptWithExecpolicyAmendment": map[string]any{}})
	if id == "" || name != "Allow and remember" || kind != acp.PermissionOptionKindAllowAlways {
		t.Fatalf("accept amendment decision = %q %q %v", id, name, kind)
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
			Method: codexReqCommandApproval,
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
		WithChatGPTAuthTokenRefresher(func(context.Context) (codex.ChatGPTAuthTokens, error) {
			return codex.ChatGPTAuthTokens{AccessToken: "new", RefreshToken: "refresh-new", AccountID: "acct", PlanType: "pro", ExpiresAtUnixSec: 456}, nil
		}),
		withClientFactory(func(context.Context, codex.Options) (codex.Client, error) { return client, nil }),
	)
	conn := newRecordingAgentClient()
	conn.permission = acp.PermissionOptionId(`{"acceptWithExecpolicyAmendment":{"execpolicy_amendment":["git status"]}}`)
	conn.elicitation = acp.UnstableCreateElicitationResponse{
		Accept: &acp.UnstableCreateElicitationAccept{Action: "accept", Content: map[string]any{"name": "value"}},
	}
	agent.setAgentClient(conn)

	ctx := context.Background()
	resp, err := agent.NewSession(ctx, NewSessionRequest("/tmp/project"))
	if err != nil {
		t.Fatalf("NewSession returned error: %v", err)
	}
	session, _ := agent.session(resp.SessionId)

	approval, err := agent.handleCodexServerRequest(ctx, codex.ServerRequest{
		Method: codexReqCommandApproval,
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
		Method: codexReqToolUserInput,
		Params: json.RawMessage(`{"questions":[{"id":"name","question":"Name?"}]}`),
	})
	if err != nil {
		t.Fatalf("tool input returned error: %v", err)
	}
	elicitMap, ok := elicit.(map[string]any)
	if !ok || elicitMap["answers"] == nil {
		t.Fatalf("elicitation response = %#v", elicit)
	}

	refresh, err := agent.handleCodexServerRequest(ctx, codex.ServerRequest{Method: codexReqAuthTokenRefresh})
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

	ctx := context.Background()
	resp, err := agent.NewSession(ctx, NewSessionRequest("/tmp/project"))
	if err != nil {
		t.Fatalf("NewSession returned error: %v", err)
	}
	session := agent.sessionMust(resp.SessionId)

	permissions, err := agent.handleCodexServerRequest(ctx, codex.ServerRequest{
		Method: codexReqPermissionsApproval,
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
		Method: codexReqMCPElicitation,
		Params: json.RawMessage(`{"mode":"form","message":"Need input","requestedSchema":{"title":"T","description":"D","required":["token"],"properties":{"token":{"type":"string"}}}}`),
	})
	if err != nil {
		t.Fatalf("MCP form elicitation returned error: %v", err)
	}
	formMap, ok := form.(map[string]any)
	if !ok || formMap["action"] != "accept" {
		t.Fatalf("form elicitation response = %#v", form)
	}

	conn.elicitation = acp.NewUnstableCreateElicitationResponseDecline()
	url, err := agent.handleCodexServerRequest(ctx, codex.ServerRequest{
		Method: codexReqMCPElicitation,
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
	if _, err := agent.handleCodexServerRequest(ctx, codex.ServerRequest{Method: codexReqAuthTokenRefresh}); err == nil {
		t.Fatal("refresh without callback succeeded")
	}
}

func TestServerRequestFallbackAndElicitationBranches(t *testing.T) {
	ctx := context.Background()
	client := newSpyCodexClient()
	agent := NewAgent(withClientFactory(func(context.Context, codex.Options) (codex.Client, error) { return client, nil }))
	resp, err := agent.NewSession(ctx, NewSessionRequest("/tmp/project"))
	if err != nil {
		t.Fatalf("NewSession returned error: %v", err)
	}
	session := agent.sessionMust(resp.SessionId)

	nilPerm := &nilPermissionClient{recordingAgentClient: newRecordingAgentClient()}
	agent.setAgentClient(nilPerm)
	if decision, err := agent.handleCodexServerRequest(ctx, codex.ServerRequest{Method: codexReqCommandApproval, Params: json.RawMessage(`{"threadId":"` + session.codexThreadID + `"}`)}); err != nil || decision.(map[string]any)["decision"] != permissionCancel {
		t.Fatalf("nil selected approval = %#v err=%v", decision, err)
	}
	if permissions, err := agent.handleCodexServerRequest(ctx, codex.ServerRequest{Method: codexReqPermissionsApproval, Params: json.RawMessage(`{"threadId":"` + session.codexThreadID + `","permissions":{"fs":true}}`)}); err != nil || permissions.(map[string]any)["scope"] != "turn" {
		t.Fatalf("nil selected permissions = %#v err=%v", permissions, err)
	}
	agent.setAgentClient(nil)
	if decision, err := agent.handleCodexServerRequest(ctx, codex.ServerRequest{Method: codexReqCommandApproval, Params: json.RawMessage(`{"threadId":"` + session.codexThreadID + `"}`)}); err != nil || decision.(map[string]any)["decision"] != permissionCancel {
		t.Fatalf("no client approval = %#v err=%v", decision, err)
	}
	if permissions, err := agent.handleCodexServerRequest(ctx, codex.ServerRequest{Method: codexReqPermissionsApproval, Params: json.RawMessage(`{"threadId":"` + session.codexThreadID + `","permissions":{"fs":true}}`)}); err != nil || permissions.(map[string]any)["scope"] != "turn" {
		t.Fatalf("no client permissions = %#v err=%v", permissions, err)
	}

	acceptConn := &acceptElicitationClient{recordingAgentClient: newRecordingAgentClient()}
	agent.setAgentClient(acceptConn)
	input, err := agent.handleCodexServerRequest(ctx, codex.ServerRequest{Method: codexReqToolUserInput, Params: json.RawMessage(`{"questions":[{"header":"Name","question":"Your name?"},{"question":"skip"}]}`)})
	if err != nil || input.(map[string]any)["answers"] == nil {
		t.Fatalf("accepted tool input = %#v err=%v", input, err)
	}
	mcp, err := agent.handleCodexServerRequest(ctx, codex.ServerRequest{Method: codexReqMCPElicitation, Params: json.RawMessage(`{"mode":"url","url":"https://example.com","message":"Open"}`)})
	if err != nil || mcp.(map[string]any)["action"] != "accept" {
		t.Fatalf("accepted MCP URL = %#v err=%v", mcp, err)
	}
	declineConn := newRecordingAgentClient()
	declineConn.elicitation = acp.NewUnstableCreateElicitationResponseDecline()
	agent.setAgentClient(declineConn)
	mcp, err = agent.handleCodexServerRequest(ctx, codex.ServerRequest{Method: codexReqMCPElicitation, Params: json.RawMessage(`{"requestedSchema":{"title":"T","description":"D","required":["x"],"properties":{"x":{"type":"string"}}}}`)})
	if err != nil || mcp.(map[string]any)["action"] != "decline" {
		t.Fatalf("declined MCP form = %#v err=%v", mcp, err)
	}
	if approvalTitle("other", map[string]any{"reason": "because"}) != "because" {
		t.Fatal("approvalTitle did not prefer reason")
	}
	if id, _, _ := codexDecisionOption(map[string]any{"bad": func() {}}); id != "" {
		t.Fatalf("unmarshalable decision map id = %q", id)
	}
	if id, name, _ := codexDecisionOption(map[string]any{"custom": true}); id == "" || name != "Allow" {
		t.Fatalf("default map decision = %q %q", id, name)
	}
	if got := codexDecisionFromOption(acp.PermissionOptionId(`{"custom":true}`), nil); got.(map[string]any)["custom"] != true {
		t.Fatalf("JSON decision option = %#v", got)
	}
	if got := codexDecisionFromOption(permissionAccept, nil); got != permissionAccept {
		t.Fatalf("accept decision = %#v", got)
	}
	cancelInputConn := newRecordingAgentClient()
	agent.setAgentClient(cancelInputConn)
	input, err = agent.handleCodexServerRequest(ctx, codex.ServerRequest{Method: codexReqToolUserInput, Params: json.RawMessage(`{"questions":[{"id":"answer"}]}`)})
	if err != nil || len(input.(map[string]any)["answers"].(map[string]any)) != 0 {
		t.Fatalf("canceled tool input = %#v err=%v", input, err)
	}
}
