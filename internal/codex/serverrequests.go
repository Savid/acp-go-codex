package codex

import (
	"encoding/json"
	"fmt"
	"strings"

	"github.com/coder/acp-go-sdk"
)

// Native app-server request method names. The root package dispatches on
// these and bridges each request onto the matching ACP client call.
const (
	// RequestCommandApproval asks the host to approve one command execution.
	RequestCommandApproval = "item/commandExecution/requestApproval"
	// RequestFileChangeApproval asks the host to approve proposed file changes.
	RequestFileChangeApproval = "item/fileChange/requestApproval"
	// RequestPermissionsApproval asks the host to grant a permission profile.
	RequestPermissionsApproval = "item/permissions/requestApproval"
	// RequestToolUserInput asks the host to collect tool input from the user.
	RequestToolUserInput = "item/tool/requestUserInput"
	// RequestMCPElicitation forwards an MCP server elicitation to the host.
	RequestMCPElicitation = "mcpServer/elicitation/request"
	// RequestAuthTokenRefresh asks the host to refresh external ChatGPT auth tokens.
	RequestAuthTokenRefresh = "account/chatgptAuthTokens/refresh" // #nosec G101 -- app-server method name, not a token.
)

const (
	decisionAccept           = "accept"
	decisionAcceptForSession = "acceptForSession"
	decisionDecline          = "decline"
	decisionCancel           = "cancel"

	mcpApprovalKindKey      = "codex_approval_kind"
	mcpToolApprovalKind     = "mcp_tool_call"
	mcpApprovalToolTitleKey = "tool_title"

	permissionNameReject             = "Reject"
	permissionNameAllowSession       = "Allow for this session"
	permissionNameAllowOnce          = "Allow once"
	permissionNameAllowRemember      = "Allow and remember"
	permissionNameAllow              = "Allow"
	keyAcceptWithExecpolicyAmendment = "acceptWithExecpolicyAmendment"
	defaultPermissionTitle           = "Codex permission request"

	fieldDecision      = "decision"
	fieldPermissions   = "permissions"
	fieldScope         = "scope"
	fieldAnswers       = "answers"
	fieldAction        = "action"
	fieldContent       = "content"
	fieldMetaObject    = "_meta"
	fieldMode          = "mode"
	fieldURL           = "url"
	fieldTitle         = "title"
	fieldDescription   = "description"
	fieldReason        = "reason"
	fieldApprovalID    = "approvalId"
	fieldItemID        = "itemId"
	fieldElicitationID = "elicitationId"
	fieldToolCallID    = "toolCallId"
	fieldGrantRoot     = "grantRoot"
	fieldCommand       = "command"
	fieldQuestions     = "questions"
	metaVendorKey      = "codex"

	defaultToolUserInputMessage = "Codex needs input"
	schemaTypeString            = "string"

	scopeTurn          = "turn"
	scopeSession       = "session"
	optionGrantTurn    = "grant-turn"
	optionGrantSession = "grant-session"
	modeForm           = "form"
	modeURL            = "url"
)

// ServerRequestParams decodes a native server request's params as a generic
// JSON object. Malformed params yield a nil map.
func ServerRequestParams(req ServerRequest) map[string]any {
	var out map[string]any
	if len(req.Params) > 0 {
		_ = json.Unmarshal(req.Params, &out)
	}

	return out
}

// RequestThreadID returns the native thread ID the request addresses, if any.
func RequestThreadID(params map[string]any) string {
	return stringValue(params, fieldThreadID)
}

// ServerInteractionKey identifies one interactive server request so a later
// duplicate for the same native artifact replaces the earlier interaction.
func ServerInteractionKey(req ServerRequest, params map[string]any) string {
	return req.Method + ":" + firstNonEmpty(
		string(req.ID),
		stringValue(params, fieldApprovalID),
		stringValue(params, fieldElicitationID),
		stringValue(params, fieldItemID),
	)
}

// ServerRequestID renders a native JSON-RPC request ID as a plain string.
func ServerRequestID(raw json.RawMessage) string {
	var id string
	if len(raw) > 0 && json.Unmarshal(raw, &id) == nil {
		return id
	}

	return string(raw)
}

// RequestIDFromRaw decodes a native JSON-RPC request ID as an ACP request ID.
func RequestIDFromRaw(raw json.RawMessage) *acp.RequestId {
	if len(raw) == 0 {
		return nil
	}

	var id acp.RequestId
	if err := json.Unmarshal(raw, &id); err != nil {
		return nil
	}

	return &id
}

// ApprovalToolCallID chooses the ACP tool-call ID for a command or
// file-change approval request.
func ApprovalToolCallID(req ServerRequest, params map[string]any) string {
	return firstNonEmpty(stringValue(params, fieldApprovalID), stringValue(params, fieldItemID), req.Method)
}

// ApprovalTitle renders the host-facing title for an approval request.
func ApprovalTitle(method string, params map[string]any) string {
	if reason := stringValue(params, fieldReason); reason != "" {
		return reason
	}

	switch method {
	case RequestCommandApproval:
		return firstNonEmpty(stringValue(params, fieldCommand), "Run command")
	case RequestFileChangeApproval:
		return firstNonEmpty(stringValue(params, fieldGrantRoot), "Apply file changes")
	default:
		return defaultPermissionTitle
	}
}

// ApprovalContent renders the host-facing content for an approval request.
func ApprovalContent(method string, params map[string]any) []acp.ToolCallContent {
	switch method {
	case RequestCommandApproval:
		if command := stringValue(params, fieldCommand); command != "" {
			return []acp.ToolCallContent{textToolCallContent(command)}
		}
	case RequestFileChangeApproval:
		if root := stringValue(params, fieldGrantRoot); root != "" {
			return []acp.ToolCallContent{textToolCallContent("Write access requested: " + root)}
		}
	}

	if reason := stringValue(params, fieldReason); reason != "" {
		return []acp.ToolCallContent{textToolCallContent(reason)}
	}

	return nil
}

// ApprovalOptions maps the request's native decisions onto ACP permission
// options, falling back to the standard decision set.
func ApprovalOptions(params map[string]any) []acp.PermissionOption {
	if decisions, ok := params["availableDecisions"].([]any); ok && len(decisions) > 0 {
		options := make([]acp.PermissionOption, 0, len(decisions))
		for _, decision := range decisions {
			id, name, kind := decisionOption(decision)
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
		{OptionId: decisionAccept, Name: permissionNameAllowOnce, Kind: acp.PermissionOptionKindAllowOnce},
		{OptionId: decisionAcceptForSession, Name: permissionNameAllowSession, Kind: acp.PermissionOptionKindAllowAlways},
		{OptionId: decisionDecline, Name: permissionNameReject, Kind: acp.PermissionOptionKindRejectOnce},
		{OptionId: decisionCancel, Name: "Reject and stop", Kind: acp.PermissionOptionKindRejectAlways},
	}
	if amendment, ok := params["proposedExecpolicyAmendment"].([]any); ok && len(amendment) > 0 {
		withAmendment := make([]acp.PermissionOption, 0, len(options)+1)
		withAmendment = append(withAmendment, options[:2]...)
		withAmendment = append(withAmendment, acp.PermissionOption{
			OptionId: keyAcceptWithExecpolicyAmendment,
			Name:     permissionNameAllowRemember,
			Kind:     acp.PermissionOptionKindAllowAlways,
		})
		withAmendment = append(withAmendment, options[2:]...)
		options = withAmendment
	}

	return options
}

func decisionOption(decision any) (string, string, acp.PermissionOptionKind) {
	switch typed := decision.(type) {
	case string:
		switch typed {
		case decisionAccept:
			return typed, permissionNameAllowOnce, acp.PermissionOptionKindAllowOnce
		case decisionAcceptForSession:
			return typed, permissionNameAllowSession, acp.PermissionOptionKindAllowAlways
		case decisionDecline:
			return typed, permissionNameReject, acp.PermissionOptionKindRejectOnce
		case decisionCancel:
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
		case typed[keyAcceptWithExecpolicyAmendment] != nil:
			return string(raw), permissionNameAllowRemember, acp.PermissionOptionKindAllowAlways
		case typed["applyNetworkPolicyAmendment"] != nil:
			return string(raw), "Apply network policy", acp.PermissionOptionKindAllowAlways
		default:
			return string(raw), permissionNameAllow, acp.PermissionOptionKindAllowOnce
		}
	default:
		return "", "", acp.PermissionOptionKindAllowOnce
	}
}

// ApprovalCancelResponse is the native "cancel" decision payload.
func ApprovalCancelResponse() map[string]any {
	return map[string]any{fieldDecision: decisionCancel}
}

// ApprovalDecisionResponse maps a selected ACP permission option back onto
// the native decision payload.
func ApprovalDecisionResponse(optionID acp.PermissionOptionId, params map[string]any) map[string]any {
	return map[string]any{fieldDecision: decisionFromOption(optionID, params)}
}

func decisionFromOption(optionID acp.PermissionOptionId, params map[string]any) any {
	value := string(optionID)
	if strings.HasPrefix(strings.TrimSpace(value), "{") {
		var decision any
		if err := json.Unmarshal([]byte(value), &decision); err == nil {
			return decision
		}
	}

	switch value {
	case keyAcceptWithExecpolicyAmendment:
		return map[string]any{
			keyAcceptWithExecpolicyAmendment: map[string]any{
				"execpolicy_amendment": params["proposedExecpolicyAmendment"],
			},
		}
	case decisionAccept, decisionAcceptForSession, decisionDecline, decisionCancel:
		return string(optionID)
	default:
		return decisionCancel
	}
}

// PermissionsToolCallID chooses the ACP tool-call ID for a permissions
// approval request.
func PermissionsToolCallID(req ServerRequest, params map[string]any) string {
	return firstNonEmpty(stringValue(params, fieldItemID), req.Method)
}

// PermissionsApprovalTitle renders the host-facing title for a permissions
// approval request.
func PermissionsApprovalTitle(params map[string]any) string {
	return firstNonEmpty(stringValue(params, fieldReason), defaultPermissionTitle)
}

// PermissionsApprovalContent renders the requested permission profile as
// host-facing content.
func PermissionsApprovalContent(params map[string]any) []acp.ToolCallContent {
	return []acp.ToolCallContent{textToolCallContent(permissionProfileText(params[fieldPermissions]))}
}

// PermissionsApprovalOptions is the fixed ACP option set for a permissions
// approval request.
func PermissionsApprovalOptions() []acp.PermissionOption {
	return []acp.PermissionOption{
		{OptionId: optionGrantTurn, Name: "Allow for this turn", Kind: acp.PermissionOptionKindAllowOnce},
		{OptionId: optionGrantSession, Name: permissionNameAllowSession, Kind: acp.PermissionOptionKindAllowAlways},
		{OptionId: decisionDecline, Name: permissionNameReject, Kind: acp.PermissionOptionKindRejectOnce},
	}
}

// PermissionsDeniedResponse is the native payload that grants nothing for
// the current turn.
func PermissionsDeniedResponse() map[string]any {
	return map[string]any{fieldPermissions: map[string]any{}, fieldScope: scopeTurn}
}

// PermissionsApprovalResponse maps a selected ACP permission option back
// onto the native permissions grant payload.
func PermissionsApprovalResponse(optionID acp.PermissionOptionId, params map[string]any) map[string]any {
	if optionID == decisionDecline {
		return PermissionsDeniedResponse()
	}

	scope := scopeTurn
	if optionID == optionGrantSession {
		scope = scopeSession
	}

	return map[string]any{
		fieldPermissions: params[fieldPermissions],
		fieldScope:       scope,
	}
}

// ToolUserInputToolCallID chooses the ACP tool-call ID for a tool user-input
// request.
func ToolUserInputToolCallID(req ServerRequest, params map[string]any) string {
	return firstNonEmpty(stringValue(params, fieldItemID), ServerRequestID(req.ID), RequestToolUserInput)
}

// ToolUserInputForm renders the native tool questions as an ACP form
// elicitation request.
func ToolUserInputForm(params map[string]any, meta map[string]any) *acp.UnstableCreateElicitationForm {
	questions := mapSlice(params, fieldQuestions)
	properties, required := schemaFromToolQuestions(questions)

	return &acp.UnstableCreateElicitationForm{
		Message: toolUserInputMessage(questions),
		Mode:    modeForm,
		RequestedSchema: acp.UnstableElicitationSchema{
			Title:      acp.Ptr("Codex input"),
			Type:       acp.UnstableElicitationSchemaTypeObject,
			Properties: properties,
			Required:   required,
		},
		Meta: meta,
	}
}

// EmptyToolUserInputResponse is the native payload carrying no answers.
func EmptyToolUserInputResponse() map[string]any {
	return map[string]any{fieldAnswers: map[string]any{}}
}

// ToolUserInputResponse renders accepted ACP form content as the native
// answers payload.
func ToolUserInputResponse(content map[string]any) map[string]any {
	answers := make(map[string]any, len(content))
	for key, value := range content {
		answers[key] = map[string]any{fieldAnswers: stringAnswersFromAny(value)}
	}

	return map[string]any{fieldAnswers: answers}
}

// IsMCPToolApproval reports whether an MCP elicitation request is a tool-call
// approval in disguise.
func IsMCPToolApproval(params map[string]any) bool {
	return mcpMetaString(mcpMeta(params), mcpApprovalKindKey) == mcpToolApprovalKind
}

func mcpMeta(params map[string]any) map[string]any {
	meta := mapValue(params, fieldMetaObject)
	if codexMeta := mapValue(meta, metaVendorKey); codexMeta != nil {
		return codexMeta
	}

	if codexMeta := mapValue(params, metaVendorKey); codexMeta != nil {
		return codexMeta
	}

	return nil
}

func mcpMetaString(meta map[string]any, key string) string {
	if detail := mapValue(meta, fieldMetaObject); detail != nil {
		if value := stringValue(detail, key); value != "" {
			return value
		}
	}

	return stringValue(meta, key)
}

// MCPToolApprovalToolCallID chooses the ACP tool-call ID for an MCP tool
// approval request.
func MCPToolApprovalToolCallID(req ServerRequest, params map[string]any) string {
	return firstNonEmpty(
		stringValue(params, fieldElicitationID),
		stringValue(params, fieldToolCallID),
		ServerRequestID(req.ID),
		RequestMCPElicitation,
	)
}

// MCPToolApprovalTitle renders the host-facing title for an MCP tool
// approval request.
func MCPToolApprovalTitle(params map[string]any) string {
	meta := mcpMeta(params)
	if title := mcpMetaString(meta, mcpApprovalToolTitleKey); title != "" {
		return title
	}

	if title := stringValue(params, "toolTitle"); title != "" {
		return title
	}

	if title := stringValue(params, "toolName"); title != "" {
		return title
	}

	return firstNonEmpty(stringValue(params, fieldMessage), "MCP tool call")
}

// MCPToolApprovalContent renders the host-facing content for an MCP tool
// approval request.
func MCPToolApprovalContent(params map[string]any) []acp.ToolCallContent {
	meta := mcpMeta(params)

	parts := make([]string, 0, 3)
	if message := stringValue(params, fieldMessage); message != "" {
		parts = append(parts, message)
	}

	if serverName := stringValue(meta, "serverName"); serverName != "" {
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

	return []acp.ToolCallContent{textToolCallContent(strings.Join(parts, "\n\n"))}
}

// MCPToolApprovalOptions is the fixed ACP option set for an MCP tool
// approval request.
func MCPToolApprovalOptions() []acp.PermissionOption {
	return []acp.PermissionOption{
		{OptionId: decisionAccept, Name: permissionNameAllowOnce, Kind: acp.PermissionOptionKindAllowOnce},
		{OptionId: decisionDecline, Name: permissionNameReject, Kind: acp.PermissionOptionKindRejectOnce},
	}
}

// MCPToolApprovalResponse maps a selected ACP permission option back onto
// the native MCP approval action payload.
func MCPToolApprovalResponse(optionID acp.PermissionOptionId) map[string]any {
	return map[string]any{fieldAction: mcpToolApprovalAction(optionID)}
}

func mcpToolApprovalAction(optionID acp.PermissionOptionId) string {
	switch optionID {
	case decisionAccept, decisionAcceptForSession:
		return decisionAccept
	case decisionDecline:
		return decisionDecline
	default:
		return decisionCancel
	}
}

// ElicitationCancelResponse is the native "cancel" action payload.
func ElicitationCancelResponse() map[string]any {
	return map[string]any{fieldAction: decisionCancel}
}

// ElicitationDeclineResponse is the native "decline" action payload.
func ElicitationDeclineResponse() map[string]any {
	return map[string]any{fieldAction: decisionDecline}
}

// ElicitationAcceptResponse renders an accepted ACP elicitation as the
// native action payload.
func ElicitationAcceptResponse(content map[string]any, meta map[string]any) map[string]any {
	return map[string]any{fieldAction: decisionAccept, fieldContent: content, fieldMetaObject: meta}
}

// ElicitationDeclineMetaResponse renders a declined ACP elicitation as the
// native action payload.
func ElicitationDeclineMetaResponse(meta map[string]any) map[string]any {
	return map[string]any{fieldAction: decisionDecline, fieldMetaObject: meta}
}

// IsURLElicitation reports whether an MCP elicitation request asks for the
// URL flow.
func IsURLElicitation(params map[string]any) bool {
	return stringValue(params, fieldMode) == modeURL
}

// MCPUserElicitationToolCallID returns only an explicit native tool/item
// association. JSON-RPC request IDs and standalone elicitation IDs are request
// correlations, never ACP tool-call identities.
func MCPUserElicitationToolCallID(params map[string]any) string {
	return firstNonEmpty(
		stringValue(params, fieldToolCallID),
		stringValue(params, fieldItemID),
	)
}

// MCPUserElicitationRequestID returns the request correlation for a standalone
// MCP elicitation. The native JSON-RPC request ID is authoritative; URL-flow
// elicitation IDs are the fallback when no JSON-RPC ID is available.
func MCPUserElicitationRequestID(req ServerRequest, params map[string]any) *acp.RequestId {
	if requestID := RequestIDFromRaw(req.ID); requestID != nil {
		return requestID
	}

	value := stringValue(params, fieldElicitationID)
	if value == "" {
		return nil
	}

	requestIDValue := acp.RequestIdStr(value)
	requestID := acp.RequestId{Str: &requestIDValue}

	return &requestID
}

// MCPElicitationRequest renders a native MCP elicitation as an ACP
// elicitation request in either URL or form mode.
func MCPElicitationRequest(params map[string]any, meta map[string]any) acp.UnstableCreateElicitationRequest {
	message := firstNonEmpty(stringValue(params, fieldMessage), "MCP server needs input")

	var request acp.UnstableCreateElicitationRequest

	if IsURLElicitation(params) {
		request.Url = &acp.UnstableCreateElicitationUrl{
			ElicitationId: acp.UnstableElicitationId(stringValue(params, fieldElicitationID)),
			Message:       message,
			Mode:          modeURL,
			Url:           stringValue(params, fieldURL),
			Meta:          meta,
		}

		return request
	}

	request.Form = &acp.UnstableCreateElicitationForm{
		Message:         message,
		Mode:            modeForm,
		RequestedSchema: elicitationSchemaFromMap(mapValue(params, "requestedSchema")),
		Meta:            meta,
	}

	return request
}

// AuthTokensRefreshResponse renders refreshed external ChatGPT tokens as the
// native response payload.
func AuthTokensRefreshResponse(tokens ChatGPTAuthTokens) map[string]any {
	resp := map[string]any{
		"accessToken":         tokens.AccessToken,
		fieldChatGPTAccountID: tokens.AccountID,
		fieldChatGPTPlanType:  tokens.PlanType,
	}
	if tokens.RefreshToken != "" {
		resp["refreshToken"] = tokens.RefreshToken
	}

	if tokens.ExpiresAtUnixSec != 0 {
		resp["expiresAt"] = tokens.ExpiresAtUnixSec
	}

	return resp
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
		if question := stringValue(questions[0], "question"); question != "" {
			return question
		}
	}

	return defaultToolUserInputMessage
}

func schemaFromToolQuestions(questions []map[string]any) (map[string]any, []string) {
	properties := make(map[string]any, len(questions))

	required := make([]string, 0, len(questions))
	for _, question := range questions {
		id := firstNonEmpty(stringValue(question, "id"), stringValue(question, "header"))
		if id == "" {
			continue
		}

		required = append(required, id)

		property := map[string]any{
			fieldType:        schemaTypeString,
			fieldTitle:       firstNonEmpty(stringValue(question, "header"), id),
			fieldDescription: stringValue(question, "question"),
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
		properties["answer"] = map[string]any{fieldType: schemaTypeString}
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
		option, _ := value.(map[string]any)

		label := firstNonEmpty(stringValue(option, "label"), stringValue(option, "value"))
		if label == "" {
			continue
		}

		item := map[string]any{
			"const":    label,
			fieldTitle: label,
		}
		if desc := stringValue(option, fieldDescription); desc != "" {
			item[fieldDescription] = desc
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

	if title := stringValue(raw, fieldTitle); title != "" {
		schema.Title = &title
	}

	if desc := stringValue(raw, fieldDescription); desc != "" {
		schema.Description = &desc
	}

	if required, ok := raw["required"].([]any); ok {
		for _, item := range required {
			if value, ok := item.(string); ok && value != "" {
				schema.Required = append(schema.Required, value)
			}
		}
	}

	if properties := mapValue(raw, "properties"); properties != nil {
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

func textToolCallContent(text string) acp.ToolCallContent {
	return acp.ToolCallContent{
		Content: &acp.ToolCallContentContent{
			Content: acp.TextBlock(text),
		},
	}
}
