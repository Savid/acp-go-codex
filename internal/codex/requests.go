package codex

import (
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strconv"
	"strings"

	"github.com/coder/acp-go-sdk"
)

// ErrElicitationSecret reports a form that asks the host to collect a
// credential. The adapter refuses the whole form; the error names no
// property and carries no schema content.
var ErrElicitationSecret = errors.New("codex elicitation marks a field as a secret")

// Server request methods the adapter answers.
const (
	RequestCommandApproval     = "item/commandExecution/requestApproval"
	RequestFileChangeApproval  = "item/fileChange/requestApproval"
	RequestPermissionsApproval = "item/permissions/requestApproval"
	RequestToolUserInput       = "item/tool/requestUserInput"
	RequestMCPElicitation      = "mcpServer/elicitation/request"
)

const (
	decisionAccept           = "accept"
	decisionAcceptForSession = "acceptForSession"
	decisionDecline          = "decline"
	decisionCancel           = "cancel"

	keyExecpolicyAmendment = "acceptWithExecpolicyAmendment"

	optionGrantTurn    = "grant-turn"
	optionGrantSession = "grant-session"

	fieldDecision    = "decision"
	fieldPermissions = "permissions"
	fieldScope       = "scope"
	fieldAnswers     = "answers"
	fieldAction      = "action"
	fieldContent     = "content"
	fieldMeta        = "_meta"
	fieldReason      = "reason"
	fieldQuestions   = "questions"
	fieldTitle       = "title"
	fieldDescription = "description"

	nameReject       = "Reject"
	schemaTypeString = "string"
	modeForm         = "form"
	modeURL          = "url"
)

// RequestParams decodes a server request's params. Malformed params yield a
// nil map.
func RequestParams(request ServerRequest) map[string]any {
	var params map[string]any
	if len(request.Params) > 0 {
		_ = json.Unmarshal(request.Params, &params)
	}

	return params
}

// RequestThreadID returns the thread a server request addresses.
func RequestThreadID(params map[string]any) string { return stringValue(params, fieldThreadID) }

// RequestItemID returns the item a server request concerns.
func RequestItemID(params map[string]any) string { return stringValue(params, fieldItemID) }

// ApprovalTitle renders the host-facing title of an approval request.
func ApprovalTitle(method string, params map[string]any) string {
	if reason := stringValue(params, fieldReason); reason != "" {
		return reason
	}

	switch method {
	case RequestCommandApproval:
		return firstNonEmpty(commandText(params["command"]), "Run command")
	case RequestFileChangeApproval:
		return firstNonEmpty(stringValue(params, "grantRoot"), "Apply file changes")
	default:
		return "Codex permission request"
	}
}

// ApprovalContent renders the host-facing content of an approval request.
func ApprovalContent(method string, params map[string]any) []acp.ToolCallContent {
	var text string

	switch method {
	case RequestCommandApproval:
		text = commandText(params["command"])
	case RequestFileChangeApproval:
		if root := stringValue(params, "grantRoot"); root != "" {
			text = "Write access requested: " + root
		}
	case RequestPermissionsApproval:
		if encoded, err := json.MarshalIndent(params[fieldPermissions], "", "  "); err == nil {
			text = string(encoded)
		}
	}

	if text == "" {
		text = stringValue(params, fieldReason)
	}

	if text == "" {
		return nil
	}

	return []acp.ToolCallContent{acp.ToolContent(acp.TextBlock(text))}
}

// ApprovalKind is the ACP tool kind of an approval request.
func ApprovalKind(method string) acp.ToolKind {
	switch method {
	case RequestCommandApproval:
		return acp.ToolKindExecute
	case RequestFileChangeApproval:
		return acp.ToolKindEdit
	default:
		return acp.ToolKindOther
	}
}

// ApprovalOptions maps a command or file-change approval's native decisions
// onto ACP permission options.
func ApprovalOptions(params map[string]any) []acp.PermissionOption {
	options := []acp.PermissionOption{
		{OptionId: decisionAccept, Name: "Allow once", Kind: acp.PermissionOptionKindAllowOnce},
		{OptionId: decisionAcceptForSession, Name: "Allow for this session", Kind: acp.PermissionOptionKindAllowAlways},
		{OptionId: decisionDecline, Name: nameReject, Kind: acp.PermissionOptionKindRejectOnce},
		{OptionId: decisionCancel, Name: "Reject and stop", Kind: acp.PermissionOptionKindRejectAlways},
	}

	if amendment, ok := params["proposedExecpolicyAmendment"].([]any); ok && len(amendment) > 0 {
		options = append(options[:2:2], append([]acp.PermissionOption{{
			OptionId: keyExecpolicyAmendment, Name: "Allow and remember", Kind: acp.PermissionOptionKindAllowAlways,
		}}, options[2:]...)...)
	}

	return options
}

// ApprovalResponse maps the selected option back onto the native decision. A
// nil selection is the native cancel.
func ApprovalResponse(selected *acp.PermissionOptionId, params map[string]any) map[string]any {
	if selected == nil {
		return map[string]any{fieldDecision: decisionCancel}
	}

	switch *selected {
	case keyExecpolicyAmendment:
		return map[string]any{fieldDecision: map[string]any{
			keyExecpolicyAmendment: map[string]any{"execpolicy_amendment": params["proposedExecpolicyAmendment"]},
		}}
	case decisionAccept, decisionAcceptForSession, decisionDecline, decisionCancel:
		return map[string]any{fieldDecision: string(*selected)}
	default:
		return map[string]any{fieldDecision: decisionCancel}
	}
}

// PermissionsOptions is the option set of a permissions approval request.
func PermissionsOptions() []acp.PermissionOption {
	return []acp.PermissionOption{
		{OptionId: optionGrantTurn, Name: "Allow for this turn", Kind: acp.PermissionOptionKindAllowOnce},
		{OptionId: optionGrantSession, Name: "Allow for this session", Kind: acp.PermissionOptionKindAllowAlways},
		{OptionId: decisionDecline, Name: nameReject, Kind: acp.PermissionOptionKindRejectOnce},
	}
}

// PermissionsResponse maps the selected option back onto the native grant. A
// nil selection or a rejection grants nothing.
func PermissionsResponse(selected *acp.PermissionOptionId, params map[string]any) map[string]any {
	if selected == nil || *selected == decisionDecline {
		return map[string]any{fieldPermissions: map[string]any{}, fieldScope: "turn"}
	}

	scope := "turn"
	if *selected == optionGrantSession {
		scope = "session"
	}

	return map[string]any{fieldPermissions: params[fieldPermissions], fieldScope: scope}
}

// IsMCPToolApproval reports whether an MCP elicitation is a tool-call
// approval, which the host answers as a permission.
func IsMCPToolApproval(params map[string]any) bool {
	return stringValue(mapValue(params, fieldMeta), "codex_approval_kind") == "mcp_tool_call"
}

// MCPToolApprovalTitle renders the host-facing title of an MCP tool approval.
func MCPToolApprovalTitle(params map[string]any) string {
	meta := mapValue(params, fieldMeta)

	return firstNonEmpty(
		stringValue(meta, "tool_title"), stringValue(meta, "tool_name"),
		stringValue(params, "toolTitle"), stringValue(params, "toolName"),
		stringValue(params, fieldMessage), "MCP tool call",
	)
}

// MCPToolApprovalOptions is the option set of an MCP tool approval.
func MCPToolApprovalOptions() []acp.PermissionOption {
	return []acp.PermissionOption{
		{OptionId: decisionAccept, Name: "Allow once", Kind: acp.PermissionOptionKindAllowOnce},
		{OptionId: decisionDecline, Name: nameReject, Kind: acp.PermissionOptionKindRejectOnce},
	}
}

// MCPToolApprovalResponse maps the selected option onto the native action.
func MCPToolApprovalResponse(selected *acp.PermissionOptionId) map[string]any {
	action := decisionCancel

	if selected != nil {
		switch *selected {
		case decisionAccept, decisionAcceptForSession:
			action = decisionAccept
		case decisionDecline:
			action = decisionDecline
		}
	}

	return map[string]any{fieldAction: action}
}

// ElicitationCancelResponse is the native cancel of an MCP elicitation.
func ElicitationCancelResponse() map[string]any { return map[string]any{fieldAction: decisionCancel} }

// ElicitationResponse maps an ACP elicitation answer onto the native action.
func ElicitationResponse(resp acp.UnstableCreateElicitationResponse) map[string]any {
	switch {
	case resp.Accept != nil:
		return map[string]any{fieldAction: decisionAccept, fieldContent: resp.Accept.Content, fieldMeta: resp.Accept.Meta}
	case resp.Decline != nil:
		return map[string]any{fieldAction: decisionDecline, fieldMeta: resp.Decline.Meta}
	default:
		return ElicitationCancelResponse()
	}
}

// MCPElicitationRequest renders an MCP elicitation as an ACP elicitation in
// url or form mode, or refuses with ErrElicitationSecret.
func MCPElicitationRequest(params map[string]any, meta map[string]any) (acp.UnstableCreateElicitationRequest, error) {
	message := firstNonEmpty(stringValue(params, fieldMessage), "MCP server needs input")

	if stringValue(params, "mode") == modeURL {
		return acp.UnstableCreateElicitationRequest{Url: &acp.UnstableCreateElicitationUrl{
			ElicitationId: acp.UnstableElicitationId(stringValue(params, "elicitationId")),
			Message:       message,
			Mode:          modeURL,
			Url:           stringValue(params, modeURL),
			Meta:          meta,
		}}, nil
	}

	schema, err := elicitationSchema(mapValue(params, "requestedSchema"))
	if err != nil {
		return acp.UnstableCreateElicitationRequest{}, err
	}

	return acp.UnstableCreateElicitationRequest{Form: &acp.UnstableCreateElicitationForm{
		Message: message, Mode: modeForm, RequestedSchema: schema, Meta: meta,
	}}, nil
}

// ToolUserInputForm renders the tool questions as an ACP form elicitation, or
// refuses with ErrElicitationSecret when a question asks for a secret.
func ToolUserInputForm(params map[string]any, meta map[string]any) (*acp.UnstableCreateElicitationForm, error) {
	questions := mapSlice(params, fieldQuestions)
	properties := make(map[string]any, len(questions))
	required := make([]string, 0, len(questions))

	for _, question := range questions {
		if truthy(question["isSecret"]) {
			return nil, ErrElicitationSecret
		}

		id := firstNonEmpty(stringValue(question, fieldID), stringValue(question, "header"))
		if id == "" {
			continue
		}

		property := map[string]any{
			fieldType:        schemaTypeString,
			fieldTitle:       firstNonEmpty(stringValue(question, "header"), id),
			fieldDescription: stringValue(question, "question"),
		}
		if options := questionOptions(question["options"]); len(options) > 0 {
			property["oneOf"] = options
		}

		properties[id] = property
		required = append(required, id)
	}

	if len(properties) == 0 {
		properties["answer"] = map[string]any{fieldType: schemaTypeString}
		required = []string{"answer"}
	}

	message := "Codex needs input"
	if len(questions) == 1 {
		message = firstNonEmpty(stringValue(questions[0], "question"), message)
	}

	return &acp.UnstableCreateElicitationForm{
		Message: message,
		Mode:    modeForm,
		RequestedSchema: acp.UnstableElicitationSchema{
			Title: new("Codex input"), Type: acp.UnstableElicitationSchemaTypeObject, Properties: properties, Required: required,
		},
		Meta: meta,
	}, nil
}

// ToolUserInputResponse renders accepted form content as the native answers.
// Nil content answers nothing.
func ToolUserInputResponse(content map[string]any) map[string]any {
	answers := make(map[string]any, len(content))

	for key, value := range content {
		answers[key] = map[string]any{fieldAnswers: stringAnswers(value)}
	}

	return map[string]any{fieldAnswers: answers}
}

func questionOptions(raw any) []map[string]any {
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

		item := map[string]any{"const": label, fieldTitle: label}
		if description := stringValue(option, fieldDescription); description != "" {
			item[fieldDescription] = description
		}

		out = append(out, item)
	}

	return out
}

func elicitationSchema(raw map[string]any) (acp.UnstableElicitationSchema, error) {
	schema := acp.UnstableElicitationSchema{Type: acp.UnstableElicitationSchemaTypeObject, Properties: map[string]any{}}
	if raw == nil {
		return schema, nil
	}

	if title := stringValue(raw, fieldTitle); title != "" {
		schema.Title = &title
	}

	if description := stringValue(raw, fieldDescription); description != "" {
		schema.Description = &description
	}

	schema.Required = stringSliceValue(raw["required"])

	if properties := mapValue(raw, "properties"); properties != nil {
		if secretSchema(properties) {
			return acp.UnstableElicitationSchema{}, ErrElicitationSecret
		}

		schema.Properties = properties
	}

	return schema, nil
}

// secretSchema reports whether a schema asks for a credential anywhere in
// its tree.
func secretSchema(value any) bool {
	switch typed := value.(type) {
	case map[string]any:
		if strings.EqualFold(stringValue(typed, "format"), "password") || truthy(typed["writeOnly"]) || truthy(typed["isSecret"]) {
			return true
		}

		for _, nested := range typed {
			if secretSchema(nested) {
				return true
			}
		}
	case []any:
		if slices.ContainsFunc(typed, secretSchema) {
			return true
		}
	}

	return false
}

func truthy(value any) bool {
	switch typed := value.(type) {
	case bool:
		return typed
	case string:
		parsed, err := strconv.ParseBool(typed)

		return err == nil && parsed
	default:
		return false
	}
}

func stringAnswers(value any) []string {
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
