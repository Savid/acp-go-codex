package codexacp

import (
	"context"
	"fmt"

	"github.com/coder/acp-go-sdk"
	"github.com/savid/acp-go-codex/internal/codex"
	"github.com/savid/acp-go-codex/internal/lifecycle"
)

// handleCodexServerRequest bridges native Codex app-server requests onto ACP
// client calls against the owning session. Native request decoding and native
// response payloads live in internal/codex; this layer owns session lookup,
// client capability gating, and the ACP calls themselves.
func (a *Agent) handleCodexServerRequest(ctx context.Context, req codex.ServerRequest) (any, error) {
	params := codex.ServerRequestParams(req)
	if req.Method != codex.RequestAuthTokenRefresh {
		threadID := codex.RequestThreadID(params)
		if threadID == "" {
			return nil, fmt.Errorf("codex server request %q omitted required threadId", req.Method)
		}

		session := a.sessionByCodexThread(threadID)
		if session == nil {
			return nil, fmt.Errorf("codex server request %q addressed unknown threadId", req.Method)
		}

		var finish func()

		ctx, finish = session.beginInteraction(ctx, codex.ServerInteractionKey(req, params))
		defer finish()
		// A request names the authoritative native turn. On the session-owned
		// stream it either binds to the accepted prompt or is itself the proof
		// that opens an agent-origin turn between prompts.
		turn, lifecycleOwned, err := session.claimLifecycleTurn(ctx, codex.RequestTurnID(params))
		if err == nil && lifecycleOwned {
			ctx = withLifecycleActionTurn(withTurnRoute(ctx, turn.turnNonce), turn)
		}

		if err == nil && !lifecycleOwned {
			err = session.waitForTurnBinding(ctx)
		}

		if err != nil {
			if cancellation, ok := codexPermissionCancellationResponse(req.Method, params); ok {
				return cancellation, nil
			}

			return nil, err
		}
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

	if ctxErr := ctx.Err(); ctxErr != nil {
		if cancellation, ok := codexPermissionCancellationResponse(req.Method, params); ok {
			return cancellation, nil
		}

		return nil, ctxErr
	}

	return result, err
}

func codexPermissionCancellationResponse(method string, params map[string]any) (any, bool) {
	switch method {
	case codex.RequestCommandApproval, codex.RequestFileChangeApproval:
		return codex.ApprovalCancelResponse(), true
	case codex.RequestPermissionsApproval:
		return codex.PermissionsDeniedResponse(), true
	case codex.RequestMCPElicitation:
		if codex.IsMCPToolApproval(params) {
			return codex.ElicitationCancelResponse(), true
		}
	}

	return nil, false
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

	if _, active := session.activeTurnNonceForNativeTurn(codex.RequestTurnID(params)); !active {
		return codex.ApprovalCancelResponse(), nil
	}

	conn := a.connection()
	if conn == nil {
		return codex.ApprovalCancelResponse(), nil
	}

	title := codex.ApprovalTitle(req.Method, params)
	status := acp.ToolCallStatusPending

	resp, requested, err := session.requestPermissionForTool(ctx, conn, acp.RequestPermissionRequest{
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
	}, permissionToolClassForApprovalKind(kind))
	if err != nil {
		return nil, err
	}

	if !requested {
		return codex.ApprovalCancelResponse(), nil
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

	if _, active := session.activeTurnNonceForNativeTurn(codex.RequestTurnID(params)); !active {
		return codex.PermissionsDeniedResponse(), nil
	}

	conn := a.connection()
	if conn == nil {
		return codex.PermissionsDeniedResponse(), nil
	}

	kind := acp.ToolKindOther
	status := acp.ToolCallStatusPending
	title := codex.PermissionsApprovalTitle(params)

	resp, requested, err := session.requestPermissionForTool(ctx, conn, acp.RequestPermissionRequest{
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
	}, permissionToolPermissions)
	if err != nil {
		return nil, err
	}

	if !requested {
		return codex.PermissionsDeniedResponse(), nil
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

	turnNonce, active := session.activeTurnNonceForNativeTurn(codex.RequestTurnID(params))
	if !active {
		return codex.EmptyToolUserInputResponse(), nil
	}

	conn := a.connection()
	if conn == nil {
		return codex.EmptyToolUserInputResponse(), nil
	}

	if !a.clientSupportsFormElicitation() {
		return codex.EmptyToolUserInputResponse(), nil
	}

	form, err := codex.ToolUserInputForm(params, map[string]any{codexMetaKey: params})
	if err != nil {
		a.log.WarnContext(ctx, "refused a Codex tool question that asks the user for a secret")

		// The tool question surface has no decline verb: an empty answer set is
		// how every refusal on this leg is reported, and it keeps the refused
		// question away from ToolUserInputResponse entirely.
		return codex.EmptyToolUserInputResponse(), nil //nolint:nilerr // A refused question is answered, not failed.
	}

	action, correlation, err := session.beginAction(ctx, lifecycle.ActionElicitation, true)
	if err != nil {
		return nil, err
	}

	resp, err := createElicitationWithAction(ctx, conn, acp.UnstableCreateElicitationRequest{
		Form: form,
	}, elicitationScope{
		SessionID:         session.id,
		TurnNonce:         turnNonce,
		ToolCallID:        acp.ToolCallId(codex.ToolUserInputToolCallID(req, params)),
		ActionCorrelation: correlation,
	}, action, nil)

	if resolveErr := action.resolve(ctx, elicitationActionState(resp, err)); resolveErr != nil {
		return nil, resolveErr
	}

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

	if _, active := session.activeTurnNonceForNativeTurn(codex.RequestTurnID(params)); !active {
		return codex.ElicitationCancelResponse(), nil
	}

	conn := a.connection()
	if conn == nil {
		return codex.ElicitationCancelResponse(), nil
	}

	title := codex.MCPToolApprovalTitle(params)
	kind := acp.ToolKindOther
	status := acp.ToolCallStatusPending

	resp, requested, err := session.requestPermissionForTool(ctx, conn, acp.RequestPermissionRequest{
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
	}, permissionToolMCP)
	if err != nil {
		return nil, err
	}

	if !requested {
		return codex.ElicitationCancelResponse(), nil
	}

	if resp.Outcome.Selected == nil {
		return codex.ElicitationCancelResponse(), nil
	}

	return codex.MCPToolApprovalResponse(resp.Outcome.Selected.OptionId), nil
}

func permissionToolClassForApprovalKind(kind acp.ToolKind) permissionToolClass {
	if kind == acp.ToolKindEdit {
		return permissionToolFileChange
	}

	return permissionToolCommand
}

func (a *Agent) handleCodexMCPUserElicitation(ctx context.Context, req codex.ServerRequest, params map[string]any) (any, error) {
	session := a.sessionByCodexThread(codex.RequestThreadID(params))
	if session == nil {
		return codex.ElicitationCancelResponse(), nil
	}

	turnNonce, active := session.activeTurnNonceForNativeTurn(codex.RequestTurnID(params))
	if !active {
		return codex.ElicitationCancelResponse(), nil
	}

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

	request, err := codex.MCPElicitationRequest(params, map[string]any{codexMetaKey: params})
	if err != nil {
		a.log.WarnContext(ctx, "refused an MCP elicitation whose schema asks the user for a secret")

		// Decline, not cancel: the request reached a decision and was refused,
		// which is what the MCP server and the model should be told. Cancel
		// claims the interaction went away, which would be a lie here.
		return codex.ElicitationDeclineResponse(), nil //nolint:nilerr // A refused elicitation is declined, not failed.
	}

	nativeToolID := codex.MCPUserElicitationToolCallID(params)

	var resp acp.UnstableCreateElicitationResponse

	if nativeToolID != "" {
		var associated bool

		resp, associated, err = session.createElicitationForMCPTool(ctx, conn, request, nativeToolID, params)
		if !associated && err == nil {
			return codex.ElicitationCancelResponse(), nil
		}
	} else {
		requestID := codex.MCPUserElicitationRequestID(req, params)
		if requestID == nil {
			adapterID, idErr := newElicitationRequestID()
			if idErr != nil {
				return nil, idErr
			}

			requestIDValue := acp.RequestIdStr(adapterID)
			requestID = &acp.RequestId{Str: &requestIDValue}
		}

		var action *liveAction

		action, correlation, actionErr := session.beginAction(ctx, lifecycle.ActionElicitation, true)
		if actionErr != nil {
			return nil, actionErr
		}

		resp, err = createElicitationWithAction(ctx, conn, request, elicitationScope{
			SessionID:         session.id,
			TurnNonce:         turnNonce,
			RequestID:         requestID,
			ActionCorrelation: correlation,
		}, action, nil)

		if resolveErr := action.resolve(ctx, elicitationActionState(resp, err)); resolveErr != nil {
			return nil, resolveErr
		}
	}

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
