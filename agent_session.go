package codexacp

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/coder/acp-go-sdk"
	"github.com/savid/acp-go-codex/internal/codex"
)

func (a *Agent) NewSession(ctx context.Context, params acp.NewSessionRequest) (acp.NewSessionResponse, error) {
	ctx = a.observe.Extract(ctx, params.Meta)
	if err := a.ensureOpen(); err != nil {
		return acp.NewSessionResponse{}, err
	}
	if err := validateSessionStartPaths(params.Cwd, params.AdditionalDirectories); err != nil {
		return acp.NewSessionResponse{}, err
	}
	meta, err := sessionMetaFromLifecycle(params.Meta)
	if err != nil {
		return acp.NewSessionResponse{}, lifecycleMetaError(err)
	}
	start := codexSessionStart{
		Cwd:                   params.Cwd,
		AdditionalDirectories: params.AdditionalDirectories,
		McpServers:            params.McpServers,
		Meta:                  meta,
	}

	idValue, err := newSessionID()
	if err != nil {
		return acp.NewSessionResponse{}, err
	}
	id := acp.SessionId(idValue)
	mcpServers, err := a.prepareMCPServers(ctx, id, params.McpServers)
	if err != nil {
		return acp.NewSessionResponse{}, err
	}
	client, err := a.newClient(ctx, mcpServers, meta.Env, meta.MCPToolApprovalMode)
	if err != nil {
		return acp.NewSessionResponse{}, err
	}

	thread, err := client.StartThread(ctx, codex.ThreadStartRequest{
		Cwd:                   params.Cwd,
		AdditionalDirectories: params.AdditionalDirectories,
		Model:                 firstNonEmpty(meta.Model, a.options.DefaultModel),
		ServiceTier:           meta.ServiceTier,
		Personality:           meta.Personality,
		ApprovalPolicy:        meta.ApprovalPolicy,
		Sandbox:               sandboxMode(meta.SandboxPolicy),
	})
	if err != nil {
		_ = client.Close(context.Background())
		return acp.NewSessionResponse{}, codexAuthRequiredError(err, nil)
	}

	session := newSession(a, id, params.Cwd, params.AdditionalDirectories, thread, client, meta)
	session.fingerprint = codexSessionStartFingerprint(start)
	session.setAccount(clientAccountMeta(ctx, client))
	if err := a.storeStartedSession(session); err != nil {
		_ = session.Close(context.Background())
		return acp.NewSessionResponse{}, err
	}
	models := modelList(ctx, client)
	snapshot := session.snapshot()

	return acp.NewSessionResponse{
		SessionId:     id,
		Meta:          sessionResponseMeta(snapshot),
		ConfigOptions: sessionConfigOptions(session, models),
	}, nil
}

func (a *Agent) Prompt(ctx context.Context, params acp.PromptRequest) (resp acp.PromptResponse, err error) {
	session, err := a.session(params.SessionId)
	if err != nil {
		return acp.PromptResponse{}, err
	}
	ctx, finish := a.observe.StartPrompt(ctx, params.Meta, session.currentModel())
	defer func() { finish(promptResultForObserver(resp, err, session.currentModel())) }()

	resp, err = session.Prompt(ctx, params)
	if err != nil && fatalCodexProcessError(err) {
		if a.removeSessionIf(params.SessionId, session) {
			a.observe.AddActiveSession(ctx, -1)
		}
		_ = session.Close(context.Background())

		return acp.PromptResponse{}, acp.NewInternalError(map[string]any{
			jsonFieldError:   err.Error(),
			jsonFieldMessage: "The Codex app-server process exited unexpectedly. Please start a new session.",
		})
	}

	return resp, err
}

func (a *Agent) Cancel(ctx context.Context, params acp.CancelNotification) error {
	session, err := a.session(params.SessionId)
	if err != nil {
		return err
	}
	session.cancelTurn()
	cancelCtx, cancel := context.WithTimeout(context.Background(), closeTimeout)
	defer cancel()

	return codexThreadACPError(
		session.client.CancelTurn(cancelCtx, session.codexThreadID, session.activeTurnID()),
		session.accountMetaSnapshot(),
		codexThreadErrorData(session.id, session.codexThreadID),
	)
}

func (a *Agent) CloseSession(ctx context.Context, params acp.CloseSessionRequest) (acp.CloseSessionResponse, error) {
	session, err := a.session(params.SessionId)
	if err != nil {
		return acp.CloseSessionResponse{}, err
	}
	closeErr := session.Close(ctx)
	if a.removeSessionIf(params.SessionId, session) {
		a.observe.AddActiveSession(ctx, -1)
	}

	return acp.CloseSessionResponse{}, closeErr
}

func (a *Agent) UnstableDeleteSession(ctx context.Context, params acp.UnstableDeleteSessionRequest) (acp.UnstableDeleteSessionResponse, error) {
	ctx = a.observe.Extract(ctx, params.Meta)
	if params.SessionId == "" {
		return acp.UnstableDeleteSessionResponse{}, acp.NewInvalidParams(map[string]any{jsonFieldSessionID: validationRequired})
	}
	storeCtx, cancel := a.sessionStoreContext(ctx)
	defer cancel()
	if err := a.sessionStore().Delete(storeCtx, SessionKey{SessionID: string(params.SessionId)}); err != nil {
		return acp.UnstableDeleteSessionResponse{}, err
	}

	a.mu.Lock()
	session := a.sessions[params.SessionId]
	delete(a.sessions, params.SessionId)
	a.deleted[params.SessionId] = struct{}{}
	a.mu.Unlock()

	threadID := ""
	if session != nil {
		threadID = session.snapshot().codexThreadID
	}

	var cleanupErr error
	if err := a.deleteNativeCodexSession(ctx, params.SessionId, threadID); err != nil {
		cleanupErr = errors.Join(cleanupErr, err)
	}
	if session != nil {
		closeCtx, closeCancel := context.WithTimeout(context.Background(), closeTimeout)
		err := session.Close(closeCtx)
		closeCancel()
		if err != nil {
			cleanupErr = errors.Join(cleanupErr, err)
		}
		a.observe.AddActiveSession(ctx, -1)
	}

	if cleanupErr != nil {
		return acp.UnstableDeleteSessionResponse{}, cleanupErr
	}

	return acp.UnstableDeleteSessionResponse{}, nil
}

func (a *Agent) ListSessions(ctx context.Context, params acp.ListSessionsRequest) (acp.ListSessionsResponse, error) {
	if err := a.ensureOpen(); err != nil {
		return acp.ListSessionsResponse{}, err
	}
	if err := validateOptionalAbsolutePath(jsonFieldCwd, params.Cwd); err != nil {
		return acp.ListSessionsResponse{}, err
	}
	a.mu.Lock()
	active := make([]*session, 0, len(a.sessions))
	activeThreadIDs := map[string]struct{}{}
	for _, session := range a.sessions {
		if params.Cwd != nil && session.cwd != *params.Cwd {
			continue
		}
		active = append(active, session)
		activeThreadIDs[session.codexThreadID] = struct{}{}
	}
	a.mu.Unlock()

	sessions := make([]acp.SessionInfo, 0, len(active))
	seen := map[acp.SessionId]struct{}{}
	for _, session := range active {
		addSessionInfo(&sessions, seen, session.info())
	}
	if params.Cwd != nil {
		storeSessions, err := a.listStoredSessions(ctx, *params.Cwd, seen)
		if err != nil {
			return acp.ListSessionsResponse{}, err
		}
		for _, session := range storeSessions {
			addSessionInfo(&sessions, seen, session)
		}
	}
	a.retryDeletedNativeCodexSessions(ctx)
	if a.nativeSessionFallbackEnabled() {
		codexSessions, err := a.listCodexThreads(ctx, params, seen, activeThreadIDs)
		if err != nil {
			return acp.ListSessionsResponse{}, err
		}
		for _, session := range codexSessions {
			addSessionInfo(&sessions, seen, session)
		}
	}

	paged, nextCursor, err := paginateSessionInfos(sessions, params.Cursor)
	if err != nil {
		return acp.ListSessionsResponse{}, err
	}

	return acp.ListSessionsResponse{Sessions: paged, NextCursor: nextCursor}, nil
}

func (a *Agent) listCodexThreads(ctx context.Context, params acp.ListSessionsRequest, activeIDs map[acp.SessionId]struct{}, activeThreadIDs map[string]struct{}) ([]acp.SessionInfo, error) {
	cwd := ""
	if params.Cwd != nil {
		if err := validateRequiredAbsolutePath(jsonFieldCwd, *params.Cwd); err != nil {
			return nil, err
		}
		cwd = *params.Cwd
	}

	client, err := a.newClient(ctx, nil, nil, "")
	if err != nil {
		return nil, err
	}
	defer client.Close(context.Background())

	threads, err := client.ListThreads(ctx, codex.ThreadListRequest{Cwd: cwd})
	if err != nil {
		return nil, err
	}

	out := make([]acp.SessionInfo, 0, len(threads))
	for _, thread := range threads {
		id := acp.SessionId(firstNonEmpty(thread.SessionID, thread.ID))
		if id == "" {
			continue
		}
		if a.isDeleted(id) {
			continue
		}
		if _, ok := activeIDs[id]; ok {
			continue
		}
		if _, ok := activeThreadIDs[thread.ID]; ok {
			continue
		}
		title := firstNonEmpty(thread.Title, "Codex session "+string(id))
		updatedAt := thread.UpdatedAt
		info := acp.SessionInfo{
			SessionId: id,
			Cwd:       firstNonEmpty(thread.Cwd, cwd),
			Title:     &title,
			UpdatedAt: &updatedAt,
			Meta: map[string]any{
				codexMetaKey: map[string]any{
					codexThreadIDMetaKey: thread.ID,
					"stored":             true,
					"source":             "codex",
				},
			},
		}
		if thread.Model != "" {
			codexMeta := info.Meta[codexMetaKey].(map[string]any)
			codexMeta["model"] = thread.Model
		}
		out = append(out, info)
	}

	return out, nil
}

func (a *Agent) isDeleted(id acp.SessionId) bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	_, ok := a.deleted[id]
	return ok
}

type codexSessionStart struct {
	Cwd                   string
	AdditionalDirectories []string
	McpServers            []acp.McpServer
	Meta                  sessionMeta
	ResumeID              string
	ForkParentID          string
	MaterializedPath      string
}

func (a *Agent) activeSessionForStart(id acp.SessionId, start codexSessionStart) *session {
	fingerprint := codexSessionStartFingerprint(start)

	a.mu.Lock()
	defer a.mu.Unlock()

	session := a.sessions[id]
	if session == nil || session.fingerprint != fingerprint {
		return nil
	}

	return session
}

func codexSessionStartFingerprint(start codexSessionStart) string {
	servers := slices.Clone(start.McpServers)
	slices.SortFunc(servers, func(left, right acp.McpServer) int {
		return strings.Compare(mcpServerName(left), mcpServerName(right))
	})

	data := struct {
		Cwd                   string           `json:"cwd"`
		AdditionalDirectories []string         `json:"additionalDirectories,omitempty"`
		McpServers            []acp.McpServer  `json:"mcpServers,omitempty"`
		Meta                  codexMetaForHash `json:"meta,omitzero"`
	}{
		Cwd:                   start.Cwd,
		AdditionalDirectories: slices.Clone(start.AdditionalDirectories),
		McpServers:            servers,
		Meta:                  codexMetaFingerprint(start.Meta),
	}

	return jsonFingerprint(data)
}

type codexMetaForHash struct {
	Model           string            `json:"model,omitempty"`
	ReasoningEffort string            `json:"reasoningEffort,omitempty"`
	ServiceTier     string            `json:"serviceTier,omitempty"`
	Personality     string            `json:"personality,omitempty"`
	Env             map[string]string `json:"env,omitempty"`
	ApprovalPolicy  any               `json:"approvalPolicy,omitempty"`
	SandboxPolicy   any               `json:"sandboxPolicy,omitempty"`
	OutputSchema    any               `json:"outputSchema,omitempty"`
	RawMessages     rawMessageConfig  `json:"rawMessages,omitzero"`
}

func codexMetaFingerprint(meta sessionMeta) codexMetaForHash {
	return codexMetaForHash{
		Model:           meta.Model,
		ReasoningEffort: meta.ReasoningEffort,
		ServiceTier:     meta.ServiceTier,
		Personality:     meta.Personality,
		Env:             cloneStringMap(meta.Env),
		ApprovalPolicy:  cloneAny(meta.ApprovalPolicy),
		SandboxPolicy:   cloneAny(meta.SandboxPolicy),
		OutputSchema:    cloneAny(meta.OutputSchema),
		RawMessages:     meta.RawMessages,
	}
}

func jsonFingerprint(data any) string {
	encoded, err := json.Marshal(data)
	if err != nil {
		return fmt.Sprintf("marshal-error:%T:%v", data, err)
	}

	return string(encoded)
}

func mcpServerName(server acp.McpServer) string {
	switch {
	case server.Http != nil:
		return server.Http.Name
	case server.Sse != nil:
		return server.Sse.Name
	case server.Acp != nil:
		return server.Acp.Name
	case server.Stdio != nil:
		return server.Stdio.Name
	default:
		return ""
	}
}

func addSessionInfo(sessions *[]acp.SessionInfo, seen map[acp.SessionId]struct{}, session acp.SessionInfo) {
	if session.SessionId == "" {
		return
	}
	if _, ok := seen[session.SessionId]; ok {
		return
	}

	*sessions = append(*sessions, session)
	seen[session.SessionId] = struct{}{}
}

func paginateSessionInfos(sessions []acp.SessionInfo, cursor *string) ([]acp.SessionInfo, *string, error) {
	offset, err := decodeListCursor(cursor)
	if err != nil {
		return nil, nil, acp.NewInvalidParams(map[string]any{"cursor": "invalid cursor"})
	}
	if offset > len(sessions) {
		return nil, nil, acp.NewInvalidParams(map[string]any{"cursor": "cursor is past end"})
	}

	end := offset + listSessionsPageSize
	if end >= len(sessions) {
		return sessions[offset:], nil, nil
	}
	next := encodeListCursor(end)

	return sessions[offset:end], &next, nil
}

func decodeListCursor(cursor *string) (int, error) {
	if cursor == nil || *cursor == "" {
		return 0, nil
	}

	data, err := base64.RawURLEncoding.DecodeString(*cursor)
	if err != nil {
		return 0, err
	}
	offset, err := strconv.Atoi(string(data))
	if err != nil || offset < 0 {
		return 0, strconv.ErrSyntax
	}

	return offset, nil
}

func encodeListCursor(offset int) string {
	return base64.RawURLEncoding.EncodeToString([]byte(strconv.Itoa(offset)))
}

func (a *Agent) ResumeSession(ctx context.Context, params acp.ResumeSessionRequest) (acp.ResumeSessionResponse, error) {
	ctx = a.observe.Extract(ctx, params.Meta)
	if err := a.ensureOpen(); err != nil {
		return acp.ResumeSessionResponse{}, err
	}
	if err := validateSessionStartPaths(params.Cwd, params.AdditionalDirectories); err != nil {
		return acp.ResumeSessionResponse{}, err
	}
	if a.isDeleted(params.SessionId) {
		a.retryDeleteNativeCodexSession(ctx, params.SessionId, "")

		return acp.ResumeSessionResponse{}, newResourceNotFound(map[string]any{jsonFieldSessionID: params.SessionId})
	}
	meta, err := sessionMetaFromLifecycle(params.Meta)
	if err != nil {
		return acp.ResumeSessionResponse{}, lifecycleMetaError(err)
	}
	start := codexSessionStart{
		Cwd:                   params.Cwd,
		AdditionalDirectories: params.AdditionalDirectories,
		McpServers:            params.McpServers,
		Meta:                  meta,
		ResumeID:              string(params.SessionId),
	}
	if session := a.activeSessionForStart(params.SessionId, start); session != nil {
		models := modelList(ctx, session.client)
		snapshot := session.snapshot()

		return acp.ResumeSessionResponse{
			Meta:          sessionResponseMeta(snapshot),
			ConfigOptions: sessionConfigOptions(session, models),
		}, nil
	}

	storeEntries, loadErr := a.loadStoredSession(ctx, params.SessionId, params.Cwd)
	if loadErr != nil {
		return acp.ResumeSessionResponse{}, loadErr
	}
	if len(storeEntries) > 0 {
		return a.resumeMaterializedSession(ctx, params, storeEntries)
	}
	if !a.nativeSessionFallbackEnabled() {
		a.retryDeleteNativeCodexSession(ctx, params.SessionId, "")

		return acp.ResumeSessionResponse{}, newResourceNotFound(map[string]any{jsonFieldSessionID: params.SessionId})
	}

	mcpServers, err := a.prepareMCPServers(ctx, params.SessionId, params.McpServers)
	if err != nil {
		return acp.ResumeSessionResponse{}, err
	}
	client, err := a.newClient(ctx, mcpServers, meta.Env, meta.MCPToolApprovalMode)
	if err != nil {
		return acp.ResumeSessionResponse{}, err
	}
	thread, err := client.ResumeThread(ctx, codex.ThreadResumeRequest{
		ThreadID: string(params.SessionId),
		Cwd:      params.Cwd,
	})
	if err != nil {
		_ = client.Close(context.Background())
		return acp.ResumeSessionResponse{}, codexThreadACPError(err, nil, codexThreadErrorData(params.SessionId, string(params.SessionId)))
	}

	id := params.SessionId
	session := newSession(a, id, params.Cwd, params.AdditionalDirectories, thread, client, meta)
	session.fingerprint = codexSessionStartFingerprint(start)
	session.setAccount(clientAccountMeta(ctx, client))
	if err := a.storeStartedSession(session); err != nil {
		_ = session.Close(context.Background())
		return acp.ResumeSessionResponse{}, err
	}
	models := modelList(ctx, client)
	snapshot := session.snapshot()

	return acp.ResumeSessionResponse{
		Meta:          sessionResponseMeta(snapshot),
		ConfigOptions: sessionConfigOptions(session, models),
	}, nil
}

func (a *Agent) resumeMaterializedSession(ctx context.Context, params acp.ResumeSessionRequest, entries []SessionStoreEntry) (acp.ResumeSessionResponse, error) {
	if err := a.ensureOpen(); err != nil {
		return acp.ResumeSessionResponse{}, err
	}
	meta, err := sessionMetaFromLifecycle(params.Meta)
	if err != nil {
		return acp.ResumeSessionResponse{}, lifecycleMetaError(err)
	}
	path, err := materializeRollout(entries)
	if err != nil {
		return acp.ResumeSessionResponse{}, err
	}

	mcpServers, err := a.prepareMCPServers(ctx, params.SessionId, params.McpServers)
	if err != nil {
		_ = removeMaterializedRollout(path)
		return acp.ResumeSessionResponse{}, err
	}
	client, err := a.newClient(ctx, mcpServers, meta.Env, meta.MCPToolApprovalMode)
	if err != nil {
		_ = removeMaterializedRollout(path)
		return acp.ResumeSessionResponse{}, err
	}
	threadID := firstNonEmpty(rolloutNativeThreadID(entries), string(params.SessionId))
	thread, err := client.ResumeThread(ctx, codex.ThreadResumeRequest{
		ThreadID: threadID,
		Path:     path,
		Cwd:      params.Cwd,
	})
	if err != nil {
		_ = client.Close(context.Background())
		_ = removeMaterializedRollout(path)
		return acp.ResumeSessionResponse{}, codexThreadACPError(err, nil, codexThreadErrorData(params.SessionId, threadID))
	}

	id := params.SessionId
	session := newSession(a, id, params.Cwd, params.AdditionalDirectories, thread, client, meta)
	session.fingerprint = codexSessionStartFingerprint(codexSessionStart{
		Cwd:                   params.Cwd,
		AdditionalDirectories: params.AdditionalDirectories,
		McpServers:            params.McpServers,
		Meta:                  meta,
		ResumeID:              string(params.SessionId),
		MaterializedPath:      path,
	})
	session.setAccount(clientAccountMeta(ctx, client))
	session.materializedPath = path
	if err := a.storeStartedSession(session); err != nil {
		_ = session.Close(context.Background())
		return acp.ResumeSessionResponse{}, err
	}
	models := modelList(ctx, client)
	snapshot := session.snapshot()

	return acp.ResumeSessionResponse{
		Meta:          sessionResponseMeta(snapshot),
		ConfigOptions: sessionConfigOptions(session, models),
	}, nil
}

func (a *Agent) LoadSession(ctx context.Context, params acp.LoadSessionRequest) (acp.LoadSessionResponse, error) {
	ctx = a.observe.Extract(ctx, params.Meta)
	if err := a.ensureOpen(); err != nil {
		return acp.LoadSessionResponse{}, err
	}
	if err := validateSessionStartPaths(params.Cwd, params.AdditionalDirectories); err != nil {
		return acp.LoadSessionResponse{}, err
	}
	if a.isDeleted(params.SessionId) {
		a.retryDeleteNativeCodexSession(ctx, params.SessionId, "")

		return acp.LoadSessionResponse{}, newResourceNotFound(map[string]any{jsonFieldSessionID: params.SessionId})
	}
	meta, err := sessionMetaFromLifecycle(params.Meta)
	if err != nil {
		return acp.LoadSessionResponse{}, lifecycleMetaError(err)
	}
	start := codexSessionStart{
		Cwd:                   params.Cwd,
		AdditionalDirectories: params.AdditionalDirectories,
		McpServers:            params.McpServers,
		Meta:                  meta,
		ResumeID:              string(params.SessionId),
	}
	if existing := a.activeSessionForStart(params.SessionId, start); existing != nil {
		storeEntries, loadErr := a.loadStoredSession(ctx, params.SessionId, params.Cwd)
		switch {
		case loadErr != nil:
			return acp.LoadSessionResponse{}, loadErr
		case len(storeEntries) > 0:
			if err := existing.replayRollout(ctx, storeEntries); err != nil {
				return acp.LoadSessionResponse{}, err
			}
		default:
			if err := existing.replayThreadHistory(ctx); err != nil {
				return acp.LoadSessionResponse{}, err
			}
		}
		models := modelList(ctx, existing.client)
		snapshot := existing.snapshot()
		return acp.LoadSessionResponse{
			Meta:          sessionResponseMeta(snapshot),
			ConfigOptions: sessionConfigOptions(existing, models),
		}, nil
	}

	storeEntries, loadErr := a.loadStoredSession(ctx, params.SessionId, params.Cwd)
	if loadErr != nil {
		return acp.LoadSessionResponse{}, loadErr
	}
	if len(storeEntries) > 0 {
		return a.loadMaterializedSession(ctx, params, storeEntries)
	}
	if !a.nativeSessionFallbackEnabled() {
		a.retryDeleteNativeCodexSession(ctx, params.SessionId, "")

		return acp.LoadSessionResponse{}, newResourceNotFound(map[string]any{jsonFieldSessionID: params.SessionId})
	}

	resp, err := a.ResumeSession(ctx, acp.ResumeSessionRequest(params))
	if err != nil {
		return acp.LoadSessionResponse{}, err
	}
	if session, err := a.session(params.SessionId); err == nil {
		if replayErr := session.replayThreadHistory(ctx); replayErr != nil {
			return acp.LoadSessionResponse{}, replayErr
		}
	}

	return acp.LoadSessionResponse(resp), nil
}

func (a *Agent) listStoredSessions(ctx context.Context, cwd string, activeIDs map[acp.SessionId]struct{}) ([]acp.SessionInfo, error) {
	listCtx, cancel := a.sessionStoreContext(ctx)
	defer cancel()

	summaries, err := a.sessionStore().ListSessions(listCtx)
	if err != nil {
		return nil, err
	}

	out := make([]acp.SessionInfo, 0, len(summaries))
	for _, summary := range summaries {
		id := acp.SessionId(summary.SessionID)
		if _, ok := activeIDs[id]; ok {
			continue
		}
		if summary.Cwd != "" && summary.Cwd != cwd {
			continue
		}
		updatedAt := time.UnixMilli(summary.UpdatedAtUnixMilli).UTC().Format(time.RFC3339)
		title := firstNonEmpty(summary.Title, "Codex session "+summary.SessionID)
		codexMeta := map[string]any{
			"stored": true,
			"source": "sessionStore",
		}
		for key, value := range summary.Meta {
			codexMeta[key] = cloneAny(value)
		}
		out = append(out, acp.SessionInfo{
			SessionId: id,
			Cwd:       firstNonEmpty(summary.Cwd, cwd),
			Title:     &title,
			UpdatedAt: &updatedAt,
			Meta: map[string]any{
				codexMetaKey: codexMeta,
			},
		})
	}

	return out, nil
}

func (a *Agent) loadStoredSession(ctx context.Context, sessionID acp.SessionId, cwd string) ([]SessionStoreEntry, error) {
	loadCtx, cancel := a.sessionStoreContext(ctx)
	defer cancel()

	entries, err := a.sessionStore().Load(loadCtx, SessionKey{SessionID: string(sessionID)})
	if err != nil {
		return nil, err
	}

	return entries, nil
}

func (a *Agent) loadMaterializedSession(ctx context.Context, params acp.LoadSessionRequest, entries []SessionStoreEntry) (acp.LoadSessionResponse, error) {
	if err := a.ensureOpen(); err != nil {
		return acp.LoadSessionResponse{}, err
	}
	meta, err := sessionMetaFromLifecycle(params.Meta)
	if err != nil {
		return acp.LoadSessionResponse{}, lifecycleMetaError(err)
	}
	path, err := materializeRollout(entries)
	if err != nil {
		return acp.LoadSessionResponse{}, err
	}

	mcpServers, err := a.prepareMCPServers(ctx, params.SessionId, params.McpServers)
	if err != nil {
		_ = removeMaterializedRollout(path)
		return acp.LoadSessionResponse{}, err
	}
	client, err := a.newClient(ctx, mcpServers, meta.Env, meta.MCPToolApprovalMode)
	if err != nil {
		_ = removeMaterializedRollout(path)
		return acp.LoadSessionResponse{}, err
	}
	thread, err := client.ResumeThread(ctx, codex.ThreadResumeRequest{
		ThreadID: firstNonEmpty(rolloutNativeThreadID(entries), string(params.SessionId)),
		Path:     path,
		Cwd:      params.Cwd,
	})
	if err != nil {
		_ = client.Close(context.Background())
		_ = removeMaterializedRollout(path)
		return acp.LoadSessionResponse{}, codexThreadACPError(err, nil, codexThreadErrorData(params.SessionId, firstNonEmpty(rolloutNativeThreadID(entries), string(params.SessionId))))
	}

	id := params.SessionId
	session := newSession(a, id, params.Cwd, params.AdditionalDirectories, thread, client, meta)
	session.fingerprint = codexSessionStartFingerprint(codexSessionStart{
		Cwd:                   params.Cwd,
		AdditionalDirectories: params.AdditionalDirectories,
		McpServers:            params.McpServers,
		Meta:                  meta,
		ResumeID:              string(params.SessionId),
		MaterializedPath:      path,
	})
	session.setAccount(clientAccountMeta(ctx, client))
	session.materializedPath = path
	if err := a.storeStartedSession(session); err != nil {
		_ = session.Close(context.Background())
		return acp.LoadSessionResponse{}, err
	}
	if err := session.replayRollout(ctx, entries); err != nil {
		a.removeSession(id)
		_ = session.Close(context.Background())
		return acp.LoadSessionResponse{}, err
	}
	models := modelList(ctx, client)
	snapshot := session.snapshot()

	return acp.LoadSessionResponse{
		Meta:          sessionResponseMeta(snapshot),
		ConfigOptions: sessionConfigOptions(session, models),
	}, nil
}

func (a *Agent) sessionStoreContext(ctx context.Context) (context.Context, context.CancelFunc) {
	timeout := a.options.SessionStoreLoadTimeout
	if timeout <= 0 {
		timeout = 10 * time.Second
	}

	return context.WithTimeout(ctx, timeout)
}

func (a *Agent) nativeSessionFallbackEnabled() bool {
	return !a.explicitStore
}

func (a *Agent) retryDeletedNativeCodexSessions(ctx context.Context) {
	a.mu.Lock()
	ids := make([]acp.SessionId, 0, len(a.deleted))
	for id := range a.deleted {
		ids = append(ids, id)
	}
	a.mu.Unlock()

	for _, id := range ids {
		a.retryDeleteNativeCodexSession(ctx, id, "")
	}
}

func (a *Agent) retryDeleteNativeCodexSession(ctx context.Context, sessionID acp.SessionId, threadID string) {
	if err := a.deleteNativeCodexSession(ctx, sessionID, threadID); err != nil {
		a.log.DebugContext(ctx, "retry delete native Codex session failed", slog.String(jsonFieldSessionID, string(sessionID)), slog.String(jsonFieldError, err.Error()))
	}
}

func (a *Agent) deleteNativeCodexSession(ctx context.Context, sessionID acp.SessionId, knownThreadID string) error {
	client, err := a.newClient(ctx, nil, nil, "")
	if err != nil {
		return err
	}
	defer client.Close(context.Background())

	threadIDs := map[string]struct{}{}
	if knownThreadID != "" {
		threadIDs[knownThreadID] = struct{}{}
	}
	if sessionID != "" {
		threadIDs[string(sessionID)] = struct{}{}
	}

	threads, err := client.ListThreads(ctx, codex.ThreadListRequest{})
	if err != nil {
		return err
	}
	for _, thread := range threads {
		if thread.ID == "" {
			continue
		}
		if acp.SessionId(firstNonEmpty(thread.SessionID, thread.ID)) == sessionID || thread.ID == knownThreadID {
			threadIDs[thread.ID] = struct{}{}
		}
	}

	var cleanupErr error
	for threadID := range threadIDs {
		err := client.DeleteThread(ctx, codex.ThreadDeleteRequest{ThreadID: threadID})
		if errors.Is(err, codex.ErrThreadNotFound) {
			continue
		}
		cleanupErr = errors.Join(cleanupErr, err)
	}

	return cleanupErr
}

func (a *Agent) forkSession(ctx context.Context, params acp.UnstableForkSessionRequest) (acp.UnstableForkSessionResponse, error) {
	ctx = a.observe.Extract(ctx, params.Meta)
	parent, err := a.session(params.SessionId)
	if err != nil {
		return acp.UnstableForkSessionResponse{}, err
	}
	if err := validateSessionStartPaths(params.Cwd, params.AdditionalDirectories); err != nil {
		return acp.UnstableForkSessionResponse{}, err
	}
	meta, err := sessionMetaFromLifecycle(params.Meta)
	if err != nil {
		return acp.UnstableForkSessionResponse{}, lifecycleMetaError(err)
	}
	idValue, err := newSessionID()
	if err != nil {
		return acp.UnstableForkSessionResponse{}, err
	}
	id := acp.SessionId(idValue)
	mcpServers, err := a.prepareMCPServers(ctx, id, stableMCPServersFromUnstable(params.McpServers))
	if err != nil {
		return acp.UnstableForkSessionResponse{}, err
	}
	client, err := a.newClient(ctx, mcpServers, meta.Env, meta.MCPToolApprovalMode)
	if err != nil {
		return acp.UnstableForkSessionResponse{}, err
	}
	parentSnapshot := parent.snapshot()
	thread, err := client.ForkThread(ctx, codex.ThreadForkRequest{
		ThreadID: parentSnapshot.codexThreadID,
		Cwd:      params.Cwd,
	})
	if err != nil {
		_ = client.Close(context.Background())
		return acp.UnstableForkSessionResponse{}, codexThreadACPError(err, parentSnapshot.accountMeta, codexThreadErrorData(parent.id, parentSnapshot.codexThreadID))
	}

	session := newSession(a, id, params.Cwd, params.AdditionalDirectories, thread, client, meta)
	session.fingerprint = codexSessionStartFingerprint(codexSessionStart{
		Cwd:                   params.Cwd,
		AdditionalDirectories: params.AdditionalDirectories,
		McpServers:            stableMCPServersFromUnstable(params.McpServers),
		Meta:                  meta,
		ForkParentID:          parentSnapshot.codexThreadID,
	})
	session.setAccount(clientAccountMeta(ctx, client))
	if err := a.storeStartedSession(session); err != nil {
		_ = session.Close(context.Background())
		return acp.UnstableForkSessionResponse{}, err
	}
	models := modelList(ctx, client)
	snapshot := session.snapshot()

	return acp.UnstableForkSessionResponse{
		SessionId:     id,
		Meta:          sessionResponseMeta(snapshot),
		ConfigOptions: sessionUnstableConfigOptions(session, models),
	}, nil
}

func (a *Agent) SetSessionConfigOption(ctx context.Context, params acp.SetSessionConfigOptionRequest) (acp.SetSessionConfigOptionResponse, error) {
	switch {
	case params.ValueId != nil:
		return a.setSessionConfigValue(ctx, params.ValueId)
	case params.Boolean != nil:
		return acp.SetSessionConfigOptionResponse{}, acp.NewInvalidParams(map[string]any{"configId": params.Boolean.ConfigId})
	default:
		return acp.SetSessionConfigOptionResponse{}, acp.NewInvalidParams(map[string]any{"config": validationRequired})
	}
}

// SetSessionMode exists only because github.com/coder/acp-go-sdk's generated
// Agent interface still requires it. The local ACP dispatcher intentionally
// does not route session/set_mode; use session/set_config_option with configId
// "mode".
func (a *Agent) SetSessionMode(context.Context, acp.SetSessionModeRequest) (acp.SetSessionModeResponse, error) {
	return acp.SetSessionModeResponse{}, acp.NewMethodNotFound(acp.AgentMethodSessionSetMode)
}

func modelList(ctx context.Context, client codex.Client) []codex.Model {
	if client == nil {
		return nil
	}
	models, err := client.ModelList(ctx)
	if err != nil {
		return nil
	}

	return models
}

func nowRFC3339() string {
	return timeNow().UTC().Format(timeFormatRFC3339)
}

var timeNow = defaultTimeNow

func defaultTimeNow() time.Time { return time.Now() }

const timeFormatRFC3339 = "2006-01-02T15:04:05Z07:00"

func (a *Agent) sessionByCodexThread(threadID string) *session {
	if strings.TrimSpace(threadID) == "" {
		return nil
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	for _, session := range a.sessions {
		if session.codexThreadID == threadID {
			return session
		}
	}

	return nil
}

func clientAccountMeta(ctx context.Context, client codex.Client) map[string]any {
	if client == nil {
		return nil
	}
	account, err := client.AccountRead(ctx)
	if err != nil {
		return nil
	}

	return redactedAccountMeta(account)
}

func codexThreadACPError(err error, account map[string]any, data map[string]any) error {
	if err == nil {
		return nil
	}
	if isCodexAuthError(err) {
		return codexAuthRequiredError(err, account)
	}
	if errors.Is(err, codex.ErrThreadNotFound) {
		return newResourceNotFound(data)
	}

	return err
}

func fatalCodexProcessError(err error) bool {
	return errors.Is(err, codex.ErrConnectionClosed)
}

func newResourceNotFound(data any) *acp.RequestError {
	return &acp.RequestError{Code: -32002, Message: "Resource not found", Data: data}
}

func lifecycleMetaError(err error) error {
	var reqErr *acp.RequestError
	if errors.As(err, &reqErr) {
		return reqErr
	}

	return acp.NewInvalidParams(map[string]any{jsonFieldError: err.Error()})
}

func codexThreadErrorData(sessionID acp.SessionId, threadID string) map[string]any {
	data := map[string]any{jsonFieldSessionID: sessionID}
	if threadID != "" {
		data["threadId"] = threadID
	}

	return data
}
