package codexacp

import (
	"context"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/coder/acp-go-sdk"
	acpcore "github.com/savid/acp-go-core"
	"github.com/savid/acp-go-core/wire"
	"github.com/stretchr/testify/require"

	"github.com/savid/acp-go-codex/internal/codex"
	"github.com/savid/acp-go-core/usage/gateway"
)

func queueTestAgent(t *testing.T, extra ...Option) (*Agent, *recorder, *blockedCommitStore, func()) {
	t.Helper()
	store := &blockedCommitStore{SessionStore: acpcore.NewInMemorySessionStore(), entered: make(chan struct{}, 1), release: make(chan struct{})}
	a := NewAgent(testOptions(t, append(extra, WithSessionStore(store))...)...)
	rec := newRecorder()
	a.conn = rec
	request := acp.InitializeRequest{ProtocolVersion: acp.ProtocolVersionNumber}
	withLifecycle()(&request)
	_, err := a.Initialize(t.Context(), request)
	require.NoError(t, err)
	release := sync.OnceFunc(func() { close(store.release) })
	t.Cleanup(func() {
		release()
		require.NoError(t, a.Close())
	})

	return a, rec, store, release
}

func TestProcessExitDoesNotPublishQueuedTerminalTail(t *testing.T) {
	t.Parallel()
	a, rec, store, release := queueTestAgent(t)
	created, err := a.NewSession(t.Context(), wire.NewSessionRequest(t.TempDir()))
	require.NoError(t, err)
	store.blockID.Store(string(created.SessionId))
	a.mu.Lock()
	s := a.sessions[created.SessionId]
	a.mu.Unlock()
	s.mu.Lock()
	rt := s.rt
	s.mu.Unlock()
	request := wire.TextPromptRequest(created.SessionId, "TAIL")
	request.Meta = promptMeta(1)
	type promptResult struct {
		response acp.PromptResponse
		err      error
	}
	done := make(chan promptResult, 1)
	go func() {
		response, promptErr := a.Prompt(t.Context(), request)
		done <- promptResult{response: response, err: promptErr}
	}()
	select {
	case <-store.entered:
	case <-time.After(testTimeout):
		t.Fatal("prompt did not reach its mirror commit")
	}
	require.Eventually(t, func() bool {
		rt.mu.Lock()
		defer rt.mu.Unlock()
		queue := rt.events[s]

		return queue != nil && len(queue.messages) > 0
	}, testTimeout, time.Millisecond)
	require.NoError(t, rt.proc.Kill())
	select {
	case <-rt.done:
	case <-time.After(testTimeout):
		t.Fatal("exited generation did not finish while the commit was held")
	}
	require.Equal(t, "Hello world", agentText(rec.snapshot()))
	release()
	result := <-done
	require.NoError(t, result.err)
	require.Equal(t, acp.StopReasonEndTurn, result.response.StopReason)
}

// gateClient blocks SessionUpdate for one session id until released, so a test
// can stall one session's pump without touching any peer.
type gateClient struct {
	*recorder
	target acp.SessionId
	block  chan struct{}
}

func (g *gateClient) SessionUpdate(ctx context.Context, params acp.SessionNotification) error {
	if params.SessionId == g.target {
		select {
		case <-g.block:
		case <-ctx.Done():
			return ctx.Err()
		}
	}

	return g.recorder.SessionUpdate(ctx, params)
}

// TestSessionOverflowContainsWithoutKillingPeers proves a stalled session whose
// event queue overflows is contained alone: the shared app-server survives and
// a peer session still completes a turn.
func TestSessionOverflowContainsWithoutKillingPeers(t *testing.T) {
	gate := &gateClient{recorder: newRecorder(), block: make(chan struct{})}

	a := NewAgent(testOptions(t)...)
	t.Cleanup(func() { _ = a.Close() })
	a.attach(gate, nil)

	_, err := a.Initialize(t.Context(), acp.InitializeRequest{ProtocolVersion: acp.ProtocolVersionNumber, Meta: map[string]any{wire.LifecycleKey: map[string]any{"version": 1}}})
	require.NoError(t, err)

	stalled, err := a.NewSession(t.Context(), wire.NewSessionRequest(t.TempDir()))
	require.NoError(t, err)
	peer, err := a.NewSession(t.Context(), wire.NewSessionRequest(t.TempDir()))
	require.NoError(t, err)

	gate.target = stalled.SessionId

	sa, err := a.session(t.Context(), stalled.SessionId)
	require.NoError(t, err)
	sa.mu.Lock()
	rt := sa.rt
	sa.mu.Unlock()
	require.NotNil(t, rt)

	// A turn must be in flight for the pump to publish anything.
	done := make(chan error, 1)

	go func() {
		prompt := wire.TextPromptRequest(stalled.SessionId, "SLOW")
		prompt.Meta = promptMeta(1)
		_, promptErr := a.Prompt(t.Context(), prompt)
		done <- promptErr
	}()

	var nativeTurnID string

	require.Eventually(t, func() bool {
		sa.mu.Lock()
		defer sa.mu.Unlock()

		if sa.turn == nil || sa.turn.nativeTurnID == "" {
			return false
		}

		nativeTurnID = sa.turn.nativeTurnID

		return true
	}, 5*time.Second, 5*time.Millisecond)

	// Flood the stalled session past its 256-slot queue. Its pump blocks on the
	// first publish to the gated client, so the rest overflow and contain it.
	for range 400 {
		a.routeMessage(t.Context(), rt, sa, sessionMessage{event: codex.Event{Kind: codex.EventAgentMessageDelta, ThreadID: sa.nativeID, TurnID: nativeTurnID, ItemID: "delta", Text: "x"}})
	}

	require.True(t, rt.alive(), "one session's overflow must not kill the shared app-server")

	// The stopped queue stays registered, so a later frame neither starts a
	// second worker nor reaches the session.
	rt.mu.Lock()
	queue := rt.events[sa]
	rt.mu.Unlock()
	require.NotNil(t, queue)
	require.True(t, queue.stopped)

	a.routeMessage(t.Context(), rt, sa, sessionMessage{event: codex.Event{Kind: codex.EventAgentMessageDelta, ThreadID: sa.nativeID, TurnID: nativeTurnID, ItemID: "delta", Text: "y"}})

	rt.mu.Lock()
	require.Same(t, queue, rt.events[sa], "overflow keeps its stopped queue until containment retires it")
	rt.mu.Unlock()

	// The peer still completes a turn on the same app-server.
	prompt := wire.TextPromptRequest(peer.SessionId, "hi")
	prompt.Meta = promptMeta(2)
	response, err := a.Prompt(t.Context(), prompt)
	require.NoError(t, err)
	require.Equal(t, acp.StopReasonEndTurn, response.StopReason)

	// Releasing the stalled client lets containment finish, fail the prompt,
	// and detach the session.
	close(gate.block)

	select {
	case promptErr := <-done:
		require.Error(t, promptErr, "the contained session's prompt fails")
	case <-time.After(10 * time.Second):
		t.Fatal("the contained session's prompt did not settle")
	}
	require.Eventually(t, func() bool {
		sa.mu.Lock()
		defer sa.mu.Unlock()

		return sa.rt == nil
	}, 5*time.Second, 5*time.Millisecond, "the contained session detaches from the live generation")

	rt.mu.Lock()
	_, registered := rt.events[sa]
	rt.mu.Unlock()
	require.False(t, registered, "containment retires the queue once its worker has exited")
}

// TestCancelOfAnIgnoredInterruptContainsOnlyThatSession proves a native turn
// that ignores its interrupt costs only its own session: the prompt answers
// cancelled, the shared app-server survives, and a peer still completes.
func TestCancelOfAnIgnoredInterruptContainsOnlyThatSession(t *testing.T) {
	a := NewAgent(testOptions(t)...)
	t.Cleanup(func() { _ = a.Close() })
	a.attach(newRecorder(), nil)

	_, err := a.Initialize(t.Context(), acp.InitializeRequest{ProtocolVersion: acp.ProtocolVersionNumber, Meta: map[string]any{wire.LifecycleKey: map[string]any{"version": 1}}})
	require.NoError(t, err)

	stuck, err := a.NewSession(t.Context(), wire.NewSessionRequest(t.TempDir()))
	require.NoError(t, err)
	peer, err := a.NewSession(t.Context(), wire.NewSessionRequest(t.TempDir()))
	require.NoError(t, err)

	ss, err := a.session(t.Context(), stuck.SessionId)
	require.NoError(t, err)
	ss.mu.Lock()
	rt := ss.rt
	ss.mu.Unlock()
	require.NotNil(t, rt)

	type result struct {
		response acp.PromptResponse
		err      error
	}

	done := make(chan result, 1)

	go func() {
		prompt := wire.TextPromptRequest(stuck.SessionId, "STUCK")
		prompt.Meta = promptMeta(1)
		response, promptErr := a.Prompt(t.Context(), prompt)
		done <- result{response: response, err: promptErr}
	}()

	require.Eventually(t, func() bool {
		ss.mu.Lock()
		defer ss.mu.Unlock()

		return ss.turn != nil && ss.turn.nativeTurnID != ""
	}, 5*time.Second, 5*time.Millisecond)

	require.NoError(t, a.Cancel(t.Context(), acp.CancelNotification{SessionId: stuck.SessionId}))

	select {
	case got := <-done:
		require.NoError(t, got.err)
		require.Equal(t, acp.StopReasonCancelled, got.response.StopReason)
	case <-time.After(sessionAbortTimeout + 5*time.Second):
		t.Fatal("the cancelled prompt did not settle after the native turn ignored its interrupt")
	}

	require.True(t, rt.alive(), "an ignored interrupt must not stop the shared app-server")

	ss.mu.Lock()
	unbound := ss.rt == nil
	ss.mu.Unlock()
	require.True(t, unbound, "the contained session leaves the live generation")

	prompt := wire.TextPromptRequest(peer.SessionId, "hi")
	prompt.Meta = promptMeta(2)
	response, err := a.Prompt(t.Context(), prompt)
	require.NoError(t, err)
	require.Equal(t, acp.StopReasonEndTurn, response.StopReason)
}

func TestGatewayModelsCarryPresetEfforts(t *testing.T) {
	t.Parallel()

	ladder := []string{"minimal", effortMedium, "xhigh"}
	presets := []codex.Model{{ID: "gpt-5.6-luna", ReasoningEfforts: slices.Clone(ladder), DefaultReasoningEffort: effortMedium}}
	listed := []gateway.Model{
		{ID: "openai-codex/gpt-5.6-luna", Name: "GPT-5.6-Luna", ContextWindow: 272000, Inputs: []string{"text"}},
		{ID: "openrouter/openai/gpt-5.6-luna", Name: "Luna via OpenRouter"},
		{ID: "opencode-go/qwen3.8-flash", Name: "Qwen3.8 Flash"},
	}

	models := gatewayModelsWithPresetEfforts(listed, presets)

	require.Len(t, models, 3)
	require.Equal(t, ladder, models[0].ReasoningEfforts)
	require.Equal(t, effortMedium, models[0].DefaultReasoningEffort)
	require.Equal(t, ladder, models[1].ReasoningEfforts)
	require.Empty(t, models[2].ReasoningEfforts)
	require.Empty(t, models[2].DefaultReasoningEffort)
	require.Equal(t, int64(272000), models[0].ContextWindow)
	require.Equal(t, []string{"text"}, models[0].InputModalities)

	presets[0].ReasoningEfforts[0] = "mutated"
	require.Equal(t, ladder[0], models[0].ReasoningEfforts[0])
}

// TestCancelWithATerminalAtTheAbortDeadlineSettles lands the native terminal
// event at the moment the abort timer fires, so whichever side wins the race
// the prompt settles and the agent closes.
func TestCancelWithATerminalAtTheAbortDeadlineSettles(t *testing.T) {
	a := NewAgent(testOptions(t)...)
	a.attach(newRecorder(), nil)

	_, err := a.Initialize(t.Context(), acp.InitializeRequest{ProtocolVersion: acp.ProtocolVersionNumber, Meta: map[string]any{wire.LifecycleKey: map[string]any{"version": 1}}})
	require.NoError(t, err)

	created, err := a.NewSession(t.Context(), wire.NewSessionRequest(t.TempDir()))
	require.NoError(t, err)

	s, err := a.session(t.Context(), created.SessionId)
	require.NoError(t, err)

	done := make(chan acp.PromptResponse, 1)

	go func() {
		prompt := wire.TextPromptRequest(created.SessionId, "LATE")
		prompt.Meta = promptMeta(1)
		response, _ := a.Prompt(t.Context(), prompt)
		done <- response
	}()

	require.Eventually(t, func() bool {
		s.mu.Lock()
		defer s.mu.Unlock()

		return s.turn != nil && s.turn.nativeTurnID != ""
	}, 5*time.Second, 5*time.Millisecond)

	require.NoError(t, a.Cancel(t.Context(), acp.CancelNotification{SessionId: created.SessionId}))

	select {
	case response := <-done:
		require.Equal(t, acp.StopReasonCancelled, response.StopReason)
	case <-time.After(sessionAbortTimeout + 10*time.Second):
		t.Fatal("the cancelled prompt did not settle")
	}

	closed := make(chan error, 1)
	go func() { closed <- a.Close() }()

	select {
	case err := <-closed:
		require.NoError(t, err)
	case <-time.After(10 * time.Second):
		t.Fatal("close hung behind a worker waiting on the prompt")
	}
}
