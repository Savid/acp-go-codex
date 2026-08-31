package codexacp

import (
	"context"
	"errors"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/coder/acp-go-sdk"
	"github.com/savid/acp-go-codex/internal/codex"
	"github.com/stretchr/testify/require"
)

// The native turn id is what thread-scoped containment targets, so it must be
// readable while a turn runs and must never be cleared by an id-less event.
func TestSessionActiveTurnIDTracksNativeTurn(t *testing.T) {
	session := &session{}
	require.Empty(t, session.activeTurnID())

	session.setTurnID("native-turn-1")
	require.Equal(t, "native-turn-1", session.activeTurnID())

	session.setTurnID("")
	require.Equal(t, "native-turn-1", session.activeTurnID())

	session.setTurnID("native-turn-2")
	require.Equal(t, "native-turn-2", session.activeTurnID())
	session.stageTurnID("")
	require.Equal(t, "native-turn-2", session.activeTurnID())
}

func TestCanceledTurnIsNotAnActiveNativeRequestTarget(t *testing.T) {
	session := &session{}
	_ = session.beginTurn(context.Background(), "turn-nonce")
	session.setTurnID("native-turn")

	nonce, active := session.activeTurnNonceForNativeTurn("native-turn")
	require.Equal(t, "turn-nonce", nonce)
	require.True(t, active)

	session.cancelTurn()
	nonce, active = session.activeTurnNonceForNativeTurn("native-turn")
	require.Equal(t, "turn-nonce", nonce)
	require.False(t, active)
	session.finishTurn()
}

func TestTurnContainmentWaitBranches(t *testing.T) {
	containErr := errors.New("containment failed")
	done := make(chan struct{})
	close(done)
	completed := &session{
		cancel: func() {},
		turnContainment: &turnContainment{
			done: done, err: containErr, started: true,
		},
	}
	require.ErrorIs(t, completed.shutdownActiveTurn(context.Background(), true), containErr)

	waiting := &session{
		cancel: func() {},
		turnContainment: &turnContainment{
			done: make(chan struct{}), started: true,
		},
	}
	canceled, cancel := context.WithCancel(context.Background())
	cancel()
	require.ErrorIs(t, waiting.shutdownActiveTurn(canceled, true), context.Canceled)
	require.ErrorIs(t, waiting.awaitTurnContainment(canceled), context.Canceled)
}

func TestShutdownActiveTurnCancelsInteractionStartedDuringContainment(t *testing.T) {
	client := &blockingInterruptClient{
		spyCodexClient:   newSpyCodexClient(),
		runStarted:       make(chan struct{}),
		interruptStarted: make(chan struct{}),
		interruptRelease: make(chan struct{}),
	}
	session := &session{
		agent: NewAgent(), client: client, codexThreadID: "thread",
	}
	_ = session.beginTurn(context.Background(), "turn-nonce")
	session.setTurnID("native-turn")

	shutdownDone := make(chan error, 1)
	go func() {
		shutdownDone <- session.shutdownActiveTurn(context.Background(), true)
	}()
	<-client.interruptStarted

	interactionCtx, finishInteraction := session.beginInteraction(context.Background(), "during-containment")
	close(client.interruptRelease)

	require.NoError(t, <-shutdownDone)
	require.ErrorIs(t, interactionCtx.Err(), context.Canceled)
	session.mu.Lock()
	require.Empty(t, session.interactions)
	session.mu.Unlock()

	finishInteraction()
	session.finishTurn()
}

func TestContainDeadSessionFromRetiredGeneration(t *testing.T) {
	client := newSpyCodexClient()
	agent := NewAgent()
	session := &session{
		agent: agent, id: "session", client: client,
		codexThreadID: "thread", clientDead: true,
	}

	require.NoError(t, session.containSession(context.Background()))
}

func TestSessionInteractionCancellationBranches(t *testing.T) {
	session := &session{}
	release, err := session.acquireTurn(context.Background())
	if err != nil {
		t.Fatalf("acquireTurn returned error: %v", err)
	}
	backpressureErr := func() error {
		_, err := session.acquireTurn(context.Background())

		return err
	}()
	if backpressureErr == nil {
		t.Fatal("acquireTurn ignored prompt backpressure")
	}

	var reqErr *acp.RequestError
	if !errors.As(backpressureErr, &reqErr) || reqErr.Code != -32600 {
		t.Fatalf("backpressure error = %v, want -32600 invalid request", backpressureErr)
	}
	backpressureData, ok := reqErr.Data.(map[string]any)
	if !ok || backpressureData[jsonFieldError] != valueBackpressure || backpressureData[jsonFieldLimit] != limitSessionPrompt {
		t.Fatalf("backpressure payload = %#v, want {error:backpressure, limit:session_prompt}", reqErr.Data)
	}
	release()

	interactionCtx, finish := session.beginInteraction(context.TODO(), "")
	if interactionCtx.Err() != nil {
		t.Fatal("interaction without parent started canceled")
	}
	finish()
	if interactionCtx.Err() == nil {
		t.Fatal("interaction finish did not cancel context")
	}

	turnParent := context.Background()
	_ = session.beginTurn(turnParent, "test-turn")
	firstCtx, firstFinish := session.beginInteraction(context.Background(), "duplicate")
	secondCtx, secondFinish := session.beginInteraction(context.Background(), "duplicate")
	if firstCtx.Err() == nil {
		t.Fatal("duplicate interaction did not detach first context")
	}
	session.cancelTurn()
	if secondCtx.Err() == nil || !session.wasTurnCancelled() {
		t.Fatal("cancelTurn did not cancel pending interaction")
	}
	lateCtx, lateFinish := session.beginInteraction(context.Background(), "after-cancel")
	if !errors.Is(lateCtx.Err(), context.Canceled) {
		t.Fatal("interaction created after turn cancellation started uncanceled")
	}
	firstFinish()
	secondFinish()
	lateFinish()
	session.finishTurn()

	_ = session.beginTurn(context.Background(), "test-turn")
	turnInteraction, turnFinish := session.beginInteraction(context.Background(), "finish-turn")
	session.finishTurn()
	if turnInteraction.Err() == nil {
		t.Fatal("finishTurn did not detach pending interaction")
	}
	turnFinish()
}

func TestSessionSnapshotConcurrentAccountUpdates(t *testing.T) {
	session := &session{
		id:              "s",
		cwd:             "/tmp/project",
		codexThreadID:   "thread",
		model:           "gpt",
		modelProvider:   "openai",
		reasoningEffort: "medium",
		serviceTier:     "flex",
		personality:     "pragmatic",
		accountMeta:     map[string]any{"id": "acct"},
	}

	stop := make(chan struct{})
	done := make(chan struct{})
	go func() {
		defer close(done)
		for {
			select {
			case <-stop:
				return
			default:
				session.setAccount(map[string]any{"id": "acct", "planType": "plus"})
			}
		}
	}()
	defer func() {
		close(stop)
		<-done
	}()
	for range 1000 {
		meta := sessionResponseMeta(session.snapshot())
		codexMeta := asType[map[string]any](t, meta[codexMetaKey])
		if _, ok := meta["github.com/savid/acp-go-codex"]; ok {
			t.Fatal("deleted package-path meta was emitted")
		}
		asType[map[string]any](t, codexMeta[codexAccountMetaKey])["id"] = "changed"
		nextMeta := sessionResponseMeta(session.snapshot())
		nextCodexMeta := asType[map[string]any](t, nextMeta[codexMetaKey])
		if asType[map[string]any](t, nextCodexMeta[codexAccountMetaKey])["id"] == "changed" {
			t.Fatal("session account meta aliases response meta")
		}
		_ = codexAuthRequiredError(errors.New("not logged in"), session.accountMetaSnapshot())
	}
}

func TestSessionCloseJoinsClientAndMaterializedErrors(t *testing.T) {
	origRemoveRollout := removeMaterializedRolloutFile
	t.Cleanup(func() { removeMaterializedRolloutFile = origRemoveRollout })
	removeMaterializedRolloutFile = func(string) error {
		return errors.New("remove failed")
	}

	session := &session{
		agent:            NewAgent(),
		client:           &errorCodexClient{spyCodexClient: newSpyCodexClient(), closeErr: errors.New("close failed")},
		materializedPath: "/tmp/rollout.jsonl",
	}
	err := session.Close(context.Background())
	if err == nil || !strings.Contains(err.Error(), "remove failed") || strings.Contains(err.Error(), "close failed") {
		t.Fatalf("logical session release error = %v", err)
	}
}

type closeContextRecordingClient struct {
	*spyCodexClient
	closeCtxErr error
}

func (c *closeContextRecordingClient) Close(ctx context.Context) error {
	c.closeCtxErr = ctx.Err()

	return ctx.Err()
}

func TestSessionCloseUsesBoundedBackgroundContext(t *testing.T) {
	client := &closeContextRecordingClient{spyCodexClient: newSpyCodexClient()}
	agent := NewAgent(withClientFactory(func(context.Context, codex.Options) (codex.Client, error) { return client, nil }))

	resp, err := agent.NewSession(context.Background(), NewSessionRequest("/tmp/project"))
	if err != nil {
		t.Fatalf("NewSession returned error: %v", err)
	}

	if _, closeErr := agent.CloseSession(canceledContext(), acp.CloseSessionRequest{SessionId: resp.SessionId}); closeErr != nil {
		t.Fatalf("CloseSession with canceled caller context returned error: %v", closeErr)
	}
	if client.closeCtxErr != nil {
		t.Fatalf("native close ran under the caller's canceled context: %v", client.closeCtxErr)
	}
}

type coordinatedSessionCloseClient struct {
	*spyCodexClient
	started chan struct{}
	release chan struct{}
	calls   atomic.Int64
}

func (c *coordinatedSessionCloseClient) UnsubscribeThread(ctx context.Context, _ string) error {
	if c.calls.Add(1) == 1 {
		close(c.started)
	}

	select {
	case <-c.release:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func TestSessionCloseCoordinatesConcurrentCallersExactlyOnce(t *testing.T) {
	var commits atomic.Int64
	store := &appendFuncStore{append: func(context.Context, SessionKey, []SessionStoreEntry) error {
		commits.Add(1)

		return nil
	}}
	agent := NewAgent(WithSessionStore(store))
	client := &coordinatedSessionCloseClient{
		spyCodexClient: newSpyCodexClient(), started: make(chan struct{}), release: make(chan struct{}),
	}
	s := newSession(agent, "session", t.TempDir(), nil, codex.Thread{ID: "thread"}, client, sessionMeta{}, nil)
	s.unsyncedEntries = []SessionStoreEntry{[]byte(`{"type":"event_msg"}`)}
	agent.sessions[s.id] = s
	agent.runtimeClient = client

	const callers = 24
	results := make(chan error, callers)
	for range callers {
		go func() { results <- s.Close(t.Context()) }()
	}
	<-client.started
	require.Equal(t, int64(1), client.calls.Load())
	close(client.release)
	for range callers {
		require.NoError(t, <-results)
	}

	require.Equal(t, int64(1), client.calls.Load())
	require.Equal(t, int64(1), commits.Load())
}

func TestEnsureLiveClientRelaunchFailures(t *testing.T) {
	ctx := context.Background()

	factoryErr := errors.New("relaunch factory failed")
	newClientFails := &session{
		agent:         NewAgent(withClientFactory(func(context.Context, codex.Options) (codex.Client, error) { return nil, factoryErr })),
		id:            "relaunch-factory",
		codexThreadID: "thread",
		clientDead:    true,
	}
	newClientFails.agent.sessions[newClientFails.id] = newClientFails
	newClientFails.agent.runtimeDead = true
	if err := newClientFails.ensureLiveClient(ctx); !errors.Is(err, factoryErr) {
		t.Fatalf("ensureLiveClient factory error = %v", err)
	}
	if !newClientFails.clientDead {
		t.Fatal("failed relaunch must leave client dead")
	}

	resumeErr := errors.New("resume rejected")
	resumeClient := &errorCodexClient{spyCodexClient: newSpyCodexClient(), resumeErr: resumeErr}
	resumeFails := &session{
		agent:         NewAgent(withClientFactory(func(context.Context, codex.Options) (codex.Client, error) { return resumeClient, nil })),
		id:            "relaunch-resume",
		codexThreadID: "thread",
		clientDead:    true,
	}
	old := newSpyCodexClient()
	resumeFails.client = old
	resumeFails.agent.sessions[resumeFails.id] = resumeFails
	resumeFails.agent.runtimeClient = old
	resumeFails.agent.runtimeDead = true
	resumeFails.agent.runtimeNativeRelease = func() error { return nil }
	if err := resumeFails.ensureLiveClient(ctx); !errors.Is(err, resumeErr) {
		t.Fatalf("ensureLiveClient resume error = %v", err)
	}

	// A prompt on a dead session whose relaunch fails surfaces the transport
	// failure and keeps the session addressable.
	if _, err := resumeFails.Prompt(ctx, TextPromptRequest("relaunch-resume", "test-turn", "hi")); !isTurnFailure(err, codex.CauseTransport) {
		t.Fatalf("prompt after failed relaunch = %v, want transport failure", err)
	}
}

func TestEnsureLiveClientPublishesOnlyCurrentSessionGeneration(t *testing.T) {
	t.Run("canonical resumed rollout path", func(t *testing.T) {
		replacement := newSpyCodexClient()
		replacement.thread.Path = "/replacement/rollout.jsonl"
		agent := NewAgent(withClientFactory(func(context.Context, codex.Options) (codex.Client, error) {
			return replacement, nil
		}))
		old := newSpyCodexClient()
		active := &session{
			agent: agent, id: "path", cwd: "/tmp/project",
			codexThreadID: "thread-1", client: old, clientDead: true,
		}
		agent.sessions[active.id] = active
		agent.runtimeClient = old
		agent.runtimeDead = true

		require.NoError(t, active.ensureLiveClient(context.Background()))
		require.Equal(t, "/replacement/rollout.jsonl", active.rolloutPath)
		require.NoError(t, agent.Close())
	})

	for _, test := range []struct {
		name          string
		rebindFailure bool
		mutate        func(*Agent, *session)
		check         func(*testing.T, error)
	}{
		{
			name: "logical session removed",
			mutate: func(agent *Agent, active *session) {
				agent.mu.Lock()
				delete(agent.sessions, active.id)
				agent.mu.Unlock()
			},
			check: func(t *testing.T, err error) {
				t.Helper()

				require.ErrorContains(t, err, "unknown session")
			},
		},
		{
			name: "runtime generation retired",
			mutate: func(agent *Agent, active *session) {
				agent.mu.Lock()
				agent.runtimeDead = true
				agent.mu.Unlock()
			},
			check: func(t *testing.T, err error) {
				t.Helper()

				require.ErrorIs(t, err, codex.ErrConnectionClosed)
			},
		},
		{
			name:          "logical session removed with rebind refusal",
			rebindFailure: true,
			mutate: func(agent *Agent, active *session) {
				agent.mu.Lock()
				delete(agent.sessions, active.id)
				agent.mu.Unlock()
			},
			check: func(t *testing.T, err error) {
				t.Helper()
				require.ErrorContains(t, err, "unknown session")
				require.ErrorContains(t, err, "native lifecycle is active")
			},
		},
		{
			name:          "runtime generation retired with rebind refusal",
			rebindFailure: true,
			mutate: func(agent *Agent, active *session) {
				agent.mu.Lock()
				agent.runtimeDead = true
				agent.mu.Unlock()
			},
			check: func(t *testing.T, err error) {
				t.Helper()
				require.ErrorIs(t, err, codex.ErrConnectionClosed)
				require.ErrorContains(t, err, "native lifecycle is active")
			},
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			blocking := &blockingLifecycleCodexClient{
				spyCodexClient: newSpyCodexClient(), resumeStarted: make(chan codex.ThreadResumeRequest, 1), resumeRelease: make(chan struct{}),
			}
			var replacement codex.Client = blocking
			var activeReplacement *activeAfterSubscribeClient
			if test.rebindFailure {
				activeReplacement = &activeAfterSubscribeClient{blockingLifecycleCodexClient: blocking}
				replacement = activeReplacement
			}
			agent := NewAgent(withClientFactory(func(context.Context, codex.Options) (codex.Client, error) {
				return replacement, nil
			}))
			old := newSpyCodexClient()
			active := &session{
				agent: agent, id: acp.SessionId(test.name), cwd: "/tmp/project",
				codexThreadID: "thread-1", client: old, clientDead: true,
			}
			if activeReplacement != nil {
				activeReplacement.active = active
			}
			agent.sessions[active.id] = active
			agent.runtimeClient = old
			agent.runtimeDead = true

			done := make(chan error, 1)
			go func() { done <- active.ensureLiveClient(context.Background()) }()
			<-blocking.resumeStarted
			test.mutate(agent, active)
			close(blocking.resumeRelease)
			test.check(t, <-done)
			require.True(t, active.clientDead)
			active.lifecycleMu.Lock()
			active.lifecycleDeliveryActive = false
			active.lifecycleMu.Unlock()
			active.fenceSession()
			require.NoError(t, agent.Close())
		})
	}
}

type activeAfterSubscribeClient struct {
	*blockingLifecycleCodexClient
	active *session
}

func (c *activeAfterSubscribeClient) SubscribeThread(ctx context.Context, threadID string) (codex.ThreadEventStream, error) {
	stream, err := c.blockingLifecycleCodexClient.SubscribeThread(ctx, threadID)
	if err == nil {
		c.active.lifecycleMu.Lock()
		c.active.lifecycleDeliveryActive = true
		c.active.lifecycleMu.Unlock()
	}

	return stream, err
}

type canaryPreparationFailureClient struct {
	*runtimeRecordingClient
	session *session
}

func (c *canaryPreparationFailureClient) RunTurn(context.Context, codex.TurnStartRequest) (codex.Turn, error) {
	c.session.lifecycleMu.Lock()
	c.session.lifecycleDeliveryActive = true
	c.session.lifecycleMu.Unlock()

	return codex.Turn{}, errors.New("runtime_ready failed")
}

func TestEnsureLiveClientContainsBrokerAndCanaryRecoveryFailures(t *testing.T) {
	t.Run("broker rebind", func(t *testing.T) {
		replacement := newSpyCodexClient()
		agent := NewAgent(withClientFactory(func(context.Context, codex.Options) (codex.Client, error) { return replacement, nil }))
		old := newSpyCodexClient()
		active := &session{agent: agent, id: "session", codexThreadID: "thread", client: old, clientDead: true, lifecycleClosing: true}
		agent.sessions[active.id] = active
		agent.runtimeClient = old
		agent.runtimeDead = true
		require.ErrorContains(t, active.ensureLiveClient(t.Context()), "lifecycle is closing")
		active.lifecycleClosing = false
		require.NoError(t, agent.Close())
	})

	t.Run("canary rebind succeeds", func(t *testing.T) {
		replacement := &runtimeFailureClient{
			runtimeRecordingClient: newRuntimeRecordingClient(),
			events:                 []codex.Event{{Kind: codex.EventCompleted, StopReason: codex.StopReasonEndTurn}},
		}
		agent := NewAgent(withClientFactory(func(context.Context, codex.Options) (codex.Client, error) { return replacement, nil }))
		old := newSpyCodexClient()
		active := &session{
			agent: agent, id: "session", codexThreadID: "thread", client: old, clientDead: true,
			mcpServers: []acp.McpServer{HTTPMCPServer("marker", "https://example.test/mcp", nil)},
		}
		agent.sessions[active.id] = active
		agent.runtimeClient = old
		agent.runtimeDead = true
		require.ErrorContains(t, active.ensureLiveClient(t.Context()), "marker was not observed")
		require.NoError(t, agent.Close())
	})

	t.Run("canary rebind refusal joins", func(t *testing.T) {
		replacement := &canaryPreparationFailureClient{runtimeRecordingClient: newRuntimeRecordingClient()}
		agent := NewAgent(withClientFactory(func(context.Context, codex.Options) (codex.Client, error) { return replacement, nil }))
		old := newSpyCodexClient()
		active := &session{
			agent: agent, id: "session", codexThreadID: "thread", client: old, clientDead: true,
			mcpServers: []acp.McpServer{HTTPMCPServer("marker", "https://example.test/mcp", nil)},
		}
		replacement.session = active
		agent.sessions[active.id] = active
		agent.runtimeClient = old
		agent.runtimeDead = true
		err := active.ensureLiveClient(t.Context())
		require.ErrorContains(t, err, "marker was not observed")
		require.ErrorContains(t, err, "native lifecycle is active")
		active.lifecycleMu.Lock()
		active.lifecycleDeliveryActive = false
		active.lifecycleMu.Unlock()
		active.fenceSession()
		require.NoError(t, agent.Close())
	})
}

type blockingUnsubscribeErrorClient struct {
	*errorCodexClient
	started chan struct{}
	release chan struct{}
}

func (c *blockingUnsubscribeErrorClient) UnsubscribeThread(ctx context.Context, _ string) error {
	close(c.started)

	select {
	case <-c.release:
		return errors.New("retired connection")
	case <-ctx.Done():
		return ctx.Err()
	}
}

func TestSessionContainmentAcceptsConcurrentRuntimeRetirementProof(t *testing.T) {
	client := &blockingUnsubscribeErrorClient{
		errorCodexClient: &errorCodexClient{spyCodexClient: newSpyCodexClient()},
		started:          make(chan struct{}),
		release:          make(chan struct{}),
	}
	agent := NewAgent()
	active := &session{
		agent: agent, id: "unsubscribe-race", codexThreadID: "thread-1", client: client,
	}
	agent.sessions[active.id] = active
	agent.runtimeClient = client

	done := make(chan error, 1)
	go func() { done <- active.containSession(context.Background()) }()
	<-client.started
	active.setClientDead(true)
	close(client.release)

	require.EqualError(t, <-done, "retired connection")
	require.False(t, agent.runtimeDead)
}

func TestSessionTurnAndCloseOperationsFailClosedAtOwnershipBoundaries(t *testing.T) {
	s := &session{closing: true}
	_, err := s.beginPromptTurn(t.Context(), "nonce")
	require.Error(t, err)
	require.ErrorIs(t, s.shutdownActiveTurnForNonce(t.Context(), true, "nonce"), errTurnRouteMismatch)

	s = &session{turnNonce: "current", cancel: func() {}, turnContainment: &turnContainment{done: make(chan struct{})}}
	handled, err := s.shutdownPromptTurn(t.Context(), true, "stale", true)
	require.True(t, handled)
	require.ErrorIs(t, err, errTurnRouteMismatch)

	completed := errors.New("memoized close")
	done := make(chan struct{})
	close(done)
	s = &session{
		closeOperation:  &sessionCloseOperation{done: done, err: completed},
		closeContained:  true,
		closeCommitDone: true,
	}
	require.ErrorIs(t, s.Close(t.Context()), completed)

	done = make(chan struct{})
	s = &session{closeOperation: &sessionCloseOperation{done: done}}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	require.ErrorIs(t, s.Close(ctx), context.Canceled)

	s = &session{nativeEventDone: make(chan struct{})}
	require.ErrorIs(t, s.unsubscribeContainedThread(ctx, newSpyCodexClient(), "thread"), context.Canceled)
}
