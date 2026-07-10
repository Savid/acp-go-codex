package codexacp

import (
	"context"
	"fmt"

	"github.com/coder/acp-go-sdk"
	"github.com/savid/acp-go-codex/internal/codex"
)

// handleCodexServerRequest bridges native Codex app-server requests onto ACP
// client calls against the owning session. Native request decoding and native
// response payloads live in internal/codex; this layer owns session lookup,
// client capability gating, and the ACP calls themselves.
func (a *Agent) handleCodexServerRequest(ctx context.Context, req codex.ServerRequest) (any, error) {
	params := codex.ServerRequestParams(req)
	if session := a.sessionByCodexThread(codex.RequestThreadID(params)); session != nil {
		var finish func()

		ctx, finish = session.beginInteraction(ctx, codex.ServerInteractionKey(req, params))
		defer finish()
	}

	var (
		result any
		err    error
	)

	switch req.Method {
	case codex.RequestCommandApproval:
		result, err = a.handleCodexApproval(ctx, req, acp.ToolKindExecute)
	case codex.RequestFileChangeApproval:
		result, err = a.handleCodexApproval(ctx, req, acp.ToolKindEdit)
	case codex.RequestPermissionsApproval:
		result, err = a.handleCodexPermissionsApproval(ctx, req)
	case codex.RequestToolUserInput:
		result, err = a.handleCodexToolUserInput(ctx, req)
	case codex.RequestMCPElicitation:
		result, err = a.handleCodexMCPElicitation(ctx, req)
	case codex.RequestAuthTokenRefresh:
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

	return codex.AuthTokensRefreshResponse(toCodexAuthTokens(tokens)), nil
}

func (a *Agent) handleCodexApproval(ctx context.Context, req codex.ServerRequest, kind acp.ToolKind) (any, error) {
	params := codex.ServerRequestParams(req)

	session := a.sessionByCodexThread(codex.RequestThreadID(params))
	if session == nil {
		return codex.ApprovalCancelResponse(), nil
	}

	conn := a.connection()
	if conn == nil {
		return codex.ApprovalCancelResponse(), nil
	}

	title := codex.ApprovalTitle(req.Method, params)
	status := acp.ToolCallStatusPending

	resp, err := conn.RequestPermission(ctx, acp.RequestPermissionRequest{
		SessionId: session.id,
		ToolCall: acp.ToolCallUpdate{
			ToolCallId: acp.ToolCallId(codex.ApprovalToolCallID(req, params)),
			Title:      &title,
			Kind:       &kind,
			Status:     &status,
			Content:    codex.ApprovalContent(req.Method, params),
			RawInput:   params,
			Meta:       map[string]any{codexMetaKey: params},
		},
		Options: codex.ApprovalOptions(params),
	})
	if err != nil {
		return nil, err
	}

	if resp.Outcome.Selected == nil {
		return codex.ApprovalCancelResponse(), nil
	}

	return codex.ApprovalDecisionResponse(resp.Outcome.Selected.OptionId, params), nil
}

func (a *Agent) handleCodexPermissionsApproval(ctx context.Context, req codex.ServerRequest) (any, error) {
	params := codex.ServerRequestParams(req)

	session := a.sessionByCodexThread(codex.RequestThreadID(params))
	if session == nil {
		return codex.PermissionsDeniedResponse(), nil
	}

	conn := a.connection()
	if conn == nil {
		return codex.PermissionsDeniedResponse(), nil
	}

	kind := acp.ToolKindOther
	status := acp.ToolCallStatusPending
	title := codex.PermissionsApprovalTitle(params)

	resp, err := conn.RequestPermission(ctx, acp.RequestPermissionRequest{
		SessionId: session.id,
		ToolCall: acp.ToolCallUpdate{
			ToolCallId: acp.ToolCallId(codex.PermissionsToolCallID(req, params)),
			Title:      &title,
			Kind:       &kind,
			Status:     &status,
			Content:    codex.PermissionsApprovalContent(params),
			RawInput:   params,
			Meta:       map[string]any{codexMetaKey: params},
		},
		Options: codex.PermissionsApprovalOptions(),
	})
	if err != nil {
		return nil, err
	}

	if resp.Outcome.Selected == nil {
		return codex.PermissionsDeniedResponse(), nil
	}

	return codex.PermissionsApprovalResponse(resp.Outcome.Selected.OptionId, params), nil
}

func (a *Agent) handleCodexToolUserInput(ctx context.Context, req codex.ServerRequest) (any, error) {
	params := codex.ServerRequestParams(req)

	session := a.sessionByCodexThread(codex.RequestThreadID(params))
	if session == nil {
		return codex.EmptyToolUserInputResponse(), nil
	}

	conn := a.connection()
	if conn == nil {
		return codex.EmptyToolUserInputResponse(), nil
	}

	if !a.clientSupportsFormElicitation() {
		return codex.EmptyToolUserInputResponse(), nil
	}

	resp, err := conn.CreateElicitation(ctx, acp.UnstableCreateElicitationRequest{
		Form: codex.ToolUserInputForm(params, map[string]any{codexMetaKey: params}),
	}, elicitationScope{
		SessionID:  session.id,
		ToolCallID: acp.ToolCallId(codex.ToolUserInputToolCallID(req, params)),
	})
	if err != nil {
		return nil, err
	}

	if resp.Accept == nil {
		return codex.EmptyToolUserInputResponse(), nil
	}

	return codex.ToolUserInputResponse(resp.Accept.Content), nil
}

func (a *Agent) handleCodexMCPElicitation(ctx context.Context, req codex.ServerRequest) (any, error) {
	params := codex.ServerRequestParams(req)
	if codex.IsMCPToolApproval(params) {
		return a.handleCodexMCPToolApproval(ctx, req, params)
	}

	return a.handleCodexMCPUserElicitation(ctx, req, params)
}

func (a *Agent) handleCodexMCPToolApproval(ctx context.Context, req codex.ServerRequest, params map[string]any) (any, error) {
	session := a.sessionByCodexThread(codex.RequestThreadID(params))
	if session == nil {
		return codex.ElicitationCancelResponse(), nil
	}

	conn := a.connection()
	if conn == nil {
		return codex.ElicitationCancelResponse(), nil
	}

	title := codex.MCPToolApprovalTitle(params)
	kind := acp.ToolKindOther
	status := acp.ToolCallStatusPending

	resp, err := conn.RequestPermission(ctx, acp.RequestPermissionRequest{
		SessionId: session.id,
		ToolCall: acp.ToolCallUpdate{
			ToolCallId: acp.ToolCallId(codex.MCPToolApprovalToolCallID(req, params)),
			Title:      &title,
			Kind:       &kind,
			Status:     &status,
			Content:    codex.MCPToolApprovalContent(params),
			RawInput:   params,
			Meta:       map[string]any{codexMetaKey: params},
		},
		Options: codex.MCPToolApprovalOptions(),
	})
	if err != nil {
		return nil, err
	}

	if resp.Outcome.Selected == nil {
		return codex.ElicitationCancelResponse(), nil
	}

	return codex.MCPToolApprovalResponse(resp.Outcome.Selected.OptionId), nil
}

func (a *Agent) handleCodexMCPUserElicitation(ctx context.Context, req codex.ServerRequest, params map[string]any) (any, error) {
	conn := a.connection()
	if conn == nil {
		return codex.ElicitationCancelResponse(), nil
	}

	if codex.IsURLElicitation(params) {
		if !a.clientSupportsURLElicitation() {
			return codex.ElicitationDeclineResponse(), nil
		}
	} else if !a.clientSupportsFormElicitation() {
		return codex.ElicitationDeclineResponse(), nil
	}

	scope := elicitationScope{
		ToolCallID: acp.ToolCallId(codex.MCPElicitationToolCallID(req, params)),
		RequestID:  codex.RequestIDFromRaw(req.ID),
	}
	if session := a.sessionByCodexThread(codex.RequestThreadID(params)); session != nil {
		scope.SessionID = session.id
	}

	resp, err := conn.CreateElicitation(ctx, codex.MCPElicitationRequest(params, map[string]any{codexMetaKey: params}), scope)
	if err != nil {
		return nil, err
	}

	switch {
	case resp.Accept != nil:
		return codex.ElicitationAcceptResponse(resp.Accept.Content, resp.Accept.Meta), nil
	case resp.Decline != nil:
		return codex.ElicitationDeclineMetaResponse(resp.Decline.Meta), nil
	default:
		return codex.ElicitationCancelResponse(), nil
	}
}
