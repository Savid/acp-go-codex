package codexacp

import (
	"cmp"
	"context"
	"errors"
	"log/slog"

	"github.com/coder/acp-go-sdk"

	"github.com/savid/acp-go-codex/internal/codex"
	"github.com/savid/acp-go-core/lifecycle"
	"github.com/savid/acp-go-core/observer"
	"github.com/savid/acp-go-core/wire"
)

// optionAccept is the native decision that allows one approval once.
const optionAccept acp.PermissionOptionId = "accept"

// handleRequest answers one server request the app-server sent for this
// session's thread. Exactly one response reaches the app-server.
func (s *session) handleRequest(rt *runtime, request codex.ServerRequest, params map[string]any) {
	var c *cycle

	s.mu.Lock()
	t := s.turn

	switch {
	case s.turn != nil:
		c = &s.turn.cycle
	case s.cycle != nil:
		c = s.cycle
	}

	closing := s.closing
	s.mu.Unlock()

	ctx, cancel := context.WithCancelCause(context.Background())
	unregister := s.registerDialog(string(request.ID), cancel)

	if c == nil || closing || ctx.Err() != nil {
		cancel(nil)
		unregister()
		s.agent.respondUnowned(rt, request)

		return
	}

	if t != nil {
		s.acceptTurn(ctx, t)
	}

	go func() {
		defer cancel(nil)
		defer unregister()

		// A prompt's dialogs end with its turn, resolved as cancelled like
		// every other session-ended dialog; an agent-origin cycle has no turn
		// and its dialogs end with the session.
		if t != nil {
			stop := context.AfterFunc(t.ctx, func() { cancel(errDialogCancelled) })
			defer stop()
		}

		var response any

		switch request.Method {
		case codex.RequestCommandApproval, codex.RequestFileChangeApproval:
			selected := s.requestPermission(ctx, c, permissionRequest{
				toolCallID: cmp.Or(codex.RequestItemID(params), string(request.ID)),
				title:      codex.ApprovalTitle(request.Method, params),
				kind:       codex.ApprovalKind(request.Method),
				content:    codex.ApprovalContent(request.Method, params),
				rawInput:   params,
				options:    codex.ApprovalOptions(params),
			})
			response = codex.ApprovalResponse(selected, params)
		case codex.RequestPermissionsApproval:
			selected := s.requestPermission(ctx, c, permissionRequest{
				toolCallID: cmp.Or(codex.RequestItemID(params), string(request.ID)),
				title:      codex.ApprovalTitle(request.Method, params),
				kind:       acp.ToolKindOther,
				content:    codex.ApprovalContent(request.Method, params),
				rawInput:   params,
				options:    codex.PermissionsOptions(),
			})
			response = codex.PermissionsResponse(selected, params)
		case codex.RequestToolUserInput:
			response = s.toolUserInput(ctx, c, params)
		case codex.RequestMCPElicitation:
			if codex.IsMCPToolApproval(params) {
				selected := s.requestPermission(ctx, c, permissionRequest{
					toolCallID: cmp.Or(codex.RequestItemID(params), string(request.ID)),
					title:      codex.MCPToolApprovalTitle(params),
					kind:       acp.ToolKindOther,
					rawInput:   params,
					options:    codex.MCPToolApprovalOptions(),
				})
				response = codex.MCPToolApprovalResponse(selected)
			} else {
				response = s.mcpElicitation(ctx, c, params)
			}
		default:
			s.agent.respondUnowned(rt, request)

			return
		}

		if err := rt.client.Respond(request, response, nil); err != nil {
			s.agent.log.DebugContext(ctx, "respond to codex request failed", slog.String("session_id", string(s.id)))
		}
	}()
}

// permissionRequest is what one native approval asks the host.
type permissionRequest struct {
	toolCallID string
	title      string
	kind       acp.ToolKind
	content    []acp.ToolCallContent
	rawInput   any
	options    []acp.PermissionOption
}

// requestPermission maps one native approval to session/request_permission
// and returns the selected option, or nil when the host cancelled, declined
// to answer, or the request failed.
func (s *session) requestPermission(ctx context.Context, c *cycle, request permissionRequest) *acp.PermissionOptionId {
	conn := s.agent.connection()
	if conn == nil {
		return nil
	}

	if err := s.publishPendingTool(ctx, &c.state, request); err != nil {
		return nil
	}

	ctx, finish := s.agent.observe.StartPermission(ctx, request.title, "ask")

	status := acp.ToolCallStatusPending
	title := request.title

	toolCall := acp.ToolCallUpdate{
		ToolCallId: acp.ToolCallId(request.toolCallID),
		Title:      &title,
		Kind:       &request.kind,
		Status:     &status,
		Content:    request.content,
		RawInput:   request.rawInput,
	}

	resp, err := announcedRequest(ctx, s, c, lifecycle.ActionPermission,
		func(requestCtx context.Context, meta map[string]any) (acp.RequestPermissionResponse, error) {
			return conn.RequestPermission(requestCtx, acp.RequestPermissionRequest{
				Meta:      meta,
				SessionId: s.id,
				ToolCall:  toolCall,
				Options:   request.options,
			})
		},
		func(resp acp.RequestPermissionResponse, err error) lifecycle.ActionState {
			switch {
			case err != nil:
				return lifecycle.ActionFailed
			case resp.Outcome.Selected == nil:
				return lifecycle.ActionCancelled
			case allows(resp.Outcome.Selected.OptionId):
				return lifecycle.ActionAccepted
			default:
				return lifecycle.ActionDeclined
			}
		})

	behavior := "cancelled"
	if err == nil && resp.Outcome.Selected != nil {
		behavior = string(resp.Outcome.Selected.OptionId)
	}

	finish(observer.PermissionResult{Behavior: behavior, Mode: "ask", ToolName: request.title})

	if err != nil || resp.Outcome.Selected == nil {
		return nil
	}

	selected := resp.Outcome.Selected.OptionId

	return &selected
}

func allows(option acp.PermissionOptionId) bool {
	switch option {
	case optionAccept, "acceptForSession", "acceptWithExecpolicyAmendment", "grant-turn", "grant-session":
		return true
	default:
		return false
	}
}

// toolUserInput relays one tool question set as a form elicitation. Without
// form support on the client, or for a question asking for a secret, the
// request is answered with no answers.
func (s *session) toolUserInput(ctx context.Context, c *cycle, params map[string]any) map[string]any {
	empty := codex.ToolUserInputResponse(nil)

	conn := s.agent.connection()
	form, _ := s.agent.elicitationModes()

	if conn == nil || !form {
		return empty
	}

	resp, err := s.elicit(ctx, c, func(meta map[string]any) (acp.UnstableCreateElicitationRequest, error) {
		formRequest, err := codex.ToolUserInputForm(params, meta)
		if err != nil {
			return acp.UnstableCreateElicitationRequest{}, err
		}

		return acp.UnstableCreateElicitationRequest{Form: formRequest}, nil
	})
	if err != nil || resp.Accept == nil {
		return empty
	}

	return codex.ToolUserInputResponse(resp.Accept.Content)
}

// mcpElicitation relays one MCP elicitation in the mode it asks for, when the
// client advertised that mode; otherwise it is cancelled natively.
func (s *session) mcpElicitation(ctx context.Context, c *cycle, params map[string]any) map[string]any {
	conn := s.agent.connection()
	form, url := s.agent.elicitationModes()

	if conn == nil {
		return codex.ElicitationCancelResponse()
	}

	resp, err := s.elicit(ctx, c, func(meta map[string]any) (acp.UnstableCreateElicitationRequest, error) {
		request, err := codex.MCPElicitationRequest(params, meta)
		if err != nil {
			return acp.UnstableCreateElicitationRequest{}, err
		}

		if (request.Url != nil && !url) || (request.Form != nil && !form) {
			return acp.UnstableCreateElicitationRequest{}, errElicitationModeUnsupported
		}

		return request, nil
	})
	if err != nil {
		return codex.ElicitationCancelResponse()
	}

	return codex.ElicitationResponse(resp)
}

var errElicitationModeUnsupported = errors.New("client does not support the elicitation mode")

// elicit sends one elicitation as an announced action. build renders the
// request once the action id is minted; an error from it declines natively.
func (s *session) elicit(
	ctx context.Context,
	c *cycle,
	build func(meta map[string]any) (acp.UnstableCreateElicitationRequest, error),
) (acp.UnstableCreateElicitationResponse, error) {
	conn := s.agent.connection()

	ctx, finish := s.agent.observe.StartElicitation(ctx)

	resp, err := announcedRequest(ctx, s, c, lifecycle.ActionElicitation,
		func(requestCtx context.Context, meta map[string]any) (acp.UnstableCreateElicitationResponse, error) {
			request, buildErr := build(meta)
			if buildErr != nil {
				return acp.UnstableCreateElicitationResponse{}, buildErr
			}

			return conn.UnstableCreateElicitation(requestCtx, request)
		},
		func(resp acp.UnstableCreateElicitationResponse, err error) lifecycle.ActionState {
			switch {
			case err != nil:
				return lifecycle.ActionFailed
			case resp.Accept != nil:
				return lifecycle.ActionAccepted
			case resp.Decline != nil:
				return lifecycle.ActionDeclined
			default:
				return lifecycle.ActionCancelled
			}
		})

	finish(observer.ElicitationResult{Accepted: err == nil && resp.Accept != nil, Err: err})

	return resp, err
}

// announcedRequest sends one client request that holds native work, announces
// the action it answers once the request is on the wire, and resolves that
// action exactly once.
func announcedRequest[T any](
	ctx context.Context,
	s *session,
	c *cycle,
	kind lifecycle.ActionKind,
	send func(context.Context, map[string]any) (T, error),
	resolved func(T, error) lifecycle.ActionState,
) (T, error) {
	var zero T

	releaseCall, err := s.agent.acquireClientCall()
	if err != nil {
		return zero, err
	}
	defer releaseCall()

	actionID := s.reserveAction(c)
	if actionID == "" {
		return send(ctx, nil)
	}

	value, callErr := wire.CallAndAnnounce(ctx, s.agent.transportRef(), s.lc.Correlation(c.Cycle, actionID), send, func() {
		if err := s.lc.ActionPending(ctx, c.Cycle, actionID, kind); err != nil {
			s.agent.log.ErrorContext(ctx, "announce lifecycle action failed",
				slog.String("session_id", string(s.id)), slog.String("reason", err.Error()))
		}
	})

	state := resolved(value, callErr)

	if callErr != nil && errors.Is(context.Cause(ctx), errDialogCancelled) {
		state = lifecycle.ActionCancelled
	}

	if err := s.lc.ActionResolved(context.WithoutCancel(ctx), c.Cycle, actionID, state); err != nil {
		s.agent.log.ErrorContext(ctx, "resolve lifecycle action failed",
			slog.String("session_id", string(s.id)), slog.String("reason", err.Error()))
	}

	return value, callErr
}

// reserveAction mints the action id one announced request publishes under, or
// an empty id when the session has no open incarnation or cycle to own it.
func (s *session) reserveAction(c *cycle) string {
	s.lcMu.Lock()
	defer s.lcMu.Unlock()

	if !s.lc.Active() || c.TurnID == "" {
		return ""
	}

	return s.lc.NextID("action")
}
