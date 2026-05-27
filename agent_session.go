package codexacp

import (
	"context"
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
		return acp.NewSessionResponse{}, acp.NewInvalidParams(map[string]any{jsonFieldError: err.Error()})
	}

	idValue, err := newSessionID()
	if err != nil {
		return acp.NewSessionResponse{}, err
	}
	id := acp.SessionId(idValue)
	mcpServers, mcpBridge, err := a.prepareMCPServers(ctx, id, params.McpServers)
	if err != nil {
		return acp.NewSessionResponse{}, err
	}
	client, err := a.newClient(ctx, mcpServers)
	if err != nil {
		if mcpBridge != nil {
			_ = mcpBridge.Close()
		}
		return acp.NewSessionResponse{}, err
	}

	thread, err := client.StartThread(ctx, codex.ThreadStartRequest{
		Cwd:                   params.Cwd,
		AdditionalDirectories: params.AdditionalDirectories,
		Model:                 firstNonEmpty(meta.Model, a.options.DefaultModel),
		ServiceTier:           meta.ServiceTier,
		Personality:           meta.Personality,
	})
	if err != nil {
		_ = client.Close(context.Background())
		if mcpBridge != nil {
			_ = mcpBridge.Close()
		}
		return acp.NewSessionResponse{}, codexAuthRequiredError(err, nil)
	}

	session := newSession(a, id, params.Cwd, params.AdditionalDirectories, thread, client, meta)
	session.setAccount(clientAccountMeta(ctx, client))
	session.mcpBridge = mcpBridge
	if err := a.storeStartedSession(session); err != nil {
		_ = session.Close(context.Background())
		return acp.NewSessionResponse{}, err
	}
	models := modelList(ctx, client)
	snapshot := session.snapshot()

	return acp.NewSessionResponse{
		SessionId:     id,
		Meta:          sessionResponseMeta(snapshot),
		Modes:         modeState(snapshot.mode),
		Models:        modelState(snapshot.model, models),
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

	return resp, err
}

func (a *Agent) Cancel(ctx context.Context, params acp.CancelNotification) error {
	a.mu.Lock()
	session := a.sessions[params.SessionId]
	a.mu.Unlock()
	if session == nil {
		return nil
	}
	session.cancelTurn()

	return session.client.CancelTurn(ctx, session.codexThreadID, session.activeTurnID())
}

func (a *Agent) CloseSession(ctx context.Context, params acp.CloseSessionRequest) (acp.CloseSessionResponse, error) {
	session := a.removeSession(params.SessionId)
	if session == nil {
		return acp.CloseSessionResponse{}, nil
	}
	a.observe.AddActiveSession(ctx, -1)

	return acp.CloseSessionResponse{}, session.Close(ctx)
}

func (a *Agent) ListSessions(ctx context.Context, params acp.ListSessionsRequest) (acp.ListSessionsResponse, error) {
	a.mu.Lock()
	active := make([]*Session, 0, len(a.sessions))
	activeIDs := map[acp.SessionId]struct{}{}
	activeThreadIDs := map[string]struct{}{}
	for _, session := range a.sessions {
		if params.Cwd != nil && session.cwd != *params.Cwd {
			continue
		}
		if len(params.AdditionalDirectories) > 0 && !slicesEqual(session.additionalDirectories, params.AdditionalDirectories) {
			continue
		}
		active = append(active, session)
		activeIDs[session.id] = struct{}{}
		activeThreadIDs[session.codexThreadID] = struct{}{}
	}
	a.mu.Unlock()

	sessions := make([]acp.SessionInfo, 0, len(active))
	for _, session := range active {
		sessions = append(sessions, session.info())
	}
	if params.Cwd != nil {
		storeSessions, err := a.listStoredSessions(ctx, *params.Cwd, activeIDs)
		if err != nil {
			return acp.ListSessionsResponse{}, err
		}
		sessions = append(sessions, storeSessions...)
	}
	codexSessions, err := a.listCodexThreads(ctx, params, activeIDs, activeThreadIDs)
	if err != nil {
		return acp.ListSessionsResponse{}, err
	}
	sessions = append(sessions, codexSessions...)

	return acp.ListSessionsResponse{Sessions: sessions}, nil
}

func (a *Agent) listCodexThreads(ctx context.Context, params acp.ListSessionsRequest, activeIDs map[acp.SessionId]struct{}, activeThreadIDs map[string]struct{}) ([]acp.SessionInfo, error) {
	cwd := ""
	if params.Cwd != nil {
		if err := validateRequiredAbsolutePath(jsonFieldCwd, *params.Cwd); err != nil {
			return nil, err
		}
		cwd = *params.Cwd
	}

	client, err := a.newClient(ctx, nil)
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

func (a *Agent) ResumeSession(ctx context.Context, params acp.ResumeSessionRequest) (acp.ResumeSessionResponse, error) {
	ctx = a.observe.Extract(ctx, params.Meta)
	if err := a.ensureOpen(); err != nil {
		return acp.ResumeSessionResponse{}, err
	}
	if err := validateSessionStartPaths(params.Cwd, params.AdditionalDirectories); err != nil {
		return acp.ResumeSessionResponse{}, err
	}
	meta, err := sessionMetaFromLifecycle(params.Meta)
	if err != nil {
		return acp.ResumeSessionResponse{}, acp.NewInvalidParams(map[string]any{jsonFieldError: err.Error()})
	}

	mcpServers, mcpBridge, err := a.prepareMCPServers(ctx, params.SessionId, params.McpServers)
	if err != nil {
		return acp.ResumeSessionResponse{}, err
	}
	client, err := a.newClient(ctx, mcpServers)
	if err != nil {
		if mcpBridge != nil {
			_ = mcpBridge.Close()
		}
		return acp.ResumeSessionResponse{}, err
	}
	thread, err := client.ResumeThread(ctx, codex.ThreadResumeRequest{
		ThreadID: string(params.SessionId),
		Cwd:      params.Cwd,
	})
	if err != nil {
		_ = client.Close(context.Background())
		if mcpBridge != nil {
			_ = mcpBridge.Close()
		}
		return acp.ResumeSessionResponse{}, codexAuthRequiredError(err, nil)
	}

	id := params.SessionId
	session := newSession(a, id, params.Cwd, params.AdditionalDirectories, thread, client, meta)
	session.setAccount(clientAccountMeta(ctx, client))
	session.mcpBridge = mcpBridge
	if err := a.storeStartedSession(session); err != nil {
		_ = session.Close(context.Background())
		return acp.ResumeSessionResponse{}, err
	}
	models := modelList(ctx, client)
	snapshot := session.snapshot()

	return acp.ResumeSessionResponse{
		Meta:          sessionResponseMeta(snapshot),
		Modes:         modeState(snapshot.mode),
		Models:        modelState(snapshot.model, models),
		ConfigOptions: sessionConfigOptions(session, models),
	}, nil
}

func (a *Agent) LoadSession(ctx context.Context, params acp.LoadSessionRequest) (acp.LoadSessionResponse, error) {
	ctx = a.observe.Extract(ctx, params.Meta)
	if err := validateSessionStartPaths(params.Cwd, params.AdditionalDirectories); err != nil {
		return acp.LoadSessionResponse{}, err
	}
	if existing, err := a.session(params.SessionId); err == nil {
		snapshot := existing.snapshot()
		return acp.LoadSessionResponse{
			Meta:          sessionResponseMeta(snapshot),
			Modes:         modeState(snapshot.mode),
			Models:        modelState(snapshot.model, nil),
			ConfigOptions: sessionConfigOptions(existing, nil),
		}, nil
	}

	storeEntries, loadErr := a.loadStoredSession(ctx, params.SessionId, params.Cwd)
	if loadErr != nil {
		return acp.LoadSessionResponse{}, loadErr
	}
	if len(storeEntries) > 0 {
		return a.loadMaterializedSession(ctx, params, storeEntries)
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
	lister, ok := a.sessionStore().(SessionStoreLister)
	if !ok {
		return nil, nil
	}
	projectKey, err := projectKeyForDirectory(cwd)
	if err != nil {
		return nil, acp.NewInvalidParams(map[string]any{jsonFieldCwd: err.Error()})
	}

	listCtx, cancel := a.sessionStoreContext(ctx)
	defer cancel()

	summaries, err := lister.ListSessions(listCtx, projectKey)
	if err != nil {
		return nil, err
	}

	out := make([]acp.SessionInfo, 0, len(summaries))
	for _, summary := range summaries {
		id := acp.SessionId(summary.SessionID)
		if _, ok := activeIDs[id]; ok {
			continue
		}
		updatedAt := time.UnixMilli(summary.MTime).UTC().Format(time.RFC3339)
		title := "Codex session " + summary.SessionID
		out = append(out, acp.SessionInfo{
			SessionId: id,
			Cwd:       cwd,
			Title:     &title,
			UpdatedAt: &updatedAt,
			Meta: map[string]any{
				codexMetaKey: map[string]any{
					codexThreadIDMetaKey: summary.SessionID,
					"stored":             true,
				},
			},
		})
	}

	return out, nil
}

func (a *Agent) loadStoredSession(ctx context.Context, sessionID acp.SessionId, cwd string) ([]SessionStoreEntry, error) {
	projectKey, err := projectKeyForDirectory(cwd)
	if err != nil {
		return nil, acp.NewInvalidParams(map[string]any{jsonFieldCwd: err.Error()})
	}

	loadCtx, cancel := a.sessionStoreContext(ctx)
	defer cancel()

	entries, err := a.sessionStore().Load(loadCtx, SessionKey{ProjectKey: projectKey, SessionID: string(sessionID)})
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
		return acp.LoadSessionResponse{}, acp.NewInvalidParams(map[string]any{jsonFieldError: err.Error()})
	}
	path, err := materializeRollout(entries)
	if err != nil {
		return acp.LoadSessionResponse{}, err
	}

	mcpServers, mcpBridge, err := a.prepareMCPServers(ctx, params.SessionId, params.McpServers)
	if err != nil {
		_ = removeMaterializedRollout(path)
		return acp.LoadSessionResponse{}, err
	}
	client, err := a.newClient(ctx, mcpServers)
	if err != nil {
		_ = removeMaterializedRollout(path)
		if mcpBridge != nil {
			_ = mcpBridge.Close()
		}
		return acp.LoadSessionResponse{}, err
	}
	thread, err := client.ResumeThread(ctx, codex.ThreadResumeRequest{
		ThreadID: string(params.SessionId),
		Path:     path,
		Cwd:      params.Cwd,
	})
	if err != nil {
		_ = client.Close(context.Background())
		_ = removeMaterializedRollout(path)
		if mcpBridge != nil {
			_ = mcpBridge.Close()
		}
		return acp.LoadSessionResponse{}, codexAuthRequiredError(err, nil)
	}

	id := params.SessionId
	session := newSession(a, id, params.Cwd, params.AdditionalDirectories, thread, client, meta)
	session.setAccount(clientAccountMeta(ctx, client))
	session.materializedPath = path
	session.mcpBridge = mcpBridge
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
		Modes:         modeState(snapshot.mode),
		Models:        modelState(snapshot.model, models),
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

func (a *Agent) UnstableForkSession(ctx context.Context, params acp.UnstableForkSessionRequest) (acp.UnstableForkSessionResponse, error) {
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
		return acp.UnstableForkSessionResponse{}, acp.NewInvalidParams(map[string]any{jsonFieldError: err.Error()})
	}
	idValue, err := newSessionID()
	if err != nil {
		return acp.UnstableForkSessionResponse{}, err
	}
	id := acp.SessionId(idValue)
	mcpServers, mcpBridge, err := a.prepareMCPServers(ctx, id, stableMCPServersFromUnstable(params.McpServers))
	if err != nil {
		return acp.UnstableForkSessionResponse{}, err
	}
	client, err := a.newClient(ctx, mcpServers)
	if err != nil {
		if mcpBridge != nil {
			_ = mcpBridge.Close()
		}
		return acp.UnstableForkSessionResponse{}, err
	}
	parentSnapshot := parent.snapshot()
	thread, err := client.ForkThread(ctx, codex.ThreadForkRequest{
		ThreadID: parentSnapshot.codexThreadID,
		Cwd:      params.Cwd,
	})
	if err != nil {
		_ = client.Close(context.Background())
		if mcpBridge != nil {
			_ = mcpBridge.Close()
		}
		return acp.UnstableForkSessionResponse{}, codexAuthRequiredError(err, parentSnapshot.accountMeta)
	}

	session := newSession(a, id, params.Cwd, params.AdditionalDirectories, thread, client, meta)
	session.setAccount(clientAccountMeta(ctx, client))
	session.mcpBridge = mcpBridge
	if err := a.storeStartedSession(session); err != nil {
		_ = session.Close(context.Background())
		return acp.UnstableForkSessionResponse{}, err
	}
	models := modelList(ctx, client)
	snapshot := session.snapshot()

	return acp.UnstableForkSessionResponse{
		SessionId:     id,
		Meta:          sessionResponseMeta(snapshot),
		Modes:         modeState(snapshot.mode),
		Models:        unstableModelState(snapshot.model, models),
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

func (a *Agent) SetSessionMode(ctx context.Context, params acp.SetSessionModeRequest) (acp.SetSessionModeResponse, error) {
	session, err := a.session(params.SessionId)
	if err != nil {
		return acp.SetSessionModeResponse{}, err
	}
	if params.ModeId != modeDefault && params.ModeId != modePlan {
		return acp.SetSessionModeResponse{}, acp.NewInvalidParams(map[string]any{"modeId": params.ModeId})
	}
	releaseTurn, err := session.acquireTurn(ctx)
	if err != nil {
		return acp.SetSessionModeResponse{}, err
	}
	defer releaseTurn()

	session.mu.Lock()
	session.mode = params.ModeId
	session.updatedAt = nowRFC3339()
	session.mu.Unlock()

	options := sessionConfigOptions(session, modelList(ctx, session.client))
	if err := session.emitUpdates(ctx,
		acp.SessionUpdate{CurrentModeUpdate: &acp.SessionCurrentModeUpdate{CurrentModeId: params.ModeId}},
		acp.SessionUpdate{ConfigOptionUpdate: &acp.SessionConfigOptionUpdate{ConfigOptions: options}},
	); err != nil {
		return acp.SetSessionModeResponse{}, err
	}

	return acp.SetSessionModeResponse{}, nil
}

func (a *Agent) UnstableSetSessionModel(ctx context.Context, params acp.UnstableSetSessionModelRequest) (acp.UnstableSetSessionModelResponse, error) {
	session, err := a.session(params.SessionId)
	if err != nil {
		return acp.UnstableSetSessionModelResponse{}, err
	}
	releaseTurn, err := session.acquireTurn(ctx)
	if err != nil {
		return acp.UnstableSetSessionModelResponse{}, err
	}
	defer releaseTurn()

	session.mu.Lock()
	session.model = string(params.ModelId)
	session.updatedAt = nowRFC3339()
	session.mu.Unlock()

	options := sessionConfigOptions(session, modelList(ctx, session.client))
	if err := session.emitUpdates(ctx, acp.SessionUpdate{ConfigOptionUpdate: &acp.SessionConfigOptionUpdate{ConfigOptions: options}}); err != nil {
		return acp.UnstableSetSessionModelResponse{}, err
	}

	return acp.UnstableSetSessionModelResponse{Meta: sessionResponseMeta(session.snapshot())}, nil
}

func modelState(current string, models []codex.Model) *acp.SessionModelState {
	if current == "" {
		current = "default"
	}
	available := make([]acp.ModelInfo, 0, len(models)+1)
	seen := map[string]struct{}{}
	if len(models) == 0 {
		models = []codex.Model{{ID: current, Name: current}}
	}
	for _, model := range models {
		id := firstNonEmpty(model.ID, model.Name)
		if id == "" {
			continue
		}
		if _, ok := seen[id]; ok {
			continue
		}
		seen[id] = struct{}{}
		available = append(available, acp.ModelInfo{ModelId: acp.ModelId(id), Name: firstNonEmpty(model.Name, id)})
	}

	return &acp.SessionModelState{CurrentModelId: acp.ModelId(current), AvailableModels: available}
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

func unstableModelState(current string, models []codex.Model) *acp.UnstableSessionModelState {
	if current == "" {
		current = "default"
	}
	available := make([]acp.UnstableModelInfo, 0, len(models)+1)
	if len(models) == 0 {
		models = []codex.Model{{ID: current, Name: current}}
	}
	for _, model := range models {
		id := firstNonEmpty(model.ID, model.Name)
		if id != "" {
			available = append(available, acp.UnstableModelInfo{ModelId: acp.UnstableModelId(id), Name: firstNonEmpty(model.Name, id)})
		}
	}

	return &acp.UnstableSessionModelState{CurrentModelId: acp.UnstableModelId(current), AvailableModels: available}
}

func slicesEqual(a []string, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}

	return true
}

func nowRFC3339() string {
	return timeNow().UTC().Format(timeFormatRFC3339)
}

var timeNow = defaultTimeNow

func defaultTimeNow() time.Time { return time.Now() }

const timeFormatRFC3339 = "2006-01-02T15:04:05Z07:00"

func (a *Agent) sessionByCodexThread(threadID string) *Session {
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
