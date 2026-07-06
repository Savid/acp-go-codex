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
}

func TestServerRequestSchemaAndMCPHelpers(t *testing.T) {
	props, required := schemaFromToolQuestions(nil)
	if props["answer"] == nil || len(required) != 1 {
		t.Fatalf("default tool schema props=%#v required=%#v", props, required)
	}
	mcpApprovalParams := map[string]any{
		"_meta": map[string]any{codexMetaKey: map[string]any{
			"_meta":      map[string]any{codexMCPApprovalKindKey: codexMCPToolApprovalKind, codexMCPApprovalToolTitleKey: "Execute"},
			"serverName": "remote",
		}},
	}
	if !isCodexMCPToolApproval(mcpApprovalParams) {
		t.Fatal("MCP tool approval marker was not detected")
	}
	if isCodexMCPToolApproval(map[string]any{"_meta": map[string]any{codexMetaKey: map[string]any{"_meta": map[string]any{codexMCPApprovalKindKey: "other"}}}}) {
		t.Fatal("non-approval MCP elicitation was detected as tool approval")
	}
	directMCPApprovalParams := map[string]any{"_meta": map[string]any{codexMetaKey: map[string]any{codexMCPApprovalKindKey: codexMCPToolApprovalKind, codexMCPApprovalToolTitleKey: "Direct"}}}
	if !isCodexMCPToolApproval(directMCPApprovalParams) {
		t.Fatal("direct MCP tool approval marker was not detected")
	}
	if title := mcpToolApprovalTitle(nil, codexMCPMeta(mcpApprovalParams)); title != "Execute" {
		t.Fatalf("MCP tool approval title = %q", title)
	}
	if title := mcpToolApprovalTitle(nil, codexMCPMeta(directMCPApprovalParams)); title != "Direct" {
		t.Fatalf("direct MCP tool approval title = %q", title)
	}
	if value := codexMCPMetaString(map[string]any{"_meta": map[string]any{codexMCPApprovalToolTitleKey: ""}, codexMCPApprovalToolTitleKey: "fallback"}, codexMCPApprovalToolTitleKey); value != "fallback" {
		t.Fatalf("MCP meta fallback value = %q", value)
	}
	choiceProps, _ := schemaFromToolQuestions([]map[string]any{{
		"id":       "choice",
		"question": "Choose",
		"options":  []any{map[string]any{}, map[string]any{"label": "A", "description": "first"}},
		"isSecret": true,
	}})
	choice := asType[map[string]any](t, choiceProps["choice"])
	if choice["format"] != "password" || len(asType[[]map[string]any](t, choice["oneOf"])) != 1 {
		t.Fatalf("tool question property = %#v", choice)
	}
	if meta := codexMCPMeta(map[string]any{codexMetaKey: map[string]any{"serverName": "direct"}}); stringFromAny(meta["serverName"]) != "direct" {
		t.Fatalf("direct MCP meta = %#v", meta)
	}
	if meta := codexMCPMeta(map[string]any{}); meta != nil {
		t.Fatalf("empty MCP meta = %#v", meta)
	}
	if title := mcpToolApprovalTitle(map[string]any{"toolTitle": "Tool title"}, nil); title != "Tool title" {
		t.Fatalf("toolTitle MCP approval title = %q", title)
	}
	if title := mcpToolApprovalTitle(map[string]any{"toolName": "tool_name"}, nil); title != "tool_name" {
		t.Fatalf("toolName MCP approval title = %q", title)
	}
	if title := mcpToolApprovalTitle(map[string]any{"message": "Approve?"}, nil); title != "Approve?" {
		t.Fatalf("message MCP approval title = %q", title)
	}
	if title := mcpToolApprovalTitle(nil, nil); title != "MCP tool call" {
		t.Fatalf("default MCP approval title = %q", title)
	}
	if content := mcpToolApprovalContent(nil, nil); content != nil {
		t.Fatalf("empty MCP tool approval content = %#v", content)
	}
}

func TestServerRequestConversionHelpers(t *testing.T) {
	if action := mcpToolApprovalAction(permissionAcceptForSession); action != "accept" {
		t.Fatalf("accept-for-session MCP action = %q", action)
	}
	if action := mcpToolApprovalAction("other"); action != "cancel" {
		t.Fatalf("unknown MCP action = %q", action)
	}
	if id := serverRequestID(json.RawMessage(`"req-1"`)); id != "req-1" {
		t.Fatalf("server request string ID = %q", id)
	}
	if id := serverRequestID(json.RawMessage(`7`)); id != "7" {
		t.Fatalf("server request numeric ID = %q", id)
	}
	if requestIDFromRaw(json.RawMessage(`{bad}`)) != nil {
		t.Fatal("bad raw request id was parsed")
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
	if err != nil || asType[map[string]any](t, cancelResp)["decision"] != permissionCancel {
		t.Fatalf("approval without session = %#v err=%v", cancelResp, err)
	}
	permissions, err := agent.handleCodexServerRequest(ctx, codex.ServerRequest{
		Method: codexReqPermissionsApproval,
		Params: json.RawMessage(`{"threadId":"missing","permissions":{"network":true}}`),
	})
	if err != nil || asType[map[string]any](t, permissions)["scope"] != "turn" {
		t.Fatalf("permissions without session = %#v err=%v", permissions, err)
	}
	input, err := agent.handleCodexServerRequest(ctx, codex.ServerRequest{
		Method: codexReqToolUserInput,
		Params: json.RawMessage(`{}`),
	})
	if err != nil || asType[map[string]any](t, input)["answers"] == nil {
		t.Fatalf("tool input without client = %#v err=%v", input, err)
	}
	mcp, err := agent.handleCodexServerRequest(ctx, codex.ServerRequest{
		Method: codexReqMCPElicitation,
		Params: json.RawMessage(`{}`),
	})
	if err != nil || asType[map[string]any](t, mcp)["action"] != "cancel" {
		t.Fatalf("mcp without client = %#v err=%v", mcp, err)
	}
	mcpApproval, err := agent.handleCodexServerRequest(ctx, codex.ServerRequest{
		Method: codexReqMCPElicitation,
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
		Method: codexReqFileChangeApproval,
		Params: json.RawMessage(`{"threadId":"` + session.codexThreadID + `","grantRoot":"/repo"}`),
	}); fileErr == nil {
		t.Fatal("file approval with permission error succeeded")
	}
	if _, permErr := agent.handleCodexServerRequest(ctx, codex.ServerRequest{
		Method: codexReqPermissionsApproval,
		Params: json.RawMessage(`{"threadId":"` + session.codexThreadID + `","permissions":{"fs":true}}`),
	}); permErr == nil {
		t.Fatal("permissions approval with permission error succeeded")
	}
	enableClientElicitation(agent, true, false)
	if _, inputErr := agent.handleCodexServerRequest(ctx, codex.ServerRequest{
		Method: codexReqToolUserInput,
		Params: json.RawMessage(`{"threadId":"` + session.codexThreadID + `","questions":[{"id":"name"}]}`),
	}); inputErr == nil {
		t.Fatal("tool input with elicitation error succeeded")
	}
	if _, mcpErr := agent.handleCodexServerRequest(ctx, codex.ServerRequest{
		Method: codexReqMCPElicitation,
		Params: json.RawMessage(`{"mode":"form"}`),
	}); mcpErr == nil {
		t.Fatal("MCP elicitation with elicitation error succeeded")
	}
	if _, mcpToolErr := agent.handleCodexServerRequest(ctx, codex.ServerRequest{
		Method: codexReqMCPElicitation,
		Params: json.RawMessage(`{"threadId":"` + session.codexThreadID + `","_meta":{"codex":{"_meta":{"codex_approval_kind":"mcp_tool_call"}}}}`),
	}); mcpToolErr == nil {
		t.Fatal("MCP tool approval with permission error succeeded")
	}

	declineConn := newRecordingAgentClient()
	declineConn.permission = acp.PermissionOptionId(permissionDecline)
	declineConn.elicitation = acp.NewUnstableCreateElicitationResponseCancel()
	agent.setAgentClient(declineConn)
	permissions, err := agent.handleCodexServerRequest(ctx, codex.ServerRequest{
		Method: codexReqPermissionsApproval,
		Params: json.RawMessage(`{"threadId":"` + session.codexThreadID + `","permissions":{"fs":true}}`),
	})
	if err != nil || asType[map[string]any](t, permissions)["scope"] != "turn" {
		t.Fatalf("declined permissions = %#v err=%v", permissions, err)
	}
	mcp, err := agent.handleCodexServerRequest(ctx, codex.ServerRequest{
		Method: codexReqMCPElicitation,
		Params: json.RawMessage(`{"mode":"form"}`),
	})
	if err != nil || asType[map[string]any](t, mcp)["action"] != "cancel" {
		t.Fatalf("canceled MCP elicitation = %#v err=%v", mcp, err)
	}

	refreshAgent := NewAgent(WithCodexChatGPTAuthTokenRefresher(func(context.Context) (ChatGPTAuthTokens, error) {
		return ChatGPTAuthTokens{}, errors.New("refresh failed")
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
	enableClientElicitation(agent, true, true)

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
		ID:     json.RawMessage(`"mcp-form-1"`),
		Method: codexReqMCPElicitation,
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
		Method: codexReqMCPElicitation,
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
	if len(permission.Options) != 2 || permission.Options[0].OptionId != permissionAccept || permission.Options[1].OptionId != permissionDecline {
		t.Fatalf("MCP tool approval options = %#v", permission.Options)
	}
	if len(permission.ToolCall.Content) == 0 || permission.ToolCall.RawInput == nil || permission.ToolCall.Meta[codexMetaKey] == nil {
		t.Fatalf("MCP tool approval content/meta missing: %#v", permission.ToolCall)
	}

	conn.permission = permissionDecline
	declined, err := agent.handleCodexServerRequest(ctx, codex.ServerRequest{
		Method: codexReqMCPElicitation,
		Params: json.RawMessage(`{"threadId":"` + session.codexThreadID + `","_meta":{"codex":{"_meta":{"codex_approval_kind":"mcp_tool_call"}}}}`),
	})
	if err != nil || asType[map[string]any](t, declined)["action"] != "decline" {
		t.Fatalf("declined MCP tool approval = %#v err=%v", declined, err)
	}

	nilPerm := &nilPermissionClient{recordingAgentClient: newRecordingAgentClient()}
	agent.setAgentClient(nilPerm)
	canceled, err := agent.handleCodexServerRequest(ctx, codex.ServerRequest{
		Method: codexReqMCPElicitation,
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
	if decision, err := agent.handleCodexServerRequest(ctx, codex.ServerRequest{Method: codexReqCommandApproval, Params: json.RawMessage(`{"threadId":"` + session.codexThreadID + `"}`)}); err != nil || asType[map[string]any](t, decision)["decision"] != permissionCancel {
		t.Fatalf("nil selected approval = %#v err=%v", decision, err)
	}
	if permissions, err := agent.handleCodexServerRequest(ctx, codex.ServerRequest{Method: codexReqPermissionsApproval, Params: json.RawMessage(`{"threadId":"` + session.codexThreadID + `","permissions":{"fs":true}}`)}); err != nil || asType[map[string]any](t, permissions)["scope"] != "turn" {
		t.Fatalf("nil selected permissions = %#v err=%v", permissions, err)
	}
	agent.setAgentClient(nil)
	if decision, err := agent.handleCodexServerRequest(ctx, codex.ServerRequest{Method: codexReqCommandApproval, Params: json.RawMessage(`{"threadId":"` + session.codexThreadID + `"}`)}); err != nil || asType[map[string]any](t, decision)["decision"] != permissionCancel {
		t.Fatalf("no client approval = %#v err=%v", decision, err)
	}
	if permissions, err := agent.handleCodexServerRequest(ctx, codex.ServerRequest{Method: codexReqPermissionsApproval, Params: json.RawMessage(`{"threadId":"` + session.codexThreadID + `","permissions":{"fs":true}}`)}); err != nil || asType[map[string]any](t, permissions)["scope"] != "turn" {
		t.Fatalf("no client permissions = %#v err=%v", permissions, err)
	}
	if mcpApproval, err := agent.handleCodexServerRequest(ctx, codex.ServerRequest{Method: codexReqMCPElicitation, Params: json.RawMessage(`{"threadId":"` + session.codexThreadID + `","_meta":{"codex":{"_meta":{"codex_approval_kind":"mcp_tool_call"}}}}`)}); err != nil || asType[map[string]any](t, mcpApproval)["action"] != "cancel" {
		t.Fatalf("no client MCP approval = %#v err=%v", mcpApproval, err)
	}
	if input, err := agent.handleCodexServerRequest(ctx, codex.ServerRequest{Method: codexReqToolUserInput, Params: json.RawMessage(`{"threadId":"` + session.codexThreadID + `","questions":[{"id":"answer"}]}`)}); err != nil || len(asType[map[string]any](t, asType[map[string]any](t, input)["answers"])) != 0 {
		t.Fatalf("no client tool input = %#v err=%v", input, err)
	}
}

func TestServerRequestElicitationCapabilityBranches(t *testing.T) {
	agent, session, ctx := newServerRequestSession(t)

	noCapConn := newRecordingAgentClient()
	agent.setAgentClient(noCapConn)
	input, err := agent.handleCodexServerRequest(ctx, codex.ServerRequest{Method: codexReqToolUserInput, Params: json.RawMessage(`{"threadId":"` + session.codexThreadID + `","questions":[{"id":"answer"}]}`)})
	if err != nil || len(asType[map[string]any](t, asType[map[string]any](t, input)["answers"])) != 0 || len(noCapConn.elicitations) != 0 {
		t.Fatalf("tool input without form capability = %#v err=%v elicitations=%#v", input, err, noCapConn.elicitations)
	}
	mcpNoCap, err := agent.handleCodexServerRequest(ctx, codex.ServerRequest{Method: codexReqMCPElicitation, Params: json.RawMessage(`{"threadId":"` + session.codexThreadID + `","mode":"form"}`)})
	if err != nil || asType[map[string]any](t, mcpNoCap)["action"] != "decline" || len(noCapConn.elicitations) != 0 {
		t.Fatalf("MCP form without capability = %#v err=%v elicitations=%#v", mcpNoCap, err, noCapConn.elicitations)
	}
	mcpURLNoCap, err := agent.handleCodexServerRequest(ctx, codex.ServerRequest{Method: codexReqMCPElicitation, Params: json.RawMessage(`{"threadId":"` + session.codexThreadID + `","mode":"url","url":"https://example.com"}`)})
	if err != nil || asType[map[string]any](t, mcpURLNoCap)["action"] != "decline" || len(noCapConn.elicitations) != 0 {
		t.Fatalf("MCP URL without capability = %#v err=%v elicitations=%#v", mcpURLNoCap, err, noCapConn.elicitations)
	}

	acceptConn := &acceptElicitationClient{recordingAgentClient: newRecordingAgentClient()}
	agent.setAgentClient(acceptConn)
	enableClientElicitation(agent, true, true)
	input, err = agent.handleCodexServerRequest(ctx, codex.ServerRequest{Method: codexReqToolUserInput, Params: json.RawMessage(`{"threadId":"` + session.codexThreadID + `","questions":[{"header":"Name","question":"Your name?"},{"question":"skip"}]}`)})
	if err != nil || asType[map[string]any](t, input)["answers"] == nil {
		t.Fatalf("accepted tool input = %#v err=%v", input, err)
	}
	mcp, err := agent.handleCodexServerRequest(ctx, codex.ServerRequest{Method: codexReqMCPElicitation, Params: json.RawMessage(`{"mode":"url","url":"https://example.com","message":"Open"}`)})
	if err != nil || asType[map[string]any](t, mcp)["action"] != "accept" {
		t.Fatalf("accepted MCP URL = %#v err=%v", mcp, err)
	}
	declineConn := newRecordingAgentClient()
	declineConn.elicitation = acp.NewUnstableCreateElicitationResponseDecline()
	agent.setAgentClient(declineConn)
	mcp, err = agent.handleCodexServerRequest(ctx, codex.ServerRequest{Method: codexReqMCPElicitation, Params: json.RawMessage(`{"requestedSchema":{"title":"T","description":"D","required":["x"],"properties":{"x":{"type":"string"}}}}`)})
	if err != nil || asType[map[string]any](t, mcp)["action"] != "decline" {
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
	if got := codexDecisionFromOption(acp.PermissionOptionId(`{"custom":true}`), nil); asType[map[string]any](t, got)["custom"] != true {
		t.Fatalf("JSON decision option = %#v", got)
	}
	if got := codexDecisionFromOption(permissionAccept, nil); got != permissionAccept {
		t.Fatalf("accept decision = %#v", got)
	}
	cancelInputConn := newRecordingAgentClient()
	agent.setAgentClient(cancelInputConn)
	input, err = agent.handleCodexServerRequest(ctx, codex.ServerRequest{Method: codexReqToolUserInput, Params: json.RawMessage(`{"threadId":"` + session.codexThreadID + `","questions":[{"id":"answer"}]}`)})
	if err != nil || len(asType[map[string]any](t, asType[map[string]any](t, input)["answers"])) != 0 {
		t.Fatalf("canceled tool input = %#v err=%v", input, err)
	}
}
