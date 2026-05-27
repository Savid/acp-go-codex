package codexacp

import (
	"context"
	"errors"
	"sync"
	"time"

	"github.com/coder/acp-go-sdk"
	"github.com/savid/acp-go-codex/internal/codex"
)

// Session is an opaque handle for one Codex app-server process owned by an ACP
// session. Callers receive Session values from Agent methods; constructing one
// directly is unsupported.
type Session struct {
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
	mode                  acp.SessionModeId
	reasoningEffort       string
	serviceTier           string
	personality           string
	rawMessages           rawMessageConfig
	outputSchema          any
	accountMeta           map[string]any

	client    codex.Client
	mcpBridge *mcpSessionBridge

	turn           chan struct{}
	mu             sync.Mutex
	cancel         context.CancelFunc
	turnDone       <-chan struct{}
	turnID         string
	turnCancelled  bool
	interactions   map[string]*sessionInteraction
	mirrorMu       sync.Mutex
	mirroredRows   int
	emittedRawRows int
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
	outputSchema          any
	accountMeta           map[string]any
}

func newSession(agent *Agent, id acp.SessionId, cwd string, additionalDirectories []string, thread codex.Thread, client codex.Client, meta sessionMeta) *Session {
	title := thread.Title
	if title == "" {
		title = "Codex session"
	}
	updatedAt := thread.UpdatedAt
	if updatedAt == "" {
		updatedAt = time.Now().UTC().Format(time.RFC3339)
	}

	return &Session{
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
		reasoningEffort:       meta.ReasoningEffort,
		serviceTier:           meta.ServiceTier,
		personality:           meta.Personality,
		rawMessages:           meta.RawMessages,
		outputSchema:          meta.OutputSchema,
		client:                client,
	}
}

func (s *Session) acquireTurn(ctx context.Context) (func(), error) {
	turn := s.turnQueue()
	select {
	case turn <- struct{}{}:
		return func() { <-turn }, nil
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

func (s *Session) turnQueue() chan struct{} {
	s.mu.Lock()
	defer s.mu.Unlock()

	if s.turn == nil {
		s.turn = make(chan struct{}, 1)
	}

	return s.turn
}

func (s *Session) beginTurn(ctx context.Context) context.Context {
	s.mu.Lock()
	defer s.mu.Unlock()

	turnCtx, cancel := context.WithCancel(ctx)
	s.cancel = cancel
	s.turnDone = turnCtx.Done()
	s.turnCancelled = false

	return turnCtx
}

func (s *Session) finishTurn() {
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

func (s *Session) setTurnID(turnID string) {
	if turnID == "" {
		return
	}

	s.mu.Lock()
	s.turnID = turnID
	s.mu.Unlock()
}

func (s *Session) activeTurnID() string {
	s.mu.Lock()
	defer s.mu.Unlock()

	return s.turnID
}

func (s *Session) currentModel() string {
	s.mu.Lock()
	defer s.mu.Unlock()

	return s.model
}

func (s *Session) snapshot() sessionSnapshot {
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
		outputSchema:          cloneAny(s.outputSchema),
		accountMeta:           cloneAnyMap(s.accountMeta),
	}
}

func (s *Session) accountMetaSnapshot() map[string]any {
	s.mu.Lock()
	defer s.mu.Unlock()

	return cloneAnyMap(s.accountMeta)
}

func (s *Session) closeState() (codex.Client, string, *mcpSessionBridge, string) {
	s.mu.Lock()
	defer s.mu.Unlock()

	return s.client, s.codexThreadID, s.mcpBridge, s.materializedPath
}

func (s *Session) cancelTurn() {
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

func (s *Session) wasTurnCancelled() bool {
	s.mu.Lock()
	defer s.mu.Unlock()

	return s.turnCancelled
}

func (s *Session) setAccount(meta map[string]any) {
	if len(meta) == 0 {
		return
	}
	s.mu.Lock()
	s.accountMeta = cloneAnyMap(meta)
	s.mu.Unlock()
}

func (s *Session) beginInteraction(parent context.Context, key string) (context.Context, func()) {
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

func (s *Session) detachInteractionsLocked() []context.CancelFunc {
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

func (s *Session) Close(ctx context.Context) error {
	s.cancelTurn()
	client, codexThreadID, mcpBridge, materializedPath := s.closeState()
	var err error
	if client != nil && codexThreadID != "" {
		_ = client.UnsubscribeThread(ctx, codexThreadID)
	}
	if client != nil {
		err = client.Close(ctx)
	}
	if mcpBridge != nil {
		err = errors.Join(err, mcpBridge.Close())
	}
	if materializedPath != "" {
		err = errors.Join(err, removeMaterializedRollout(materializedPath))
	}

	return err
}

func (s *Session) info() acp.SessionInfo {
	snapshot := s.snapshot()
	title := snapshot.title
	updatedAt := snapshot.updatedAt

	return acp.SessionInfo{
		SessionId:             snapshot.id,
		Cwd:                   snapshot.cwd,
		AdditionalDirectories: snapshot.additionalDirectories,
		Title:                 &title,
		UpdatedAt:             &updatedAt,
		Meta:                  sessionResponseMeta(snapshot),
	}
}
