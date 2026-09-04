package codexacp

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/coder/acp-go-sdk"
	"github.com/savid/acp-go-codex/internal/codex"
	"github.com/savid/acp-go-codex/internal/lifecycle"
	"github.com/stretchr/testify/require"
)

type noImageSteerClient struct{ *spyCodexClient }

func (c *noImageSteerClient) ModelList(context.Context) ([]codex.Model, error) {
	return []codex.Model{{ID: "text-only", Name: "Text only", InputModalities: []string{"text"}}}, nil
}

type steerRaceClient struct {
	*spyCodexClient
	steerEntered  chan codex.TurnSteerRequest
	steerRelease  chan struct{}
	cancelEntered chan [2]string
	cancelRelease chan struct{}
	steerOnce     sync.Once
}

func (c *steerRaceClient) SteerTurn(ctx context.Context, request codex.TurnSteerRequest) error {
	c.steerOnce.Do(func() { c.steerEntered <- request })
	select {
	case <-c.steerRelease:
		return c.spyCodexClient.SteerTurn(ctx, request)
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (c *steerRaceClient) CancelTurn(_ context.Context, threadID, turnID string) error {
	c.cancelEntered <- [2]string{threadID, turnID}
	<-c.cancelRelease

	return nil
}

func (c *steerRaceClient) ListBackgroundTerminals(context.Context, codex.BackgroundTerminalListRequest) (codex.BackgroundTerminalListResponse, error) {
	return codex.BackgroundTerminalListResponse{}, nil
}

func steerExtensionParams(t *testing.T, sessionID acp.SessionId, nonce, text string) json.RawMessage {
	t.Helper()

	request := TextPromptRequest(sessionID, nonce, text)
	payload, err := json.Marshal(request)
	require.NoError(t, err)

	return payload
}

func beginTestPromptTurn(t *testing.T, s *session, nonce, turnID string) func() {
	t.Helper()

	releaseTurn, err := s.acquireTurn(t.Context())
	require.NoError(t, err)
	_, err = s.beginPromptTurn(t.Context(), nonce)
	require.NoError(t, err)
	s.markTurnDispatched()
	if turnID != "" {
		s.stageTurnID(turnID)
		s.acceptTurnBinding()
	}

	return func() {
		s.finishTurn()
		releaseTurn()
	}
}

func TestSteerExtensionFailsClosedAtEveryRequestBoundary(t *testing.T) {
	agent := NewAgent()
	s := newSession(agent, "session", t.TempDir(), nil, codex.Thread{ID: "thread"}, newSpyCodexClient(), sessionMeta{}, nil)
	agent.sessions[s.id] = s
	cleanup := beginTestPromptTurn(t, s, "nonce", "native")
	defer cleanup()

	_, err := agent.HandleExtensionMethod(t.Context(), SteerTurnMethod, json.RawMessage(`{`))
	require.Error(t, err)

	missingRoute := acp.PromptRequest{SessionId: s.id, Prompt: []acp.ContentBlock{acp.TextBlock("text")}}
	payload, err := json.Marshal(missingRoute)
	require.NoError(t, err)
	_, err = agent.HandleExtensionMethod(t.Context(), SteerTurnMethod, payload)
	require.Error(t, err)

	_, err = agent.HandleExtensionMethod(t.Context(), SteerTurnMethod, steerExtensionParams(t, "missing", "nonce", "text"))
	require.Error(t, err)

	reserved := TextPromptRequest(s.id, "nonce", "text")
	reserved.Meta[lifecycle.MetaKey] = map[string]any{}
	payload, err = json.Marshal(reserved)
	require.NoError(t, err)
	_, err = agent.HandleExtensionMethod(t.Context(), SteerTurnMethod, payload)
	require.Error(t, err)

	invalid := TextPromptRequest(s.id, "nonce", "text")
	invalid.Prompt = nil
	payload, err = json.Marshal(invalid)
	require.NoError(t, err)
	_, err = agent.HandleExtensionMethod(t.Context(), SteerTurnMethod, payload)
	require.Error(t, err)
}

func TestExactRouteSteerConsumesNativeTurnSteerForPromptAndAutonomousTurns(t *testing.T) {
	for _, autonomous := range []bool{false, true} {
		name := "prompt"
		if autonomous {
			name = "autonomous"
		}
		t.Run(name, func(t *testing.T) {
			client := newSpyCodexClient()
			agent := NewAgent()
			agent.setAgentClient(newRecordingAgentClient())
			s := newSession(agent, "session", t.TempDir(), nil, codex.Thread{ID: "thread"}, client, sessionMeta{}, nil)
			agent.sessions[s.id] = s
			t.Cleanup(s.fenceSession)

			nonce := "prompt-nonce"
			turnID := "prompt-turn"
			if autonomous {
				agent.lifecycle = lifecycle.Negotiated{Version: 1}
				require.NoError(t, s.openLifecycleStream(t.Context(), agent.lifecycle))
				s.lifecycleMu.Lock()
				in, err := s.openAutonomousTurnLocked(t.Context(), "autonomous-turn")
				s.lifecycleMu.Unlock()
				require.NoError(t, err)
				nonce, turnID = in.turnNonce, in.nativeTurnID
			} else {
				cleanup := beginTestPromptTurn(t, s, nonce, turnID)
				defer cleanup()
			}

			_, err := agent.HandleExtensionMethod(t.Context(), SteerTurnMethod, steerExtensionParams(t, s.id, nonce, "more"))
			require.NoError(t, err)
			client.mu.Lock()
			require.Equal(t, codex.TurnSteerRequest{
				ThreadID: "thread", ExpectedTurnID: turnID,
				Input: []codex.UserInput{{jsonFieldType: jsonFieldText, jsonFieldText: "more"}},
			}, client.steer)
			client.mu.Unlock()
		})
	}
}

func TestExactRouteSteerQueuesBeforeDispatchAndFailsClosedWhenBindingIsCancelled(t *testing.T) {
	client := newSpyCodexClient()
	agent := NewAgent()
	s := newSession(agent, "session", t.TempDir(), nil, codex.Thread{ID: "thread"}, client, sessionMeta{}, nil)
	agent.sessions[s.id] = s
	cleanup := beginTestPromptTurn(t, s, "queued", "")
	defer cleanup()

	ctx, cancel := context.WithCancel(t.Context())
	entered := make(chan struct{})
	done := make(chan error, 1)
	go func() {
		close(entered)
		_, err := agent.HandleExtensionMethod(ctx, SteerTurnMethod, steerExtensionParams(t, s.id, "queued", "more"))
		done <- err
	}()
	<-entered
	cancel()
	require.ErrorIs(t, <-done, context.Canceled)
	client.mu.Lock()
	require.Empty(t, client.steer.ExpectedTurnID)
	client.mu.Unlock()

	second := make(chan error, 1)
	go func() {
		_, err := agent.HandleExtensionMethod(t.Context(), SteerTurnMethod, steerExtensionParams(t, s.id, "queued", "after-ack"))
		second <- err
	}()
	s.stageTurnID("queued-turn")
	s.acceptTurnBinding()
	require.NoError(t, <-second)
	client.mu.Lock()
	require.Equal(t, "queued-turn", client.steer.ExpectedTurnID)
	client.mu.Unlock()
}

func TestExactRouteSteerCancelAndRebindRacesHaveOneOwner(t *testing.T) {
	client := &steerRaceClient{
		spyCodexClient: newSpyCodexClient(),
		steerEntered:   make(chan codex.TurnSteerRequest, 1), steerRelease: make(chan struct{}),
		cancelEntered: make(chan [2]string, 1), cancelRelease: make(chan struct{}),
	}
	agent := NewAgent()
	s := newSession(agent, "session", t.TempDir(), nil, codex.Thread{ID: "thread"}, client, sessionMeta{}, nil)
	agent.sessions[s.id] = s
	agent.runtimeClient = client
	cleanup := beginTestPromptTurn(t, s, "race", "race-turn")
	defer cleanup()

	steerDone := make(chan error, 1)
	go func() {
		_, err := agent.HandleExtensionMethod(t.Context(), SteerTurnMethod, steerExtensionParams(t, s.id, "race", "more"))
		steerDone <- err
	}()
	require.Equal(t, "race-turn", (awaitTestSignal(t, client.steerEntered, "client.steerEntered")).ExpectedTurnID)

	rebindErr := make(chan error, 1)
	go func() {
		_, err := agent.rebindActiveStoredSession(t.Context(), ResumeSessionRequest(s.id, s.cwd), []SessionStoreEntry{
			SessionStoreEntry(`{"type":"session_meta","payload":{"id":"thread"}}`),
		}, sessionMeta{}, s)
		rebindErr <- err
	}()
	require.ErrorContains(t, <-rebindErr, valueBackpressure)

	cancelDone := make(chan error, 1)
	go func() { cancelDone <- agent.Cancel(t.Context(), CancelRequest(s.id, "race")) }()
	require.ErrorIs(t, <-steerDone, context.Canceled)
	require.Equal(t, [2]string{"thread", "race-turn"}, awaitTestSignal(t, client.cancelEntered, "client.cancelEntered"))
	close(client.cancelRelease)
	require.NoError(t, <-cancelDone)

	_, err := agent.HandleExtensionMethod(t.Context(), SteerTurnMethod, steerExtensionParams(t, s.id, "race", "late"))
	var requestErr *acp.RequestError
	require.True(t, errors.As(err, &requestErr))
	require.Equal(t, -32602, requestErr.Code)
}

func TestSteerBindingAndTargetRejectEveryStaleOwner(t *testing.T) {
	s := &session{}
	require.NoError(t, s.waitForSteerBinding(t.Context(), "none"))

	ready := make(chan struct{})
	s.turnNonce = "prompt"
	promptDone := make(chan struct{})
	s.turnDone = promptDone
	s.turnReady = ready
	cancelled, cancel := context.WithCancel(t.Context())
	cancel()
	require.ErrorIs(t, s.waitForSteerBinding(cancelled, "prompt"), context.Canceled)
	close(ready)
	require.NoError(t, s.waitForSteerBinding(t.Context(), "prompt"))

	client := newSpyCodexClient()
	s.client = client
	s.codexThreadID = "thread"
	s.turnAccepted = true
	s.turnID = "native"
	gotClient, threadID, turnID, err := s.exactSteerTarget("prompt")
	require.NoError(t, err)
	require.Same(t, client, gotClient)
	require.Equal(t, "thread", threadID)
	require.Equal(t, "native", turnID)

	close(promptDone)
	gotClient, threadID, turnID, err = s.exactSteerTarget("prompt")
	require.ErrorIs(t, err, errTurnRouteMismatch)
	require.Nil(t, gotClient)
	require.Empty(t, threadID)
	require.Empty(t, turnID)

	s.turnNonce = "different"
	s.turnDone = nil
	s.clientDead = true
	gotClient, threadID, turnID, err = s.exactSteerTarget("agent")
	require.ErrorIs(t, err, errTurnRouteMismatch)
	require.Nil(t, gotClient)
	require.Empty(t, threadID)
	require.Empty(t, turnID)

	s.clientDead = false
	s.agentIncarnation = &promptIncarnation{turnNonce: "agent", nativeTurnID: "agent-native"}
	gotClient, threadID, turnID, err = s.exactSteerTarget("agent")
	require.NoError(t, err)
	require.Same(t, client, gotClient)
	require.Equal(t, "thread", threadID)
	require.Equal(t, "agent-native", turnID)

	for _, mutate := range []func(*session){
		func(s *session) { s.agentIncarnation = nil },
		func(s *session) { s.agentIncarnation.turnNonce = "other" },
		func(s *session) { s.agentIncarnation.settled = true },
		func(s *session) { s.agentIncarnation.terminating = &turnContainment{} },
		func(s *session) { s.lifecycleClosing = true },
		func(s *session) { s.nativeEventRebinding = true },
		func(s *session) { s.agentIncarnation.nativeTurnID = "" },
	} {
		fixture := &session{
			client: client, codexThreadID: "thread",
			agentIncarnation: &promptIncarnation{turnNonce: "agent", nativeTurnID: "native"},
		}
		mutate(fixture)
		_, _, _, err = fixture.exactSteerTarget("agent")
		require.ErrorIs(t, err, errTurnRouteMismatch)
	}
}

func TestSteerInputAndDispatchRejectDeadRoutesAndClients(t *testing.T) {
	agent := NewAgent()
	s := &session{agent: agent}
	require.ErrorIs(t, s.steerTurn(t.Context(), "missing", []acp.ContentBlock{acp.TextBlock("more")}), errTurnRouteMismatch)

	_, release, err := s.prepareSteerInput(t.Context(), []acp.ContentBlock{acp.TextBlock("more")})
	require.Nil(t, release)
	require.ErrorIs(t, err, codex.ErrConnectionClosed)

	s.client = newSpyCodexClient()
	s.clientDead = true
	_, release, err = s.prepareSteerInput(t.Context(), []acp.ContentBlock{acp.TextBlock("more")})
	require.Nil(t, release)
	require.ErrorIs(t, err, codex.ErrConnectionClosed)

	cancelled, cancel := context.WithCancel(t.Context())
	cancel()
	s.clientDead = false
	s.codexThreadID = "thread"
	s.turnNonce = "cancelled"
	s.turnReady = make(chan struct{})
	s.turnDone = make(chan struct{})
	require.ErrorIs(t, s.steerTurn(cancelled, "cancelled", []acp.ContentBlock{acp.TextBlock("more")}), context.Canceled)
	_, release, err = s.prepareSteerInput(cancelled, []acp.ContentBlock{acp.TextBlock("more")})
	require.Nil(t, release)
	require.ErrorIs(t, err, context.Canceled)

	s.turnNonce = "waiting"
	s.turnReady = make(chan struct{})
	s.turnDone = make(chan struct{})
	waitCtx, waitCancel := context.WithTimeout(t.Context(), time.Millisecond)
	defer waitCancel()
	require.ErrorIs(t, s.steerTurn(waitCtx, "waiting", []acp.ContentBlock{acp.TextBlock("more")}), context.DeadlineExceeded)
}

func TestSteerInputRejectsInvalidUnsupportedAndUnstageableImages(t *testing.T) {
	agent := NewAgent()
	client := newSpyCodexClient()
	s := &session{agent: agent, client: client, model: "gpt-initial"}

	_, release, err := s.prepareSteerInput(t.Context(), []acp.ContentBlock{acp.ImageBlock("not-base64", mimeImagePNG)})
	require.Nil(t, release)
	require.Error(t, err)

	png := testdataFixture(t, "valid.png")
	image := acp.ImageBlock(base64.StdEncoding.EncodeToString(png), mimeImagePNG)
	s.client = &noImageSteerClient{spyCodexClient: newSpyCodexClient()}
	s.model = "text-only"
	_, release, err = s.prepareSteerInput(t.Context(), []acp.ContentBlock{image})
	require.Nil(t, release)
	require.ErrorContains(t, err, imageErrorUnsupportedByModel)

	s.client = client
	s.model = "gpt-initial"
	originalCreate := createPromptImageTempDir
	createPromptImageTempDir = func(string, string) (string, error) { return "", errors.New("scratch failed") }
	t.Cleanup(func() { createPromptImageTempDir = originalCreate })
	large := syntheticPNG(t, codexInlineImageEnvelopeSize+1)
	largeImage := acp.ImageBlock(base64.StdEncoding.EncodeToString(large), mimeImagePNG)
	_, release, err = s.prepareSteerInput(t.Context(), []acp.ContentBlock{largeImage})
	require.Nil(t, release)
	require.ErrorContains(t, err, "image preparation failed")

	createPromptImageTempDir = originalCreate
	_, release, err = s.prepareSteerInput(t.Context(), nil)
	require.Nil(t, release)
	require.Error(t, err)
}

func TestSteerDispatchRevalidatesBindingAndExactTarget(t *testing.T) {
	client := newSpyCodexClient()
	agent := NewAgent()
	s := &session{agent: agent, client: client, codexThreadID: "thread", turnNonce: "turn"}
	s.turnDone = make(chan struct{})
	s.turnReady = make(chan struct{})

	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)
	go func() { done <- s.steerTurn(ctx, "turn", []acp.ContentBlock{acp.TextBlock("more")}) }()
	require.Eventually(t, func() bool {
		s.mu.Lock()
		defer s.mu.Unlock()

		return s.interactions["turn-steer:turn"] != nil
	}, time.Second, time.Millisecond)
	cancel()
	require.ErrorIs(t, <-done, context.Canceled)

	s.turnDone = make(chan struct{})
	s.turnReady = make(chan struct{})
	close(s.turnReady)
	s.turnAccepted = false
	require.ErrorIs(t, s.steerTurn(t.Context(), "turn", []acp.ContentBlock{acp.TextBlock("more")}), errTurnRouteMismatch)
}
