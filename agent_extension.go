package codexacp

import (
	"context"
	"encoding/json"

	"github.com/coder/acp-go-sdk"
	"github.com/savid/acp-go-codex/internal/codex"
)

const (
	codexTurnSteerMethod           = "_codex/turn/steer"
	codexThreadCompactMethod       = "_codex/thread/compact"
	codexReviewStartMethod         = "_codex/review/start"
	codexThreadReadMethod          = "_codex/thread/read"
	codexThreadTurnsListMethod     = "_codex/thread/turns/list"
	codexCollaborationListMethod   = "_codex/collaborationMode/list"
	codexMCPServerStatusListMethod = "_codex/mcpServerStatus/list"
)

// HandleExtensionMethod handles Codex-specific ACP extension methods.
func (a *Agent) HandleExtensionMethod(ctx context.Context, method string, params json.RawMessage) (any, error) {
	if err := a.ensureOpen(); err != nil {
		return nil, err
	}
	switch method {
	case codexSessionImportMethod:
		return a.importCodexSession(ctx, params)
	case codexSessionImportChunkMethod:
		return a.importCodexSessionChunk(ctx, params)
	case codexSessionCommitImportMethod:
		return a.commitCodexSessionImport(ctx, params)
	case codexSessionAbortImportMethod:
		return a.abortCodexSessionImport(ctx, params)
	case codexSessionSetGoalMethod:
		return a.setCodexGoal(ctx, params)
	case codexTurnSteerMethod:
		return a.steerCodexTurn(ctx, params)
	case codexThreadCompactMethod:
		return a.compactCodexThread(ctx, params)
	case codexReviewStartMethod:
		return a.startCodexReview(ctx, params)
	case codexThreadReadMethod:
		return a.readCodexThread(ctx, params)
	case codexThreadTurnsListMethod:
		return a.listCodexThreadTurns(ctx, params)
	case codexCollaborationListMethod:
		return a.listCodexCollaborationModes(ctx, params)
	case codexMCPServerStatusListMethod:
		return a.listCodexMCPServerStatus(ctx, params)
	default:
		return nil, acp.NewMethodNotFound(method)
	}
}

type codexSessionExtensionParams struct {
	SessionID acp.SessionId `json:"sessionId"`
	ThreadID  string        `json:"threadId,omitempty"`
}

type codexTurnSteerParams struct {
	SessionID      acp.SessionId      `json:"sessionId"`
	ThreadID       string             `json:"threadId,omitempty"`
	ExpectedTurnID string             `json:"expectedTurnId,omitempty"`
	Input          []codex.UserInput  `json:"input,omitempty"`
	Prompt         []acp.ContentBlock `json:"prompt,omitempty"`
}

type codexReviewStartParams struct {
	SessionID acp.SessionId  `json:"sessionId"`
	ThreadID  string         `json:"threadId,omitempty"`
	Target    map[string]any `json:"target,omitempty"`
	Delivery  string         `json:"delivery,omitempty"`
}

type codexTurnsListParams struct {
	SessionID     acp.SessionId `json:"sessionId"`
	ThreadID      string        `json:"threadId,omitempty"`
	Cursor        string        `json:"cursor,omitempty"`
	Limit         int           `json:"limit,omitempty"`
	SortDirection string        `json:"sortDirection,omitempty"`
}

func (a *Agent) extensionSession(params codexSessionExtensionParams) (*Session, string, error) {
	if params.SessionID != "" {
		session, err := a.session(params.SessionID)
		if err != nil {
			return nil, "", err
		}
		return session, firstNonEmpty(params.ThreadID, session.codexThreadID), nil
	}
	if params.ThreadID != "" {
		session := a.sessionByCodexThread(params.ThreadID)
		if session != nil {
			return session, params.ThreadID, nil
		}
	}

	return nil, "", acp.NewInvalidParams(map[string]any{"sessionId": validationRequired})
}

func (a *Agent) steerCodexTurn(ctx context.Context, params json.RawMessage) (any, error) {
	var req codexTurnSteerParams
	if err := json.Unmarshal(params, &req); err != nil {
		return nil, acp.NewInvalidParams(map[string]any{jsonFieldError: err.Error()})
	}
	session, threadID, err := a.extensionSession(codexSessionExtensionParams{SessionID: req.SessionID, ThreadID: req.ThreadID})
	if err != nil {
		return nil, err
	}
	if req.ExpectedTurnID == "" {
		req.ExpectedTurnID = session.activeTurnID()
	}
	if req.ExpectedTurnID == "" {
		return nil, acp.NewInvalidParams(map[string]any{"expectedTurnId": validationRequired})
	}
	input := req.Input
	if len(input) == 0 && len(req.Prompt) > 0 {
		input, err = promptToCodex(req.Prompt)
		if err != nil {
			return nil, err
		}
	}
	if len(input) == 0 {
		return nil, acp.NewInvalidParams(map[string]any{"input": validationRequired})
	}
	if err := session.client.SteerTurn(ctx, codex.TurnSteerRequest{ThreadID: threadID, ExpectedTurnID: req.ExpectedTurnID, Input: input}); err != nil {
		return nil, codexThreadACPError(err, session.accountMetaSnapshot(), codexThreadErrorData(session.id, threadID))
	}

	return map[string]any{"threadId": threadID, "turnId": req.ExpectedTurnID}, nil
}

func (a *Agent) compactCodexThread(ctx context.Context, params json.RawMessage) (any, error) {
	var req codexSessionExtensionParams
	if err := json.Unmarshal(params, &req); err != nil {
		return nil, acp.NewInvalidParams(map[string]any{jsonFieldError: err.Error()})
	}
	session, threadID, err := a.extensionSession(req)
	if err != nil {
		return nil, err
	}

	result, err := session.client.CompactThread(ctx, codex.ThreadCompactRequest{ThreadID: threadID})
	return result, codexThreadACPError(err, session.accountMetaSnapshot(), codexThreadErrorData(session.id, threadID))
}

func (a *Agent) startCodexReview(ctx context.Context, params json.RawMessage) (any, error) {
	var req codexReviewStartParams
	if err := json.Unmarshal(params, &req); err != nil {
		return nil, acp.NewInvalidParams(map[string]any{jsonFieldError: err.Error()})
	}
	session, threadID, err := a.extensionSession(codexSessionExtensionParams{SessionID: req.SessionID, ThreadID: req.ThreadID})
	if err != nil {
		return nil, err
	}
	if req.Target == nil {
		req.Target = map[string]any{"type": "uncommittedChanges"}
	}

	result, err := session.client.StartReview(ctx, codex.ReviewStartRequest{ThreadID: threadID, Target: req.Target, Delivery: req.Delivery})
	return result, codexThreadACPError(err, session.accountMetaSnapshot(), codexThreadErrorData(session.id, threadID))
}

func (a *Agent) readCodexThread(ctx context.Context, params json.RawMessage) (any, error) {
	var req codexSessionExtensionParams
	if err := json.Unmarshal(params, &req); err != nil {
		return nil, acp.NewInvalidParams(map[string]any{jsonFieldError: err.Error()})
	}
	session, threadID, err := a.extensionSession(req)
	if err != nil {
		return nil, err
	}

	result, err := session.client.ReadThread(ctx, codex.ThreadReadRequest{ThreadID: threadID})
	return result, codexThreadACPError(err, session.accountMetaSnapshot(), codexThreadErrorData(session.id, threadID))
}

func (a *Agent) listCodexThreadTurns(ctx context.Context, params json.RawMessage) (any, error) {
	var req codexTurnsListParams
	if err := json.Unmarshal(params, &req); err != nil {
		return nil, acp.NewInvalidParams(map[string]any{jsonFieldError: err.Error()})
	}
	session, threadID, err := a.extensionSession(codexSessionExtensionParams{SessionID: req.SessionID, ThreadID: req.ThreadID})
	if err != nil {
		return nil, err
	}

	result, err := session.client.ListTurns(ctx, codex.ThreadTurnsListRequest{
		ThreadID:      threadID,
		Cursor:        req.Cursor,
		Limit:         req.Limit,
		SortDirection: req.SortDirection,
	})
	return result, codexThreadACPError(err, session.accountMetaSnapshot(), codexThreadErrorData(session.id, threadID))
}

func (a *Agent) listCodexCollaborationModes(ctx context.Context, params json.RawMessage) (any, error) {
	var req codexSessionExtensionParams
	if err := json.Unmarshal(params, &req); err != nil {
		return nil, acp.NewInvalidParams(map[string]any{jsonFieldError: err.Error()})
	}
	session, _, err := a.extensionSession(req)
	if err != nil {
		return nil, err
	}

	return session.client.CollaborationModeList(ctx)
}

func (a *Agent) listCodexMCPServerStatus(ctx context.Context, params json.RawMessage) (any, error) {
	var req codexSessionExtensionParams
	if err := json.Unmarshal(params, &req); err != nil {
		return nil, acp.NewInvalidParams(map[string]any{jsonFieldError: err.Error()})
	}
	session, _, err := a.extensionSession(req)
	if err != nil {
		return nil, err
	}

	return session.client.MCPServerStatusList(ctx)
}
