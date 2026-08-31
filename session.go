package codexacp

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/coder/acp-go-sdk"
	"github.com/savid/acp-go-codex/internal/codex"
	"github.com/savid/acp-go-codex/internal/lifecycle"
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
	materializedBytes     int64
	materializedEpoch     uint64
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
	sessionOps          sync.RWMutex
	nativeControlMu     sync.Mutex
	turn                chan struct{}
	mu                  sync.Mutex
	closing             bool
	closeContained      bool
	closeCommitPending  bool
	closeCommitDone     bool
	closeRemovalPending bool
	closeOperation      *sessionCloseOperation
	cancel              context.CancelFunc
	turnDone            <-chan struct{}
	turnID              string
	turnNonce           string
	turnReady           chan struct{}
	turnAccepted        bool
	turnDispatched      bool
	turnCancelled       bool
	turnContainment     *turnContainment
	clientDead          bool
	rawEmitFailures     int64
	interactions        map[string]*sessionInteraction
	mirrorMu            sync.Mutex
	mirroredRows        int
	imageStoreMu        sync.Mutex
	rawEventMu          sync.Mutex
	rawEventSequence    int64
	permissionTools     permissionToolRegistry
	// lifecycleMu owns the one stream and native event feed for this exact
	// app-server/thread incarnation. The stream survives prompt completion and
	// is fenced only when the native binding ends.
	lifecycleMu             sync.Mutex
	lifecycleRouteMu        lifecycleRouteGate
	lifecycleStream         *lifecycle.Stream
	incarnation             *promptIncarnation
	agentIncarnation        *promptIncarnation
	lifecycleFailure        error
	lifecycleChanged        chan struct{}
	lifecycleClosing        bool
	lifecycleDeliveries     []lifecycleDelivery
	lifecycleDeliveryRun    bool
	lifecycleDeliveryActive bool
	lifecycleDeliveryStop   bool
	lifecycleDeliveryCancel context.CancelFunc
	lifecycleDeliveryDone   chan struct{}
	nativeRouteCancel       context.CancelFunc
	nativeEventCancel       context.CancelFunc
	nativeEventRelease      func()
	nativeEventDone         chan struct{}
	nativeEventBarrier      chan chan error
	nativeEventSource       bool
	nativeEventOpened       bool
	nativeEventStopping     bool
	nativeEventPumping      bool
	nativeEventAttaching    bool
	nativeEventRebinding    bool
	nativeEventReplaying    bool
	nativeRebindEvents      []codex.Event
	nativeCanary            *nativeCanary
	preOpenEvents           []codex.Event
	terminalNativeTurns     map[string]struct{}
	terminalNativeTurnOrder []string
	terminalNativeTurnNext  int
	establishment           *establishmentObligation
	establishmentRebind     bool
	establishmentErr        error
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
	// captureFailed records that a mirror pass failed before it could capture the
	// durable prefix it was reading. Such a pass retains nothing, so this is the
	// only thing left saying a prefix is still owed, and the close boundary reads
	// the rollout file again exactly when it is set.
	captureFailed bool
	// persistenceFenced stops every later commit. Delete sets it before it
	// tombstones, so no settlement writer can recreate the row it removed.
	persistenceFenced bool
}

type sessionInteraction struct {
	cancel context.CancelFunc
}

const sessionInteractionLimit = 1024

type turnContainment struct {
	done    chan struct{}
	err     error
	started bool
}

type sessionCloseOperation struct {
	done chan struct{}
	err  error
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

	if err := s.rebindNativeEvents(client); err != nil {
		return err
	}

	if err := s.agent.runtimeReadyCanary(ctx, client, s); err != nil {
		if rebindErr := s.prepareNativeEventRebind(); rebindErr != nil {
			return errors.Join(err, rebindErr)
		}

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
		unknown := newUnknownSession()
		if rebindErr := s.prepareNativeEventRebind(); rebindErr != nil {
			return errors.Join(unknown, rebindErr)
		}

		return unknown
	}

	if !runtimeCurrent {
		runtimeErr := fmt.Errorf("%w: Codex runtime generation changed during session recovery", codex.ErrConnectionClosed)
		if rebindErr := s.prepareNativeEventRebind(); rebindErr != nil {
			return errors.Join(runtimeErr, rebindErr)
		}

		return runtimeErr
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
		return nil, acp.NewInvalidRequest(map[string]any{jsonFieldError: valueBackpressure, jsonFieldLimit: limitSessionPrompt})
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

	return s.beginTurnLocked(ctx, turnNonce)
}

func (s *session) beginPromptTurn(ctx context.Context, turnNonce string) (context.Context, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	if s.closing {
		return nil, newSessionCloseInProgress()
	}

	return s.beginTurnLocked(ctx, turnNonce), nil
}

func (s *session) beginTurnLocked(ctx context.Context, turnNonce string) context.Context {
	turnCtx, cancel := context.WithCancel(ctx)
	turnCtx = withTurnRoute(turnCtx, turnNonce)
	s.cancel = cancel
	s.turnDone = turnCtx.Done()
	s.turnID = ""
	s.turnReady = make(chan struct{})
	s.turnAccepted = false
	s.turnDispatched = false
	s.turnCancelled = false
	s.turnContainment = nil
	s.turnNonce = turnNonce

	return turnCtx
}

func (s *session) finishTurn() {
	s.mu.Lock()
	cancel := s.cancel
	interactions := s.detachInteractionsLocked()
	s.releaseTurnBindingLocked(false)
	s.cancel = nil
	s.turnDone = nil
	s.turnID = ""
	s.turnNonce = ""
	s.turnReady = nil
	s.turnAccepted = false
	s.turnDispatched = false
	s.turnCancelled = false
	s.turnContainment = nil
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
	turnNonce := s.turnNonce
	turnDone := s.turnDone
	s.mu.Unlock()

	if turnNonce != "" && turnDone != nil {
		select {
		case <-turnDone:
		default:
			return turnNonce
		}
	}

	s.lifecycleMu.Lock()
	defer s.lifecycleMu.Unlock()

	if current := s.agentIncarnation; current != nil && !current.settled {
		return current.turnNonce
	}

	return ""
}

func (s *session) activeTurnNonceForNativeTurn(nativeTurnID string) (string, bool) {
	s.mu.Lock()

	active := s.turnDone != nil && s.turnAccepted && s.turnNonce != "" && s.turnID != "" && s.turnID == nativeTurnID
	if active {
		select {
		case <-s.turnDone:
			active = false
		default:
		}
	}

	turnNonce := s.turnNonce
	s.mu.Unlock()

	if active {
		return turnNonce, true
	}

	s.lifecycleMu.Lock()
	defer s.lifecycleMu.Unlock()

	if current := s.agentIncarnation; current != nil && !current.settled && current.nativeTurnID == nativeTurnID {
		return current.turnNonce, true
	}

	return turnNonce, false
}

func (s *session) setTurnID(turnID string) {
	if turnID == "" {
		return
	}

	s.mu.Lock()
	s.turnID = turnID
	s.releaseTurnBindingLocked(true)
	s.mu.Unlock()
}

func (s *session) stageTurnID(turnID string) {
	if turnID == "" {
		return
	}

	s.mu.Lock()
	s.turnID = turnID
	s.mu.Unlock()
}

func (s *session) markTurnDispatched() {
	s.mu.Lock()
	s.turnDispatched = true
	s.mu.Unlock()
}

func (s *session) acceptTurnBinding() {
	s.mu.Lock()
	s.releaseTurnBindingLocked(true)
	s.mu.Unlock()
}

func (s *session) rejectTurnBinding() {
	s.mu.Lock()
	s.releaseTurnBindingLocked(false)
	s.mu.Unlock()
}

func (s *session) releaseTurnBindingLocked(accepted bool) {
	if s.turnReady == nil {
		return
	}

	s.turnAccepted = accepted
	close(s.turnReady)
	s.turnReady = nil
}

func (s *session) waitForTurnBinding(ctx context.Context) error {
	s.mu.Lock()
	ready := s.turnReady
	s.mu.Unlock()

	if ready == nil {
		return ctx.Err()
	}

	select {
	case <-ready:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
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

func (s *session) ensureTurnContainmentLocked() *turnContainment {
	if s.cancel == nil {
		return nil
	}

	if s.turnContainment == nil {
		s.turnContainment = &turnContainment{done: make(chan struct{})}
	}

	return s.turnContainment
}

func (s *session) shutdownActiveTurn(ctx context.Context, protectPeers bool) error {
	handled, err := s.shutdownPromptTurn(ctx, protectPeers, "", false)
	if handled {
		return err
	}

	_, err = s.shutdownAgentTurn(ctx, protectPeers)

	return err
}

var errTurnRouteMismatch = errors.New("turnNonce does not identify the active turn")

func (s *session) shutdownActiveTurnForNonce(ctx context.Context, protectPeers bool, turnNonce string) error {
	handled, err := s.shutdownPromptTurn(ctx, protectPeers, turnNonce, true)
	if handled {
		return err
	}

	handled, err = s.shutdownAgentTurnForNonce(ctx, protectPeers, turnNonce)
	if !handled {
		return errTurnRouteMismatch
	}

	return err
}

func (s *session) shutdownPromptTurn(
	ctx context.Context,
	protectPeers bool,
	expectedNonce string,
	requireExactNonce bool,
) (bool, error) {
	s.mu.Lock()

	boundary := s.ensureTurnContainmentLocked()
	if boundary == nil {
		s.mu.Unlock()

		return false, nil
	}

	if requireExactNonce && s.turnNonce != expectedNonce {
		s.mu.Unlock()

		return true, errTurnRouteMismatch
	}

	if boundary.started {
		done := boundary.done
		s.mu.Unlock()

		select {
		case <-done:
			return true, boundary.err
		case <-ctx.Done():
			return true, ctx.Err()
		}
	}

	boundary.started = true

	cancelTurn := s.cancel
	if cancelTurn != nil {
		s.turnCancelled = true
	}

	interactions := s.detachInteractionsLocked()
	s.mu.Unlock()

	// Cancellation releases prompt preparation and turn/start, but settlement
	// cannot pass the boundary installed above. If native dispatch won the race,
	// binding supplies the ID that must be interrupted before that boundary is
	// completed.
	if cancelTurn != nil {
		cancelTurn()
	}

	for _, cancel := range interactions {
		cancel()
	}

	bindErr := s.waitForTurnBinding(ctx)
	s.nativeControlMu.Lock()
	defer s.nativeControlMu.Unlock()

	s.mu.Lock()
	client := s.client
	threadID := s.codexThreadID
	turnID := s.turnID
	turnDispatched := s.turnDispatched
	s.mu.Unlock()

	interruptErr := bindErr
	if interruptErr == nil && turnDispatched && turnID == "" {
		interruptErr = errors.New("codex turn dispatch outcome is unknown without a native turn ID")
	}

	if interruptErr == nil && turnID != "" {
		interruptCtx, cancelInterrupt := context.WithTimeout(context.WithoutCancel(ctx), closeTimeout)
		interruptErr = client.CancelTurn(interruptCtx, threadID, turnID)

		cancelInterrupt()
	}

	containmentErr := s.containCancelledTurnWithPolicy(ctx, client, threadID, interruptErr, protectPeers)

	s.mu.Lock()
	boundary.err = containmentErr

	cancelTurn = s.cancel
	if cancelTurn != nil {
		s.turnCancelled = true
	}

	interactions = s.detachInteractionsLocked()

	close(boundary.done)
	s.mu.Unlock()

	if cancelTurn != nil {
		cancelTurn()
	}

	for _, cancel := range interactions {
		cancel()
	}

	return true, containmentErr
}

func (s *session) awaitTurnContainment(ctx context.Context) error {
	s.mu.Lock()
	boundary := s.turnContainment
	s.mu.Unlock()

	if boundary == nil {
		return nil
	}

	select {
	case <-boundary.done:
		return boundary.err
	case <-ctx.Done():
		return ctx.Err()
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
	watchTurn := false

	if turnDone != nil {
		select {
		case <-turnDone:
			cancel()
		default:
			watchTurn = true
		}
	}

	if s.interactions == nil {
		s.interactions = make(map[string]*sessionInteraction)
	}

	if previous := s.interactions[key]; previous != nil {
		previous.cancel()
	} else if len(s.interactions) == sessionInteractionLimit {
		s.mu.Unlock()
		cancel()

		return ctx, func() {}
	}

	interaction := &sessionInteraction{cancel: cancel}
	s.interactions[key] = interaction
	s.mu.Unlock()

	if watchTurn {
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
	materializedBytes := s.materializedBytes
	materializedEpoch := s.materializedEpoch
	s.mu.Unlock()

	if err := s.agent.retireMaterializedRolloutAtEpoch(materializedPath, materializedBytes, materializedRelease, materializedEpoch); err != nil {
		return err
	}

	s.mu.Lock()
	if s.materializedPath == materializedPath {
		s.materializedPath = ""
		s.materializedRelease = nil
		s.materializedBytes = 0
		s.materializedEpoch = 0
	}
	s.mu.Unlock()

	return nil
}

// Close ends one logical session. The Agent owns the shared app-server and
// closes it only when the service itself closes, so this is the session's own
// containment boundary rather than the runtime's.
//
// A live prompt's context is cancelled to stop local consumption, but native
// interruption and containment complete before settlement may pass.
//
// The ladder travels with the boundary, so this path owes every rung a wire
// close owes — including the durable one. An embedded shutdown that dropped the
// prefix a settlement captured and could not place would lose a turn the host
// was already shown, and this session's materialized rollout is the only
// remaining copy of it: the material is released after the commit lands, never
// while the commit is still owed.
func (s *session) Close(ctx context.Context) error {
	for {
		s.mu.Lock()
		s.closing = true

		if operation := s.closeOperation; operation != nil {
			select {
			case <-operation.done:
				complete := s.closeContained && s.closeCommitDone && !s.closeCommitPending && !s.closeRemovalPending
				if complete {
					err := operation.err
					s.mu.Unlock()

					return err
				}

				s.closeOperation = nil
				s.mu.Unlock()

				continue
			default:
				done := operation.done
				s.mu.Unlock()

				select {
				case <-done:
					return operation.err
				case <-ctx.Done():
					return ctx.Err()
				}
			}
		}

		operation := &sessionCloseOperation{done: make(chan struct{})}
		s.closeOperation = operation
		s.mu.Unlock()

		err := s.closeOwned(ctx)

		s.mu.Lock()
		operation.err = err
		close(operation.done)
		s.mu.Unlock()

		return err
	}
}

func (s *session) closeOwned(ctx context.Context) error {
	s.mu.Lock()
	alreadyContained := s.closeContained
	s.mu.Unlock()

	var shutdownErr error

	if !alreadyContained {
		gateErr := s.beginLifecycleClose(ctx)
		shutdownErr = errors.Join(gateErr, s.shutdownActiveTurn(ctx, true))
		s.awaitPromptSettlement()
	}

	s.sessionOps.Lock()
	defer s.sessionOps.Unlock()

	s.mu.Lock()
	alreadyContained = s.closeContained
	commitDone := s.closeCommitDone
	s.mu.Unlock()

	if !alreadyContained {
		if containErr := errors.Join(shutdownErr, s.containSession(ctx)); containErr != nil {
			// An incomplete boundary terminalizes nothing and commits nothing new.
			// The stream ends either way, because the session it belonged to is over
			// whether or not its descendants could be proved gone.
			s.fenceSession()

			return containErr
		}

		s.mu.Lock()
		s.closeContained = true
		s.mu.Unlock()
	}

	if !commitDone {
		if commitErr := s.commitResumableSnapshot(ctx); commitErr != nil {
			s.mu.Lock()
			s.closeCommitPending = true
			s.mu.Unlock()
			s.fenceSession()

			return commitErr
		}

		s.mu.Lock()
		s.closeCommitDone = true
		s.closeCommitPending = false
		s.mu.Unlock()
	}

	s.fenceSession()

	releaseErr := s.releaseMaterialized()
	s.mu.Lock()
	s.closeRemovalPending = releaseErr != nil
	s.mu.Unlock()

	return releaseErr
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

// containSession finishes this session's protocol boundary without taking
// ownership of the shared app-server runtime.
func (s *session) containSession(ctx context.Context) error {
	client, codexThreadID, clientDead := s.closeState()

	closeCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), closeTimeout)
	defer cancel()

	if client == nil || codexThreadID == "" {
		return nil
	}

	if clientDead {
		return s.stopNativeEventsContext(closeCtx)
	}

	if err := terminateThreadBackgroundTerminals(closeCtx, client, codexThreadID); err != nil {
		return err
	}

	return s.unsubscribeContainedThread(closeCtx, client, codexThreadID)
}

func (s *session) unsubscribeContainedThread(
	ctx context.Context,
	client codex.Client,
	codexThreadID string,
) error {
	// Publish the expected source stop and join the pump before native
	// unsubscribe can close the broker channel.
	if err := s.stopNativeEventsContext(ctx); err != nil {
		return err
	}

	unsubscribeErr := client.UnsubscribeThread(ctx, codexThreadID)
	if unsubscribeErr == nil {
		return nil
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
