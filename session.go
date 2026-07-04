package codexacp

import (
	"context"
	"errors"
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
	approvalPolicy        any
	sandboxPolicy         any
	rawMessages           rawMessageConfig
	outputSchema          any
	accountMeta           map[string]any

	client codex.Client

	turn             chan struct{}
	mu               sync.Mutex
	cancel           context.CancelFunc
	turnDone         <-chan struct{}
	turnID           string
	turnCancelled    bool
	interactions     map[string]*sessionInteraction
	mirrorMu         sync.Mutex
	mirroredRows     int
	emittedRawRows   int
	rawEventSequence int64
	completionRows   int
	visibleRows      int
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
	approvalPolicy        any
	sandboxPolicy         any
	outputSchema          any
	accountMeta           map[string]any
}

func newSession(agent *Agent, id acp.SessionId, cwd string, additionalDirectories []string, thread codex.Thread, client codex.Client, meta sessionMeta) *session {
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
		approvalPolicy:        cloneAny(meta.ApprovalPolicy),
		sandboxPolicy:         cloneAny(meta.SandboxPolicy),
		rawMessages:           meta.RawMessages,
		outputSchema:          meta.OutputSchema,
		client:                client,
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
		return nil, acp.NewInvalidRequest(map[string]any{jsonFieldError: "backpressure", "limit": "session_prompt"})
	}
}

func (s *session) turnQueue() chan struct{} {
	s.mu.Lock()
	defer s.mu.Unlock()

	if s.turn == nil {
		limit := defaultMaxConcurrentPrompts
		if s.agent != nil && s.agent.options.ConcurrencyLimits.MaxConcurrentPrompts > 0 {
			limit = s.agent.options.ConcurrencyLimits.MaxConcurrentPrompts
		}
		s.turn = make(chan struct{}, limit)
	}

	return s.turn
}

func (s *session) beginTurn(ctx context.Context) context.Context {
	s.mu.Lock()
	defer s.mu.Unlock()

	turnCtx, cancel := context.WithCancel(ctx)
	s.cancel = cancel
	s.turnDone = turnCtx.Done()
	s.turnCancelled = false

	return turnCtx
}

func (s *session) finishTurn() {
	s.mu.Lock()
	cancel := s.cancel
	interactions := s.detachInteractionsLocked()
	s.cancel = nil
	s.turnDone = nil
	s.turnID = ""
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

func (s *session) closeState() (codex.Client, string, string) {
	s.mu.Lock()
	defer s.mu.Unlock()

	return s.client, s.codexThreadID, s.materializedPath
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

func (s *session) Close(ctx context.Context) error {
	s.cancelTurn()
	client, codexThreadID, materializedPath := s.closeState()
	var err error
	if client != nil && codexThreadID != "" {
		_ = client.UnsubscribeThread(ctx, codexThreadID)
	}
	if client != nil {
		err = client.Close(ctx)
	}
	if materializedPath != "" {
		err = errors.Join(err, removeMaterializedRollout(materializedPath))
	}

	return err
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
