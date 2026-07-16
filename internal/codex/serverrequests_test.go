package codex

import (
	"encoding/json"
	"testing"

	"github.com/coder/acp-go-sdk"
)

func TestServerRequestApprovalHelpers(t *testing.T) {
	if ApprovalTitle(RequestFileChangeApproval, map[string]any{"grantRoot": "/repo"}) != "/repo" {
		t.Fatal("file approval title did not use grant root")
	}
	if ApprovalTitle("other", nil) != "Codex permission request" {
		t.Fatal("default approval title changed")
	}
	if ApprovalTitle(RequestCommandApproval, map[string]any{"command": "ls"}) != "ls" {
		t.Fatal("command approval title failed")
	}
	if ApprovalTitle("other", map[string]any{"reason": "because"}) != "because" {
		t.Fatal("ApprovalTitle did not prefer reason")
	}
	if len(ApprovalContent(RequestFileChangeApproval, map[string]any{"grantRoot": "/repo"})) != 1 {
		t.Fatal("file approval content missing")
	}
	if len(ApprovalContent(RequestCommandApproval, map[string]any{"command": "ls"})) != 1 {
		t.Fatal("command approval content missing")
	}
	if len(ApprovalContent("other", map[string]any{"reason": "because"})) != 1 {
		t.Fatal("reason approval content missing")
	}
	if ApprovalContent("other", nil) != nil {
		t.Fatal("empty approval content should be nil")
	}

	options := ApprovalOptions(map[string]any{"proposedExecpolicyAmendment": []any{"git status"}})
	if len(options) != 5 || options[2].OptionId != "acceptWithExecpolicyAmendment" {
		t.Fatalf("amendment options = %#v", options)
	}
	options = ApprovalOptions(map[string]any{"availableDecisions": []any{"accept", "custom", 7}})
	if len(options) != 2 || options[1].OptionId != "custom" {
		t.Fatalf("available decision options = %#v", options)
	}
	if got := ApprovalCancelResponse(); got[fieldDecision] != decisionCancel {
		t.Fatalf("approval cancel response = %#v", got)
	}
}

func TestServerRequestDecisionHelpers(t *testing.T) {
	id, name, kind := decisionOption(map[string]any{"applyNetworkPolicyAmendment": map[string]any{"allow": true}})
	if id == "" || name != "Apply network policy" || kind != acp.PermissionOptionKindAllowAlways {
		t.Fatalf("network decision option = %q %q %v", id, name, kind)
	}
	if badID, _, _ := decisionOption(func() {}); badID != "" {
		t.Fatalf("unmarshalable decision id = %q", badID)
	}
	if badMapID, _, _ := decisionOption(map[string]any{"bad": func() {}}); badMapID != "" {
		t.Fatalf("unmarshalable decision map id = %q", badMapID)
	}
	if mapID, mapName, _ := decisionOption(map[string]any{"custom": true}); mapID == "" || mapName != "Allow" {
		t.Fatalf("default map decision = %q %q", mapID, mapName)
	}
	for _, decision := range []string{decisionAcceptForSession, decisionDecline, decisionCancel} {
		roundTripped, _, _ := decisionOption(decision)
		if roundTripped != decision {
			t.Fatalf("decision option %q -> %q", decision, roundTripped)
		}
	}
	id, name, kind = decisionOption(map[string]any{"acceptWithExecpolicyAmendment": map[string]any{}})
	if id == "" || name != "Allow and remember" || kind != acp.PermissionOptionKindAllowAlways {
		t.Fatalf("accept amendment decision = %q %q %v", id, name, kind)
	}

	got := decisionFromOption("acceptWithExecpolicyAmendment", map[string]any{"proposedExecpolicyAmendment": []any{"ls"}})
	if amendment, ok := got.(map[string]any); !ok || amendment["acceptWithExecpolicyAmendment"] == nil {
		t.Fatalf("decision from amendment = %#v", got)
	}
	if unknown := decisionFromOption("unknown", nil); unknown != decisionCancel {
		t.Fatalf("unknown decision = %#v", unknown)
	}
	got = decisionFromOption(acp.PermissionOptionId(`{"custom":true}`), nil)
	if custom, ok := got.(map[string]any); !ok || custom["custom"] != true {
		t.Fatalf("JSON decision option = %#v", got)
	}
	if got := decisionFromOption(decisionAccept, nil); got != decisionAccept {
		t.Fatalf("accept decision = %#v", got)
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
		"_meta": map[string]any{metaVendorKey: map[string]any{
			"_meta":      map[string]any{mcpApprovalKindKey: mcpToolApprovalKind, mcpApprovalToolTitleKey: "Execute"},
			"serverName": "remote",
		}},
	}
	if !IsMCPToolApproval(mcpApprovalParams) {
		t.Fatal("MCP tool approval marker was not detected")
	}
	if IsMCPToolApproval(map[string]any{"_meta": map[string]any{metaVendorKey: map[string]any{"_meta": map[string]any{mcpApprovalKindKey: "other"}}}}) {
		t.Fatal("non-approval MCP elicitation was detected as tool approval")
	}
	directMCPApprovalParams := map[string]any{"_meta": map[string]any{metaVendorKey: map[string]any{mcpApprovalKindKey: mcpToolApprovalKind, mcpApprovalToolTitleKey: "Direct"}}}
	if !IsMCPToolApproval(directMCPApprovalParams) {
		t.Fatal("direct MCP tool approval marker was not detected")
	}
	if title := MCPToolApprovalTitle(mcpApprovalParams); title != "Execute" {
		t.Fatalf("MCP tool approval title = %q", title)
	}
	if title := MCPToolApprovalTitle(directMCPApprovalParams); title != "Direct" {
		t.Fatalf("direct MCP tool approval title = %q", title)
	}
	if value := mcpMetaString(map[string]any{"_meta": map[string]any{mcpApprovalToolTitleKey: ""}, mcpApprovalToolTitleKey: "fallback"}, mcpApprovalToolTitleKey); value != "fallback" {
		t.Fatalf("MCP meta fallback value = %q", value)
	}
	choiceProps, _ := schemaFromToolQuestions([]map[string]any{{
		"id":       "choice",
		"question": "Choose",
		"options":  []any{map[string]any{}, map[string]any{"label": "A", "description": "first"}},
		"isSecret": true,
	}})
	choice, ok := choiceProps["choice"].(map[string]any)
	if !ok {
		t.Fatalf("tool question property type = %#v", choiceProps["choice"])
	}
	oneOf, ok := choice["oneOf"].([]map[string]any)
	if !ok || choice["format"] != "password" || len(oneOf) != 1 {
		t.Fatalf("tool question property = %#v", choice)
	}
	if meta := mcpMeta(map[string]any{metaVendorKey: map[string]any{"serverName": "direct"}}); stringValue(meta, "serverName") != "direct" {
		t.Fatalf("direct MCP meta = %#v", meta)
	}
	if meta := mcpMeta(map[string]any{}); meta != nil {
		t.Fatalf("empty MCP meta = %#v", meta)
	}
	if title := MCPToolApprovalTitle(map[string]any{"toolTitle": "Tool title"}); title != "Tool title" {
		t.Fatalf("toolTitle MCP approval title = %q", title)
	}
	if title := MCPToolApprovalTitle(map[string]any{"toolName": "tool_name"}); title != "tool_name" {
		t.Fatalf("toolName MCP approval title = %q", title)
	}
	if title := MCPToolApprovalTitle(map[string]any{"message": "Approve?"}); title != "Approve?" {
		t.Fatalf("message MCP approval title = %q", title)
	}
	if title := MCPToolApprovalTitle(nil); title != "MCP tool call" {
		t.Fatalf("default MCP approval title = %q", title)
	}
	if content := MCPToolApprovalContent(nil); content != nil {
		t.Fatalf("empty MCP tool approval content = %#v", content)
	}
}

func TestServerRequestConversionHelpers(t *testing.T) {
	if action := mcpToolApprovalAction(decisionAcceptForSession); action != "accept" {
		t.Fatalf("accept-for-session MCP action = %q", action)
	}
	if action := mcpToolApprovalAction("other"); action != "cancel" {
		t.Fatalf("unknown MCP action = %q", action)
	}
	if id := ServerRequestID(json.RawMessage(`"req-1"`)); id != "req-1" {
		t.Fatalf("server request string ID = %q", id)
	}
	if id := ServerRequestID(json.RawMessage(`7`)); id != "7" {
		t.Fatalf("server request numeric ID = %q", id)
	}
	if RequestIDFromRaw(json.RawMessage(`{bad}`)) != nil {
		t.Fatal("bad raw request id was parsed")
	}
	if RequestIDFromRaw(nil) != nil {
		t.Fatal("empty raw request id was parsed")
	}
	if RequestIDFromRaw(json.RawMessage(`"req-1"`)) == nil {
		t.Fatal("valid raw request id was not parsed")
	}
	if params := ServerRequestParams(ServerRequest{Params: json.RawMessage(`{"threadId":"t1"}`)}); RequestThreadID(params) != "t1" {
		t.Fatalf("server request params = %#v", params)
	}
	if ServerRequestParams(ServerRequest{}) != nil {
		t.Fatal("empty server request params decoded to a map")
	}
	key := ServerInteractionKey(ServerRequest{Method: RequestCommandApproval}, map[string]any{"approvalId": "a1"})
	if key != RequestCommandApproval+":a1" {
		t.Fatalf("server interaction key = %q", key)
	}
}

func TestServerRequestResponseBuilders(t *testing.T) {
	req := ServerRequest{ID: json.RawMessage(`"req-9"`), Method: RequestCommandApproval}

	if got := ApprovalToolCallID(req, map[string]any{"approvalId": "a1", "itemId": "i1"}); got != "a1" {
		t.Fatalf("approval tool-call id = %q", got)
	}
	if got := ApprovalToolCallID(req, map[string]any{"itemId": "i1"}); got != "i1" {
		t.Fatalf("approval tool-call id fallback = %q", got)
	}
	if got := ApprovalToolCallID(req, nil); got != RequestCommandApproval {
		t.Fatalf("approval tool-call id method fallback = %q", got)
	}
	if got := ApprovalDecisionResponse("accept", nil); got[fieldDecision] != "accept" {
		t.Fatalf("approval decision response = %#v", got)
	}

	if got := PermissionsToolCallID(ServerRequest{Method: RequestPermissionsApproval}, map[string]any{"itemId": "p1"}); got != "p1" {
		t.Fatalf("permissions tool-call id = %q", got)
	}
	if got := PermissionsApprovalTitle(map[string]any{"reason": "need fs"}); got != "need fs" {
		t.Fatalf("permissions title = %q", got)
	}
	if got := PermissionsApprovalTitle(nil); got != defaultPermissionTitle {
		t.Fatalf("default permissions title = %q", got)
	}
	if got := PermissionsApprovalContent(map[string]any{"permissions": map[string]any{"fs": true}}); len(got) != 1 {
		t.Fatalf("permissions content = %#v", got)
	}
	options := PermissionsApprovalOptions()
	if len(options) != 3 || options[0].OptionId != "grant-turn" || options[1].OptionId != "grant-session" || options[2].OptionId != "decline" {
		t.Fatalf("permissions options = %#v", options)
	}
	if got := PermissionsDeniedResponse(); got[fieldScope] != scopeTurn {
		t.Fatalf("denied permissions response = %#v", got)
	}
	declinedPermissions := PermissionsApprovalResponse("decline", nil)
	if declinedProfile, ok := declinedPermissions[fieldPermissions].(map[string]any); !ok || declinedPermissions[fieldScope] != scopeTurn || len(declinedProfile) != 0 {
		t.Fatalf("declined permissions response = %#v", declinedPermissions)
	}
	granted := PermissionsApprovalResponse("grant-session", map[string]any{"permissions": map[string]any{"fs": true}})
	if granted[fieldScope] != scopeSession || granted[fieldPermissions] == nil {
		t.Fatalf("granted permissions response = %#v", granted)
	}
	if got := PermissionsApprovalResponse("grant-turn", nil); got[fieldScope] != scopeTurn {
		t.Fatalf("turn permissions response = %#v", got)
	}
}

func TestToolUserInputBuilders(t *testing.T) {
	req := ServerRequest{ID: json.RawMessage(`"input-9"`), Method: RequestToolUserInput}

	if got := ToolUserInputToolCallID(req, map[string]any{"itemId": "i1"}); got != "i1" {
		t.Fatalf("tool input id = %q", got)
	}
	if got := ToolUserInputToolCallID(req, nil); got != "input-9" {
		t.Fatalf("tool input id request fallback = %q", got)
	}
	if got := ToolUserInputToolCallID(ServerRequest{Method: RequestToolUserInput}, nil); got != RequestToolUserInput {
		t.Fatalf("tool input id method fallback = %q", got)
	}

	form := ToolUserInputForm(map[string]any{"questions": []any{map[string]any{"id": "name", "question": "Name?"}}}, map[string]any{"m": true})
	if form == nil || form.Message != "Name?" || form.Mode != modeForm || form.RequestedSchema.Properties["name"] == nil {
		t.Fatalf("tool input form = %#v", form)
	}

	multi := ToolUserInputForm(map[string]any{"questions": []any{
		map[string]any{"id": "a", "question": "A?"},
		map[string]any{"question": "skipped"},
		map[string]any{"id": "opts", "options": "bad"},
	}}, nil)
	if multi.Message != defaultToolUserInputMessage || len(multi.RequestedSchema.Required) != 2 {
		t.Fatalf("multi-question form = %#v", multi)
	}

	if got := toolUserInputMessage([]map[string]any{{"header": "no question"}}); got != defaultToolUserInputMessage {
		t.Fatalf("question-less message = %q", got)
	}

	empty := EmptyToolUserInputResponse()
	if emptyAnswers, ok := empty[fieldAnswers].(map[string]any); !ok || len(emptyAnswers) != 0 {
		t.Fatalf("empty tool input response = %#v", empty)
	}
	answers := ToolUserInputResponse(map[string]any{"name": "value", "multi": []any{"a", 2}, "preset": []string{"x"}, "empty": nil})
	answerMap, ok := answers[fieldAnswers].(map[string]any)
	if !ok || len(answerMap) != 4 {
		t.Fatalf("tool input answers = %#v", answers)
	}
	for key, want := range map[string]int{"multi": 2, "preset": 1, "empty": 0} {
		entry, entryOK := answerMap[key].(map[string]any)
		if !entryOK {
			t.Fatalf("tool input answer %q = %#v", key, answerMap[key])
		}
		values, _ := entry[fieldAnswers].([]string)
		if len(values) != want {
			t.Fatalf("tool input answer %q = %#v, want %d values", key, entry[fieldAnswers], want)
		}
	}
}

func TestMCPElicitationBuilders(t *testing.T) {
	req := ServerRequest{ID: json.RawMessage(`"mcp-9"`), Method: RequestMCPElicitation}

	if got := MCPToolApprovalToolCallID(req, map[string]any{"elicitationId": "e1", "toolCallId": "t1"}); got != "e1" {
		t.Fatalf("mcp approval id = %q", got)
	}
	if got := MCPToolApprovalToolCallID(req, map[string]any{"toolCallId": "t1"}); got != "t1" {
		t.Fatalf("mcp approval id toolCallId = %q", got)
	}
	if got := MCPToolApprovalToolCallID(ServerRequest{Method: RequestMCPElicitation}, nil); got != RequestMCPElicitation {
		t.Fatalf("mcp approval id fallback = %q", got)
	}

	content := MCPToolApprovalContent(map[string]any{
		"message":    "Approve?",
		"toolParams": map[string]any{"code": "x"},
		"_meta":      map[string]any{metaVendorKey: map[string]any{"serverName": "remote"}},
	})
	if len(content) != 1 {
		t.Fatalf("mcp approval content = %#v", content)
	}
	if got := MCPToolApprovalContent(map[string]any{"tool_params": map[string]any{"bad": func() {}}}); got != nil {
		t.Fatalf("unserializable tool params content = %#v", got)
	}

	approvalOptions := MCPToolApprovalOptions()
	if len(approvalOptions) != 2 || approvalOptions[0].OptionId != "accept" || approvalOptions[1].OptionId != "decline" {
		t.Fatalf("mcp approval options = %#v", approvalOptions)
	}
	if got := MCPToolApprovalResponse("accept"); got[fieldAction] != "accept" {
		t.Fatalf("mcp approval response = %#v", got)
	}
	if got := mcpToolApprovalAction("decline"); got != "decline" {
		t.Fatalf("mcp decline action = %q", got)
	}
}

func TestMCPElicitationRequestBuilders(t *testing.T) {
	if got := ElicitationCancelResponse(); got[fieldAction] != "cancel" {
		t.Fatalf("cancel response = %#v", got)
	}
	if got := ElicitationDeclineResponse(); got[fieldAction] != "decline" {
		t.Fatalf("decline response = %#v", got)
	}
	if got := ElicitationAcceptResponse(map[string]any{"a": 1}, map[string]any{"m": true}); got[fieldAction] != "accept" || got[fieldContent] == nil || got[fieldMetaObject] == nil {
		t.Fatalf("accept response = %#v", got)
	}
	if got := ElicitationDeclineMetaResponse(map[string]any{"m": true}); got[fieldAction] != "decline" || got[fieldMetaObject] == nil {
		t.Fatalf("decline meta response = %#v", got)
	}

	if !IsURLElicitation(map[string]any{"mode": "url"}) || IsURLElicitation(map[string]any{"mode": "form"}) {
		t.Fatal("IsURLElicitation mode detection failed")
	}
	urlReq := MCPElicitationRequest(map[string]any{"mode": "url", "url": "https://example.com", "elicitationId": "e1"}, map[string]any{"m": true})
	if urlReq.Url == nil || urlReq.Url.Url != "https://example.com" || urlReq.Url.Message != "MCP server needs input" {
		t.Fatalf("url elicitation request = %#v", urlReq)
	}
	formReq := MCPElicitationRequest(map[string]any{
		"message": "Need input",
		"requestedSchema": map[string]any{
			"title":       "T",
			"description": "D",
			"required":    []any{"token", ""},
			"properties":  map[string]any{"token": map[string]any{"type": "string"}},
		},
	}, nil)
	if formReq.Form == nil || formReq.Form.Message != "Need input" || formReq.Form.RequestedSchema.Title == nil || len(formReq.Form.RequestedSchema.Required) != 1 {
		t.Fatalf("form elicitation request = %#v", formReq)
	}
	bareForm := MCPElicitationRequest(nil, nil)
	if bareForm.Form == nil || len(bareForm.Form.RequestedSchema.Properties) != 0 {
		t.Fatalf("bare form elicitation request = %#v", bareForm)
	}

	if text := permissionProfileText(map[string]any{"fs": true}); text == "" {
		t.Fatalf("permission profile text = %q", text)
	}
	if answers := stringAnswersFromAny(nil); answers != nil {
		t.Fatalf("nil answers = %#v", answers)
	}
	if answers := stringAnswersFromAny("solo"); len(answers) != 1 || answers[0] != "solo" {
		t.Fatalf("scalar answers = %#v", answers)
	}
}

func TestMCPUserElicitationCorrelationBuilders(t *testing.T) {
	req := ServerRequest{ID: json.RawMessage(`"mcp-9"`), Method: RequestMCPElicitation}

	if got := MCPUserElicitationToolCallID(map[string]any{"toolCallId": "t1", "itemId": "i1"}); got != "t1" {
		t.Fatalf("mcp elicitation id = %q", got)
	}
	if got := MCPUserElicitationToolCallID(map[string]any{"itemId": "i1"}); got != "i1" {
		t.Fatalf("mcp elicitation id itemId = %q", got)
	}
	if got := MCPUserElicitationToolCallID(nil); got != "" {
		t.Fatalf("unassociated mcp elicitation tool id = %q", got)
	}
	if got := MCPUserElicitationRequestID(req, nil); got == nil || got.Str == nil || *got.Str != "mcp-9" {
		t.Fatalf("mcp elicitation request id = %#v", got)
	}
	if got := MCPUserElicitationRequestID(ServerRequest{}, map[string]any{"elicitationId": "e1"}); got == nil || got.Str == nil || *got.Str != "e1" {
		t.Fatalf("mcp elicitation fallback request id = %#v", got)
	}
	if got := MCPUserElicitationRequestID(ServerRequest{}, nil); got != nil {
		t.Fatalf("empty mcp elicitation request id = %#v", got)
	}
}

func TestAuthTokensRefreshResponse(t *testing.T) {
	full := AuthTokensRefreshResponse(ChatGPTAuthTokens{
		AccessToken:      "access",
		RefreshToken:     "refresh",
		AccountID:        "acct",
		PlanType:         "pro",
		ExpiresAtUnixSec: 456,
	})
	if full["accessToken"] != "access" || full["refreshToken"] != "refresh" || full["expiresAt"] != int64(456) {
		t.Fatalf("full refresh response = %#v", full)
	}

	minimal := AuthTokensRefreshResponse(ChatGPTAuthTokens{AccessToken: "access"})
	if _, ok := minimal["refreshToken"]; ok {
		t.Fatalf("minimal refresh response carries refreshToken: %#v", minimal)
	}
	if _, ok := minimal["expiresAt"]; ok {
		t.Fatalf("minimal refresh response carries expiresAt: %#v", minimal)
	}
}
