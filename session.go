package codexacp

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/coder/acp-go-sdk"
	"github.com/savid/acp-go-codex/internal/codex"
)

// session is one Codex app-server process owned by an ACP session.
type session struct {
	agent                 *Agent
	id                    acp.SessionId
	cwd                   string
	additionalDirectories []string
	codexThreadID         string
	rolloutPath           string
	materializedPath      string
	materializedRelease   func()
	title                 string
	updatedAt             string
	model                 string
	modelProvider         string
	fingerprint           string
	mode                  acp.SessionModeId
	reasoningEffort       string
	serviceTier           string
	personality           string
	env                   map[string]string
	extraPathDirs         []string
	approvalPolicy        any
	sandboxPolicy         any
	rawMessages           rawMessageConfig
	outputSchema          any
	accountMeta           map[string]any

	mcpServers      []acp.McpServer
	mcpApprovalMode string

	client codex.Client

	// sessionOps serializes the session-scoped native operations — close,
	// delete, and the runtime rebinds that stand behind them — against the
	// ordinary requests that read the session while they run.
	sessionOps       sync.RWMutex
	turn             chan struct{}
	mu               sync.Mutex
	closing          bool
	cancel           context.CancelFunc
	turnDone         <-chan struct{}
	turnID           string
	turnNonce        string
	turnCancelled    bool
	clientDead       bool
	rawEmitFailures  int64
	interactions     map[string]*sessionInteraction
	mirrorMu         sync.Mutex
	mirroredRows     int
	imageStoreMu     sync.Mutex
	rawEventMu       sync.Mutex
	rawEventSequence int64
	completionRows   int
	visibleRows      int
	rolloutIdentity  nativeTurnIdentity
	permissionTools  permissionToolRegistry
	// incarnation is the lifecycle stream the live prompt speaks for. A session
	// holds at most one, because this configuration opens one incarnation per
	// prompt and fences it when that prompt ends.
	incarnation *promptIncarnation
	// settleGate is held for one prompt's whole settlement order, from the turn
	// it acquired through the durable commit, the terminal lifecycle event, and
	// the v1 result. Close and delete take it, so neither returns while a prompt
	// can still write.
	settleGate sync.Mutex
	// unsyncedEntries is the exact durable prefix a failed commit did not place.
	// It is retained rather than dropped, and the next prompt blocks loudly on
	// it until the store holds it.
	unsyncedEntries []SessionStoreEntry
	unsyncedRow     int
	// persistenceFenced stops every later commit. Delete sets it before it
	// tombstones, so no settlement writer can recreate the row it removed.
	persistenceFenced bool
	// rolloutLiveFenced records that this incarnation could not establish where
	// the shared rollout file stood when it opened, so the live tail publishes
	// nothing rather than replaying a previous incarnation's rows.
	rolloutLiveFenced bool
}

type sessionInteraction struct {
	cancel context.CancelFunc
}

type sessionSnapshot struct {
	id                    acp.SessionId
	cwd                   string
	additionalDirectories []string
	codexThreadID         string
	title                 string
	updatedAt             string
	model                 string
	modelProvider         string
	mode                  acp.SessionModeId
	reasoningEffort       string
	serviceTier           string
	personality           string
	env                   map[string]string
	extraPathDirs         []string
	approvalPolicy        any
	sandboxPolicy         any
	outputSchema          any
	accountMeta           map[string]any
}

func newSession(agent *Agent, id acp.SessionId, cwd string, additionalDirectories []string, thread codex.Thread, client codex.Client, meta sessionMeta, mcpServers []acp.McpServer) *session {
	title := thread.Title
	if title == "" {
		title = "Codex session"
	}

	updatedAt := thread.UpdatedAt
	if updatedAt == "" {
		updatedAt = time.Now().UTC().Format(time.RFC3339)
	}

	return &session{
		agent:                 agent,
		id:                    id,
		cwd:                   cwd,
		additionalDirectories: append([]string(nil), additionalDirectories...),
		codexThreadID:         thread.ID,
		rolloutPath:           thread.Path,
		title:                 title,
		updatedAt:             updatedAt,
		model:                 firstNonEmpty(thread.Model, meta.Model),
		modelProvider:         thread.Provider,
		mode:                  modeDefault,
		reasoningEffort:       firstNonEmpty(meta.ReasoningEffort, thread.ReasoningEffort),
		serviceTier:           meta.ServiceTier,
		personality:           meta.Personality,
		env:                   cloneStringMap(meta.Env),
		extraPathDirs:         cloneStrings(meta.ExtraPathDirs),
		approvalPolicy:        cloneAny(meta.ApprovalPolicy),
		sandboxPolicy:         cloneAny(meta.SandboxPolicy),
		rawMessages:           meta.RawMessages,
		outputSchema:          meta.OutputSchema,
		mcpServers:            append([]acp.McpServer(nil), mcpServers...),
		mcpApprovalMode:       meta.MCPToolApprovalMode,
		client:                client,
	}
}

func (s *session) markClientDead() {
	s.mu.Lock()
	client := s.client
	s.clientDead = true
	s.mu.Unlock()

	s.agent.markRuntimeDead(client)
}

// ensureLiveClient relaunches the Codex app-server and reattaches to the thread
// when the previous connection died mid-turn. The session is never removed on a
// transport/process failure, so a follow-up prompt re-drives the turn.
func (s *session) ensureLiveClient(ctx context.Context) error {
	s.mu.Lock()
	if !s.clientDead {
		s.mu.Unlock()

		return nil
	}

	s.mu.Unlock()

	client, err := s.agent.sharedRuntime(ctx)
	if err != nil {
		return err
	}

	thread, err := s.agent.resumeRuntimeSession(ctx, client, s)
	if err != nil {
		return err
	}

	// Publish the rebound client only if this is still the current runtime
	// generation and the logical session was not closed while native resume was
	// in flight. Agent.mu before session.mu preserves the lifecycle lock order.
	s.agent.mu.Lock()
	current := s.agent.sessions[s.id]
	runtimeCurrent := s.agent.runtimeClient == client && !s.agent.runtimeDead && !s.agent.closed

	s.mu.Lock()

	closing := s.closing
	if current == s && runtimeCurrent && !closing {
		s.client = client

		s.clientDead = false

		if thread.Path != "" {
			s.rolloutPath = thread.Path
		}
	}
	s.mu.Unlock()
	s.agent.mu.Unlock()

	if current != s || closing {
		return newUnknownSession()
	}

	if !runtimeCurrent {
		return fmt.Errorf("%w: Codex runtime generation changed during session recovery", codex.ErrConnectionClosed)
	}

	return nil
}

func (s *session) setClientDead(dead bool) {
	s.mu.Lock()
	s.clientDead = dead
	s.mu.Unlock()
}

func (s *session) threadConfig() map[string]any {
	s.mu.Lock()
	servers := cloneMCPServers(s.mcpServers)
	approvalMode := s.mcpApprovalMode
	s.mu.Unlock()

	return codex.MCPServerThreadConfig(servers, approvalMode)
}

func (s *session) resumeRequest() codex.ThreadResumeRequest {
	s.mu.Lock()
	threadID := s.codexThreadID
	path := s.materializedPath
	cwd := s.cwd
	servers := cloneMCPServers(s.mcpServers)
	approvalMode := s.mcpApprovalMode
	env := cloneStringMap(s.env)
	extraPathDirs := cloneStrings(s.extraPathDirs)
	s.mu.Unlock()

	return codex.ThreadResumeRequest{
		ThreadID:      threadID,
		Path:          path,
		Cwd:           cwd,
		Config:        codex.MCPServerThreadConfig(servers, approvalMode),
		Environment:   env,
		ExtraPathDirs: extraPathDirs,
	}
}

// activeThreadOwnership returns the native identity and rollout path owned by
// this live session. A store-backed lifecycle request must use this identity
// while the thread is still attached to the same app-server; materializing the
// mirrored rows at a different path would ask Codex to attach an already-live
// thread to a second rollout.
func (s *session) activeThreadOwnership() (codex.Client, string, string, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()

	return s.client, s.codexThreadID, s.rolloutPath, !s.clientDead
}

func (s *session) applyActiveRebind(
	thread codex.Thread,
	cwd string,
	additionalDirectories []string,
	meta sessionMeta,
	mcpServers []acp.McpServer,
	fingerprint string,
	accountMeta map[string]any,
) {
	s.mu.Lock()
	defer s.mu.Unlock()

	s.cwd = cwd

	s.additionalDirectories = append([]string(nil), additionalDirectories...)
	s.rolloutPath = thread.Path

	s.title = thread.Title
	if s.title == "" {
		s.title = "Codex session"
	}

	s.updatedAt = thread.UpdatedAt
	if s.updatedAt == "" {
		s.updatedAt = time.Now().UTC().Format(time.RFC3339)
	}

	s.model = firstNonEmpty(thread.Model, meta.Model)
	s.modelProvider = thread.Provider
	s.fingerprint = fingerprint
	s.mode = modeDefault
	s.reasoningEffort = firstNonEmpty(meta.ReasoningEffort, thread.ReasoningEffort)
	s.serviceTier = meta.ServiceTier
	s.personality = meta.Personality
	s.env = cloneStringMap(meta.Env)
	s.extraPathDirs = cloneStrings(meta.ExtraPathDirs)
	s.approvalPolicy = cloneAny(meta.ApprovalPolicy)
	s.sandboxPolicy = cloneAny(meta.SandboxPolicy)
	s.rawMessages = meta.RawMessages
	s.outputSchema = cloneAny(meta.OutputSchema)

	s.mcpServers = append([]acp.McpServer(nil), mcpServers...)

	s.mcpApprovalMode = meta.MCPToolApprovalMode

	if len(accountMeta) > 0 {
		s.accountMeta = cloneAnyMap(accountMeta)
	}
}

func (s *session) acquireTurn(ctx context.Context) (func(), error) {
	turn := s.turnQueue()
	select {
	case turn <- struct{}{}:
		return func() { <-turn }, nil
	case <-ctx.Done():
		return nil, ctx.Err()
	default:
		return nil, acp.NewInvalidRequest(map[string]any{jsonFieldError: valueBackpressure, jsonFieldLimit: "session_prompt"})
	}
}

func (s *session) turnQueue() chan struct{} {
	s.mu.Lock()
	defer s.mu.Unlock()

	if s.turn == nil {
		s.turn = make(chan struct{}, sessionTurnCapacity)
	}

	return s.turn
}

func (s *session) beginTurn(ctx context.Context, turnNonce string) context.Context {
	s.permissionTools.reset()

	s.mu.Lock()
	defer s.mu.Unlock()

	turnCtx, cancel := context.WithCancel(ctx)
	turnCtx = withTurnRoute(turnCtx, turnNonce)
	s.cancel = cancel
	s.turnDone = turnCtx.Done()
	s.turnCancelled = false
	s.turnNonce = turnNonce

	return turnCtx
}

func (s *session) finishTurn() {
	s.mu.Lock()
	cancel := s.cancel
	interactions := s.detachInteractionsLocked()
	s.cancel = nil
	s.turnDone = nil
	s.turnID = ""
	s.turnNonce = ""
	s.turnCancelled = false
	s.updatedAt = time.Now().UTC().Format(time.RFC3339)
	s.mu.Unlock()

	if cancel != nil {
		cancel()
	}

	for _, cancel := range interactions {
		cancel()
	}
}

func (s *session) activeTurnNonce() string {
	s.mu.Lock()
	defer s.mu.Unlock()

	return s.turnNonce
}

func (s *session) activeTurnNonceForNativeTurn(nativeTurnID string) (string, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()

	active := s.turnDone != nil && s.turnNonce != "" && s.turnID != "" && s.turnID == nativeTurnID

	return s.turnNonce, active
}

func (s *session) setTurnID(turnID string) {
	if turnID == "" {
		return
	}

	s.mu.Lock()
	s.turnID = turnID
	s.mu.Unlock()
}

func (s *session) activeTurnID() string {
	s.mu.Lock()
	defer s.mu.Unlock()

	return s.turnID
}

func (s *session) activeTurnTarget() (codex.Client, string, string) {
	s.mu.Lock()
	defer s.mu.Unlock()

	return s.client, s.codexThreadID, s.turnID
}

func (s *session) currentModel() string {
	s.mu.Lock()
	defer s.mu.Unlock()

	return s.model
}

func (s *session) snapshot() sessionSnapshot {
	s.mu.Lock()
	defer s.mu.Unlock()

	return sessionSnapshot{
		id:                    s.id,
		cwd:                   s.cwd,
		additionalDirectories: append([]string(nil), s.additionalDirectories...),
		codexThreadID:         s.codexThreadID,
		title:                 s.title,
		updatedAt:             s.updatedAt,
		model:                 s.model,
		modelProvider:         s.modelProvider,
		mode:                  s.mode,
		reasoningEffort:       s.reasoningEffort,
		serviceTier:           s.serviceTier,
		personality:           s.personality,
		env:                   cloneStringMap(s.env),
		extraPathDirs:         cloneStrings(s.extraPathDirs),
		approvalPolicy:        cloneAny(s.approvalPolicy),
		sandboxPolicy:         cloneAny(s.sandboxPolicy),
		outputSchema:          cloneAny(s.outputSchema),
		accountMeta:           cloneAnyMap(s.accountMeta),
	}
}

func (s *session) accountMetaSnapshot() map[string]any {
	s.mu.Lock()
	defer s.mu.Unlock()

	return cloneAnyMap(s.accountMeta)
}

func (s *session) closeState() (codex.Client, string, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()

	return s.client, s.codexThreadID, s.clientDead
}

func (s *session) cancelTurn() {
	s.mu.Lock()

	cancel := s.cancel
	if cancel != nil {
		s.turnCancelled = true
	}

	interactions := s.detachInteractionsLocked()
	s.mu.Unlock()

	if cancel != nil {
		cancel()
	}

	for _, cancel := range interactions {
		cancel()
	}
}

func (s *session) wasTurnCancelled() bool {
	s.mu.Lock()
	defer s.mu.Unlock()

	return s.turnCancelled
}

func (s *session) setAccount(meta map[string]any) {
	if len(meta) == 0 {
		return
	}

	s.mu.Lock()
	s.accountMeta = cloneAnyMap(meta)
	s.mu.Unlock()
}

func (s *session) beginInteraction(parent context.Context, key string) (context.Context, func()) {
	if key == "" {
		key = "codex-server-request"
	}

	s.mu.Lock()
	base := parent
	turnDone := s.turnDone
	ctx, cancel := context.WithCancel(base)

	if s.interactions == nil {
		s.interactions = make(map[string]*sessionInteraction)
	}

	if previous := s.interactions[key]; previous != nil {
		previous.cancel()
	}

	interaction := &sessionInteraction{cancel: cancel}
	s.interactions[key] = interaction
	s.mu.Unlock()

	if turnDone != nil {
		go func() {
			defer recoverAgentGoroutine(parent, agentLogger(s.agent), "Codex interaction watcher")

			select {
			case <-turnDone:
				cancel()
			case <-ctx.Done():
			}
		}()
	}

	finish := func() {
		s.mu.Lock()
		if s.interactions[key] == interaction {
			delete(s.interactions, key)
		}
		s.mu.Unlock()
		cancel()
	}

	return ctx, finish
}

func (s *session) detachInteractionsLocked() []context.CancelFunc {
	if len(s.interactions) == 0 {
		return nil
	}

	cancels := make([]context.CancelFunc, 0, len(s.interactions))
	for key, interaction := range s.interactions {
		cancels = append(cancels, interaction.cancel)

		delete(s.interactions, key)
	}

	return cancels
}

func (s *session) releaseMaterialized() error {
	s.mu.Lock()
	materializedPath := s.materializedPath
	materializedRelease := s.materializedRelease
	s.mu.Unlock()

	if materializedPath != "" {
		if err := removeMaterializedRollout(materializedPath); err != nil {
			return err
		}
	}

	s.mu.Lock()
	if s.materializedPath == materializedPath {
		s.materializedPath = ""
		s.materializedRelease = nil
	}
	s.mu.Unlock()

	if materializedRelease != nil {
		materializedRelease()
	}

	return nil
}

// Close ends one logical session. The Agent owns the shared app-server and
// closes it only when the service itself closes, so this is the session's own
// containment boundary rather than the runtime's.
//
// The live prompt is cancelled and its whole settlement order awaited first: a
// close that returned while a prompt could still commit or still emit would
// fence a stream the host has not finished reducing.
func (s *session) Close(ctx context.Context) error {
	s.awaitPromptSettlement()

	s.sessionOps.Lock()
	defer s.sessionOps.Unlock()

	return errors.Join(s.containSession(ctx), s.releaseMaterialized())
}

// awaitPromptSettlement stops the live prompt and waits for the settlement it
// owes: the durable commit, the terminal lifecycle event, and the v1 result.
func (s *session) awaitPromptSettlement() {
	s.cancelTurn()
	s.waitForSettleGate()
}

func (s *session) waitForSettleGate() {
	s.settleGate.Lock()
	defer s.settleGate.Unlock() //nolint:gocritic // Deliberate synchronization barrier.
}

// containSession finishes this session's containment boundary. The thread-scoped
// background-terminal sweep is the only targeted containment the app-server
// offers; when it cannot be proved — because the app-server does not carry those
// methods, or because a terminal outlived the sweep — the shared generation is
// fenced instead, which is the contract's answer for a multiplexed sibling that
// cannot contain one target alone.
func (s *session) containSession(ctx context.Context) error {
	client, codexThreadID, clientDead := s.closeState()

	closeCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), closeTimeout)
	defer cancel()

	if client == nil || codexThreadID == "" {
		return nil
	}

	if clientDead {
		return s.agent.quiesceRuntimeAfterCancel(closeCtx, client)
	}

	if err := s.containThreadOrFenceGeneration(closeCtx, client, codexThreadID); err != nil {
		return err
	}

	return s.unsubscribeContainedThread(closeCtx, client, codexThreadID)
}

// containThreadOrFenceGeneration proves this thread's background terminals gone,
// or fences the generation they live in when it cannot.
func (s *session) containThreadOrFenceGeneration(
	ctx context.Context,
	client codex.Client,
	codexThreadID string,
) error {
	containErr := terminateThreadBackgroundTerminals(ctx, client, codexThreadID)
	if containErr == nil {
		return nil
	}

	fenceErr := s.agent.quiesceRuntimeAfterCancel(ctx, client)

	// Where the app-server offers no thread-scoped containment at all, the
	// generation fence is the boundary rather than a fallback after a failure,
	// so its result is the boundary's result. A sweep that was offered and did
	// not prove the thread contained is a failure in its own right, and both are
	// reported.
	if errors.Is(containErr, codex.ErrBackgroundTerminalsUnsupported) {
		return fenceErr
	}

	return errors.Join(containErr, fenceErr)
}

func (s *session) unsubscribeContainedThread(
	ctx context.Context,
	client codex.Client,
	codexThreadID string,
) error {
	unsubscribeErr := client.UnsubscribeThread(ctx, codexThreadID)
	if unsubscribeErr == nil {
		return nil
	}

	// Cancellation may retire the generation concurrently with unsubscribe.
	// Once that transition owns the client, its containment proof supersedes a
	// connection-closed unsubscribe result.
	current, _, nowDead := s.closeState()
	if current == client && nowDead {
		return s.agent.quiesceRuntimeAfterCancel(ctx, client)
	}

	return unsubscribeErr
}

func (s *session) info() acp.SessionInfo {
	snapshot := s.snapshot()
	title := snapshot.title
	updatedAt := snapshot.updatedAt

	return acp.SessionInfo{
		SessionId:             snapshot.id,
		Cwd:                   snapshot.cwd,
		AdditionalDirectories: snapshot.additionalDirectories,
		Title:                 &title,
		UpdatedAt:             &updatedAt,
		Meta:                  sessionInfoMeta(snapshot),
	}
}
