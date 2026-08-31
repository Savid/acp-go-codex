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
	"sync"
	"time"

	"github.com/coder/acp-go-sdk"
	"github.com/savid/acp-go-codex/internal/codex"
)

func (a *Agent) NewSession(ctx context.Context, params acp.NewSessionRequest) (acp.NewSessionResponse, error) {
	if err := rejectLifecycleKey(params.Meta); err != nil {
		return acp.NewSessionResponse{}, err
	}

	ctx = a.observe.Extract(ctx, params.Meta)
	if err := a.ensureOpen(); err != nil {
		return acp.NewSessionResponse{}, err
	}

	if err := validateSessionStartPaths(params.Cwd, params.AdditionalDirectories); err != nil {
		return acp.NewSessionResponse{}, err
	}

	meta, err := a.sessionMetaForLifecycle(params.Meta)
	if err != nil {
		return acp.NewSessionResponse{}, err
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

	threadConfig := codex.MCPServerThreadConfig(mcpServers, meta.MCPToolApprovalMode)

	client, err := a.sharedRuntime(ctx)
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
		Config:                threadConfig,
		Environment:           cloneStringMap(meta.Env),
		ExtraPathDirs:         cloneStrings(meta.ExtraPathDirs),
	})
	if err != nil {
		return acp.NewSessionResponse{}, codexAuthRequiredError(err, nil)
	}

	session := newSession(a, id, params.Cwd, params.AdditionalDirectories, thread, client, meta, mcpServers)
	if err := session.armLifecycleEstablishment(establishmentFromContext(ctx)); err != nil {
		_ = session.Close(context.TODO())

		return acp.NewSessionResponse{}, err
	}

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

	return resp, err
}

func (a *Agent) Cancel(ctx context.Context, params acp.CancelNotification) error {
	session, err := a.session(params.SessionId)
	if err != nil {
		return err
	}

	// The route nonce authenticates the request against the session's current
	// turn, so it is validated first; the lifecycle literal is then refused
	// before the native interrupt, so a rejected cancel is never applied. Being
	// a notification, both refusals are wire-silent.
	route, err := parseInboundRoute(params.Meta)
	if err != nil {
		return routeInvalidParams(err)
	}

	if route.TurnNonce != session.activeTurnNonce() {
		return routeInvalidParams(errTurnRouteMismatch)
	}

	if lifecycleErr := rejectLifecycleKey(params.Meta); lifecycleErr != nil {
		return lifecycleErr
	}

	err = session.shutdownActiveTurnForNonce(ctx, false, route.TurnNonce)

	return cancelACPError(err, session.accountMetaSnapshot())
}

func cancelACPError(err error, account map[string]any) error {
	if errors.Is(err, errTurnRouteMismatch) {
		return routeInvalidParams(err)
	}

	return codexThreadACPError(err, account)
}

func (a *Agent) CloseSession(ctx context.Context, params acp.CloseSessionRequest) (acp.CloseSessionResponse, error) {
	if err := rejectLifecycleKey(params.Meta); err != nil {
		return acp.CloseSessionResponse{}, err
	}

	session, err := a.beginSessionClose(params.SessionId)
	if err != nil {
		return acp.CloseSessionResponse{}, err
	}

	session.mu.Lock()
	retryCommit := session.closeContained
	commitDone := session.closeCommitDone
	session.mu.Unlock()

	var gateErr error
	if !retryCommit {
		gateErr = session.beginLifecycleClose(ctx)
	}

	// Pending provider-auth flows are cancelled before anything native is torn
	// down, because a flow abandoned to a process already being interrupted has
	// nobody left to cancel it.
	a.closeSessionProviderAuth(params.SessionId)

	// Close interrupts and contains a live native turn before allowing its ACP
	// settlement to terminalize. Admission is already closed, so no later prompt
	// can enter behind this boundary.
	var shutdownErr error
	if !retryCommit {
		shutdownErr = errors.Join(gateErr, session.shutdownActiveTurn(ctx, true))
		session.awaitPromptSettlement()
	}

	session.sessionOps.Lock()
	defer session.sessionOps.Unlock()

	if !retryCommit {
		if containErr := errors.Join(shutdownErr, session.containSession(ctx)); containErr != nil {
			// An incomplete boundary terminalizes nothing and commits nothing new.
			// The stream is fenced either way, because the session it belonged to is
			// over whether or not its descendants could be proved gone.
			session.fenceSession()
			a.abortSessionClose(params.SessionId, session)

			return acp.CloseSessionResponse{}, codexThreadACPError(containErr, session.accountMetaSnapshot())
		}

		session.mu.Lock()
		session.closeContained = true
		session.mu.Unlock()
	}

	// The durable rung belongs to the session, not to the incarnation: the
	// emission rungs apply only to a live incarnation on the persistent thread
	// stream, so a close between foreground turns still owes the commit. A prefix
	// a settlement captured and could not place is the
	// resumable state, so a capture the store refuses fails the close rather
	// than being dropped with the session wrapper. The commit runs on a detached
	// context: a host that cancelled its call is not a reason to lose frames it
	// was already shown.
	if !commitDone {
		if commitErr := session.commitResumableSnapshot(ctx); commitErr != nil {
			session.mu.Lock()
			session.closeCommitPending = true
			session.mu.Unlock()
			session.fenceSession()

			return acp.CloseSessionResponse{}, commitErr
		}

		session.mu.Lock()
		session.closeCommitDone = true
		session.mu.Unlock()
	}

	session.fenceSession()

	removed, retained, removeErr := a.finishSessionCloseRetainingThread(params.SessionId, session)
	if removeErr != nil {
		session.mu.Lock()
		session.closeRemovalPending = true
		session.mu.Unlock()

		return acp.CloseSessionResponse{}, removeErr
	}

	var closeErr error

	if removed && !retained {
		closeErr = session.releaseMaterialized()
	}

	if removed {
		a.observe.AddActiveSession(ctx, -1)
	}

	return acp.CloseSessionResponse{}, closeErr
}

func (a *Agent) UnstableDeleteSession(ctx context.Context, params acp.UnstableDeleteSessionRequest) (acp.UnstableDeleteSessionResponse, error) {
	if err := rejectLifecycleKey(params.Meta); err != nil {
		return acp.UnstableDeleteSessionResponse{}, err
	}

	ctx = a.observe.Extract(ctx, params.Meta)
	if params.SessionId == "" {
		return acp.UnstableDeleteSessionResponse{}, acp.NewInvalidParams(map[string]any{jsonFieldSessionID: validationRequired})
	}

	if err := a.ensureOpen(); err != nil {
		return acp.UnstableDeleteSessionResponse{}, err
	}

	// The id is fenced before anything is inspected, because a store-only delete
	// has no wrapper whose close flag could carry the fence. Load and resume are
	// refused for the whole delete, so a native resume already in flight cannot
	// register a wrapper behind the delete that already found none.
	defer a.claimSessionDelete(params.SessionId)()

	// The durable tombstone is the delete's first act, and it is bounded by the
	// caller's own context. Nothing is torn down ahead of it, so a delete that
	// cannot tombstone has changed nothing and the id stays entirely the host's;
	// once it is written the id is gone from the wire whatever the teardown
	// behind it goes on to do.
	storeCtx, cancel := a.sessionStoreContext(ctx)
	tombstoneErr := a.sessionStore().Delete(storeCtx, SessionKey{SessionID: string(params.SessionId)})

	cancel()

	if tombstoneErr != nil {
		return acp.UnstableDeleteSessionResponse{}, tombstoneErr
	}

	// Hiding follows the tombstone immediately and never waits on teardown: from
	// here the id answers every session-scoped request method as unknown and
	// appears in no listing, while the agent goes on owning whatever native scope
	// the teardown has not yet released, so a later delete and Agent.Close can
	// still reach it.
	a.mu.Lock()
	a.deleted[params.SessionId] = struct{}{}
	a.mu.Unlock()

	return acp.UnstableDeleteSessionResponse{}, a.tearDownDeletedSession(ctx, params.SessionId)
}

// tearDownDeletedSession runs the shutdown ladder behind a committed tombstone.
// The id is already hidden, so every rung answers only for native scope the
// agent still owns: a rung that fails surfaces its error with the session hidden
// and retained, and the next delete runs the ladder again.
func (a *Agent) tearDownDeletedSession(ctx context.Context, id acp.SessionId) error {
	active, err := a.beginSessionDelete(id)
	if errors.Is(err, errNoActiveSessionForDelete) {
		active = nil
	} else if err != nil {
		return err
	}

	// The ladder's fourth rung runs on delete exactly as it does on close, and
	// it runs whether or not a wrapper is still active: the id is being retired
	// for good, and a flow left armed against it would hold a nonterminal record
	// for a session nothing can readmit.
	a.closeSessionProviderAuth(id)

	if active != nil {
		gateErr := active.beginLifecycleClose(ctx)
		shutdownErr := errors.Join(gateErr, active.shutdownActiveTurn(ctx, true))
		active.awaitPromptSettlement()
		active.sessionOps.Lock()

		containErr := errors.Join(shutdownErr, active.containSession(ctx))
		if containErr != nil {
			active.sessionOps.Unlock()
			active.fenceSession()

			// Only the closed mark this rung set is undone, and only so the delete
			// that retries this teardown can take it again. Nothing is re-admitted
			// by that: the tombstone is what hides the id now, and the flows the
			// fourth rung cancelled belong to a session nothing can readmit.
			a.clearSessionClosing(id, active)

			return codexThreadACPError(containErr, active.accountMetaSnapshot())
		}
	}

	var claimedRetained *retainedRuntimeThread
	if active == nil {
		claimedRetained, err = a.claimRetainedRuntimeThreadForDelete(id)
		if errors.Is(err, errNoRetainedRuntimeThread) {
			claimedRetained = nil
		} else if err != nil {
			return err
		}
	}
	defer a.releaseRetainedRuntimeThreadClaim(claimedRetained)

	if claimedRetained != nil {
		retainedSession := &session{
			agent:         a,
			id:            claimedRetained.sessionID,
			client:        claimedRetained.client,
			codexThreadID: claimedRetained.threadID,
		}
		if err := retainedSession.containSession(ctx); err != nil {
			return err
		}
	}

	a.mu.Lock()

	retained := claimedRetained
	if retained == nil {
		retained = a.retainedThreads[id]
	}

	removed := active != nil && a.sessions[id] == active
	if removed {
		delete(a.sessions, id)
	}
	a.mu.Unlock()

	// The persistence fence stands well behind the tombstone, and what makes the
	// gap harmless is store tombstone finality: a settlement that commits in it
	// writes nothing, because the store refuses every write addressed to a key it
	// has tombstoned. The fence is still taken here so the wrapper stops trying.
	if active != nil {
		active.fenceSession()
		active.fencePersistence()
		active.sessionOps.Unlock()
	}

	threadID := ""
	if active != nil {
		threadID = active.snapshot().codexThreadID
	} else if retained != nil {
		threadID = retained.threadID
	}

	var cleanupErr error
	if err := a.deleteNativeCodexSession(ctx, id, threadID); err != nil {
		cleanupErr = errors.Join(cleanupErr, err)
	} else if err := a.endRetainedRuntimeThread(retained); err != nil {
		cleanupErr = errors.Join(cleanupErr, err)
	}

	if removed {
		cleanupErr = errors.Join(cleanupErr, active.releaseMaterialized())

		a.observe.AddActiveSession(ctx, -1)
	}

	return cleanupErr
}

func (a *Agent) ListSessions(ctx context.Context, params acp.ListSessionsRequest) (acp.ListSessionsResponse, error) {
	if err := rejectLifecycleKey(params.Meta); err != nil {
		return acp.ListSessionsResponse{}, err
	}

	if err := a.ensureOpen(); err != nil {
		return acp.ListSessionsResponse{}, err
	}

	if err := validateOptionalAbsolutePath(jsonFieldCwd, params.Cwd); err != nil {
		return acp.ListSessionsResponse{}, err
	}

	a.mu.Lock()
	active := make([]*session, 0, len(a.sessions))

	for _, session := range a.sessions {
		// A wrapper the agent still owns for a tombstoned id is teardown state,
		// never a listable session: hiding is a wire fact and does not wait for
		// the native scope to be released.
		if a.deleteCommittedLocked(session.id) {
			continue
		}

		if params.Cwd != nil && session.cwd != *params.Cwd {
			continue
		}

		active = append(active, session)
	}
	a.mu.Unlock()

	sessions := make([]acp.SessionInfo, 0, len(active))

	seen := map[acp.SessionId]struct{}{}
	for _, session := range active {
		addSessionInfo(&sessions, seen, session.info())
	}

	storeSessions, err := a.listStoredSessions(ctx, params.Cwd, seen)
	if err != nil {
		return acp.ListSessionsResponse{}, err
	}

	for _, session := range storeSessions {
		addSessionInfo(&sessions, seen, session)
	}

	a.retryDeletedNativeCodexSessions(ctx)

	paged, nextCursor, err := paginateSessionInfos(sessions, params.Cursor)
	if err != nil {
		return acp.ListSessionsResponse{}, err
	}

	return acp.ListSessionsResponse{Sessions: paged, NextCursor: nextCursor}, nil
}
func (a *Agent) isDeleted(id acp.SessionId) bool {
	a.mu.Lock()
	defer a.mu.Unlock()

	return a.deleteCommittedLocked(id)
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
	if session == nil {
		return nil
	}

	session.mu.Lock()
	matches := session.fingerprint == fingerprint
	session.mu.Unlock()

	if !matches {
		return nil
	}

	return session
}

func (a *Agent) activeSession(id acp.SessionId) *session {
	a.mu.Lock()
	defer a.mu.Unlock()

	return a.sessions[id]
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
	Model               string            `json:"model,omitempty"`
	ReasoningEffort     string            `json:"reasoningEffort,omitempty"`
	ServiceTier         string            `json:"serviceTier,omitempty"`
	Personality         string            `json:"personality,omitempty"`
	Env                 map[string]string `json:"env,omitempty"`
	ExtraPathDirs       []string          `json:"extraPathDirs,omitempty"`
	ApprovalPolicy      any               `json:"approvalPolicy,omitempty"`
	SandboxPolicy       any               `json:"sandboxPolicy,omitempty"`
	OutputSchema        any               `json:"outputSchema,omitempty"`
	RawMessages         rawMessageConfig  `json:"rawMessages,omitzero"`
	MCPToolApprovalMode string            `json:"mcpToolApprovalMode,omitempty"`
}

func codexMetaFingerprint(meta sessionMeta) codexMetaForHash {
	return codexMetaForHash{
		Model:               meta.Model,
		ReasoningEffort:     meta.ReasoningEffort,
		ServiceTier:         meta.ServiceTier,
		Personality:         meta.Personality,
		Env:                 cloneStringMap(meta.Env),
		ExtraPathDirs:       cloneStrings(meta.ExtraPathDirs),
		ApprovalPolicy:      cloneAny(meta.ApprovalPolicy),
		SandboxPolicy:       cloneAny(meta.SandboxPolicy),
		OutputSchema:        cloneAny(meta.OutputSchema),
		RawMessages:         meta.RawMessages,
		MCPToolApprovalMode: meta.MCPToolApprovalMode,
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

func (a *Agent) ResumeSession(ctx context.Context, params acp.ResumeSessionRequest) (response acp.ResumeSessionResponse, resultErr error) {
	if err := rejectLifecycleKey(params.Meta); err != nil {
		return acp.ResumeSessionResponse{}, err
	}

	ctx = a.observe.Extract(ctx, params.Meta)
	if err := a.ensureOpen(); err != nil {
		return acp.ResumeSessionResponse{}, err
	}

	if err := validateSessionStartPaths(params.Cwd, params.AdditionalDirectories); err != nil {
		return acp.ResumeSessionResponse{}, err
	}

	// The tombstone is read before lifecycle admission, because a delete whose
	// teardown failed keeps its wrapper — and that wrapper's closed mark would
	// otherwise answer a retriable conflict for an id that is permanently gone.
	if a.isDeleted(params.SessionId) {
		a.retryDeleteNativeCodexSession(ctx, params.SessionId, "")

		return acp.ResumeSessionResponse{}, newUnknownSession()
	}

	releaseLifecycle, err := a.acquireSessionLifecycle(params.SessionId)
	if err != nil {
		return acp.ResumeSessionResponse{}, err
	}
	defer releaseLifecycle()

	meta, err := a.sessionMetaForLifecycle(params.Meta)
	if err != nil {
		return acp.ResumeSessionResponse{}, err
	}

	start := codexSessionStart{
		Cwd:                   params.Cwd,
		AdditionalDirectories: params.AdditionalDirectories,
		McpServers:            params.McpServers,
		Meta:                  meta,
		ResumeID:              string(params.SessionId),
	}
	if session := a.activeSessionForStart(params.SessionId, start); session != nil {
		finish, admissionErr := a.beginActiveLifecycleAdmission(ctx, session)
		if admissionErr != nil {
			return acp.ResumeSessionResponse{}, admissionErr
		}
		defer func() { resultErr = finish(resultErr) }()

		models := modelList(ctx, session.client)
		snapshot := session.snapshot()

		return acp.ResumeSessionResponse{
			Meta:          sessionResponseMeta(snapshot),
			ConfigOptions: sessionConfigOptions(session, models),
		}, nil
	}

	storeEntries, loadErr := a.loadStoredSession(ctx, params.SessionId)
	if loadErr != nil {
		return acp.ResumeSessionResponse{}, loadErr
	}

	if len(storeEntries) > 0 {
		return a.resumeMaterializedSession(ctx, params, storeEntries)
	}

	if a.activeSession(params.SessionId) == nil {
		a.retryDeleteNativeCodexSession(ctx, params.SessionId, "")
	}

	return acp.ResumeSessionResponse{}, newUnknownSession()
}

func (a *Agent) resumeMaterializedSession(ctx context.Context, params acp.ResumeSessionRequest, entries []SessionStoreEntry) (acp.ResumeSessionResponse, error) {
	if err := a.ensureOpen(); err != nil {
		return acp.ResumeSessionResponse{}, err
	}

	meta, err := a.sessionMetaForLifecycle(params.Meta)
	if err != nil {
		return acp.ResumeSessionResponse{}, err
	}

	if active := a.activeSession(params.SessionId); active != nil {
		return a.rebindActiveStoredSession(ctx, params, entries, meta, active, nil)
	}

	retained, err := a.claimRetainedRuntimeThreadForStore(params.SessionId, rolloutNativeThreadID(entries))
	if errors.Is(err, errNoRetainedRuntimeThread) {
		retained = nil
	} else if err != nil {
		return acp.ResumeSessionResponse{}, err
	}

	if retained != nil {
		resp, _, resumeErr := a.resumeRetainedRuntimeSession(ctx, params, meta, retained)

		return resp, resumeErr
	}

	mcpServers, err := a.prepareMCPServers(ctx, params.SessionId, params.McpServers)
	if err != nil {
		return acp.ResumeSessionResponse{}, err
	}

	config := codex.MCPServerThreadConfig(mcpServers, meta.MCPToolApprovalMode)

	hydrated, materializedBytes, err := a.hydrateStoredRollout(ctx, params.SessionId, entries)
	if err != nil {
		return acp.ResumeSessionResponse{}, err
	}

	residenceRelease, capacityErr := a.reserveNativeResidenceCapacity(ctx, materializedBytes)
	if capacityErr != nil {
		return acp.ResumeSessionResponse{}, capacityErr
	}

	client, err := a.sharedRuntime(ctx)
	if err != nil {
		residenceRelease()

		return acp.ResumeSessionResponse{}, err
	}

	path, scratchRelease, materializedBytes, err := a.materializeStoredRollout(ctx, hydrated, residenceRelease)
	if err != nil {
		return acp.ResumeSessionResponse{}, err
	}

	a.mu.Lock()
	materializedEpoch := a.runtimeEpoch
	a.mu.Unlock()

	threadID := firstNonEmpty(rolloutNativeThreadID(entries), string(params.SessionId))

	thread, err := client.ResumeThread(ctx, codex.ThreadResumeRequest{
		ThreadID:      threadID,
		Path:          path,
		Cwd:           params.Cwd,
		Config:        config,
		Environment:   cloneStringMap(meta.Env),
		ExtraPathDirs: cloneStrings(meta.ExtraPathDirs),
	})
	if err != nil {
		_ = a.retireMaterializedRolloutAtEpoch(path, materializedBytes, scratchRelease, materializedEpoch)

		return acp.ResumeSessionResponse{}, codexThreadACPError(err, nil)
	}

	id := params.SessionId

	session := newSession(a, id, params.Cwd, params.AdditionalDirectories, thread, client, meta, mcpServers)
	if err := session.armLifecycleEstablishment(establishmentFromContext(ctx)); err != nil {
		_ = session.Close(context.TODO())

		return acp.ResumeSessionResponse{}, err
	}

	session.materializedPath = path
	session.materializedRelease = scratchRelease
	session.materializedBytes = materializedBytes
	session.materializedEpoch = materializedEpoch
	session.fingerprint = codexSessionStartFingerprint(codexSessionStart{
		Cwd:                   params.Cwd,
		AdditionalDirectories: params.AdditionalDirectories,
		McpServers:            params.McpServers,
		Meta:                  meta,
		ResumeID:              string(params.SessionId),
		MaterializedPath:      path,
	})
	session.setAccount(clientAccountMeta(ctx, client))

	if err := a.runtimeReadyCanary(ctx, client, session); err != nil {
		_ = session.Close(context.Background())

		return acp.ResumeSessionResponse{}, err
	}

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

func (a *Agent) resumeRetainedRuntimeSession(
	ctx context.Context,
	params acp.ResumeSessionRequest,
	meta sessionMeta,
	retained *retainedRuntimeThread,
) (acp.ResumeSessionResponse, *session, error) {
	releaseClaim := true
	defer func() {
		if releaseClaim {
			a.releaseRetainedRuntimeThreadClaim(retained)
		}
	}()

	mcpServers, err := a.prepareMCPServers(ctx, params.SessionId, params.McpServers)
	if err != nil {
		return acp.ResumeSessionResponse{}, nil, err
	}

	config := codex.MCPServerThreadConfig(mcpServers, meta.MCPToolApprovalMode)

	thread, err := retained.client.ResumeThread(ctx, codex.ThreadResumeRequest{
		ThreadID:      retained.threadID,
		Path:          retained.path,
		Cwd:           params.Cwd,
		Config:        config,
		Environment:   cloneStringMap(meta.Env),
		ExtraPathDirs: cloneStrings(meta.ExtraPathDirs),
	})
	if err != nil {
		return acp.ResumeSessionResponse{}, nil, codexThreadACPError(err, nil)
	}

	var session *session

	rollback := func(cause error) error {
		closeCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), closeTimeout)
		defer cancel()

		if session != nil {
			session.abandonLifecycleEstablishment()

			if rebindErr := session.prepareNativeEventRebind(); rebindErr != nil {
				cause = errors.Join(cause, rebindErr)
			}
		}

		threadID := firstNonEmpty(thread.ID, retained.threadID)
		if unsubscribeErr := retained.client.UnsubscribeThread(closeCtx, threadID); unsubscribeErr != nil {
			releaseClaim = false

			return errors.Join(cause, fmt.Errorf("rollback retained Codex resume: %w", unsubscribeErr))
		}

		return cause
	}

	if thread.ID != retained.threadID {
		cause := acp.NewInvalidRequest(map[string]any{
			jsonFieldError: "Codex resumed a different retained native thread",
		})

		return acp.ResumeSessionResponse{}, nil, rollback(cause)
	}

	if thread.Path == "" {
		thread.Path = retained.path
	} else if retained.path != "" && thread.Path != retained.path {
		cause := acp.NewInvalidRequest(map[string]any{
			jsonFieldError: "Codex resumed the retained thread at a different rollout path",
		})

		return acp.ResumeSessionResponse{}, nil, rollback(cause)
	}

	session = newSession(a, params.SessionId, params.Cwd, params.AdditionalDirectories, thread, retained.client, meta, mcpServers)
	if err := session.armLifecycleEstablishment(establishmentFromContext(ctx)); err != nil {
		return acp.ResumeSessionResponse{}, nil, rollback(err)
	}

	session.fingerprint = codexSessionStartFingerprint(codexSessionStart{
		Cwd:                   params.Cwd,
		AdditionalDirectories: params.AdditionalDirectories,
		McpServers:            params.McpServers,
		Meta:                  meta,
		ResumeID:              string(params.SessionId),
	})
	session.setAccount(clientAccountMeta(ctx, retained.client))

	if err := a.runtimeReadyCanary(ctx, retained.client, session); err != nil {
		return acp.ResumeSessionResponse{}, nil, rollback(err)
	}

	if err := a.storeRetainedRuntimeSession(session, retained); err != nil {
		return acp.ResumeSessionResponse{}, nil, rollback(err)
	}

	models := modelList(ctx, retained.client)
	snapshot := session.snapshot()

	return acp.ResumeSessionResponse{
		Meta:          sessionResponseMeta(snapshot),
		ConfigOptions: sessionConfigOptions(session, models),
	}, session, nil
}

// rebindActiveStoredSession refreshes lifecycle configuration without
// hydrating a second rollout for a thread already owned by this app-server.
// The durable snapshot may corroborate the native thread ID, but it cannot
// redirect an active ACP session to another native thread or path.
func (a *Agent) rebindActiveStoredSession(
	ctx context.Context,
	params acp.ResumeSessionRequest,
	entries []SessionStoreEntry,
	meta sessionMeta,
	active *session,
	afterRebind ...func() error,
) (response acp.ResumeSessionResponse, err error) {
	finish, err := a.beginActiveLifecycleAdmission(ctx, active)
	if err != nil {
		return acp.ResumeSessionResponse{}, err
	}
	defer func() { err = finish(err) }()

	if liveErr := active.ensureLiveClient(ctx); liveErr != nil {
		return acp.ResumeSessionResponse{}, codexThreadACPError(liveErr, active.accountMetaSnapshot())
	}

	client, ownedThreadID, ownedPath, live := active.activeThreadOwnership()
	if !live || client == nil || ownedThreadID == "" {
		return acp.ResumeSessionResponse{}, acp.NewInvalidRequest(map[string]any{
			jsonFieldError: "active Codex thread ownership is unavailable",
		})
	}

	storedThreadID := rolloutNativeThreadID(entries)
	if storedThreadID != "" && storedThreadID != ownedThreadID {
		return acp.ResumeSessionResponse{}, acp.NewInvalidRequest(map[string]any{
			jsonFieldError: "stored Codex thread does not match the active session",
		})
	}

	mcpServers, err := a.prepareMCPServers(ctx, params.SessionId, params.McpServers)
	if err != nil {
		return acp.ResumeSessionResponse{}, err
	}

	config := codex.MCPServerThreadConfig(mcpServers, meta.MCPToolApprovalMode)

	thread, err := client.ResumeThread(ctx, codex.ThreadResumeRequest{
		ThreadID:      ownedThreadID,
		Path:          ownedPath,
		Cwd:           params.Cwd,
		Config:        config,
		Environment:   cloneStringMap(meta.Env),
		ExtraPathDirs: cloneStrings(meta.ExtraPathDirs),
	})
	if err != nil {
		return acp.ResumeSessionResponse{}, codexThreadACPError(err, active.accountMetaSnapshot())
	}

	if thread.ID != ownedThreadID {
		active.setClientDead(true)

		return acp.ResumeSessionResponse{}, acp.NewInvalidRequest(map[string]any{
			jsonFieldError: "Codex resumed a different native thread",
		})
	}

	if thread.Path == "" {
		thread.Path = ownedPath
	} else if ownedPath != "" && thread.Path != ownedPath {
		active.setClientDead(true)

		return acp.ResumeSessionResponse{}, acp.NewInvalidRequest(map[string]any{
			jsonFieldError: "Codex resumed the active thread at a different rollout path",
		})
	}

	if err := a.runtimeReadyCanaryWithConfig(ctx, client, active, config); err != nil {
		return acp.ResumeSessionResponse{}, err
	}

	fingerprint := codexSessionStartFingerprint(codexSessionStart{
		Cwd:                   params.Cwd,
		AdditionalDirectories: params.AdditionalDirectories,
		McpServers:            params.McpServers,
		Meta:                  meta,
		ResumeID:              string(params.SessionId),
	})
	accountMeta := clientAccountMeta(ctx, client)

	a.mu.Lock()
	current := a.sessions[params.SessionId]
	runtimeCurrent := a.runtimeClient == client && !a.runtimeDead

	if current == active && runtimeCurrent {
		active.applyActiveRebind(
			thread,
			params.Cwd,
			params.AdditionalDirectories,
			meta,
			mcpServers,
			fingerprint,
			accountMeta,
		)
	}
	a.mu.Unlock()

	if current != active || !runtimeCurrent {
		return acp.ResumeSessionResponse{}, acp.NewInvalidRequest(map[string]any{
			jsonFieldError: "active Codex thread ownership changed during resume",
		})
	}

	if len(afterRebind) != 0 && afterRebind[0] != nil {
		if err := afterRebind[0](); err != nil {
			return acp.ResumeSessionResponse{}, err
		}
	}

	models := modelList(ctx, client)
	snapshot := active.snapshot()

	return acp.ResumeSessionResponse{
		Meta:          sessionResponseMeta(snapshot),
		ConfigOptions: sessionConfigOptions(active, models),
	}, nil
}

// beginActiveLifecycleAdmission is the single ownership gate for every active
// Resume and Load path. It excludes foreground and agent-origin turns before
// establishment is armed, and keeps native broker handoff and replay exclusive
// until either the direct call finishes or the registered response settles the
// establishment obligation.
func (a *Agent) beginActiveLifecycleAdmission(
	ctx context.Context,
	active *session,
) (func(error) error, error) {
	releaseTurn, err := active.acquireTurn(ctx)
	if err != nil {
		return nil, err
	}

	if err := active.beginActiveNativeRebind(ctx); err != nil {
		releaseTurn()

		return nil, err
	}

	obligation := establishmentFromContext(ctx)
	if err := active.armLifecycleEstablishment(obligation); err != nil {
		finishErr := active.completeActiveNativeRebind(ctx)

		releaseTurn()

		return nil, errors.Join(err, finishErr)
	}

	responseBound := obligation != nil && a.negotiatedLifecycle().Present()

	var (
		once      sync.Once
		finishErr error
	)

	return func(operationErr error) error {
		once.Do(func() {
			if !responseBound {
				finishErr = active.completeActiveNativeRebind(ctx)
			}

			releaseTurn()
		})

		return errors.Join(operationErr, finishErr)
	}, nil
}

func (a *Agent) LoadSession(ctx context.Context, params acp.LoadSessionRequest) (response acp.LoadSessionResponse, resultErr error) {
	if err := rejectLifecycleKey(params.Meta); err != nil {
		return acp.LoadSessionResponse{}, err
	}

	ctx = a.observe.Extract(ctx, params.Meta)
	if err := a.ensureOpen(); err != nil {
		return acp.LoadSessionResponse{}, err
	}

	if err := validateSessionStartPaths(params.Cwd, params.AdditionalDirectories); err != nil {
		return acp.LoadSessionResponse{}, err
	}

	// The tombstone is read before lifecycle admission, because a delete whose
	// teardown failed keeps its wrapper — and that wrapper's closed mark would
	// otherwise answer a retriable conflict for an id that is permanently gone.
	if a.isDeleted(params.SessionId) {
		a.retryDeleteNativeCodexSession(ctx, params.SessionId, "")

		return acp.LoadSessionResponse{}, newUnknownSession()
	}

	releaseLifecycle, err := a.acquireSessionLifecycle(params.SessionId)
	if err != nil {
		return acp.LoadSessionResponse{}, err
	}
	defer releaseLifecycle()

	meta, err := a.sessionMetaForLifecycle(params.Meta)
	if err != nil {
		return acp.LoadSessionResponse{}, err
	}

	start := codexSessionStart{
		Cwd:                   params.Cwd,
		AdditionalDirectories: params.AdditionalDirectories,
		McpServers:            params.McpServers,
		Meta:                  meta,
		ResumeID:              string(params.SessionId),
	}
	if existing := a.activeSessionForStart(params.SessionId, start); existing != nil {
		finish, admissionErr := a.beginActiveLifecycleAdmission(ctx, existing)
		if admissionErr != nil {
			return acp.LoadSessionResponse{}, admissionErr
		}
		defer func() { resultErr = finish(resultErr) }()

		storeEntries, loadErr := a.loadStoredSession(ctx, params.SessionId)
		if loadErr != nil {
			return acp.LoadSessionResponse{}, loadErr
		}

		if len(storeEntries) == 0 {
			return acp.LoadSessionResponse{}, newUnknownSession()
		}

		if replayErr := existing.replayRollout(ctx, storeEntries); replayErr != nil {
			return acp.LoadSessionResponse{}, replayErr
		}

		models := modelList(ctx, existing.client)
		snapshot := existing.snapshot()

		return acp.LoadSessionResponse{
			Meta:          sessionResponseMeta(snapshot),
			ConfigOptions: sessionConfigOptions(existing, models),
		}, nil
	}

	storeEntries, loadErr := a.loadStoredSession(ctx, params.SessionId)
	if loadErr != nil {
		return acp.LoadSessionResponse{}, loadErr
	}

	if len(storeEntries) > 0 {
		return a.loadMaterializedSession(ctx, params, storeEntries)
	}

	if a.activeSession(params.SessionId) == nil {
		a.retryDeleteNativeCodexSession(ctx, params.SessionId, "")
	}

	return acp.LoadSessionResponse{}, newUnknownSession()
}

// listStoredSessions lists store-backed session summaries unconditionally.
// When cwd is provided it filters summaries whose recorded cwd differs;
// summaries with an empty recorded cwd are always retained.
func (a *Agent) listStoredSessions(ctx context.Context, cwd *string, activeIDs map[acp.SessionId]struct{}) ([]acp.SessionInfo, error) {
	listCtx, cancel := a.sessionStoreContext(ctx)
	defer cancel()

	summaries, err := a.sessionStore().ListSessions(listCtx)
	if err != nil {
		return nil, err
	}

	for _, summary := range summaries {
		if err := a.sweepSessionImageArtifacts(listCtx, summary.SessionID); err != nil {
			return nil, err
		}
	}

	filterCwd := ""
	if cwd != nil {
		filterCwd = *cwd
	}

	out := make([]acp.SessionInfo, 0, len(summaries))
	for _, summary := range summaries {
		id := acp.SessionId(summary.SessionID)
		if _, ok := activeIDs[id]; ok {
			continue
		}

		if cwd != nil && summary.Cwd != "" && summary.Cwd != filterCwd {
			continue
		}

		updatedAt := time.UnixMilli(summary.UpdatedAtUnixMilli).UTC().Format(time.RFC3339)
		title := firstNonEmpty(summary.Title, "Codex session "+summary.SessionID)

		codexMeta := map[string]any{
			valueStored:     true,
			jsonFieldSource: "sessionStore",
		}
		for key, value := range summary.Meta {
			codexMeta[key] = cloneAny(value)
		}

		out = append(out, acp.SessionInfo{
			SessionId: id,
			Cwd:       firstNonEmpty(summary.Cwd, filterCwd),
			Title:     &title,
			UpdatedAt: &updatedAt,
			Meta: map[string]any{
				codexMetaKey: codexMeta,
			},
		})
	}

	return out, nil
}

func (a *Agent) loadStoredSession(ctx context.Context, sessionID acp.SessionId) ([]SessionStoreEntry, error) {
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

	meta, err := a.sessionMetaForLifecycle(params.Meta)
	if err != nil {
		return acp.LoadSessionResponse{}, err
	}

	if active := a.activeSession(params.SessionId); active != nil {
		resp, rebindErr := a.rebindActiveStoredSession(ctx, acp.ResumeSessionRequest(params), entries, meta, active, func() error {
			return active.replayRollout(ctx, entries)
		})
		if rebindErr != nil {
			return acp.LoadSessionResponse{}, rebindErr
		}

		return acp.LoadSessionResponse(resp), nil
	}

	retained, err := a.claimRetainedRuntimeThreadForStore(params.SessionId, rolloutNativeThreadID(entries))
	if errors.Is(err, errNoRetainedRuntimeThread) {
		retained = nil
	} else if err != nil {
		return acp.LoadSessionResponse{}, err
	}

	if retained != nil {
		resp, active, resumeErr := a.resumeRetainedRuntimeSession(ctx, acp.ResumeSessionRequest(params), meta, retained)
		if resumeErr != nil {
			return acp.LoadSessionResponse{}, resumeErr
		}

		if replayErr := active.replayRollout(ctx, entries); replayErr != nil {
			_, closeErr := a.CloseSession(context.WithoutCancel(ctx), acp.CloseSessionRequest{SessionId: params.SessionId})

			return acp.LoadSessionResponse{}, errors.Join(replayErr, closeErr)
		}

		return acp.LoadSessionResponse(resp), nil
	}

	mcpServers, err := a.prepareMCPServers(ctx, params.SessionId, params.McpServers)
	if err != nil {
		return acp.LoadSessionResponse{}, err
	}

	config := codex.MCPServerThreadConfig(mcpServers, meta.MCPToolApprovalMode)

	hydrated, materializedBytes, err := a.hydrateStoredRollout(ctx, params.SessionId, entries)
	if err != nil {
		return acp.LoadSessionResponse{}, err
	}

	residenceRelease, capacityErr := a.reserveNativeResidenceCapacity(ctx, materializedBytes)
	if capacityErr != nil {
		return acp.LoadSessionResponse{}, capacityErr
	}

	client, err := a.sharedRuntime(ctx)
	if err != nil {
		residenceRelease()

		return acp.LoadSessionResponse{}, err
	}

	path, scratchRelease, materializedBytes, err := a.materializeStoredRollout(ctx, hydrated, residenceRelease)
	if err != nil {
		return acp.LoadSessionResponse{}, err
	}

	a.mu.Lock()
	materializedEpoch := a.runtimeEpoch
	a.mu.Unlock()

	thread, err := client.ResumeThread(ctx, codex.ThreadResumeRequest{
		ThreadID:      firstNonEmpty(rolloutNativeThreadID(entries), string(params.SessionId)),
		Path:          path,
		Cwd:           params.Cwd,
		Config:        config,
		Environment:   cloneStringMap(meta.Env),
		ExtraPathDirs: cloneStrings(meta.ExtraPathDirs),
	})
	if err != nil {
		_ = a.retireMaterializedRolloutAtEpoch(path, materializedBytes, scratchRelease, materializedEpoch)

		return acp.LoadSessionResponse{}, codexThreadACPError(err, nil)
	}

	id := params.SessionId

	session := newSession(a, id, params.Cwd, params.AdditionalDirectories, thread, client, meta, mcpServers)
	if err := session.armLifecycleEstablishment(establishmentFromContext(ctx)); err != nil {
		_ = session.Close(context.TODO())

		return acp.LoadSessionResponse{}, err
	}

	session.materializedPath = path
	session.materializedRelease = scratchRelease
	session.materializedBytes = materializedBytes
	session.materializedEpoch = materializedEpoch
	session.fingerprint = codexSessionStartFingerprint(codexSessionStart{
		Cwd:                   params.Cwd,
		AdditionalDirectories: params.AdditionalDirectories,
		McpServers:            params.McpServers,
		Meta:                  meta,
		ResumeID:              string(params.SessionId),
		MaterializedPath:      path,
	})
	session.setAccount(clientAccountMeta(ctx, client))

	if err := a.runtimeReadyCanary(ctx, client, session); err != nil {
		_ = session.Close(context.Background())

		return acp.LoadSessionResponse{}, err
	}

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

// retryDeleteNativeCodexSession finishes the native cleanup a delete could not.
// It answers only for ids nothing owns: a tombstoned id whose wrapper the agent
// still holds is torn down by that wrapper's own boundary — another delete, or
// Agent.Close — and removing its thread here would make that boundary fail on a
// thread this retry took away.
func (a *Agent) retryDeleteNativeCodexSession(ctx context.Context, sessionID acp.SessionId, threadID string) {
	a.mu.Lock()

	owned := a.sessions[sessionID] != nil
	retained := a.retainedThreads[sessionID]

	if threadID == "" && retained != nil {
		threadID = retained.threadID
	}
	a.mu.Unlock()

	if owned {
		return
	}

	if err := a.deleteNativeCodexSession(ctx, sessionID, threadID); err != nil {
		a.log.DebugContext(ctx, "retry delete native Codex session failed", slog.String(jsonFieldSessionID, string(sessionID)))
	} else if err := a.endRetainedRuntimeThread(retained); err != nil {
		a.log.DebugContext(ctx, "release deleted native Codex session failed", slog.String(jsonFieldSessionID, string(sessionID)))
	}
}

func (a *Agent) deleteNativeCodexSession(ctx context.Context, sessionID acp.SessionId, knownThreadID string) error {
	client, err := a.sharedRuntime(ctx)
	if err != nil {
		return err
	}

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

	for i := range threads {
		thread := &threads[i]
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

func (a *Agent) forkSession(ctx context.Context, params acp.UnstableForkSessionRequest) (
	response acp.UnstableForkSessionResponse,
	returnErr error,
) {
	ctx = a.observe.Extract(ctx, params.Meta)

	parent, err := a.session(params.SessionId)
	if err != nil {
		return acp.UnstableForkSessionResponse{}, err
	}

	if parent.lifecycleEstablishmentPending() {
		return acp.UnstableForkSessionResponse{}, acp.NewInvalidRequest(map[string]any{
			jsonFieldError: "Codex parent session establishment response is still outstanding",
			jsonFieldLimit: limitSessionPrompt,
		})
	}

	if pathErr := validateSessionStartPaths(params.Cwd, params.AdditionalDirectories); pathErr != nil {
		return acp.UnstableForkSessionResponse{}, pathErr
	}

	meta, err := a.sessionMetaForLifecycle(params.Meta)
	if err != nil {
		return acp.UnstableForkSessionResponse{}, err
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

	client, err := a.sharedRuntime(ctx)
	if err != nil {
		return acp.UnstableForkSessionResponse{}, err
	}

	parentSnapshot := parent.snapshot()
	config := codex.MCPServerThreadConfig(mcpServers, meta.MCPToolApprovalMode)

	thread, err := client.ForkThread(ctx, codex.ThreadForkRequest{
		ThreadID:      parentSnapshot.codexThreadID,
		Cwd:           params.Cwd,
		Config:        cloneAnyMap(config),
		Environment:   cloneStringMap(meta.Env),
		ExtraPathDirs: cloneStrings(meta.ExtraPathDirs),
	})
	if err != nil {
		return acp.UnstableForkSessionResponse{}, codexThreadACPError(err, parentSnapshot.accountMeta)
	}

	if thread.ID == "" {
		cleanupCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), closeTimeout)
		defer cancel()

		return acp.UnstableForkSessionResponse{}, errors.Join(
			errors.New("codex fork acknowledgement omitted its child thread identity"),
			a.retireRuntimeGeneration(cleanupCtx, client),
		)
	}

	childThreadID := thread.ID
	childOwned := true

	var childSession *session

	defer func() {
		if childOwned {
			returnErr = errors.Join(returnErr, a.cleanupUnregisteredFork(ctx, client, childThreadID, childSession))
		}
	}()

	thread, err = client.ResumeThread(ctx, codex.ThreadResumeRequest{
		ThreadID:      childThreadID,
		Cwd:           params.Cwd,
		Config:        cloneAnyMap(config),
		Environment:   cloneStringMap(meta.Env),
		ExtraPathDirs: cloneStrings(meta.ExtraPathDirs),
	})
	if err != nil {
		return acp.UnstableForkSessionResponse{}, codexThreadACPError(err, parentSnapshot.accountMeta)
	}

	if thread.ID != childThreadID {
		cleanupErr := a.cleanupUnregisteredFork(ctx, client, childThreadID, nil)
		childOwned = false
		cleanupCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), closeTimeout)
		fenceErr := a.retireRuntimeGeneration(cleanupCtx, client)

		cancel()

		return acp.UnstableForkSessionResponse{}, errors.Join(
			errors.New("codex resumed a different child thread after fork"), cleanupErr, fenceErr,
		)
	}

	session := newSession(a, id, params.Cwd, params.AdditionalDirectories, thread, client, meta, mcpServers)

	childSession = session
	if err := session.armLifecycleEstablishment(establishmentFromContext(ctx)); err != nil {
		return acp.UnstableForkSessionResponse{}, err
	}

	session.fingerprint = codexSessionStartFingerprint(codexSessionStart{
		Cwd:                   params.Cwd,
		AdditionalDirectories: params.AdditionalDirectories,
		McpServers:            stableMCPServersFromUnstable(params.McpServers),
		Meta:                  meta,
		ForkParentID:          parentSnapshot.codexThreadID,
	})
	session.setAccount(clientAccountMeta(ctx, client))

	if err := a.runtimeReadyCanary(ctx, client, session); err != nil {
		return acp.UnstableForkSessionResponse{}, err
	}

	if err := a.storeStartedSession(session); err != nil {
		return acp.UnstableForkSessionResponse{}, err
	}

	childOwned = false

	models := modelList(ctx, client)
	snapshot := session.snapshot()

	return acp.UnstableForkSessionResponse{
		SessionId:     id,
		Meta:          sessionResponseMeta(snapshot),
		ConfigOptions: sessionUnstableConfigOptions(session, models),
	}, nil
}

func (a *Agent) cleanupUnregisteredFork(
	parent context.Context,
	client codex.Client,
	threadID string,
	session *session,
) error {
	cleanupCtx, cancel := context.WithTimeout(context.WithoutCancel(parent), closeTimeout)
	defer cancel()

	var cleanupErr error
	if session != nil {
		cleanupErr = session.beginLifecycleClose(cleanupCtx)
		cleanupErr = errors.Join(cleanupErr, session.stopNativeEventsContext(cleanupCtx))
	}

	deleteErr := client.DeleteThread(cleanupCtx, codex.ThreadDeleteRequest{ThreadID: threadID})
	if errors.Is(deleteErr, codex.ErrThreadNotFound) {
		deleteErr = nil
	}

	cleanupErr = errors.Join(cleanupErr, deleteErr)

	if deleteErr != nil {
		cleanupErr = errors.Join(cleanupErr, a.retireRuntimeGeneration(cleanupCtx, client))
	}

	return cleanupErr
}

// SetSessionConfigOption accepts only the value-id variant, and the two ways to
// miss it fault different request members. Every advertised config option is a
// select, so a boolean payload picked the wrong `type` discriminator — `value`
// would name a member that request never carried. A request with neither
// variant omitted `value`, which is the member the union decodes on.
func (a *Agent) SetSessionConfigOption(ctx context.Context, params acp.SetSessionConfigOptionRequest) (acp.SetSessionConfigOptionResponse, error) {
	if err := rejectLifecycleKey(setSessionConfigOptionMeta(params)); err != nil {
		return acp.SetSessionConfigOptionResponse{}, err
	}

	if params.Boolean != nil {
		return acp.SetSessionConfigOptionResponse{}, unsupportedField(jsonFieldType)
	}

	if params.ValueId == nil {
		return acp.SetSessionConfigOptionResponse{}, unsupportedField(jsonFieldValue)
	}

	return a.setSessionConfigValue(ctx, params.ValueId)
}

// setSessionConfigOptionMeta reads the `_meta` of whichever variant the request
// carries. The family literal is refused on the request whatever its shape, so a
// variant this adapter goes on to reject for its own reasons still reports the
// reserved key first.
func setSessionConfigOptionMeta(params acp.SetSessionConfigOptionRequest) map[string]any {
	switch {
	case params.Boolean != nil:
		return params.Boolean.Meta
	case params.ValueId != nil:
		return params.ValueId.Meta
	default:
		return nil
	}
}

// SetSessionMode exists only because github.com/coder/acp-go-sdk's generated
// Agent interface still requires it. The local ACP dispatcher intentionally
// does not route session/set_mode; use session/set_config_option with configId
// "mode".
func (a *Agent) SetSessionMode(_ context.Context, params acp.SetSessionModeRequest) (acp.SetSessionModeResponse, error) {
	if err := rejectLifecycleKey(params.Meta); err != nil {
		return acp.SetSessionModeResponse{}, err
	}

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

func codexThreadACPError(err error, account map[string]any) error {
	if err == nil {
		return nil
	}

	if isCodexAuthError(err) {
		return codexAuthRequiredError(err, account)
	}

	if errors.Is(err, codex.ErrThreadNotFound) {
		return newUnknownSession()
	}

	return err
}

func newUnknownSession() *acp.RequestError {
	return acp.NewInvalidParams(map[string]any{jsonFieldError: "unknown session", jsonFieldField: jsonFieldSessionID})
}
