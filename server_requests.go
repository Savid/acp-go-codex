package codexacp

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/coder/acp-go-sdk"
	"github.com/savid/acp-go-codex/internal/codex"
)

const (
	codexReqCommandApproval     = "item/commandExecution/requestApproval"
	codexReqFileChangeApproval  = "item/fileChange/requestApproval"
	codexReqPermissionsApproval = "item/permissions/requestApproval"
	codexReqToolUserInput       = "item/tool/requestUserInput"
	codexReqMCPElicitation      = "mcpServer/elicitation/request"
	codexReqAuthTokenRefresh    = "account/chatgptAuthTokens/refresh" // #nosec G101 -- app-server method name, not a token.

	permissionAccept           = "accept"
	permissionAcceptForSession = "acceptForSession"
	permissionDecline          = "decline"
	permissionCancel           = "cancel"

	codexMCPApprovalKindKey      = "codex_approval_kind"
	codexMCPToolApprovalKind     = "mcp_tool_call"
	codexMCPApprovalToolTitleKey = "tool_title"
)

func (a *Agent) handleCodexServerRequest(ctx context.Context, req codex.ServerRequest) (any, error) {
	params := mapFromRaw(req.Params)
	if session := a.sessionByCodexThread(stringFromAny(params["threadId"])); session != nil {
		var finish func()
		ctx, finish = session.beginInteraction(ctx, codexServerInteractionKey(req, params))
		defer finish()
	}

	var result any
	var err error
	switch req.Method {
	case codexReqCommandApproval:
		result, err = a.handleCodexApproval(ctx, req, acp.ToolKindExecute)
	case codexReqFileChangeApproval:
		result, err = a.handleCodexApproval(ctx, req, acp.ToolKindEdit)
	case codexReqPermissionsApproval:
		result, err = a.handleCodexPermissionsApproval(ctx, req)
	case codexReqToolUserInput:
		result, err = a.handleCodexToolUserInput(ctx, req)
	case codexReqMCPElicitation:
		result, err = a.handleCodexMCPElicitation(ctx, req)
	case codexReqAuthTokenRefresh:
		result, err = a.handleCodexAuthTokenRefresh(ctx)
	default:
		return nil, fmt.Errorf("unsupported Codex server request %q", req.Method)
	}
	if err == nil {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return nil, ctxErr
		}
	}

	return result, err
}

func (a *Agent) handleCodexAuthTokenRefresh(ctx context.Context) (any, error) {
	if a.options.ChatGPTAuthTokenRefresher == nil {
		return nil, fmt.Errorf("external ChatGPT token refresh callback is not configured")
	}
	tokens, err := a.options.ChatGPTAuthTokenRefresher(ctx)
	if err != nil {
		return nil, err
	}
	a.setExternalAuthTokens(tokens)

	resp := map[string]any{
		"accessToken":      tokens.AccessToken,
		"chatgptAccountId": tokens.AccountID,
		"chatgptPlanType":  tokens.PlanType,
	}
	if tokens.RefreshToken != "" {
		resp["refreshToken"] = tokens.RefreshToken
	}
	if tokens.ExpiresAtUnixSec != 0 {
		resp["expiresAt"] = tokens.ExpiresAtUnixSec
	}

	return resp, nil
}

func (a *Agent) handleCodexApproval(ctx context.Context, req codex.ServerRequest, kind acp.ToolKind) (any, error) {
	params := mapFromRaw(req.Params)
	session := a.sessionByCodexThread(stringFromAny(params["threadId"]))
	if session == nil {
		return map[string]any{"decision": permissionCancel}, nil
	}

	conn := a.connection()
	if conn == nil {
		return map[string]any{"decision": permissionCancel}, nil
	}

	toolID := firstNonEmpty(stringFromAny(params["approvalId"]), stringFromAny(params["itemId"]), req.Method)
	title := approvalTitle(req.Method, params)
	status := acp.ToolCallStatusPending

	resp, err := conn.RequestPermission(ctx, acp.RequestPermissionRequest{
		SessionId: session.id,
		ToolCall: acp.ToolCallUpdate{
			ToolCallId: acp.ToolCallId(toolID),
			Title:      &title,
			Kind:       &kind,
			Status:     &status,
			Content:    approvalContent(req.Method, params),
			RawInput:   params,
			Meta:       map[string]any{codexMetaKey: params},
		},
		Options: codexApprovalOptions(params),
	})
	if err != nil {
		return nil, err
	}
	if resp.Outcome.Selected == nil {
		return map[string]any{"decision": permissionCancel}, nil
	}

	return map[string]any{"decision": codexDecisionFromOption(resp.Outcome.Selected.OptionId, params)}, nil
}

func (a *Agent) handleCodexPermissionsApproval(ctx context.Context, req codex.ServerRequest) (any, error) {
	params := mapFromRaw(req.Params)
	session := a.sessionByCodexThread(stringFromAny(params["threadId"]))
	if session == nil {
		return map[string]any{"permissions": map[string]any{}, "scope": "turn"}, nil
	}

	conn := a.connection()
	if conn == nil {
		return map[string]any{"permissions": map[string]any{}, "scope": "turn"}, nil
	}

	kind := acp.ToolKindOther
	status := acp.ToolCallStatusPending
	title := firstNonEmpty(stringFromAny(params["reason"]), "Codex permission request")
	toolID := firstNonEmpty(stringFromAny(params["itemId"]), req.Method)

	resp, err := conn.RequestPermission(ctx, acp.RequestPermissionRequest{
		SessionId: session.id,
		ToolCall: acp.ToolCallUpdate{
			ToolCallId: acp.ToolCallId(toolID),
			Title:      &title,
			Kind:       &kind,
			Status:     &status,
			Content:    []acp.ToolCallContent{textToolContent(permissionProfileText(params["permissions"]))},
			RawInput:   params,
			Meta:       map[string]any{codexMetaKey: params},
		},
		Options: []acp.PermissionOption{
			{OptionId: "grant-turn", Name: "Allow for this turn", Kind: acp.PermissionOptionKindAllowOnce},
			{OptionId: "grant-session", Name: "Allow for this session", Kind: acp.PermissionOptionKindAllowAlways},
			{OptionId: "decline", Name: "Reject", Kind: acp.PermissionOptionKindRejectOnce},
		},
	})
	if err != nil {
		return nil, err
	}
	if resp.Outcome.Selected == nil || resp.Outcome.Selected.OptionId == "decline" {
		return map[string]any{"permissions": map[string]any{}, "scope": "turn"}, nil
	}

	scope := "turn"
	if resp.Outcome.Selected.OptionId == "grant-session" {
		scope = "session"
	}

	return map[string]any{
		"permissions": params["permissions"],
		"scope":       scope,
	}, nil
}

func (a *Agent) handleCodexToolUserInput(ctx context.Context, req codex.ServerRequest) (any, error) {
	params := mapFromRaw(req.Params)
	session := a.sessionByCodexThread(stringFromAny(params["threadId"]))
	if session == nil {
		return map[string]any{"answers": map[string]any{}}, nil
	}

	conn := a.connection()
	if conn == nil {
		return map[string]any{"answers": map[string]any{}}, nil
	}
	if !a.clientSupportsFormElicitation() {
		return map[string]any{"answers": map[string]any{}}, nil
	}

	questions := sliceOfMaps(params["questions"])
	schema, required := schemaFromToolQuestions(questions)
	resp, err := conn.CreateElicitation(ctx, acp.UnstableCreateElicitationRequest{
		Form: &acp.UnstableCreateElicitationForm{
			Message: toolUserInputMessage(questions),
			Mode:    "form",
			RequestedSchema: acp.UnstableElicitationSchema{
				Title:      acp.Ptr("Codex input"),
				Type:       acp.UnstableElicitationSchemaTypeObject,
				Properties: schema,
				Required:   required,
			},
			Meta: map[string]any{codexMetaKey: params},
		},
	}, elicitationScope{
		SessionID:  session.id,
		ToolCallID: acp.ToolCallId(firstNonEmpty(stringFromAny(params["itemId"]), serverRequestID(req.ID), codexReqToolUserInput)),
	})
	if err != nil {
		return nil, err
	}
	if resp.Accept == nil {
		return map[string]any{"answers": map[string]any{}}, nil
	}

	answers := make(map[string]any, len(resp.Accept.Content))
	for key, value := range resp.Accept.Content {
		answers[key] = map[string]any{"answers": stringAnswersFromAny(value)}
	}

	return map[string]any{"answers": answers}, nil
}

func (a *Agent) handleCodexMCPElicitation(ctx context.Context, req codex.ServerRequest) (any, error) {
	params := mapFromRaw(req.Params)
	if isCodexMCPToolApproval(params) {
		return a.handleCodexMCPToolApproval(ctx, req, params)
	}

	return a.handleCodexMCPUserElicitation(ctx, req, params)
}

func (a *Agent) handleCodexMCPToolApproval(ctx context.Context, req codex.ServerRequest, params map[string]any) (any, error) {
	session := a.sessionByCodexThread(stringFromAny(params["threadId"]))
	if session == nil {
		return map[string]any{"action": "cancel"}, nil
	}

	conn := a.connection()
	if conn == nil {
		return map[string]any{"action": "cancel"}, nil
	}

	meta := codexMCPMeta(params)
	title := mcpToolApprovalTitle(params, meta)
	kind := acp.ToolKindOther
	status := acp.ToolCallStatusPending
	toolID := firstNonEmpty(stringFromAny(params["elicitationId"]), stringFromAny(params["toolCallId"]), serverRequestID(req.ID), codexReqMCPElicitation)

	resp, err := conn.RequestPermission(ctx, acp.RequestPermissionRequest{
		SessionId: session.id,
		ToolCall: acp.ToolCallUpdate{
			ToolCallId: acp.ToolCallId(toolID),
			Title:      &title,
			Kind:       &kind,
			Status:     &status,
			Content:    mcpToolApprovalContent(params, meta),
			RawInput:   params,
			Meta:       map[string]any{codexMetaKey: params},
		},
		Options: []acp.PermissionOption{
			{OptionId: permissionAccept, Name: "Allow once", Kind: acp.PermissionOptionKindAllowOnce},
			{OptionId: permissionDecline, Name: "Reject", Kind: acp.PermissionOptionKindRejectOnce},
		},
	})
	if err != nil {
		return nil, err
	}
	if resp.Outcome.Selected == nil {
		return map[string]any{"action": "cancel"}, nil
	}

	return map[string]any{"action": mcpToolApprovalAction(resp.Outcome.Selected.OptionId)}, nil
}

func (a *Agent) handleCodexMCPUserElicitation(ctx context.Context, req codex.ServerRequest, params map[string]any) (any, error) {
	conn := a.connection()
	if conn == nil {
		return map[string]any{"action": "cancel"}, nil
	}

	mode := stringFromAny(params["mode"])
	message := firstNonEmpty(stringFromAny(params["message"]), "MCP server needs input")

	var request acp.UnstableCreateElicitationRequest
	if mode == "url" {
		if !a.clientSupportsURLElicitation() {
			return map[string]any{"action": "decline"}, nil
		}
		request.Url = &acp.UnstableCreateElicitationUrl{
			ElicitationId: acp.UnstableElicitationId(stringFromAny(params["elicitationId"])),
			Message:       message,
			Mode:          "url",
			Url:           stringFromAny(params["url"]),
			Meta:          map[string]any{codexMetaKey: params},
		}
	} else {
		if !a.clientSupportsFormElicitation() {
			return map[string]any{"action": "decline"}, nil
		}
		request.Form = &acp.UnstableCreateElicitationForm{
			Message:         message,
			Mode:            "form",
			RequestedSchema: elicitationSchemaFromMap(mapFromAny(params["requestedSchema"])),
			Meta:            map[string]any{codexMetaKey: params},
		}
	}

	scope := elicitationScope{
		ToolCallID: acp.ToolCallId(firstNonEmpty(stringFromAny(params["toolCallId"]), stringFromAny(params["itemId"]), stringFromAny(params["elicitationId"]), serverRequestID(req.ID))),
		RequestID:  requestIDFromRaw(req.ID),
	}
	if session := a.sessionByCodexThread(stringFromAny(params["threadId"])); session != nil {
		scope.SessionID = session.id
	}

	resp, err := conn.CreateElicitation(ctx, request, scope)
	if err != nil {
		return nil, err
	}
	switch {
	case resp.Accept != nil:
		return map[string]any{"action": "accept", "content": resp.Accept.Content, "_meta": resp.Accept.Meta}, nil
	case resp.Decline != nil:
		return map[string]any{"action": "decline", "_meta": resp.Decline.Meta}, nil
	default:
		return map[string]any{"action": "cancel"}, nil
	}
}

func isCodexMCPToolApproval(params map[string]any) bool {
	meta := codexMCPMeta(params)

	return codexMCPMetaString(meta, codexMCPApprovalKindKey) == codexMCPToolApprovalKind
}

func codexMCPMeta(params map[string]any) map[string]any {
	meta := mapFromAny(params["_meta"])
	if codexMeta := mapFromAny(meta[codexMetaKey]); codexMeta != nil {
		return codexMeta
	}
	if codexMeta := mapFromAny(params[codexMetaKey]); codexMeta != nil {
		return codexMeta
	}

	return nil
}

func mcpToolApprovalTitle(params map[string]any, meta map[string]any) string {
	if title := codexMCPMetaString(meta, codexMCPApprovalToolTitleKey); title != "" {
		return title
	}
	if title := stringFromAny(params["toolTitle"]); title != "" {
		return title
	}
	if title := stringFromAny(params["toolName"]); title != "" {
		return title
	}

	return firstNonEmpty(stringFromAny(params["message"]), "MCP tool call")
}

func codexMCPMetaString(meta map[string]any, key string) string {
	if detail := mapFromAny(meta["_meta"]); detail != nil {
		if value := stringFromAny(detail[key]); value != "" {
			return value
		}
	}

	return stringFromAny(meta[key])
}

func mcpToolApprovalContent(params map[string]any, meta map[string]any) []acp.ToolCallContent {
	parts := make([]string, 0, 3)
	if message := stringFromAny(params["message"]); message != "" {
		parts = append(parts, message)
	}
	if serverName := stringFromAny(meta["serverName"]); serverName != "" {
		parts = append(parts, "Server: "+serverName)
	}
	for _, key := range []string{"tool_params", "toolParams"} {
		if toolParams := params[key]; toolParams != nil {
			if raw, err := json.MarshalIndent(toolParams, "", "  "); err == nil {
				parts = append(parts, "Input:\n"+string(raw))
			}
			break
		}
	}
	if len(parts) == 0 {
		return nil
	}

	return []acp.ToolCallContent{textToolContent(strings.Join(parts, "\n\n"))}
}

func mcpToolApprovalAction(optionID acp.PermissionOptionId) string {
	switch optionID {
	case permissionAccept, permissionAcceptForSession:
		return "accept"
	case permissionDecline:
		return "decline"
	default:
		return "cancel"
	}
}

func serverRequestID(raw json.RawMessage) string {
	var id string
	if len(raw) > 0 && json.Unmarshal(raw, &id) == nil {
		return id
	}

	return string(raw)
}

func requestIDFromRaw(raw json.RawMessage) *acp.RequestId {
	if len(raw) == 0 {
		return nil
	}
	var id acp.RequestId
	if err := json.Unmarshal(raw, &id); err != nil {
		return nil
	}

	return &id
}

func approvalTitle(method string, params map[string]any) string {
	if reason := stringFromAny(params["reason"]); reason != "" {
		return reason
	}
	switch method {
	case codexReqCommandApproval:
		return firstNonEmpty(stringFromAny(params["command"]), "Run command")
	case codexReqFileChangeApproval:
		return firstNonEmpty(stringFromAny(params["grantRoot"]), "Apply file changes")
	default:
		return "Codex permission request"
	}
}

func approvalContent(method string, params map[string]any) []acp.ToolCallContent {
	switch method {
	case codexReqCommandApproval:
		if command := stringFromAny(params["command"]); command != "" {
			return []acp.ToolCallContent{textToolContent(command)}
		}
	case codexReqFileChangeApproval:
		if root := stringFromAny(params["grantRoot"]); root != "" {
			return []acp.ToolCallContent{textToolContent("Write access requested: " + root)}
		}
	}
	if reason := stringFromAny(params["reason"]); reason != "" {
		return []acp.ToolCallContent{textToolContent(reason)}
	}

	return nil
}

func codexApprovalOptions(params map[string]any) []acp.PermissionOption {
	if decisions, ok := params["availableDecisions"].([]any); ok && len(decisions) > 0 {
		options := make([]acp.PermissionOption, 0, len(decisions))
		for _, decision := range decisions {
			id, name, kind := codexDecisionOption(decision)
			if id == "" {
				continue
			}
			options = append(options, acp.PermissionOption{
				OptionId: acp.PermissionOptionId(id),
				Name:     name,
				Kind:     kind,
			})
		}
		if len(options) > 0 {
			return options
		}
	}

	options := []acp.PermissionOption{
		{OptionId: permissionAccept, Name: "Allow once", Kind: acp.PermissionOptionKindAllowOnce},
		{OptionId: permissionAcceptForSession, Name: "Allow for this session", Kind: acp.PermissionOptionKindAllowAlways},
		{OptionId: permissionDecline, Name: "Reject", Kind: acp.PermissionOptionKindRejectOnce},
		{OptionId: permissionCancel, Name: "Reject and stop", Kind: acp.PermissionOptionKindRejectAlways},
	}
	if amendment, ok := params["proposedExecpolicyAmendment"].([]any); ok && len(amendment) > 0 {
		withAmendment := make([]acp.PermissionOption, 0, len(options)+1)
		withAmendment = append(withAmendment, options[:2]...)
		withAmendment = append(withAmendment, acp.PermissionOption{
			OptionId: "acceptWithExecpolicyAmendment",
			Name:     "Allow and remember",
			Kind:     acp.PermissionOptionKindAllowAlways,
		})
		withAmendment = append(withAmendment, options[2:]...)
		options = withAmendment
	}

	return options
}

func codexDecisionOption(decision any) (string, string, acp.PermissionOptionKind) {
	switch typed := decision.(type) {
	case string:
		switch typed {
		case permissionAccept:
			return typed, "Allow once", acp.PermissionOptionKindAllowOnce
		case permissionAcceptForSession:
			return typed, "Allow for this session", acp.PermissionOptionKindAllowAlways
		case permissionDecline:
			return typed, "Reject", acp.PermissionOptionKindRejectOnce
		case permissionCancel:
			return typed, "Reject and stop", acp.PermissionOptionKindRejectAlways
		default:
			return typed, typed, acp.PermissionOptionKindAllowOnce
		}
	case map[string]any:
		raw, err := json.Marshal(typed)
		if err != nil {
			return "", "", acp.PermissionOptionKindAllowOnce
		}
		switch {
		case typed["acceptWithExecpolicyAmendment"] != nil:
			return string(raw), "Allow and remember", acp.PermissionOptionKindAllowAlways
		case typed["applyNetworkPolicyAmendment"] != nil:
			return string(raw), "Apply network policy", acp.PermissionOptionKindAllowAlways
		default:
			return string(raw), "Allow", acp.PermissionOptionKindAllowOnce
		}
	default:
		return "", "", acp.PermissionOptionKindAllowOnce
	}
}

func codexDecisionFromOption(optionID acp.PermissionOptionId, params map[string]any) any {
	value := string(optionID)
	if strings.HasPrefix(strings.TrimSpace(value), "{") {
		var decision any
		if err := json.Unmarshal([]byte(value), &decision); err == nil {
			return decision
		}
	}
	switch value {
	case "acceptWithExecpolicyAmendment":
		return map[string]any{
			"acceptWithExecpolicyAmendment": map[string]any{
				"execpolicy_amendment": params["proposedExecpolicyAmendment"],
			},
		}
	case permissionAccept, permissionAcceptForSession, permissionDecline, permissionCancel:
		return string(optionID)
	default:
		return permissionCancel
	}
}

func codexServerInteractionKey(req codex.ServerRequest, params map[string]any) string {
	return req.Method + ":" + firstNonEmpty(string(req.ID), stringFromAny(params["approvalId"]), stringFromAny(params["elicitationId"]), stringFromAny(params["itemId"]))
}

func permissionProfileText(value any) string {
	raw, err := json.MarshalIndent(value, "", "  ")
	if err != nil {
		return fmt.Sprint(value)
	}

	return string(raw)
}

func toolUserInputMessage(questions []map[string]any) string {
	if len(questions) == 1 {
		if question := stringFromAny(questions[0]["question"]); question != "" {
			return question
		}
	}

	return "Codex needs input"
}

func schemaFromToolQuestions(questions []map[string]any) (map[string]any, []string) {
	properties := make(map[string]any, len(questions))
	required := make([]string, 0, len(questions))
	for _, question := range questions {
		id := firstNonEmpty(stringFromAny(question["id"]), stringFromAny(question["header"]))
		if id == "" {
			continue
		}
		required = append(required, id)
		property := map[string]any{
			"type":        "string",
			"title":       firstNonEmpty(stringFromAny(question["header"]), id),
			"description": stringFromAny(question["question"]),
		}
		if options := toolQuestionOptions(question["options"]); len(options) > 0 {
			property["oneOf"] = options
		}
		if secret, ok := question["isSecret"].(bool); ok && secret {
			property["format"] = "password"
			property["writeOnly"] = true
		}
		properties[id] = property
	}
	if len(properties) == 0 {
		properties["answer"] = map[string]any{"type": "string"}
		required = []string{"answer"}
	}

	return properties, required
}

func toolQuestionOptions(raw any) []map[string]any {
	values, ok := raw.([]any)
	if !ok {
		return nil
	}
	out := make([]map[string]any, 0, len(values))
	for _, value := range values {
		option := mapFromAny(value)
		label := firstNonEmpty(stringFromAny(option["label"]), stringFromAny(option["value"]))
		if label == "" {
			continue
		}
		item := map[string]any{
			"const": label,
			"title": label,
		}
		if desc := stringFromAny(option["description"]); desc != "" {
			item["description"] = desc
		}
		out = append(out, item)
	}

	return out
}

func elicitationSchemaFromMap(raw map[string]any) acp.UnstableElicitationSchema {
	schema := acp.UnstableElicitationSchema{
		Type:       acp.UnstableElicitationSchemaTypeObject,
		Properties: map[string]any{},
	}
	if raw == nil {
		return schema
	}
	if title := stringFromAny(raw["title"]); title != "" {
		schema.Title = &title
	}
	if desc := stringFromAny(raw["description"]); desc != "" {
		schema.Description = &desc
	}
	if required, ok := raw["required"].([]any); ok {
		for _, item := range required {
			if value := stringFromAny(item); value != "" {
				schema.Required = append(schema.Required, value)
			}
		}
	}
	if properties := mapFromAny(raw["properties"]); properties != nil {
		schema.Properties = properties
	}

	return schema
}

func stringAnswersFromAny(value any) []string {
	switch typed := value.(type) {
	case nil:
		return nil
	case []string:
		return append([]string(nil), typed...)
	case []any:
		out := make([]string, 0, len(typed))
		for _, item := range typed {
			out = append(out, fmt.Sprint(item))
		}
		return out
	default:
		return []string{fmt.Sprint(value)}
	}
}

func stringFromAny(value any) string {
	switch typed := value.(type) {
	case string:
		return typed
	case fmt.Stringer:
		return typed.String()
	default:
		return ""
	}
}

func mapFromAny(value any) map[string]any {
	switch typed := value.(type) {
	case map[string]any:
		return typed
	default:
		return nil
	}
}

func sliceOfMaps(value any) []map[string]any {
	values, ok := value.([]any)
	if !ok {
		return nil
	}
	out := make([]map[string]any, 0, len(values))
	for _, value := range values {
		if item := mapFromAny(value); item != nil {
			out = append(out, item)
		}
	}

	return out
}
