package codexacp

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/coder/acp-go-sdk"
	"github.com/savid/acp-go-codex/internal/codex"
	"github.com/savid/acp-go-codex/internal/lifecycle"
	"github.com/stretchr/testify/require"
)

type gatedAgentOriginStore struct {
	*InMemorySessionStore
	started chan struct{}
	release chan struct{}
	once    sync.Once
}

type blockingLifecycleDeliveryClient struct {
	*recordingAgentClient
	started chan struct{}
	once    sync.Once
	mu      sync.Mutex
	calls   int
}

type blockingAgentOriginUpdateClient struct {
	*recordingAgentClient
	started chan struct{}
	once    sync.Once
	mu      sync.Mutex
	calls   int
}

func (c *blockingAgentOriginUpdateClient) SessionUpdate(ctx context.Context, update acp.SessionNotification) error {
	if _, lifecycleUpdate := update.Meta[lifecycle.MetaKey]; !lifecycleUpdate && update.Update.AgentMessageChunk != nil {
		c.mu.Lock()
		c.calls++
		c.mu.Unlock()
		c.once.Do(func() { close(c.started) })
		<-ctx.Done()

		return ctx.Err()
	}

	return c.recordingAgentClient.SessionUpdate(ctx, update)
}

func (c *blockingAgentOriginUpdateClient) callCount() int {
	c.mu.Lock()
	defer c.mu.Unlock()

	return c.calls
}

func (c *blockingLifecycleDeliveryClient) SessionUpdate(ctx context.Context, update acp.SessionNotification) error {
	if _, lifecycleUpdate := update.Meta[lifecycle.MetaKey]; lifecycleUpdate {
		c.mu.Lock()
		c.calls++
		c.mu.Unlock()
		c.once.Do(func() { close(c.started) })
		<-ctx.Done()

		return ctx.Err()
	}

	return c.recordingAgentClient.SessionUpdate(ctx, update)
}

func (c *blockingLifecycleDeliveryClient) callCount() int {
	c.mu.Lock()
	defer c.mu.Unlock()

	return c.calls
}

func (s *gatedAgentOriginStore) Append(ctx context.Context, key SessionKey, entries []SessionStoreEntry) error {
	s.once.Do(func() { close(s.started) })
	select {
	case <-s.release:
		return s.InMemorySessionStore.Append(ctx, key, entries)
	case <-ctx.Done():
		return ctx.Err()
	}
}

type agentOriginControlClient struct {
	*spyCodexClient
	muControl sync.Mutex
	cancelled [][2]string
}

func (c *agentOriginControlClient) CancelTurn(_ context.Context, threadID, turnID string) error {
	c.muControl.Lock()
	c.cancelled = append(c.cancelled, [2]string{threadID, turnID})
	c.muControl.Unlock()

	return nil
}

func (c *agentOriginControlClient) cancellationTargets() [][2]string {
	c.muControl.Lock()
	defer c.muControl.Unlock()

	return append([][2]string(nil), c.cancelled...)
}

type agentOriginFixture struct {
	agent          *Agent
	session        *session
	client         *agentOriginControlClient
	recorder       *recordingAgentClient
	incarnation    *promptIncarnation
	action         *liveAction
	interactionErr func() error
	finish         func()
}

func newAgentOriginFixture(t *testing.T) *agentOriginFixture {
	t.Helper()
	agent := NewAgent()
	agent.lifecycle = lifecycle.Negotiated{Versions: []int{1}, ActivityKinds: []lifecycle.ActivityKind{}}
	recorder := newRecordingAgentClient()
	agent.setAgentClient(recorder)
	client := &agentOriginControlClient{spyCodexClient: newSpyCodexClient()}
	s := newSession(agent, "session", t.TempDir(), nil, codex.Thread{ID: "thread"}, client, sessionMeta{}, nil)
	agent.sessions[s.id] = s
	require.NoError(t, s.openLifecycleStream(t.Context(), agent.lifecycle))
	require.NoError(t, s.routeNativeEvent(codex.Event{
		Kind: codex.EventAgentMessageDelta, Scope: codex.EventScopeThread,
		ThreadID: "thread", TurnID: "agent-turn", ItemID: "message", Text: "background",
	}))
	s.lifecycleMu.Lock()
	in := s.agentIncarnation
	s.lifecycleMu.Unlock()
	require.NotNil(t, in)
	action, _, err := s.beginAction(withLifecycleActionTurn(t.Context(), in), lifecycle.ActionPermission, true)
	require.NoError(t, err)
	require.NoError(t, action.register(t.Context()))
	interaction, finish := s.beginInteraction(t.Context(), "agent-origin-pending-action")
	t.Cleanup(finish)

	return &agentOriginFixture{
		agent: agent, session: s, client: client, recorder: recorder,
		incarnation: in, action: action, interactionErr: interaction.Err, finish: finish,
	}
}

func TestAgentOriginCancelUsesItsOwnRouteAndContinues(t *testing.T) {
	fixture := newAgentOriginFixture(t)
	promptNonce := fixture.session.turnNonce
	require.Empty(t, promptNonce, "fixture accidentally borrowed a prompt route")
	require.True(t, recorderHasTurnRoute(fixture.recorder, fixture.incarnation.turnNonce), "agent-origin update omitted its exact route nonce")

	err := fixture.agent.Cancel(t.Context(), CancelRequest(fixture.session.id, fixture.incarnation.turnNonce))
	require.NoError(t, err)
	require.ErrorIs(t, fixture.interactionErr(), context.Canceled)
	require.Equal(t, [][2]string{{"thread", "agent-turn"}}, fixture.client.cancellationTargets())
	require.Nil(t, fixture.session.agentIncarnation)
	require.False(t, fixture.incarnation.stream.Fenced())
	requireAgentOriginTerminal(t, fixture.recorder, "cancelled", "cancelled")

	require.NoError(t, fixture.session.routeNativeEvent(codex.Event{
		Kind: codex.EventCompleted, Scope: codex.EventScopeThread,
		ThreadID: "thread", TurnID: "continuation", StopReason: codex.StopReasonEndTurn,
	}))
	require.Nil(t, fixture.session.agentIncarnation)
	require.False(t, fixture.incarnation.stream.Fenced())
}

func TestAgentOriginServerRequestUsesItsOwnRoute(t *testing.T) {
	fixture := newAgentOriginFixture(t)
	enableClientElicitation(fixture.agent, true, true)
	_, err := fixture.agent.handleCodexServerRequest(t.Context(), codex.ServerRequest{
		ID:     json.RawMessage(`"request"`),
		Method: codex.RequestToolUserInput,
		Params: json.RawMessage(`{"threadId":"thread","turnId":"agent-turn","questions":[{"id":"name","header":"Name","question":"Your name?"}]}`),
	})
	require.NoError(t, err)
	require.NotEmpty(t, fixture.recorder.scopes)
	require.Equal(t, fixture.incarnation.turnNonce, fixture.recorder.scopes[len(fixture.recorder.scopes)-1].TurnNonce)
	require.Empty(t, fixture.session.turnNonce, "agent-origin server request borrowed prompt route")
	require.NoError(t, fixture.agent.Cancel(t.Context(), CancelRequest(fixture.session.id, fixture.incarnation.turnNonce)))
}

func TestAgentOriginRawAndTypedUpdatesUseTheSameExactRoute(t *testing.T) {
	fixture := newAgentOriginFixture(t)
	fixture.session.rawMessages = rawMessageConfig{enabled: true}
	require.NoError(t, fixture.session.routeNativeEvent(codex.Event{
		Kind: codex.EventAgentMessageDelta, Scope: codex.EventScopeThread,
		ThreadID: "thread", TurnID: "agent-turn", ItemID: "routed", Text: "routed",
		RawMethod: "item/agentMessage/delta", RawParams: json.RawMessage(`{"delta":"routed"}`),
	}))

	var typedRoute map[string]any
	for _, update := range fixture.recorder.updates {
		if update.Update.AgentMessageChunk != nil && update.Update.AgentMessageChunk.Content.Text != nil &&
			update.Update.AgentMessageChunk.Content.Text.Text == "routed" {
			typedRoute, _ = update.Meta[routeMetaKey].(map[string]any)
		}
	}
	require.NotNil(t, typedRoute)
	require.NotEmpty(t, fixture.recorder.extensions)
	rawPayload := rawEventPayload(t, fixture.recorder.extensions[len(fixture.recorder.extensions)-1])
	rawMeta, _ := rawPayload[jsonFieldMeta].(map[string]any)
	rawRoute, _ := rawMeta[routeMetaKey].(map[string]any)
	require.Equal(t, typedRoute, rawRoute)
	require.Equal(t, fixture.incarnation.turnNonce, typedRoute[routeTurnNonceKey])
	require.Equal(t, fixture.incarnation.turnNonce, fixture.session.activeTurnNonce())

	require.Error(t, fixture.agent.Cancel(t.Context(), CancelRequest(fixture.session.id, "stale")))
	require.Empty(t, fixture.client.cancellationTargets())
	require.NoError(t, fixture.agent.Cancel(t.Context(), CancelRequest(fixture.session.id, fixture.incarnation.turnNonce)))
	require.Equal(t, [][2]string{{"thread", "agent-turn"}}, fixture.client.cancellationTargets())
}

func recorderHasTurnRoute(recorder *recordingAgentClient, turnNonce string) bool {
	for _, update := range recorder.updates {
		route, _ := update.Meta[routeMetaKey].(map[string]any)
		if route[routeTurnNonceKey] == turnNonce {
			return true
		}
	}

	return false
}

func TestAgentOriginCloseTerminalizesBeforeFence(t *testing.T) {
	fixture := newAgentOriginFixture(t)
	_, err := fixture.agent.CloseSession(t.Context(), acp.CloseSessionRequest{SessionId: fixture.session.id})
	require.NoError(t, err)
	require.ErrorIs(t, fixture.interactionErr(), context.Canceled)
	require.Equal(t, [][2]string{{"thread", "agent-turn"}}, fixture.client.cancellationTargets())
	requireAgentOriginTerminal(t, fixture.recorder, "cancelled", "cancelled")
	require.True(t, fixture.incarnation.stream.Fenced())
	require.Nil(t, fixture.agent.activeSession(fixture.session.id))
}

func TestAgentOriginNativeLossFailsActionsThenFences(t *testing.T) {
	fixture := newAgentOriginFixture(t)
	nativeErr := errors.New("native source lost")
	fixture.session.failNativeIncarnation(nativeErr)

	require.ErrorIs(t, fixture.interactionErr(), context.Canceled)
	require.Nil(t, fixture.session.agentIncarnation)
	require.True(t, fixture.incarnation.stream.Fenced())
	require.ErrorIs(t, fixture.session.lifecycleFailure, nativeErr)
	requireAgentOriginTerminal(t, fixture.recorder, "failed", "failed")
}

func TestAgentOriginActiveRebindRejectsUntilContained(t *testing.T) {
	fixture := newAgentOriginFixture(t)
	err := fixture.session.prepareNativeEventRebind()
	require.Error(t, err)
	require.Same(t, fixture.incarnation, fixture.session.agentIncarnation)
	require.False(t, fixture.incarnation.stream.Fenced())
	requireAgentOriginActionState(t, fixture.recorder, "pending")

	err = fixture.agent.Cancel(t.Context(), CancelRequest(fixture.session.id, fixture.incarnation.turnNonce))
	require.NoError(t, err)
	require.NoError(t, fixture.session.prepareNativeEventRebind())
	require.True(t, fixture.incarnation.stream.Fenced())
	require.Nil(t, fixture.session.lifecycleStream)
}

func TestAgentOriginSettlementMirrorsBeforeActionAndIdle(t *testing.T) {
	for _, test := range []struct {
		name   string
		settle func(*agentOriginFixture) <-chan error
	}{
		{
			name: "natural completion",
			settle: func(fixture *agentOriginFixture) <-chan error {
				done := make(chan error, 1)
				go func() {
					done <- fixture.session.routeNativeEvent(codex.Event{
						Kind: codex.EventCompleted, Scope: codex.EventScopeThread,
						ThreadID: "thread", TurnID: "agent-turn", StopReason: codex.StopReasonEndTurn,
					})
				}()

				return done
			},
		},
		{
			name: "native loss",
			settle: func(fixture *agentOriginFixture) <-chan error {
				done := make(chan error, 1)
				go func() {
					fixture.session.failNativeIncarnation(errors.New("native source lost"))
					done <- nil
				}()

				return done
			},
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			fixture := newAgentOriginFixture(t)
			store := &gatedAgentOriginStore{
				InMemorySessionStore: NewInMemorySessionStore(),
				started:              make(chan struct{}),
				release:              make(chan struct{}),
			}
			fixture.agent.options.SessionStore = store
			rollout := fixture.session.cwd + "/rollout.jsonl"
			require.NoError(t, os.WriteFile(rollout, []byte("{\"type\":\"event_msg\"}\n"), 0o600))
			fixture.session.rolloutPath = rollout

			done := test.settle(fixture)
			<-store.started
			requireAgentOriginActionState(t, fixture.recorder, "pending")
			require.False(t, hasAgentOriginIdle(fixture.recorder), "terminal idle overtook durable mirror")
			close(store.release)
			require.NoError(t, <-done)

			entries, err := store.Load(t.Context(), SessionKey{SessionID: string(fixture.session.id)})
			require.NoError(t, err)
			require.Len(t, entries, 1)
			if test.name == "native loss" {
				requireAgentOriginTerminal(t, fixture.recorder, "failed", "failed")
			} else {
				requireAgentOriginTerminal(t, fixture.recorder, "failed", "success")
			}
		})
	}
}

func TestAgentOriginMirrorFailureFencesWithoutIdleAndRetainsOwedPrefix(t *testing.T) {
	fixture := newAgentOriginFixture(t)
	store := NewInMemorySessionStore()
	fixture.agent.options.SessionStore = store
	rollout := fixture.session.cwd + "/rollout.jsonl"
	require.NoError(t, os.WriteFile(rollout, []byte("not-json\n"), 0o600))
	fixture.session.rolloutPath = rollout

	err := fixture.session.routeNativeEvent(codex.Event{
		Kind: codex.EventCompleted, Scope: codex.EventScopeThread,
		ThreadID: "thread", TurnID: "agent-turn", StopReason: codex.StopReasonEndTurn,
	})
	require.Error(t, err)
	requireAgentOriginActionState(t, fixture.recorder, "pending")
	require.False(t, hasAgentOriginIdle(fixture.recorder))
	require.True(t, fixture.incarnation.stream.Fenced())
	require.True(t, fixture.session.captureFailed)

	require.NoError(t, os.WriteFile(rollout, []byte("{\"type\":\"event_msg\"}\n"), 0o600))
	require.NoError(t, fixture.session.commitResumableSnapshot(t.Context()))
	entries, loadErr := store.Load(t.Context(), SessionKey{SessionID: string(fixture.session.id)})
	require.NoError(t, loadErr)
	require.Len(t, entries, 1)
}

func TestAgentOriginNativeFailureAndMirrorFailureRemainExactWithoutIdle(t *testing.T) {
	fixture := newAgentOriginFixture(t)
	fixture.agent.options.SessionStore = NewInMemorySessionStore()
	rollout := fixture.session.cwd + "/rollout.jsonl"
	require.NoError(t, os.WriteFile(rollout, []byte("not-json\n"), 0o600))
	fixture.session.rolloutPath = rollout
	nativeErr := errors.New("native source lost exactly")

	fixture.session.failNativeIncarnation(nativeErr)

	require.ErrorIs(t, fixture.session.lifecycleFailure, nativeErr)
	require.True(t, fixture.session.captureFailed)
	requireAgentOriginActionState(t, fixture.recorder, "pending")
	require.False(t, hasAgentOriginIdle(fixture.recorder))
	require.True(t, fixture.incarnation.stream.Fenced())
}

func TestAgentOriginCloseBoundsBlockedLifecycleDelivery(t *testing.T) {
	fixture := newAgentOriginFixture(t)
	blocked := &blockingLifecycleDeliveryClient{
		recordingAgentClient: newRecordingAgentClient(),
		started:              make(chan struct{}),
	}
	fixture.agent.setAgentClient(blocked)
	ctx, cancel := context.WithTimeout(t.Context(), 50*time.Millisecond)
	defer cancel()

	closed := make(chan error, 1)
	go func() {
		_, err := fixture.agent.CloseSession(ctx, acp.CloseSessionRequest{SessionId: fixture.session.id})
		closed <- err
	}()
	select {
	case <-blocked.started:
	case <-time.After(time.Second):
		t.Fatal("close did not reach blocked lifecycle delivery")
	}
	err := <-closed
	require.ErrorIs(t, err, context.DeadlineExceeded)
	require.True(t, fixture.incarnation.stream.Fenced())
	require.Nil(t, fixture.session.agentIncarnation)

	calls := blocked.callCount()
	require.Equal(t, calls, blocked.callCount(), "lifecycle delivery continued after close returned")
}

func TestAgentOriginCloseCancelsBlockedHostUpdateBeforeOwningSettlement(t *testing.T) {
	fixture := newAgentOriginFixture(t)
	blocked := &blockingAgentOriginUpdateClient{
		recordingAgentClient: newRecordingAgentClient(),
		started:              make(chan struct{}),
	}
	fixture.agent.setAgentClient(blocked)

	routed := make(chan error, 1)
	go func() {
		routed <- fixture.session.routeNativeEvent(codex.Event{
			Kind: codex.EventAgentMessageDelta, Scope: codex.EventScopeThread,
			ThreadID: "thread", TurnID: "agent-turn", ItemID: "blocked", Text: "blocked",
		})
	}()
	select {
	case <-blocked.started:
	case <-time.After(time.Second):
		t.Fatal("agent-origin update did not reach the blocked host")
	}

	ctx, cancel := context.WithTimeout(t.Context(), 250*time.Millisecond)
	defer cancel()
	_, err := fixture.agent.CloseSession(ctx, acp.CloseSessionRequest{SessionId: fixture.session.id})
	require.NoError(t, err)
	require.ErrorIs(t, <-routed, context.Canceled)
	requireAgentOriginTerminal(t, blocked.recordingAgentClient, "cancelled", "cancelled")
	require.True(t, fixture.incarnation.stream.Fenced())

	calls := blocked.callCount()
	require.Equal(t, calls, blocked.callCount(), "host delivery continued after close returned")
}

func TestAgentOriginCloseContainsBlockedServerRequestOpening(t *testing.T) {
	agent := NewAgent()
	agent.lifecycle = lifecycle.Negotiated{Versions: []int{1}, ActivityKinds: []lifecycle.ActivityKind{}}
	recorder := newRecordingAgentClient()
	agent.setAgentClient(recorder)
	client := &agentOriginControlClient{spyCodexClient: newSpyCodexClient()}
	s := newSession(agent, "session", t.TempDir(), nil, codex.Thread{ID: "thread"}, client, sessionMeta{}, nil)
	agent.sessions[s.id] = s
	require.NoError(t, s.openLifecycleStream(t.Context(), agent.lifecycle))

	blocked := &blockingLifecycleDeliveryClient{
		recordingAgentClient: newRecordingAgentClient(),
		started:              make(chan struct{}),
	}
	agent.setAgentClient(blocked)
	enableClientElicitation(agent, true, true)
	requestDone := make(chan error, 1)
	go func() {
		_, err := agent.handleCodexServerRequest(t.Context(), codex.ServerRequest{
			ID:     json.RawMessage(`"opening"`),
			Method: codex.RequestToolUserInput,
			Params: json.RawMessage(`{"threadId":"thread","turnId":"opening-turn","questions":[{"id":"name","header":"Name","question":"Your name?"}]}`),
		})
		requestDone <- err
	}()
	select {
	case <-blocked.started:
	case <-time.After(time.Second):
		t.Fatal("server-request opening did not reach lifecycle delivery")
	}

	ctx, cancel := context.WithTimeout(t.Context(), 500*time.Millisecond)
	defer cancel()
	_, closeErr := agent.CloseSession(ctx, acp.CloseSessionRequest{SessionId: s.id})
	require.Error(t, closeErr)
	require.Error(t, <-requestDone)
	require.Equal(t, [][2]string{{"thread", "opening-turn"}}, client.cancellationTargets())
	require.Nil(t, s.agentIncarnation)
	require.True(t, s.lifecycleStream.Fenced())

	calls := blocked.callCount()
	require.Equal(t, calls, blocked.callCount(), "server-request delivery continued after close returned")
}

func TestAgentOriginCloseGatePreventsLaterTurnAdmission(t *testing.T) {
	agent := NewAgent()
	agent.lifecycle = lifecycle.Negotiated{Versions: []int{1}, ActivityKinds: []lifecycle.ActivityKind{}}
	recorder := newRecordingAgentClient()
	agent.setAgentClient(recorder)
	s := newSession(agent, "session", t.TempDir(), nil, codex.Thread{ID: "thread"}, newSpyCodexClient(), sessionMeta{}, nil)
	require.NoError(t, s.openLifecycleStream(t.Context(), agent.lifecycle))
	require.NoError(t, s.beginLifecycleClose(t.Context()))
	updates := len(recorder.updates)
	require.NoError(t, s.routeNativeEvent(codex.Event{
		Kind: codex.EventAgentMessageDelta, Scope: codex.EventScopeThread,
		ThreadID: "thread", TurnID: "too-late", ItemID: "message", Text: "late",
	}))
	s.lifecycleMu.Lock()
	require.Nil(t, s.agentIncarnation)
	s.lifecycleMu.Unlock()
	require.Len(t, recorder.updates, updates)
	s.fenceSession()
}

func hasAgentOriginIdle(recorder *recordingAgentClient) bool {
	for _, update := range recorder.updates {
		event := lifecycleEvent(update)
		if event["type"] == "state_update" && event["state"] == "idle" && event["turnId"] == "agent-turn" {
			return true
		}
	}

	return false
}

func requireAgentOriginTerminal(t *testing.T, recorder *recordingAgentClient, actionState, outcome string) {
	t.Helper()
	requireAgentOriginActionState(t, recorder, actionState)
	var idleIndex = -1
	var actionIndex = -1
	for index, update := range recorder.updates {
		event := lifecycleEvent(update)
		action, _ := event["action"].(map[string]any)
		if event["type"] == "action_update" && action["state"] == actionState {
			actionIndex = index
		}
		if event["type"] == "state_update" && event["state"] == "idle" && event["turnId"] == "agent-turn" {
			idleIndex = index
			require.Equal(t, outcome, event["outcome"])
			require.Equal(t, string(lifecycle.CauseActivity), event["cause"])
		}
	}
	require.NotEqual(t, -1, actionIndex)
	require.Greater(t, idleIndex, actionIndex, "agent-origin idle overtook action terminalization")
}

func requireAgentOriginActionState(t *testing.T, recorder *recordingAgentClient, state string) {
	t.Helper()
	for _, update := range recorder.updates {
		event := lifecycleEvent(update)
		action, _ := event["action"].(map[string]any)
		if event["type"] == "action_update" && action["state"] == state {
			return
		}
	}
	t.Fatalf("agent-origin action state %q not emitted", state)
}

func lifecycleEvent(update acp.SessionNotification) map[string]any {
	envelope, _ := update.Meta[lifecycle.MetaKey].(map[string]any)
	event, _ := envelope["event"].(map[string]any)

	return event
}
