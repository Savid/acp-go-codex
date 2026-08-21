package codexacp

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/coder/acp-go-sdk"
	"github.com/savid/acp-go-codex/internal/codex"
	"github.com/savid/acp-go-codex/internal/lifecycle"
	"github.com/stretchr/testify/require"
)

const lifecycleTestToolID = "exec"

func TestLifecycleDeliveryOwnerHasNoTotalDeadline(t *testing.T) {
	owner, cancel := newLifecycleDeliveryOwner()
	defer cancel()

	_, hasDeadline := owner.Deadline()
	require.False(t, hasDeadline, "a progressing delivery queue must not inherit the old settlement timeout")
}

func TestLifecycleDeliveryQueueBoundsAndUnavailableConnection(t *testing.T) {
	past, cancelPast := context.WithDeadline(t.Context(), time.Now().Add(-time.Second))
	defer cancelPast()
	require.Equal(t, time.Nanosecond, lifecycleDeliveryTimeout(past))

	s := &session{lifecycleDeliveryRun: true}
	s.lifecycleDeliveries = make([]lifecycleDelivery, sessionLifecycleDeliveryBuffer)
	_, err := s.enqueueLifecycleDeliveryLocked(t.Context(), acp.SessionNotification{})
	require.ErrorContains(t, err, "buffer is full")

	s = &session{}
	done := make(chan struct{})
	s.runLifecycleDeliveries(t.Context(), done)
	_, open := <-done
	require.False(t, open)

	result := make(chan error, 1)
	cancelledOwner, cancelOwner := context.WithCancel(t.Context())
	cancelOwner()
	s = &session{
		lifecycleDeliveryRun: true,
		lifecycleDeliveries:  []lifecycleDelivery{{result: result}},
	}
	done = make(chan struct{})
	s.runLifecycleDeliveries(cancelledOwner, done)
	require.ErrorIs(t, <-result, context.Canceled)
	_, open = <-done
	require.False(t, open)

	cancelCalled := false
	s = &session{lifecycleDeliveryCancel: func() { cancelCalled = true }}
	done = make(chan struct{})
	s.runLifecycleDeliveries(t.Context(), done)
	require.True(t, cancelCalled)
	_, open = <-done
	require.False(t, open)

	result = make(chan error, 1)
	s = &session{
		agent: NewAgent(), lifecycleDeliveryRun: true,
		lifecycleDeliveries: []lifecycleDelivery{{timeout: time.Second, result: result}},
	}
	done = make(chan struct{})
	s.runLifecycleDeliveries(t.Context(), done)
	require.ErrorContains(t, <-result, "connection is unavailable")
	_, open = <-done
	require.False(t, open)

	s = &session{lifecycleDeliveryDone: make(chan struct{})}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	require.ErrorIs(t, s.stopLifecycleDeliveries(ctx), context.Canceled)
}

func TestPromptAndAutonomousForegroundAreMutuallyExclusive(t *testing.T) {
	negotiated := lifecycle.Negotiated{Versions: []int{1}, ActivityKinds: []lifecycle.ActivityKind{}}
	newFixture := func(t *testing.T) *session {
		t.Helper()

		agent := NewAgent()
		agent.lifecycle = negotiated
		agent.setAgentClient(newRecordingAgentClient())
		s := &session{
			agent: agent, id: "session", codexThreadID: "thread",
			nativeEventOpened: true, nativeEventSource: true,
		}
		require.NoError(t, s.openLifecycleStream(t.Context(), negotiated))
		t.Cleanup(s.fenceSession)

		return s
	}
	foreign := codex.Event{
		Kind: codex.EventAgentMessageDelta, Scope: codex.EventScopeThread,
		ThreadID: "thread", TurnID: "autonomous", Text: "foreign",
	}

	t.Run("foreign turn arrives before prompt acknowledgement", func(t *testing.T) {
		s := newFixture(t)
		in, err := s.openIncarnation(t.Context(), negotiated)
		require.NoError(t, err)
		require.NoError(t, s.routeNativeEvent(foreign))

		s.lifecycleMu.Lock()
		require.Nil(t, s.agentIncarnation)
		require.Len(t, in.preBind, 1)
		s.lifecycleMu.Unlock()

		err = in.acceptNative(t.Context(), lifecycle.Submission{SubmissionID: "submission", ClientNonce: "nonce"}, "prompt")
		require.ErrorContains(t, err, "different native foreground turn")
		s.lifecycleMu.Lock()
		require.Nil(t, s.agentIncarnation)
		s.lifecycleMu.Unlock()
	})

	t.Run("foreign turn arrives after prompt acknowledgement", func(t *testing.T) {
		s := newFixture(t)
		in, err := s.openIncarnation(t.Context(), negotiated)
		require.NoError(t, err)
		require.NoError(t, in.acceptNative(
			t.Context(), lifecycle.Submission{SubmissionID: "submission", ClientNonce: "nonce"}, "prompt",
		))

		err = s.routeNativeEvent(foreign)
		require.ErrorContains(t, err, "prompt foreground turn was live")
		s.lifecycleMu.Lock()
		require.Nil(t, s.agentIncarnation)
		s.lifecycleMu.Unlock()
	})

	t.Run("prompt arrives after autonomous turn", func(t *testing.T) {
		s := newFixture(t)
		require.NoError(t, s.routeNativeEvent(foreign))
		s.lifecycleMu.Lock()
		require.NotNil(t, s.agentIncarnation)
		s.lifecycleMu.Unlock()

		_, err := s.openIncarnation(t.Context(), negotiated)
		require.ErrorContains(t, err, valueBackpressure)
	})
}

func TestPromptAcceptanceQueuesPreAckPrefixBeforeLifecycleDeliveryUnlock(t *testing.T) {
	negotiated := lifecycle.Negotiated{Versions: []int{1}, ActivityKinds: []lifecycle.ActivityKind{}}
	agent := NewAgent()
	agent.lifecycle = negotiated
	agent.setAgentClient(newRecordingAgentClient())
	s := &session{
		agent: agent, id: "session", codexThreadID: "thread",
		nativeEventOpened: true, nativeEventSource: true,
	}
	require.NoError(t, s.openLifecycleStream(t.Context(), negotiated))
	t.Cleanup(s.fenceSession)

	in, err := s.openIncarnation(t.Context(), negotiated)
	require.NoError(t, err)
	delta := codex.Event{
		Kind: codex.EventAgentMessageDelta, Scope: codex.EventScopeThread,
		ThreadID: "thread", TurnID: "native-turn", Text: "before acknowledgement",
	}
	require.NoError(t, s.routeNativeEvent(delta))

	blocked := &blockingLifecycleUpdateClient{
		recordingAgentClient: newRecordingAgentClient(),
		entered:              make(chan struct{}),
		release:              make(chan struct{}),
	}
	agent.setAgentClient(blocked)

	accepted := make(chan error, 1)
	go func() {
		accepted <- in.acceptNative(
			t.Context(),
			lifecycle.Submission{SubmissionID: "submission", ClientNonce: "nonce"},
			"native-turn",
		)
	}()
	<-blocked.entered

	terminal := codex.Event{
		Kind: codex.EventCompleted, Scope: codex.EventScopeThread,
		ThreadID: "thread", TurnID: "native-turn", StopReason: codex.StopReasonEndTurn,
	}
	require.NoError(t, s.routeNativeEvent(terminal))
	close(blocked.release)
	require.NoError(t, <-accepted)

	require.Equal(t, delta, <-in.events)
	require.Equal(t, terminal, <-in.events)
	_, open := <-in.events
	require.False(t, open)
}

type failingLifecycleClient struct {
	*recordingAgentClient
	err error
}

type nthFailLifecycleClient struct {
	*recordingAgentClient
	failAt int
	calls  int
}

type unregisteredActionClient struct{ agentClient }

type cancellingExtensionClient struct {
	*recordingAgentClient
	cancel context.CancelFunc
}

func (c *cancellingExtensionClient) NotifyExtension(context.Context, string, any) error {
	c.cancel()

	return nil
}

type cancellingUpdateClient struct {
	*recordingAgentClient
	cancel context.CancelFunc
}

func (c *cancellingUpdateClient) SessionUpdate(context.Context, acp.SessionNotification) error {
	c.cancel()

	return nil
}

type callbackUpdateClient struct {
	*recordingAgentClient
	callback func()
}

func (c *callbackUpdateClient) SessionUpdate(ctx context.Context, notification acp.SessionNotification) error {
	c.callback()

	return c.recordingAgentClient.SessionUpdate(ctx, notification)
}

type triggerReader struct {
	reader  io.Reader
	once    sync.Once
	trigger func()
}

func (r *triggerReader) Read(payload []byte) (int, error) {
	r.once.Do(r.trigger)

	return r.reader.Read(payload)
}

type nthFailSessionUpdateClient struct {
	*recordingAgentClient
	failAt int
	calls  int
}

type blockingLifecycleUpdateClient struct {
	*recordingAgentClient
	entered chan struct{}
	release chan struct{}
	once    sync.Once
}

func (c *blockingLifecycleUpdateClient) SessionUpdate(
	ctx context.Context,
	notification acp.SessionNotification,
) error {
	if _, lifecycleUpdate := notification.Meta[lifecycle.MetaKey]; lifecycleUpdate {
		c.once.Do(func() { close(c.entered) })
		select {
		case <-c.release:
		case <-ctx.Done():
			return ctx.Err()
		}
	}

	return c.recordingAgentClient.SessionUpdate(ctx, notification)
}

func (c *nthFailSessionUpdateClient) SessionUpdate(_ context.Context, notification acp.SessionNotification) error {
	c.calls++
	if c.calls == c.failAt {
		return errors.New("session update delivery failed")
	}

	return c.recordingAgentClient.SessionUpdate(context.Background(), notification)
}

type deterministicThreadFeedClient struct {
	*spyCodexClient
	events chan codex.Event
	once   sync.Once
}

type failingSubscribeClient struct {
	*spyCodexClient
	err error
}

func (c *failingSubscribeClient) SubscribeThread(context.Context, string) (codex.ThreadEventStream, error) {
	return codex.ThreadEventStream{}, c.err
}

type racingSubscribeClient struct {
	*spyCodexClient
	entered  chan struct{}
	release  chan struct{}
	released chan struct{}
}

func (c *racingSubscribeClient) SubscribeThread(context.Context, string) (codex.ThreadEventStream, error) {
	close(c.entered)
	<-c.release
	events := make(chan codex.Event)

	return codex.ThreadEventStream{Events: events, Release: func() {
		close(events)
		close(c.released)
	}}, nil
}

type failedAckThreadFeedClient struct {
	*deterministicThreadFeedClient
	malformed bool
	cancelled chan [2]string
}

type exactCancelRunClient struct {
	*runEventsClient
	muCancel      sync.Mutex
	cancels       [][2]string
	cancelStarted chan [2]string
	cancelRelease chan struct{}
}

func (c *exactCancelRunClient) CancelTurn(_ context.Context, threadID, turnID string) error {
	c.muCancel.Lock()
	c.cancels = append(c.cancels, [2]string{threadID, turnID})
	c.muCancel.Unlock()
	c.cancelStarted <- [2]string{threadID, turnID}
	<-c.cancelRelease

	return nil
}

func (c *exactCancelRunClient) cancellationTargets() [][2]string {
	c.muCancel.Lock()
	defer c.muCancel.Unlock()

	return append([][2]string(nil), c.cancels...)
}

type closeOrderThreadFeedClient struct {
	*spyCodexClient
	events  chan codex.Event
	once    sync.Once
	session *session
	order   chan error
}

func newCloseOrderThreadFeedClient() *closeOrderThreadFeedClient {
	return &closeOrderThreadFeedClient{
		spyCodexClient: newSpyCodexClient(), events: make(chan codex.Event), order: make(chan error, 1),
	}
}

func (c *closeOrderThreadFeedClient) SubscribeThread(context.Context, string) (codex.ThreadEventStream, error) {
	release := func() { c.once.Do(func() { close(c.events) }) }

	return codex.ThreadEventStream{Events: c.events, Release: release}, nil
}

func (c *closeOrderThreadFeedClient) UnsubscribeThread(context.Context, string) error {
	c.session.mu.Lock()
	closing := c.session.closing
	c.session.mu.Unlock()
	c.session.lifecycleMu.Lock()
	stopping := c.session.nativeEventStopping
	pumping := c.session.nativeEventPumping
	c.session.lifecycleMu.Unlock()
	if !closing || !stopping || pumping {
		c.order <- fmt.Errorf("unsubscribe order closing=%v stopping=%v pumping=%v", closing, stopping, pumping)
	} else {
		c.order <- nil
	}

	return nil
}

func newFailedAckThreadFeedClient(malformed bool) *failedAckThreadFeedClient {
	client := &failedAckThreadFeedClient{
		deterministicThreadFeedClient: &deterministicThreadFeedClient{
			spyCodexClient: newSpyCodexClient(), events: make(chan codex.Event),
		},
		malformed: malformed,
		cancelled: make(chan [2]string, 1),
	}

	return client
}

func (c *failedAckThreadFeedClient) RunTurn(_ context.Context, req codex.TurnStartRequest) (codex.Turn, error) {
	// The second unbuffered send cannot be received until the pump has routed
	// the first one into preBind, making the acknowledgement race deterministic.
	c.events <- codex.Event{
		Kind: codex.EventAgentMessageDelta, Scope: codex.EventScopeThread,
		ThreadID: req.ThreadID, TurnID: "pre-ack-turn", Text: "retained",
	}
	c.events <- codex.Event{
		Kind: codex.EventRaw, Scope: codex.EventScopeThread,
		ThreadID: req.ThreadID, TurnID: "pre-ack-turn",
	}
	if c.malformed {
		return codex.Turn{}, nil
	}

	return codex.Turn{}, errors.New("turn acknowledgement failed")
}

func (c *failedAckThreadFeedClient) CancelTurn(_ context.Context, threadID, turnID string) error {
	c.cancelled <- [2]string{threadID, turnID}

	return nil
}

func newDeterministicThreadFeedClient() *deterministicThreadFeedClient {
	return &deterministicThreadFeedClient{spyCodexClient: newSpyCodexClient(), events: make(chan codex.Event, 8)}
}

func (c *deterministicThreadFeedClient) SubscribeThread(ctx context.Context, _ string) (codex.ThreadEventStream, error) {
	release := func() { c.once.Do(func() { close(c.events) }) }
	go func() {
		<-ctx.Done()
		release()
	}()

	return codex.ThreadEventStream{Events: c.events, Release: release}, nil
}

func (c *deterministicThreadFeedClient) RunTurn(_ context.Context, req codex.TurnStartRequest) (codex.Turn, error) {
	c.events <- codex.Event{
		Kind: codex.EventAgentMessageDelta, Scope: codex.EventScopeThread,
		ThreadID: req.ThreadID, TurnID: "native-turn", ItemID: "message", Text: "native only",
	}
	c.events <- codex.Event{
		Kind: codex.EventCompleted, Scope: codex.EventScopeThread,
		ThreadID: req.ThreadID, TurnID: "native-turn", StopReason: codex.StopReasonEndTurn,
	}

	return codex.Turn{ID: "native-turn"}, nil
}

func (c *nthFailLifecycleClient) SessionUpdate(_ context.Context, notification acp.SessionNotification) error {
	if _, lifecycleUpdate := notification.Meta[lifecycle.MetaKey]; lifecycleUpdate {
		c.calls++
		if c.calls == c.failAt {
			return errors.New("stream failed")
		}
	}

	return c.recordingAgentClient.SessionUpdate(context.Background(), notification)
}

func TestLifecyclePermissionActionFailureBoundaries(t *testing.T) {
	for _, failAt := range []int{1, 3} {
		agent, s, _, turnCtx := newStrictPermissionSession(t)
		agent.setAgentClient(nil)
		in, err := s.openIncarnation(turnCtx, lifecycle.Negotiated{Versions: []int{1}, ActivityKinds: []lifecycle.ActivityKind{}})
		require.NoError(t, err)
		require.NoError(t, in.accept(turnCtx, lifecycle.Submission{SubmissionID: "s", ClientNonce: "n"}))
		emitNativePermissionToolEvent(t, s, turnCtx, codex.Event{
			Kind: codex.EventToolStarted,
			Tool: codex.ToolEvent{ID: lifecycleTestToolID, Kind: toolKindMcpToolCall, Raw: map[string]any{"server": "wagie", "tool": "execute"}},
		})
		conn := &nthFailLifecycleClient{recordingAgentClient: newRecordingAgentClient(), failAt: failAt}
		agent.setAgentClient(conn)
		actionCtx := withLifecycleActionTurn(turnCtx, in)
		_, _, err = s.requestPermissionForTool(actionCtx, conn, acp.RequestPermissionRequest{ToolCall: acp.ToolCallUpdate{
			ToolCallId: lifecycleTestToolID,
			RawInput:   map[string]any{"turnId": "native-permission-turn", "serverName": "wagie", "_meta": map[string]any{"tool_name": "execute"}},
		}}, permissionToolMCP)
		require.Error(t, err)
	}
}

func TestLifecycleMCPElicitationActionFailureBoundaries(t *testing.T) {
	for _, failAt := range []int{1, 3} {
		agent, s, _, turnCtx := newStrictPermissionSession(t)
		agent.setAgentClient(nil)
		in, err := s.openIncarnation(turnCtx, lifecycle.Negotiated{Versions: []int{1}, ActivityKinds: []lifecycle.ActivityKind{}})
		require.NoError(t, err)
		require.NoError(t, in.accept(turnCtx, lifecycle.Submission{SubmissionID: "s", ClientNonce: "n"}))
		params := map[string]any{"turnId": "native-permission-turn", "serverName": "wagie", "_meta": map[string]any{"tool_name": "execute"}}
		s.permissionTools.tools = map[acp.ToolCallId]*permissionToolRecord{
			lifecycleTestToolID: {id: lifecycleTestToolID, class: permissionToolMCP, fingerprint: permissionFingerprint(nil, params, permissionToolMCP)},
		}
		s.permissionTools.aliases = map[string]acp.ToolCallId{lifecycleTestToolID: lifecycleTestToolID}
		require.Equal(t, "native-permission-turn", codex.RequestTurnID(params))
		require.Equal(t, "native-permission-turn", s.turnID)
		require.NotNil(t, s.turnDone)
		_, valid := s.permissionTools.matchPermissionTool(lifecycleTestToolID, permissionToolMCP, permissionFingerprint(nil, params, permissionToolMCP))
		require.True(t, valid)
		conn := &nthFailLifecycleClient{recordingAgentClient: newRecordingAgentClient(), failAt: failAt}
		agent.setAgentClient(conn)
		_, associated, err := s.createElicitationForMCPTool(
			withLifecycleActionTurn(turnCtx, in), conn, acp.UnstableCreateElicitationRequest{}, lifecycleTestToolID, params,
		)
		require.Equal(t, failAt == 3, associated)
		require.Error(t, err)
	}
}

func TestLifecycleServerElicitationActionFailureBoundaries(t *testing.T) {
	for _, method := range []string{codex.RequestToolUserInput, codex.RequestMCPElicitation} {
		for _, failAt := range []int{1, 3} {
			agent, s, _, turnCtx := newStrictPermissionSession(t)
			agent.setAgentClient(nil)
			in, err := s.openIncarnation(turnCtx, lifecycle.Negotiated{Versions: []int{1}, ActivityKinds: []lifecycle.ActivityKind{}})
			require.NoError(t, err)
			require.NoError(t, in.accept(turnCtx, lifecycle.Submission{SubmissionID: "s", ClientNonce: "n"}))
			conn := &nthFailLifecycleClient{recordingAgentClient: newRecordingAgentClient(), failAt: failAt}
			agent.setAgentClient(conn)
			enableClientElicitation(agent, true, true)
			params := `{"threadId":"` + s.codexThreadID + `","turnId":"native-permission-turn","questions":[{"id":"name","header":"Name","question":"Your name?"}]}`
			if method == codex.RequestMCPElicitation {
				params = `{"threadId":"` + s.codexThreadID + `","turnId":"native-permission-turn","mode":"form","message":"Need input","requestedSchema":{"type":"object","properties":{}}}`
			}
			_, err = agent.handleCodexServerRequest(turnCtx, codex.ServerRequest{ID: json.RawMessage(`"request"`), Method: method, Params: json.RawMessage(params)})
			require.Error(t, err)
		}
	}
}

// blockingPermissionClient holds the inbound permission open until its context
// ends, which is exactly what a teardown does to a pending request: the answer
// is a context failure, never an outcome.
type blockingPermissionClient struct {
	*recordingAgentClient

	entered chan struct{}
}

func (c *blockingPermissionClient) RequestPermission(
	ctx context.Context,
	_ acp.RequestPermissionRequest,
) (acp.RequestPermissionResponse, error) {
	close(c.entered)
	<-ctx.Done()

	return acp.RequestPermissionResponse{}, ctx.Err()
}

func (c *blockingPermissionClient) RequestPermissionRegistered(
	ctx context.Context,
	request acp.RequestPermissionRequest,
	_ string,
	registered func() error,
) (acp.RequestPermissionResponse, error) {
	if err := registered(); err != nil {
		return acp.RequestPermissionResponse{}, err
	}

	return c.RequestPermission(ctx, request)
}

// SessionUpdate refuses a notification offered on a dead context, exactly as the
// pinned SDK's own send does. A terminal patch emitted on the context a cancel
// just ended is never written to the peer.
func (c *blockingPermissionClient) SessionUpdate(ctx context.Context, notification acp.SessionNotification) error {
	if err := ctx.Err(); err != nil {
		return acp.NewInternalError(map[string]any{jsonFieldError: err.Error()})
	}

	return c.recordingAgentClient.SessionUpdate(ctx, notification)
}

// resolvedActionStates reads every terminal action patch the host was actually
// sent, in order. A state the stream reduced but never delivered is not a
// terminal the host has: its sequence is spent either way.
func resolvedActionStates(conn *recordingAgentClient, actionID string) []string {
	states := make([]string, 0)

	for _, update := range conn.updates {
		envelope, carried := update.Meta[lifecycle.MetaKey].(map[string]any)
		if !carried {
			continue
		}

		event, _ := envelope["event"].(map[string]any)
		if event == nil || event["type"] != "action_update" {
			continue
		}

		action, _ := event["action"].(map[string]any)
		if action == nil || action["actionId"] != actionID {
			continue
		}

		if state, ok := action["state"].(string); ok && state != string(lifecycle.ActionPending) {
			states = append(states, state)
		}
	}

	return states
}

// TestValidatedCancelTerminalizesAPendingActionAsCancelled pins both halves of
// what a cancel owes a pending permission. Incarnation loss terminalizes
// `failed` and cancel terminalizes `cancelled` — the two paths never share a
// terminal state, and the teardown's own context cancellation is what the
// pending request answers with, so the state must be read from the cause rather
// than from the error. And the terminal patch must reach the host: emitting it
// on the context the cancel just ended spends the sequence and delivers nothing,
// leaving a gap where a terminal should be.
func TestValidatedCancelTerminalizesAPendingActionAsCancelled(t *testing.T) {
	agent, s, _, turnCtx := newStrictPermissionSession(t)

	// A cancel whose targeted sweep proves the thread contained leaves the
	// generation and every peer's stream alone, which is the ordinary path a
	// pending action resolves on.
	sweep := &threadScopedTerminalClient{
		spyCodexClient: newSpyCodexClient(),
		terminals:      map[string]map[string]struct{}{},
	}

	s.mu.Lock()
	s.client = sweep
	s.mu.Unlock()

	conn := &blockingPermissionClient{recordingAgentClient: newRecordingAgentClient(), entered: make(chan struct{})}
	agent.setAgentClient(conn)

	incarnation, err := s.openIncarnation(turnCtx, lifecycle.Negotiated{Versions: []int{1}, ActivityKinds: []lifecycle.ActivityKind{}})
	require.NoError(t, err)
	require.NoError(t, incarnation.accept(turnCtx, lifecycle.Submission{SubmissionID: "s", ClientNonce: "n"}))
	emitNativePermissionToolEvent(t, s, turnCtx, codex.Event{
		Kind: codex.EventToolStarted,
		Tool: codex.ToolEvent{
			ID:   lifecycleTestToolID,
			Kind: toolKindMcpToolCall,
			Raw:  map[string]any{"server": "wagie", "tool": "execute"},
		},
	})

	interactionCtx, finish := s.beginInteraction(turnCtx, "codex-permission")
	defer finish()

	var permissionErr error

	answered := make(chan struct{})

	go func() {
		defer close(answered)

		_, _, permissionErr = s.requestPermissionForTool(withLifecycleActionTurn(interactionCtx, incarnation), conn, acp.RequestPermissionRequest{
			ToolCall: acp.ToolCallUpdate{
				ToolCallId: lifecycleTestToolID,
				RawInput: map[string]any{
					"turnId": "native-permission-turn", "serverName": "wagie",
					"_meta": map[string]any{"tool_name": "execute"},
				},
			},
		}, permissionToolMCP)
	}()

	<-conn.entered

	require.NoError(t, agent.Cancel(context.Background(), CancelRequest(s.id, "permission-turn")))
	<-answered
	require.Error(t, permissionErr, "a cancelled permission answers with the context failure")

	actions := incarnation.stream.State().Actions
	require.Len(t, actions, 1)
	require.Equal(t, lifecycle.ActionCancelled, actions[0].State,
		"a cancel-caused teardown terminalizes cancelled, never failed")
	require.Equal(t, []string{string(lifecycle.ActionCancelled)},
		resolvedActionStates(conn.recordingAgentClient, actions[0].ActionID),
		"the terminal patch was delivered on a context the cancel could not end")
}

type failingBackgroundTerminalClient struct {
	*spyCodexClient
	err error
}

func (c *failingBackgroundTerminalClient) ListBackgroundTerminals(
	context.Context,
	codex.BackgroundTerminalListRequest,
) (codex.BackgroundTerminalListResponse, error) {
	return codex.BackgroundTerminalListResponse{}, c.err
}

func (c *failingLifecycleClient) SessionUpdate(context.Context, acp.SessionNotification) error {
	return c.err
}

func TestPromptIncarnationLifecycleAndActionSettlement(t *testing.T) {
	ctx := context.Background()
	agent := NewAgent()
	recorder := newRecordingAgentClient()
	agent.setAgentClient(recorder)
	s := &session{agent: agent, id: "session"}
	negotiated := lifecycle.Negotiated{Versions: []int{lifecycle.Version}, ActivityKinds: []lifecycle.ActivityKind{}}

	incarnation, err := s.openIncarnation(ctx, negotiated)
	require.NoError(t, err)
	require.Same(t, incarnation, s.liveIncarnation())
	require.Len(t, recorder.updates, 1)
	require.Empty(t, incarnation.provenQuiescenceForTest())

	// Before native acceptance, inbound requests are deliberately not announced.
	actionCtx := withLifecycleActionTurn(ctx, incarnation)
	early, correlation, err := s.beginAction(actionCtx, lifecycle.ActionPermission, true)
	require.NoError(t, err)
	require.NotNil(t, early)
	require.NotNil(t, correlation)
	require.NoError(t, early.resolve(ctx, lifecycle.ActionAccepted))
	require.Len(t, recorder.updates, 1)

	require.NoError(t, incarnation.accept(ctx, lifecycle.Submission{
		SubmissionID: "submission", ClientNonce: "nonce", RunID: "run",
	}))
	require.Len(t, recorder.updates, 3)

	blocking, blockingCorrelation, err := s.beginAction(actionCtx, lifecycle.ActionPermission, true)
	require.NoError(t, err)
	require.Equal(t, incarnation.stream.ID(), blockingCorrelation["streamId"])
	require.Len(t, recorder.updates, 3, "minting correlation does not publish an unregistered host request")
	require.NoError(t, blocking.register(ctx))
	require.Error(t, blocking.register(ctx), "one host request cannot register twice")
	require.Len(t, recorder.updates, 5)
	require.NoError(t, blocking.resolve(ctx, lifecycle.ActionAccepted))
	require.NoError(t, blocking.resolve(ctx, lifecycle.ActionDeclined)) // terminal finality
	require.Len(t, recorder.updates, 7)

	nonblocking, _, err := s.beginAction(actionCtx, lifecycle.ActionElicitation, false)
	require.NoError(t, err)
	require.NoError(t, nonblocking.register(ctx))
	require.NoError(t, nonblocking.resolve(ctx, lifecycle.ActionDeclined))
	require.Len(t, recorder.updates, 9)

	pending, _, err := s.beginAction(actionCtx, lifecycle.ActionPermission, true)
	require.NoError(t, err)
	require.NoError(t, pending.register(ctx))
	require.NoError(t, incarnation.settle(ctx, acp.StopReasonCancelled, lifecycle.OutcomeCancelled))
	require.NoError(t, pending.resolve(ctx, lifecycle.ActionAccepted))
	settledCount := len(recorder.updates)
	require.NoError(t, incarnation.settle(ctx, acp.StopReasonEndTurn, lifecycle.OutcomeSuccess))
	require.Equal(t, settledCount, len(recorder.updates), "final settlement must latch")

	s.clearIncarnation(incarnation)
	require.Nil(t, s.liveIncarnation())
	require.Error(t, incarnation.emit(ctx, lifecycle.SnapshotEvent("later", lifecycle.QuiescenceFact{})))
	s.clearIncarnation(nil)
	s.fenceSession()
}

func TestLifecycleActionOwnerNeverBorrowsCurrentNonce(t *testing.T) {
	agent := NewAgent()
	agent.lifecycle = lifecycle.Negotiated{Versions: []int{1}}
	agent.setAgentClient(newRecordingAgentClient())
	s := &session{agent: agent, id: "session"}
	in, err := s.openIncarnation(t.Context(), agent.lifecycle)
	require.NoError(t, err)
	in.turnNonce = "shared-nonce"
	s.incarnation = in

	action, _, err := s.beginAction(withTurnRoute(t.Context(), "shared-nonce"), lifecycle.ActionPermission, true)
	require.ErrorContains(t, err, "omitted its exact native turn")
	require.Nil(t, action)

	action, _, err = s.beginAction(withLifecycleActionTurn(t.Context(), in), lifecycle.ActionPermission, true)
	require.NoError(t, err)
	require.NotNil(t, action)
	s.fenceSession()
}

func TestPromptIncarnationResumesOnlyAfterLastForegroundBlocker(t *testing.T) {
	ctx := context.Background()
	agent := NewAgent()
	agent.setAgentClient(newRecordingAgentClient())
	s := &session{agent: agent, id: "session"}
	incarnation, err := s.openIncarnation(ctx, lifecycle.Negotiated{
		Versions: []int{lifecycle.Version}, ActivityKinds: []lifecycle.ActivityKind{},
	})
	require.NoError(t, err)
	require.NoError(t, incarnation.accept(ctx, lifecycle.Submission{SubmissionID: "submission", ClientNonce: "nonce"}))
	require.NoError(t, incarnation.announceAction(ctx, "permission", lifecycle.ActionPermission, true))
	require.NoError(t, incarnation.announceAction(ctx, "elicitation", lifecycle.ActionElicitation, true))

	require.NoError(t, incarnation.resolveAction(ctx, "permission", lifecycle.ActionAccepted))
	require.Equal(t, lifecycle.ForegroundRequiresAction, incarnation.stream.State().Foreground.State)

	require.NoError(t, incarnation.resolveAction(ctx, "elicitation", lifecycle.ActionAccepted))
	require.Equal(t, lifecycle.ForegroundRunning, incarnation.stream.State().Foreground.State)
}

// provenQuiescenceForTest makes the zero-value assertion readable without
// weakening the production API around process evidence.
func (in *promptIncarnation) provenQuiescenceForTest() lifecycle.QuiescenceFact {
	return in.session.provenQuiescence()
}

func TestSessionLifecycleRoutesAgentOriginBetweenPromptsAndContinues(t *testing.T) {
	ctx := context.Background()
	agent := NewAgent()
	recorder := newRecordingAgentClient()
	agent.setAgentClient(recorder)
	s := &session{agent: agent, id: "session", codexThreadID: "thread"}
	negotiated := lifecycle.Negotiated{Versions: []int{1}, ActivityKinds: []lifecycle.ActivityKind{}}

	first, err := s.openIncarnation(ctx, negotiated)
	require.NoError(t, err)
	require.NoError(t, first.acceptNative(ctx, lifecycle.Submission{SubmissionID: "first", ClientNonce: "nonce"}, "prompt-turn"))
	require.NoError(t, first.settle(ctx, acp.StopReasonEndTurn, lifecycle.OutcomeSuccess))
	s.clearIncarnation(first)
	streamID := first.stream.ID()

	require.NoError(t, s.routeNativeEvent(codex.Event{
		Kind: codex.EventAgentMessageDelta, Scope: codex.EventScopeThread,
		ThreadID: "thread", TurnID: "agent-turn", ItemID: "message", Text: "between prompts",
	}))
	require.NoError(t, s.routeNativeEvent(codex.Event{
		Kind: codex.EventCompleted, Scope: codex.EventScopeThread,
		ThreadID: "thread", TurnID: "agent-turn", StopReason: codex.StopReasonEndTurn,
	}))
	require.Nil(t, s.agentIncarnation)
	require.False(t, first.stream.Fenced())

	second, err := s.openIncarnation(ctx, negotiated)
	require.NoError(t, err)
	require.Equal(t, streamID, second.stream.ID(), "prompt continuation keeps the native thread incarnation")
	require.NoError(t, second.acceptNative(ctx, lifecycle.Submission{SubmissionID: "second", ClientNonce: "nonce"}, "next-prompt"))
	require.NoError(t, second.settle(ctx, acp.StopReasonEndTurn, lifecycle.OutcomeSuccess))
	s.clearIncarnation(second)

	var (
		snapshots int
		agentRun  bool
		agentIdle bool
		visible   bool
	)
	for _, update := range recorder.updates {
		if chunk := update.Update.AgentMessageChunk; chunk != nil {
			visible = true
		}
		envelope, _ := update.Meta[lifecycle.MetaKey].(map[string]any)
		event, _ := envelope["event"].(map[string]any)
		switch event["type"] {
		case "lifecycle_snapshot":
			snapshots++
		case "state_update":
			if event["cause"] == string(lifecycle.CauseActivity) && event["turnId"] == "agent-turn" {
				agentRun = agentRun || event["state"] == string(lifecycle.ForegroundRunning)
				agentIdle = agentIdle || event["state"] == string(lifecycle.ForegroundIdle)
			}
		}
	}
	require.Equal(t, 1, snapshots)
	require.True(t, agentRun)
	require.True(t, agentIdle)
	require.True(t, visible)

	s.fenceSession()
	require.True(t, first.stream.Fenced())
}

func TestPromptUsesTheNativeThreadFeedWithoutLifecycleNegotiation(t *testing.T) {
	client := newDeterministicThreadFeedClient()
	agent := NewAgent(withClientFactory(func(context.Context, codex.Options) (codex.Client, error) {
		return client, nil
	}))
	cleanupSessionNativePumps(t, agent)
	recorder := newRecordingAgentClient()
	agent.setAgentClient(recorder)
	created, err := agent.NewSession(context.Background(), NewSessionRequest(t.TempDir()))
	require.NoError(t, err)

	response, err := agent.Prompt(context.Background(), TextPromptRequest(created.SessionId, "nonce", "run"))
	require.NoError(t, err)
	require.Equal(t, acp.StopReasonEndTurn, response.StopReason)
	require.NotEmpty(t, recorder.updates)
	require.NotNil(t, recorder.updates[0].Update.AgentMessageChunk)
	for _, update := range recorder.updates {
		require.NotContains(t, update.Meta, lifecycle.MetaKey)
	}

	_, err = agent.CloseSession(context.Background(), acp.CloseSessionRequest{SessionId: created.SessionId})
	require.NoError(t, err)
}

func TestSessionLifecycleBetweenPromptFailureEOFAndBoundsFailClosed(t *testing.T) {
	ctx := context.Background()
	negotiated := lifecycle.Negotiated{Versions: []int{1}, ActivityKinds: []lifecycle.ActivityKind{}}

	t.Run("provider turn failure is terminal and continuation stays open", func(t *testing.T) {
		agent := NewAgent()
		agent.setAgentClient(newRecordingAgentClient())
		s := &session{agent: agent, id: "session", codexThreadID: "thread"}
		in, err := s.openIncarnation(ctx, negotiated)
		require.NoError(t, err)
		s.clearIncarnation(in)
		require.NoError(t, s.routeNativeEvent(codex.Event{
			Kind: codex.EventError, Scope: codex.EventScopeThread,
			ThreadID: "thread", TurnID: "failed-turn", Err: errors.New("provider failed"),
		}))
		require.Nil(t, s.agentIncarnation)
		require.False(t, in.stream.Fenced())
		require.NoError(t, s.routeNativeEvent(codex.Event{
			Kind: codex.EventCompleted, Scope: codex.EventScopeThread,
			ThreadID: "thread", TurnID: "continued-turn", StopReason: codex.StopReasonEndTurn,
		}))
		require.False(t, in.stream.Fenced())
	})

	t.Run("unexpected broker EOF fences the exact incarnation", func(t *testing.T) {
		agent := NewAgent()
		s := &session{agent: agent, id: "session", codexThreadID: "thread", client: newSpyCodexClient()}
		in, err := s.openIncarnation(ctx, negotiated)
		require.NoError(t, err)
		events := make(chan codex.Event)
		close(events)
		done := make(chan struct{})
		go s.runNativeEventPump(events, make(chan chan error), done)
		<-done
		require.ErrorIs(t, s.lifecycleFailure, codex.ErrConnectionClosed)
		require.True(t, in.stream.Fenced())
	})

	t.Run("pre-open retention is bounded", func(t *testing.T) {
		s := &session{agent: NewAgent(), codexThreadID: "thread"}
		for range sessionNativeEventBuffer {
			require.NoError(t, s.routeNativeEvent(codex.Event{
				Kind: codex.EventRaw, Scope: codex.EventScopeThread,
				ThreadID: "thread", TurnID: "turn",
			}))
		}
		require.ErrorIs(t, s.routeNativeEvent(codex.Event{
			Kind: codex.EventRaw, Scope: codex.EventScopeThread,
			ThreadID: "thread", TurnID: "turn",
		}), codex.ErrTurnEventOverflow)
	})

	t.Run("terminal retention exhaustion fences instead of evicting", func(t *testing.T) {
		agent := NewAgent()
		s := &session{agent: agent, id: "session", codexThreadID: "thread", client: newSpyCodexClient()}
		in, err := s.openIncarnation(ctx, negotiated)
		require.NoError(t, err)
		s.clearIncarnation(in)
		s.terminalNativeTurns = make(map[string]struct{}, terminalNativeTurnLimit)
		for index := range terminalNativeTurnLimit {
			s.terminalNativeTurns[fmt.Sprintf("old-%d", index)] = struct{}{}
		}
		events := make(chan codex.Event, 1)
		events <- codex.Event{
			Kind: codex.EventCompleted, Scope: codex.EventScopeThread,
			ThreadID: "thread", TurnID: "new", StopReason: codex.StopReasonEndTurn,
		}
		close(events)
		done := make(chan struct{})
		go s.runNativeEventPump(events, make(chan chan error), done)
		<-done
		require.ErrorContains(t, s.lifecycleFailure, "retention limit")
		require.True(t, in.stream.Fenced())
	})
}

func TestLifecycleInactiveFailureAndCorrelationHelpers(t *testing.T) {
	ctx := context.Background()
	agent := NewAgent()
	s := &session{agent: agent, id: "session"}

	incarnation, err := s.openIncarnation(ctx, lifecycle.Negotiated{})
	require.NoError(t, err)
	require.Nil(t, incarnation)
	action, correlation, err := s.beginAction(ctx, lifecycle.ActionPermission, true)
	require.NoError(t, err)
	require.Nil(t, action)
	require.Nil(t, correlation)
	require.NoError(t, (*liveAction)(nil).resolve(ctx, lifecycle.ActionFailed))
	require.NoError(t, (*promptIncarnation)(nil).emit(ctx, lifecycle.Event{}))
	require.NoError(t, (*promptIncarnation)(nil).accept(ctx, lifecycle.Submission{}))
	require.NoError(t, (*promptIncarnation)(nil).announceAction(ctx, "a", lifecycle.ActionPermission, true))
	require.NoError(t, (*promptIncarnation)(nil).resolveAction(ctx, "a", lifecycle.ActionFailed))
	require.NoError(t, (*promptIncarnation)(nil).settle(ctx, acp.StopReasonEndTurn, lifecycle.OutcomeSuccess))
	require.NoError(t, (*liveAction)(nil).register(ctx))

	inactive := &promptIncarnation{}
	action, correlation, err = s.beginAction(withLifecycleActionTurn(ctx, inactive), lifecycle.ActionPermission, true)
	require.NoError(t, err)
	require.Nil(t, action)
	require.Nil(t, correlation)

	failure := errors.New("delivery failed")
	agent.setAgentClient(&failingLifecycleClient{recordingAgentClient: newRecordingAgentClient(), err: failure})
	active := &promptIncarnation{
		session: s,
		stream: lifecycle.NewStream("stream", lifecycle.Negotiated{
			Versions: []int{lifecycle.Version}, ActivityKinds: []lifecycle.ActivityKind{},
		}),
		cycleID: "cycle", turnID: "turn",
	}
	s.lifecycleStream = active.stream
	require.ErrorIs(t, active.emit(ctx, lifecycle.SnapshotEvent("cycle", lifecycle.QuiescenceFact{})), failure)

	originalRand := sessionIDRandReader
	sessionIDRandReader = strings.NewReader("short")
	_, _, err = s.beginAction(withLifecycleActionTurn(ctx, active), lifecycle.ActionPermission, true)
	sessionIDRandReader = originalRand
	require.Error(t, err)

	unregistered := unregisteredActionClient{agentClient: newRecordingAgentClient()}
	live := &liveAction{incarnation: active, id: "action", kind: lifecycle.ActionPermission}
	_, err = requestPermissionWithAction(ctx, unregistered, acp.RequestPermissionRequest{}, live, nil)
	require.ErrorContains(t, err, "cannot prove permission")
	_, err = createElicitationWithAction(ctx, unregistered, acp.UnstableCreateElicitationRequest{}, elicitationScope{}, live, nil)
	require.ErrorContains(t, err, "cannot prove elicitation")
	hookCalled := false
	_, err = createElicitationWithAction(ctx, newRecordingAgentClient(), acp.UnstableCreateElicitationRequest{}, elicitationScope{}, nil, func() {
		hookCalled = true
	})
	require.NoError(t, err)
	require.True(t, hookCalled)

	original := map[string]any{"route": map[string]any{"token": "kept"}}
	stamped := stampActionCorrelation(original, map[string]any{"version": 1})
	require.Equal(t, original["route"], stamped["route"], "reserved routing must survive correlation")
	require.NotContains(t, original, lifecycle.MetaKey)
	require.Equal(t, original, stampActionCorrelation(original, nil))
	require.Contains(t, stampActionCorrelation(nil, map[string]any{"version": 1}), lifecycle.MetaKey)

	require.Equal(t, lifecycle.ActionFailed, permissionActionState(acp.RequestPermissionResponse{}, failure))
	require.Equal(t, lifecycle.ActionCancelled, permissionActionState(acp.RequestPermissionResponse{Outcome: acp.NewRequestPermissionOutcomeCancelled()}, nil))
	require.Equal(t, lifecycle.ActionAccepted, permissionActionState(acp.RequestPermissionResponse{Outcome: acp.NewRequestPermissionOutcomeSelected("yes")}, nil))
	require.Equal(t, lifecycle.ActionDeclined, permissionActionState(acp.RequestPermissionResponse{}, nil))
	require.Equal(t, lifecycle.ActionFailed, elicitationActionState(acp.UnstableCreateElicitationResponse{}, failure))
	require.Equal(t, lifecycle.ActionAccepted, elicitationActionState(acp.NewUnstableCreateElicitationResponseAccept(), nil))
	require.Equal(t, lifecycle.ActionCancelled, elicitationActionState(acp.NewUnstableCreateElicitationResponseCancel(), nil))
	require.Equal(t, lifecycle.ActionDeclined, elicitationActionState(acp.NewUnstableCreateElicitationResponseDecline(), nil))
}

func TestNativeEventAttachmentRejectsInvalidAndRacingOwners(t *testing.T) {
	s := &session{codexThreadID: "thread"}
	require.ErrorContains(t, s.attachNativeEventsFrom(nil), "requires a native client")

	s.lifecycleClosing = true
	require.ErrorContains(t, s.attachNativeEventsFrom(newSpyCodexClient()), "closing")
	s.lifecycleClosing = false
	s.nativeEventSource = true
	require.NoError(t, s.attachNativeEventsFrom(newSpyCodexClient()))
	s.nativeEventSource = false
	s.nativeEventAttaching = true
	require.ErrorContains(t, s.attachNativeEventsFrom(newSpyCodexClient()), "already in progress")
	s.nativeEventAttaching = false

	subscribeErr := errors.New("subscribe failed")
	require.ErrorIs(t, s.attachNativeEventsFrom(&failingSubscribeClient{spyCodexClient: newSpyCodexClient(), err: subscribeErr}), subscribeErr)
	require.False(t, s.nativeEventAttaching)

	racing := &racingSubscribeClient{
		spyCodexClient: newSpyCodexClient(), entered: make(chan struct{}),
		release: make(chan struct{}), released: make(chan struct{}),
	}
	result := make(chan error, 1)
	go func() { result <- s.attachNativeEventsFrom(racing) }()
	<-racing.entered
	s.lifecycleMu.Lock()
	s.lifecycleClosing = true
	s.lifecycleMu.Unlock()
	close(racing.release)
	require.ErrorContains(t, <-result, "was contained")
	<-racing.released
	require.False(t, s.nativeEventAttaching)
}

func TestSessionRegistrationPropagatesLifecycleBrokerAttachmentFailure(t *testing.T) {
	agent := NewAgent()
	agent.lifecycle = lifecycle.Negotiated{Versions: []int{lifecycle.Version}}
	failure := errors.New("subscribe failed")
	client := &failingSubscribeClient{spyCodexClient: newSpyCodexClient(), err: failure}
	candidate := &session{agent: agent, id: "session", codexThreadID: "thread", client: client}
	require.ErrorIs(t, agent.storeStartedSession(candidate), failure)
	require.ErrorIs(t, agent.storeRetainedRuntimeSession(candidate, &retainedRuntimeThread{}), failure)
}

func TestNativeCanaryRejectsAmbiguityOverflowAndStaleOwnership(t *testing.T) {
	s := &session{codexThreadID: "thread"}
	s.lifecycleFailure = errors.New("fenced")
	_, err := s.beginNativeCanary()
	require.ErrorContains(t, err, "fenced")
	s.lifecycleFailure = nil
	_, err = s.beginNativeCanary()
	require.ErrorContains(t, err, "live thread event broker")
	s.nativeEventSource = true
	s.lifecycleClosing = true
	_, err = s.beginNativeCanary()
	require.ErrorContains(t, err, "cannot overlap")
	s.lifecycleClosing = false

	canary, err := s.beginNativeCanary()
	require.NoError(t, err)
	_, err = s.beginNativeCanary()
	require.ErrorContains(t, err, "cannot overlap")
	require.ErrorContains(t, s.bindNativeCanary(&nativeCanary{}, "turn"), "no longer current")
	require.ErrorContains(t, s.bindNativeCanary(canary, ""), "omitted")

	canary.preBind = []codex.Event{{TurnID: "other"}}
	require.NoError(t, s.bindNativeCanary(canary, "turn"))
	require.Len(t, s.nativeRebindEvents, 1)
	s.endNativeCanary(&nativeCanary{})
	s.endNativeCanary(canary)
	require.True(t, canary.closed)
	require.False(t, s.enqueueCanaryEventLocked(canary, codex.Event{}))
	s.closeCanaryEventsLocked(nil)
	s.closeCanaryEventsLocked(canary)

	overflow := &nativeCanary{turnID: "turn", events: make(chan codex.Event, sessionNativeEventBuffer+1)}
	for range sessionNativeEventBuffer {
		overflow.events <- codex.Event{}
	}
	require.False(t, s.enqueueCanaryEventLocked(overflow, codex.Event{}))
	require.True(t, overflow.closed)

	terminal := &nativeCanary{turnID: "turn", events: make(chan codex.Event, 2)}
	require.True(t, s.enqueueCanaryEventLocked(terminal, codex.Event{Kind: codex.EventCompleted}))
	require.True(t, terminal.closed)

	s.nativeCanary = &nativeCanary{preBind: []codex.Event{{TurnID: "one"}, {TurnID: "two"}}}
	ambiguous := s.nativeCanary
	turnID, rejectErr := s.rejectNativeCanaryAck(ambiguous, errors.New("ack failed"))
	require.Empty(t, turnID)
	require.ErrorContains(t, rejectErr, "ambiguous native activity")
	require.Error(t, s.lifecycleFailure)

	s.lifecycleFailure = nil
	s.nativeCanary = &nativeCanary{preBind: []codex.Event{{TurnID: "one"}}}
	bound := s.nativeCanary
	turnID, rejectErr = s.rejectNativeCanaryAck(bound, errors.New("ack failed"))
	require.Equal(t, "one", turnID)
	require.ErrorContains(t, rejectErr, "after native activity")
	turnID, rejectErr = s.rejectNativeCanaryAck(&nativeCanary{}, errors.New("stale"))
	require.Empty(t, turnID)
	require.ErrorContains(t, rejectErr, "stale")
}

func TestNativeCanaryBindingFailsClosedOnRebindAndQueueOverflow(t *testing.T) {
	s := &session{codexThreadID: "thread", nativeEventSource: true}
	canary, err := s.beginNativeCanary()
	require.NoError(t, err)
	s.nativeRebindEvents = make([]codex.Event, sessionNativeEventBuffer)
	canary.preBind = []codex.Event{{TurnID: "other"}}
	require.ErrorIs(t, s.bindNativeCanary(canary, "turn"), codex.ErrTurnEventOverflow)

	s.nativeRebindEvents = nil
	s.nativeCanary = nil
	canary, err = s.beginNativeCanary()
	require.NoError(t, err)
	for range sessionNativeEventBuffer {
		canary.events <- codex.Event{}
	}
	canary.preBind = []codex.Event{{TurnID: "turn"}}
	require.ErrorIs(t, s.bindNativeCanary(canary, "turn"), codex.ErrTurnEventOverflow)
}

func TestNativeEventPumpDrainsBarriersAndClassifiesUnexpectedStops(t *testing.T) {
	t.Run("barrier drains and a contained stop stays clean", func(t *testing.T) {
		s := &session{agent: NewAgent(), codexThreadID: "thread", nativeEventStopping: true}
		events := make(chan codex.Event)
		barriers := make(chan chan error)
		done := make(chan struct{})
		go s.runNativeEventPump(events, barriers, done)
		result := make(chan error, 1)
		barriers <- result
		require.NoError(t, <-result)
		_, open := <-result
		require.False(t, open)
		close(events)
		<-done
		require.NoError(t, s.lifecycleFailure)
		require.False(t, s.nativeEventPumping)
	})

	t.Run("invalid routed event fences the source", func(t *testing.T) {
		s := &session{agent: NewAgent(), codexThreadID: "thread", nativeEventOpened: true}
		events := make(chan codex.Event, 1)
		events <- codex.Event{Scope: codex.EventScopeThread, ThreadID: "other", TurnID: "turn"}
		close(events)
		done := make(chan struct{})
		go s.runNativeEventPump(events, make(chan chan error), done)
		<-done
		require.ErrorContains(t, s.lifecycleFailure, "outside its exact thread")
		require.True(t, s.clientDead)
	})

	t.Run("unexpected EOF is suppressed by an existing failure or session close", func(t *testing.T) {
		for _, fixture := range []*session{
			{agent: NewAgent(), lifecycleFailure: errors.New("known")},
			{agent: NewAgent(), closing: true},
		} {
			events := make(chan codex.Event)
			close(events)
			done := make(chan struct{})
			go fixture.runNativeEventPump(events, make(chan chan error), done)
			<-done
			require.False(t, fixture.clientDead)
		}
	})

	t.Run("panic is fenced", func(t *testing.T) {
		s := &session{agent: NewAgent()}
		result := make(chan error)
		close(result)
		barriers := make(chan chan error, 1)
		barriers <- result
		done := make(chan struct{})
		go s.runNativeEventPump(make(chan codex.Event), barriers, done)
		<-done
		require.ErrorContains(t, s.lifecycleFailure, "event pump panicked")
	})

	t.Run("barrier observes source close", func(t *testing.T) {
		observed := false
		for attempt := 0; attempt < 100 && !observed; attempt++ {
			s := &session{agent: NewAgent(), nativeEventStopping: true}
			events := make(chan codex.Event)
			close(events)
			result := make(chan error, 1)
			barriers := make(chan chan error, 1)
			barriers <- result
			done := make(chan struct{})
			go s.runNativeEventPump(events, barriers, done)
			select {
			case err := <-result:
				require.NoError(t, err)
				observed = true
			case <-done:
			}
			<-done
		}
		require.True(t, observed)
	})

	t.Run("barrier reports routed failure", func(t *testing.T) {
		observed := false
		for attempt := 0; attempt < 100 && !observed; attempt++ {
			s := &session{agent: NewAgent(), codexThreadID: "thread", nativeEventOpened: true}
			events := make(chan codex.Event, 1)
			events <- codex.Event{Scope: codex.EventScopeThread, ThreadID: "wrong", TurnID: "turn"}
			result := make(chan error, 1)
			barriers := make(chan chan error, 1)
			barriers <- result
			done := make(chan struct{})
			go s.runNativeEventPump(events, barriers, done)
			select {
			case err := <-result:
				if err != nil {
					observed = true
				}
			case <-done:
			}
			close(events)
			<-done
		}
		require.True(t, observed)
	})
}

func TestNativeEventDrainAndStopRespectCallerBounds(t *testing.T) {
	s := &session{}
	require.NoError(t, s.drainNativeEvents(t.Context()))

	s.nativeEventSource = true
	s.nativeEventBarrier = make(chan chan error)
	cancelled, cancel := context.WithCancel(t.Context())
	cancel()
	require.ErrorIs(t, s.drainNativeEvents(cancelled), context.Canceled)

	s.nativeEventBarrier = make(chan chan error, 1)
	waiting, cancelWaiting := context.WithCancel(t.Context())
	drainDone := make(chan error, 1)
	go func() { drainDone <- s.drainNativeEvents(waiting) }()
	<-s.nativeEventBarrier
	cancelWaiting()
	require.ErrorIs(t, <-drainDone, context.Canceled)

	blockedDone := make(chan struct{})
	s.nativeEventDone = blockedDone
	s.nativeEventCancel = func() {}
	s.nativeEventRelease = func() {}
	stopCtx, stopCancel := context.WithCancel(t.Context())
	stopCancel()
	require.ErrorIs(t, s.stopNativeEventsContext(stopCtx), context.Canceled)
	require.True(t, s.nativeEventStopping)

	close(blockedDone)
	require.NoError(t, s.stopNativeEventsContext(t.Context()))
	require.Nil(t, s.nativeEventDone)
	require.NoError(t, s.stopNativeEventsContext(t.Context()))
}

func TestNativeEventRebindAdmissionAndReplayAreExact(t *testing.T) {
	active := &session{incarnation: &promptIncarnation{}}
	require.ErrorContains(t, active.prepareNativeEventRebind(), "native lifecycle is active")

	negotiated := lifecycle.Negotiated{Versions: []int{1}}
	agent := NewAgent()
	agent.lifecycle = negotiated
	agent.setAgentClient(newRecordingAgentClient())
	s := &session{agent: agent, id: "session", codexThreadID: "thread"}
	require.NoError(t, s.openLifecycleStream(t.Context(), negotiated))
	old := s.lifecycleStream
	s.nativeRebindEvents = []codex.Event{{
		Kind: codex.EventRaw, Scope: codex.EventScopeThread, ThreadID: "thread", TurnID: "turn",
	}}
	require.NoError(t, s.prepareNativeEventRebind())
	require.True(t, old.Fenced())
	require.Nil(t, s.lifecycleStream)
	require.Len(t, s.preOpenEvents, 1)
	require.False(t, s.nativeEventRebinding)

	cancelled, cancel := context.WithCancel(t.Context())
	cancel()
	require.ErrorIs(t, s.beginActiveNativeRebind(cancelled), context.Canceled)

	s.establishment = &establishmentObligation{}
	s.nativeEventRebinding = true
	require.NoError(t, s.beginActiveNativeRebind(t.Context()))
	s.establishment = nil
	s.nativeEventRebinding = false
	s.lifecycleFailure = errors.New("fenced")
	require.ErrorContains(t, s.beginActiveNativeRebind(t.Context()), "native lifecycle is active")
	s.lifecycleFailure = nil
	s.nativeEventOpened = true
	s.preOpenEvents = nil
	require.NoError(t, s.beginActiveNativeRebind(t.Context()))
	require.True(t, s.nativeEventRebinding)

	s.nativeRebindEvents = []codex.Event{{Scope: codex.EventScopeThread, ThreadID: "wrong", TurnID: "turn"}}
	require.ErrorContains(t, s.finishActiveNativeRebind(t.Context()), "outside its exact thread")
	require.Error(t, s.lifecycleFailure)

	s.lifecycleFailure = nil
	s.nativeEventRebinding = false
	require.NoError(t, s.finishActiveNativeRebind(t.Context()))
	require.NoError(t, s.completeActiveNativeRebind(t.Context()))
}

func TestLifecycleEstablishmentAndStreamOpenFailClosedAtEveryBoundary(t *testing.T) {
	negotiated := lifecycle.Negotiated{Versions: []int{lifecycle.Version}, ActivityKinds: []lifecycle.ActivityKind{}}
	newLifecycleSession := func() *session {
		agent := NewAgent()
		agent.lifecycle = negotiated
		agent.setAgentClient(newRecordingAgentClient())

		return &session{agent: agent, id: "session", codexThreadID: "thread"}
	}
	newObligation := func(t *testing.T, id string) *establishmentObligation {
		t.Helper()
		obligation, err := newEstablishmentHooks(NewAgent().log).reserve(id)
		require.NoError(t, err)

		return obligation
	}

	s := newLifecycleSession()
	require.NoError(t, s.armLifecycleEstablishment(nil))
	owned := newObligation(t, "owned")
	require.NoError(t, owned.bind(&session{}))
	require.ErrorContains(t, s.armLifecycleEstablishment(owned), "changed owner")

	s = newLifecycleSession()
	s.lifecycleClosing = true
	require.ErrorContains(t, s.armLifecycleEstablishment(newObligation(t, "closing")), "raced session close")
	s = newLifecycleSession()
	s.establishment = newObligation(t, "existing")
	require.ErrorContains(t, s.armLifecycleEstablishment(newObligation(t, "second")), "already outstanding")
	s = newLifecycleSession()
	s.establishmentErr = errors.New("prior establishment failure")
	require.ErrorContains(t, s.armLifecycleEstablishment(newObligation(t, "prior")), "prior establishment failure")

	s = newLifecycleSession()
	abandoned := newObligation(t, "abandoned")
	require.NoError(t, s.armLifecycleEstablishment(abandoned))
	require.True(t, s.lifecycleEstablishmentPending())
	s.abandonLifecycleEstablishment()
	require.NoError(t, abandoned.wait(t.Context()))
	require.False(t, s.lifecycleEstablishmentPending())

	s = newLifecycleSession()
	closing := newObligation(t, "complete-closing")
	require.NoError(t, s.armLifecycleEstablishment(closing))
	s.lifecycleClosing = true
	require.ErrorIs(t, s.completeLifecycleEstablishment(t.Context(), closing, nil), errEstablishmentCancelled)
	require.ErrorIs(t, s.establishmentErr, errEstablishmentCancelled)

	s = newLifecycleSession()
	s.lifecycleFailure = errors.New("stream fenced")
	require.ErrorContains(t, s.openLifecycleStream(t.Context(), negotiated), "stream fenced")
	s = newLifecycleSession()
	s.lifecycleClosing = true
	require.ErrorIs(t, s.openLifecycleStream(t.Context(), negotiated), errEstablishmentCancelled)

	originalRand := sessionIDRandReader
	t.Cleanup(func() { sessionIDRandReader = originalRand })
	s = newLifecycleSession()
	sessionIDRandReader = strings.NewReader("short")
	require.Error(t, s.openLifecycleStream(t.Context(), negotiated))
	s = newLifecycleSession()
	sessionIDRandReader = strings.NewReader(strings.Repeat("x", 16))
	require.Error(t, s.openLifecycleStream(t.Context(), negotiated))
	sessionIDRandReader = originalRand

	s = newLifecycleSession()
	s.preOpenEvents = []codex.Event{{Scope: codex.EventScopeThread, ThreadID: "wrong", TurnID: "turn"}}
	require.ErrorContains(t, s.openLifecycleStream(t.Context(), negotiated), "outside its exact thread")
	require.True(t, s.lifecycleStream.Fenced())

	s = newLifecycleSession()
	s.establishmentErr = errors.New("establishment failed")
	_, err := s.openIncarnation(t.Context(), negotiated)
	require.ErrorContains(t, err, "establishment failed")
	s = newLifecycleSession()
	s.lifecycleFailure = errors.New("stream failed")
	_, err = s.openIncarnation(t.Context(), negotiated)
	require.ErrorContains(t, err, "stream failed")

	s = newLifecycleSession()
	require.NoError(t, s.openLifecycleStream(t.Context(), negotiated))
	sessionIDRandReader = strings.NewReader("short")
	_, err = s.openIncarnation(t.Context(), negotiated)
	require.Error(t, err)
	sessionIDRandReader = strings.NewReader(strings.Repeat("x", 16))
	_, err = s.openIncarnation(t.Context(), negotiated)
	require.Error(t, err)
	sessionIDRandReader = originalRand

	s = newLifecycleSession()
	require.NoError(t, s.openLifecycleStream(t.Context(), negotiated))
	sessionIDRandReader = &triggerReader{
		reader: strings.NewReader(strings.Repeat("x", 32)),
		trigger: func() {
			s.lifecycleMu.Lock()
			s.lifecycleFailure = errors.New("late failure")
			s.lifecycleMu.Unlock()
		},
	}
	_, err = s.openIncarnation(t.Context(), negotiated)
	require.ErrorContains(t, err, "late failure")
	sessionIDRandReader = originalRand
	s = newLifecycleSession()
	require.NoError(t, s.openLifecycleStream(t.Context(), negotiated))
	s.incarnation = &promptIncarnation{}
	_, err = s.openIncarnation(t.Context(), negotiated)
	require.ErrorContains(t, err, valueBackpressure)

	s = &session{}
	require.Nil(t, s.nativePromptEvents(nil))
	s.nativeEventSource = true
	require.Nil(t, s.nativePromptEvents(nil))
	in := &promptIncarnation{events: make(chan codex.Event)}
	require.Equal(t, (<-chan codex.Event)(in.events), s.nativePromptEvents(in))

	s = &session{agent: NewAgent()}
	in = &promptIncarnation{session: s, accepted: true, nativeTurnID: "overflow", events: make(chan codex.Event, 1)}
	s.incarnation = in
	s.terminalNativeTurns = make(map[string]struct{}, terminalNativeTurnLimit)
	for index := range terminalNativeTurnLimit {
		s.terminalNativeTurns[fmt.Sprintf("old-%d", index)] = struct{}{}
	}
	s.clearIncarnation(in)
	require.ErrorContains(t, s.lifecycleFailure, "retention limit")
	require.True(t, s.clientDead)
}

func TestNativeEventRoutingRejectsEveryAmbiguousOwnershipShape(t *testing.T) {
	threadEvent := func(turn string) codex.Event {
		return codex.Event{Kind: codex.EventRaw, Scope: codex.EventScopeThread, ThreadID: "thread", TurnID: turn}
	}

	s := &session{agent: NewAgent(), codexThreadID: "thread", lifecycleFailure: errors.New("fenced")}
	require.ErrorContains(t, s.routeNativeEvent(threadEvent("turn")), "fenced")
	s.lifecycleFailure = nil
	transportErr := errors.New("transport lost")
	require.ErrorIs(t, s.routeNativeEvent(codex.Event{Scope: codex.EventScopeTransportLost, Err: transportErr}), transportErr)
	require.ErrorIs(t, s.routeNativeEvent(codex.Event{Scope: codex.EventScopeTransportLost}), codex.ErrConnectionClosed)
	require.ErrorContains(t, s.routeNativeEvent(codex.Event{Scope: codex.EventScopeThread, ThreadID: "other"}), "outside its exact thread")

	s.nativeCanary = &nativeCanary{events: make(chan codex.Event, sessionNativeEventBuffer+1)}
	require.ErrorContains(t, s.routeNativeEvent(threadEvent("")), "omitted its native turn identity")
	s.nativeCanary.preBind = make([]codex.Event, sessionNativeEventBuffer)
	require.ErrorIs(t, s.routeNativeEvent(threadEvent("turn")), codex.ErrTurnEventOverflow)
	s.nativeCanary.preBind = nil
	require.NoError(t, s.routeNativeEvent(threadEvent("turn")))
	require.Len(t, s.nativeCanary.preBind, 1)

	s.nativeCanary.turnID = "turn"
	s.nativeCanary.preBind = nil
	require.ErrorContains(t, s.routeNativeEvent(threadEvent("other")), "concurrent native turn")
	s.nativeEventRebinding = true
	require.NoError(t, s.routeNativeEvent(threadEvent("other")))
	s.nativeRebindEvents = make([]codex.Event, sessionNativeEventBuffer)
	require.ErrorIs(t, s.routeNativeEvent(threadEvent("other")), codex.ErrTurnEventOverflow)

	s.nativeRebindEvents = nil
	s.nativeEventRebinding = false
	for range sessionNativeEventBuffer {
		s.nativeCanary.events <- codex.Event{}
	}
	require.ErrorIs(t, s.routeNativeEvent(threadEvent("turn")), codex.ErrTurnEventOverflow)

	s.nativeCanary = nil
	s.nativeEventRebinding = true
	s.nativeRebindEvents = make([]codex.Event, sessionNativeEventBuffer)
	require.ErrorIs(t, s.routeNativeEvent(threadEvent("turn")), codex.ErrTurnEventOverflow)
	s.nativeRebindEvents = nil
	require.NoError(t, s.routeNativeEvent(threadEvent("turn")))

	s.nativeEventRebinding = false
	s.nativeEventOpened = true
	require.NoError(t, s.routeNativeEvent(codex.Event{
		Kind: codex.EventAccountUpdated, Scope: codex.EventScopeThread, ThreadID: "thread",
		Account: codex.Account{PlanType: "pro"},
	}))
	require.NoError(t, s.routeNativeEvent(threadEvent("")))
	require.ErrorContains(t, s.routeNativeEvent(codex.Event{Kind: codex.EventAgentMessageDelta, Scope: codex.EventScopeThread, ThreadID: "thread"}), "omitted its native turn identity")

	in := &promptIncarnation{session: s, events: make(chan codex.Event, sessionNativeEventBuffer+1)}
	s.incarnation = in
	in.preBind = make([]codex.Event, sessionNativeEventBuffer)
	require.ErrorIs(t, s.routeNativeEvent(threadEvent("turn")), codex.ErrTurnEventOverflow)
	in.preBind = nil
	require.NoError(t, s.routeNativeEvent(threadEvent("turn")))
	require.Len(t, in.preBind, 1)

	in.nativeTurnID = "turn"
	for range sessionNativeEventBuffer {
		in.events <- codex.Event{}
	}
	require.ErrorIs(t, s.routeNativeEvent(threadEvent("turn")), codex.ErrTurnEventOverflow)
	require.True(t, in.eventsClosed)
	require.False(t, s.enqueuePromptEventLocked(in, threadEvent("turn")))
	s.closeCycleEventsLocked(nil)
	s.closeCycleEventsLocked(in)

	s.incarnation = &promptIncarnation{nativeTurnID: "prompt"}
	s.lifecycleClosing = true
	require.NoError(t, s.routeNativeEvent(threadEvent("foreign")))
}

func TestBufferedPromptAcceptanceRequiresOneExactNativeIdentity(t *testing.T) {
	turnID, events, identities, err := (*promptIncarnation)(nil).acceptBufferedNative(t.Context(), lifecycle.Submission{})
	require.NoError(t, err)
	require.Empty(t, turnID)
	require.Nil(t, events)
	require.Nil(t, identities)

	newIncarnation := func() (*session, *promptIncarnation) {
		s := &session{agent: NewAgent()}
		in := &promptIncarnation{session: s, events: make(chan codex.Event, sessionNativeEventBuffer+1)}
		s.incarnation = in

		return s, in
	}

	s, in := newIncarnation()
	s.incarnation = nil
	turnID, events, identities, err = in.acceptBufferedNative(t.Context(), lifecycle.Submission{})
	require.ErrorContains(t, err, "no longer current")
	require.Empty(t, turnID)
	require.Nil(t, events)
	require.Nil(t, identities)

	_, in = newIncarnation()
	turnID, events, identities, err = in.acceptBufferedNative(t.Context(), lifecycle.Submission{})
	require.NoError(t, err)
	require.Empty(t, turnID)
	require.Nil(t, events)
	require.Nil(t, identities)

	s, in = newIncarnation()
	in.preBind = []codex.Event{{}}
	_, _, identities, err = in.acceptBufferedNative(t.Context(), lifecycle.Submission{})
	require.ErrorContains(t, err, "without a native turn identity")
	require.Empty(t, identities)
	require.Error(t, s.lifecycleFailure)

	_, in = newIncarnation()
	in.preBind = []codex.Event{{TurnID: "one"}, {TurnID: "one"}, {TurnID: "two"}}
	turnID, events, identities, err = in.acceptBufferedNative(t.Context(), lifecycle.Submission{})
	require.ErrorContains(t, err, "concurrent native turns")
	require.Empty(t, turnID)
	require.Nil(t, events)
	require.Equal(t, []string{"one", "two"}, identities)

	_, in = newIncarnation()
	in.preBind = []codex.Event{{TurnID: "one"}}
	turnID, events, identities, err = in.acceptBufferedNative(t.Context(), lifecycle.Submission{})
	require.NoError(t, err)
	require.Equal(t, "one", turnID)
	require.Len(t, events, 1)
	require.Equal(t, []string{"one"}, identities)
	require.True(t, in.accepted)
}

func TestLifecycleTurnWaitObservesExactOwnerAndCancellation(t *testing.T) {
	accepted := &promptIncarnation{accepted: true, nativeTurnID: "native"}
	s := &session{incarnation: accepted}
	got, err := s.waitForLifecycleTurn(t.Context(), "native")
	require.NoError(t, err)
	require.Same(t, accepted, got)

	_, err = s.waitForLifecycleTurn(t.Context(), "other")
	require.ErrorContains(t, err, "concurrent native turn")

	agentTurn := &promptIncarnation{nativeTurnID: "agent"}
	s.incarnation = nil
	s.agentIncarnation = agentTurn
	got, err = s.waitForLifecycleTurn(t.Context(), "agent")
	require.NoError(t, err)
	require.Same(t, agentTurn, got)

	s.agentIncarnation = nil
	s.lifecycleFailure = errors.New("fenced")
	_, err = s.waitForLifecycleTurn(t.Context(), "native")
	require.ErrorContains(t, err, "fenced")

	s.lifecycleFailure = nil
	cancelled, cancel := context.WithCancel(t.Context())
	cancel()
	_, err = s.waitForLifecycleTurn(cancelled, "native")
	require.ErrorIs(t, err, context.Canceled)

	s.lifecycleChanged = make(chan struct{})
	result := make(chan *promptIncarnation, 1)
	errs := make(chan error, 1)
	go func() {
		owner, waitErr := s.waitForLifecycleTurn(t.Context(), "later")
		result <- owner
		errs <- waitErr
	}()
	s.lifecycleMu.Lock()
	later := &promptIncarnation{accepted: true, nativeTurnID: "later"}
	s.incarnation = later
	s.signalLifecycleChangedLocked()
	s.lifecycleMu.Unlock()
	require.Same(t, later, <-result)
	require.NoError(t, <-errs)
}

func TestLifecycleTurnClaimFailsClosedAcrossEveryOwnershipBoundary(t *testing.T) {
	newClaimSession := func(t *testing.T) *session {
		t.Helper()
		agent := NewAgent()
		agent.setAgentClient(newRecordingAgentClient())
		s := &session{agent: agent, id: "session", codexThreadID: "thread", nativeEventOpened: true}
		require.NoError(t, s.openLifecycleStream(t.Context(), lifecycle.Negotiated{Versions: []int{1}}))

		return s
	}

	s := newClaimSession(t)
	s.lifecycleClosing = true
	_, claimed, err := s.claimLifecycleTurn(t.Context(), "native")
	require.True(t, claimed)
	require.ErrorIs(t, err, context.Canceled)

	s = newClaimSession(t)
	s.nativeEventRebinding = true
	cancelled, cancel := context.WithCancel(t.Context())
	cancel()
	_, claimed, err = s.claimLifecycleTurn(cancelled, "native")
	require.True(t, claimed)
	require.ErrorIs(t, err, context.Canceled)

	s = &session{}
	owner, claimed, err := s.claimLifecycleTurn(t.Context(), "native")
	require.NoError(t, err)
	require.False(t, claimed)
	require.Nil(t, owner)

	s = newClaimSession(t)
	_, claimed, err = s.claimLifecycleTurn(t.Context(), "")
	require.True(t, claimed)
	require.ErrorContains(t, err, "omitted its native turn identity")

	s = newClaimSession(t)
	s.lifecycleFailure = errors.New("fenced")
	_, claimed, err = s.claimLifecycleTurn(t.Context(), "native")
	require.True(t, claimed)
	require.ErrorContains(t, err, "fenced")

	s = newClaimSession(t)
	prompt := &promptIncarnation{accepted: true, nativeTurnID: "native"}
	s.incarnation = prompt
	owner, claimed, err = s.claimLifecycleTurn(t.Context(), "native")
	require.NoError(t, err)
	require.True(t, claimed)
	require.Same(t, prompt, owner)
	_, _, err = s.claimLifecycleTurn(t.Context(), "other")
	require.ErrorContains(t, err, "concurrent native turn")

	s = newClaimSession(t)
	agentTurn := &promptIncarnation{nativeTurnID: "agent"}
	s.agentIncarnation = agentTurn
	owner, claimed, err = s.claimLifecycleTurn(t.Context(), "agent")
	require.NoError(t, err)
	require.True(t, claimed)
	require.Same(t, agentTurn, owner)
	_, _, err = s.claimLifecycleTurn(t.Context(), "other")
	require.ErrorContains(t, err, "concurrent agent-origin turn")

	s = newClaimSession(t)
	s.terminalNativeTurns = map[string]struct{}{"terminal": {}}
	_, _, err = s.claimLifecycleTurn(t.Context(), "terminal")
	require.ErrorContains(t, err, "terminal native turn")

	s = newClaimSession(t)
	owner, claimed, err = s.claimLifecycleTurn(t.Context(), "new")
	require.NoError(t, err)
	require.True(t, claimed)
	require.NotNil(t, owner)
	require.Same(t, owner, s.agentIncarnation)
}

func TestTerminalNativeTurnRetentionIsIdempotentAndBounded(t *testing.T) {
	s := &session{}
	require.NoError(t, s.rememberTerminalNativeTurnLocked(""))
	require.NoError(t, s.rememberTerminalNativeTurnLocked("turn"))
	require.Contains(t, s.terminalNativeTurns, "turn")
	require.NoError(t, s.rememberTerminalNativeTurnLocked("turn"))

	s.terminalNativeTurns = make(map[string]struct{}, terminalNativeTurnLimit)
	for index := range terminalNativeTurnLimit {
		s.terminalNativeTurns[fmt.Sprintf("turn-%d", index)] = struct{}{}
	}
	require.ErrorContains(t, s.rememberTerminalNativeTurnLocked("overflow"), "retention limit")
}

func TestAutonomousTurnAdmissionRejectsClosedPromptAndRandomFailure(t *testing.T) {
	for _, s := range []*session{{lifecycleClosing: true}, {nativeEventRebinding: true}} {
		_, err := s.openAutonomousTurnLocked(t.Context(), "native")
		require.ErrorContains(t, err, "admission is closed")
	}
	_, err := (&session{incarnation: &promptIncarnation{}}).openAutonomousTurnLocked(t.Context(), "native")
	require.ErrorContains(t, err, "cannot overlap")

	original := sessionIDRandReader
	t.Cleanup(func() { sessionIDRandReader = original })
	sessionIDRandReader = strings.NewReader("short")
	_, err = (&session{}).openAutonomousTurnLocked(t.Context(), "native")
	require.Error(t, err)
}

func TestAutonomousTurnSettlementRejectsChangedAndTerminalOwners(t *testing.T) {
	boundary := &turnContainment{done: make(chan struct{}), started: true}
	in := &promptIncarnation{nativeTurnID: "native"}
	s := &session{agent: NewAgent()}
	err := s.completeAutonomousSettlement(t.Context(), in, boundary, errors.New("prior"), lifecycle.ActionFailed, "", lifecycle.OutcomeFailed)
	require.ErrorContains(t, err, "turn changed during settlement")
	require.ErrorContains(t, err, "prior")
	<-boundary.done

	require.NoError(t, s.settleAutonomousTurnLocked(t.Context(), nil, lifecycle.ActionFailed, "", lifecycle.OutcomeFailed))
	in.settled = true
	require.NoError(t, s.settleAutonomousTurnLocked(t.Context(), in, lifecycle.ActionFailed, "", lifecycle.OutcomeFailed))

	in = &promptIncarnation{session: s, nativeTurnID: "overflow"}
	s.agentIncarnation = in
	s.terminalNativeTurns = make(map[string]struct{}, terminalNativeTurnLimit)
	for index := range terminalNativeTurnLimit {
		s.terminalNativeTurns[fmt.Sprintf("old-%d", index)] = struct{}{}
	}
	err = s.settleAutonomousTurnLocked(t.Context(), in, lifecycle.ActionFailed, "", lifecycle.OutcomeFailed)
	require.ErrorContains(t, err, "retention limit")
}

func TestAutonomousRoutingRejectsTerminalConcurrentAndTerminatingTurns(t *testing.T) {
	newAutonomousSession := func(t *testing.T) *session {
		t.Helper()
		agent := NewAgent()
		agent.setAgentClient(newRecordingAgentClient())
		s := &session{agent: agent, id: "session", codexThreadID: "thread", nativeEventOpened: true}
		require.NoError(t, s.openLifecycleStream(t.Context(), lifecycle.Negotiated{Versions: []int{1}}))

		return s
	}
	event := func(turn string) codex.Event {
		return codex.Event{Kind: codex.EventRaw, Scope: codex.EventScopeThread, ThreadID: "thread", TurnID: turn}
	}

	s := newAutonomousSession(t)
	s.terminalNativeTurns = map[string]struct{}{"terminal": {}}
	require.ErrorContains(t, s.routeNativeEvent(event("terminal")), "terminal native turn")

	s = newAutonomousSession(t)
	s.agentIncarnation = &promptIncarnation{nativeTurnID: "one"}
	require.ErrorContains(t, s.routeNativeEvent(event("two")), "concurrent agent-origin")

	s = newAutonomousSession(t)
	s.agentIncarnation = &promptIncarnation{nativeTurnID: "turn", terminating: &turnContainment{}}
	require.NoError(t, s.routeNativeEvent(event("turn")))

	s = newAutonomousSession(t)
	errEvent := event("failed")
	errEvent.Kind = codex.EventError
	errEvent.Err = errors.New("native failed")
	require.NoError(t, s.routeNativeEvent(errEvent))
	require.Nil(t, s.agentIncarnation)
	require.Contains(t, s.terminalNativeTurns, "failed")

	s = newAutonomousSession(t)
	invalidStop := event("invalid-stop")
	invalidStop.Kind = codex.EventCompleted
	invalidStop.StopReason = "unknown"
	require.NoError(t, s.routeNativeEvent(invalidStop))
	require.Nil(t, s.agentIncarnation)
	require.Contains(t, s.terminalNativeTurns, "invalid-stop")
}

func TestPromptLifecycleAcceptanceActionAndSettlementFailureBoundaries(t *testing.T) {
	negotiated := lifecycle.Negotiated{Versions: []int{lifecycle.Version}, ActivityKinds: []lifecycle.ActivityKind{}}
	newActive := func(conn agentClient) (*session, *promptIncarnation) {
		agent := NewAgent()
		agent.lifecycle = negotiated
		agent.setAgentClient(conn)
		s := &session{agent: agent, id: "session"}
		stream := lifecycle.NewStream("stream", negotiated)
		_, err := stream.Emit(lifecycle.SnapshotEvent("cycle", lifecycle.QuiescenceFact{}))
		require.NoError(t, err)
		s.lifecycleStream = stream
		in := &promptIncarnation{
			session: s, stream: stream, cycleID: "cycle", turnID: "turn",
			events: make(chan codex.Event, sessionNativeEventBuffer+1),
		}
		s.incarnation = in

		return s, in
	}

	s, in := newActive(newRecordingAgentClient())
	require.NoError(t, s.latchLifecycleFailureLocked(nil))
	require.NoError(t, (*promptIncarnation)(nil).acceptNative(t.Context(), lifecycle.Submission{}, "native"))
	s.incarnation = nil
	require.ErrorContains(t, in.acceptNative(t.Context(), lifecycle.Submission{}, "native"), "no longer current")

	s, in = newActive(newRecordingAgentClient())
	s.lifecycleFailure = errors.New("fenced")
	require.ErrorContains(t, in.acceptNative(t.Context(), lifecycle.Submission{}, "native"), "fenced")

	_, in = newActive(newRecordingAgentClient())
	for range sessionNativeEventBuffer {
		in.events <- codex.Event{}
	}
	in.preBind = []codex.Event{{TurnID: "native"}}
	require.ErrorIs(t, in.acceptNative(t.Context(), lifecycle.Submission{}, "native"), codex.ErrTurnEventOverflow)

	for _, failAt := range []int{1, 2} {
		recorder := newRecordingAgentClient()
		_, in = newActive(&nthFailLifecycleClient{recordingAgentClient: recorder, failAt: failAt})
		in.preBind = []codex.Event{{TurnID: "native"}}
		turnID, events, identities, err := in.acceptBufferedNative(t.Context(), lifecycle.Submission{SubmissionID: "submission", ClientNonce: "nonce"})
		require.ErrorContains(t, err, "stream failed")
		require.Equal(t, "native", turnID)
		require.Len(t, events, 1)
		require.Equal(t, []string{"native"}, identities)
	}

	_, in = newActive(newRecordingAgentClient())
	require.ErrorContains(t, in.announceAction(t.Context(), "action", lifecycle.ActionPermission, true), "no live owning turn")

	_, in = newActive(newRecordingAgentClient())
	require.NoError(t, in.settle(t.Context(), acp.StopReasonEndTurn, lifecycle.OutcomeSuccess))
	require.True(t, in.settled)

	_, in = newActive(newRecordingAgentClient())
	in.accepted = true
	in.autonomous = true
	_, err := in.stream.Emit(lifecycle.TransitionEventWithCause(
		lifecycle.ForegroundRunning, in.cycleID, in.turnID, lifecycle.CauseActivity,
	))
	require.NoError(t, err)
	require.NoError(t, in.settle(t.Context(), acp.StopReasonEndTurn, lifecycle.OutcomeSuccess))

	s, in = newActive(newRecordingAgentClient())
	s.lifecycleDeliveryStop = true
	require.ErrorContains(t, in.emit(t.Context(), lifecycle.TransitionEventWithCause(
		lifecycle.ForegroundRunning, in.cycleID, in.turnID, lifecycle.CauseActivity,
	)), "delivery is closed")
}

func TestAutonomousEventHandlingAndShutdownFailClosedAtEveryBoundary(t *testing.T) {
	state := func(s *session) *promptEventState {
		var text strings.Builder

		return &promptEventState{
			snapshot: s.snapshot(), agentDeltaItems: map[string]struct{}{}, reasoningDeltaItems: map[string]struct{}{},
			agentText: &text, toolContents: make(map[acp.ToolCallId][]acp.ToolCallContent), imageTools: newImageToolState(),
		}
	}

	s := &session{agent: NewAgent()}
	cancelled, cancel := context.WithCancel(t.Context())
	cancel()
	require.ErrorIs(t, s.handleAutonomousEvent(cancelled, codex.Event{}, state(s)), context.Canceled)

	s = &session{agent: NewAgent()}
	require.NoError(t, s.handleAutonomousEvent(t.Context(), codex.Event{
		Kind: codex.EventAccountUpdated, Account: codex.Account{PlanType: "pro"},
	}, state(s)))
	require.Equal(t, "pro", s.accountMetaSnapshot()["planType"])

	agent := NewAgent()
	agent.setAgentClient(&extensionErrorClient{recordingAgentClient: newRecordingAgentClient()})
	s = &session{agent: agent, id: "session", rawMessages: rawMessageConfig{enabled: true}}
	require.Error(t, s.handleAutonomousEvent(t.Context(), codex.Event{RawParams: json.RawMessage(`{"value":1}`)}, state(s)))

	ctx, cancel := context.WithCancel(t.Context())
	agent = NewAgent()
	agent.setAgentClient(&cancellingExtensionClient{recordingAgentClient: newRecordingAgentClient(), cancel: cancel})
	s = &session{agent: agent, id: "session", rawMessages: rawMessageConfig{enabled: true}}
	require.ErrorIs(t, s.handleAutonomousEvent(ctx, codex.Event{RawParams: json.RawMessage(`{"value":1}`)}, state(s)), context.Canceled)

	ctx, cancel = context.WithCancel(t.Context())
	agent = NewAgent()
	agent.setAgentClient(&cancellingUpdateClient{recordingAgentClient: newRecordingAgentClient(), cancel: cancel})
	s = &session{agent: agent, id: "session"}
	require.ErrorIs(t, s.handleAutonomousEvent(ctx, codex.Event{Kind: codex.EventAgentMessageDelta, Text: "text"}, state(s)), context.Canceled)

	agent = NewAgent()
	agent.setAgentClient(newRecordingAgentClient())
	s = &session{agent: agent, id: "session", nativeEventOpened: true, nativeEventRebinding: true}
	s.lifecycleMu.Lock()
	err := s.routeAutonomousEventLocked(t.Context(), codex.Event{TurnID: "native"})
	s.lifecycleMu.Unlock()
	require.ErrorContains(t, err, "admission is closed")

	agent = NewAgent()
	agent.setAgentClient(&errorAgentClient{recordingAgentClient: newRecordingAgentClient(), updateErr: errors.New("update failed")})
	s = &session{agent: agent, id: "session", codexThreadID: "thread", nativeEventOpened: true, client: newSpyCodexClient()}
	err = s.routeNativeEvent(codex.Event{
		Kind: codex.EventAgentMessageDelta, Scope: codex.EventScopeThread, ThreadID: "thread", TurnID: "native", Text: "text",
	})
	require.ErrorContains(t, err, "update failed")

	s = &session{agent: NewAgent()}
	s.failNativeIncarnation(nil)
	require.ErrorIs(t, s.lifecycleFailure, codex.ErrConnectionClosed)
	s = &session{agent: NewAgent(), agentIncarnation: &promptIncarnation{terminating: &turnContainment{}}}
	s.failNativeIncarnation(errors.New("failed"))
	require.True(t, s.clientDead)

	s = &session{agentIncarnation: &promptIncarnation{turnNonce: "current"}}
	contained, err := s.shutdownAgentTurnForNonce(t.Context(), true, "stale")
	require.True(t, contained)
	require.ErrorIs(t, err, errTurnRouteMismatch)

	done := make(chan struct{})
	boundary := &turnContainment{done: done, err: errors.New("settled")}
	s = &session{agentIncarnation: &promptIncarnation{terminating: boundary}}
	close(done)
	contained, err = s.shutdownAgentTurn(t.Context(), true)
	require.True(t, contained)
	require.ErrorContains(t, err, "settled")

	s = &session{agentIncarnation: &promptIncarnation{terminating: &turnContainment{done: make(chan struct{})}}}
	ctx, cancel = context.WithCancel(t.Context())
	cancel()
	contained, err = s.shutdownAgentTurn(ctx, true)
	require.True(t, contained)
	require.ErrorIs(t, err, context.Canceled)

	s = &session{agent: NewAgent()}
	in := &promptIncarnation{session: s, nativeTurnID: "native", turnNonce: "nonce"}
	s.agentIncarnation = in
	contained, err = s.shutdownAgentTurn(t.Context(), true)
	require.True(t, contained)
	require.ErrorContains(t, err, "no exact native cancellation target")
}

func TestLifecycleRebindSettlementAdmissionAndCloseRemainBounded(t *testing.T) {
	negotiated := lifecycle.Negotiated{Versions: []int{lifecycle.Version}, ActivityKinds: []lifecycle.ActivityKind{}}

	active := &session{incarnation: &promptIncarnation{}}
	require.ErrorContains(t, active.rebindNativeEvents(newSpyCodexClient()), "native lifecycle is active")

	stalled := &session{agent: NewAgent(), nativeEventDone: make(chan struct{})}
	started := time.Now()
	require.Error(t, stalled.prepareNativeEventRebind())
	require.Less(t, time.Since(started), closeTimeout+time.Second)
	require.Error(t, stalled.lifecycleFailure)
	require.False(t, stalled.nativeEventRebinding)

	locked := &session{nativeEventRebinding: true}
	locked.lifecycleRouteMu.Lock()
	finishCtx, cancelFinish := context.WithTimeout(t.Context(), 10*time.Millisecond)
	defer cancelFinish()
	started = time.Now()
	require.Error(t, locked.finishActiveNativeRebind(finishCtx))
	require.Less(t, time.Since(started), time.Second)
	locked.lifecycleRouteMu.Unlock()

	agent := NewAgent()
	recorder := newRecordingAgentClient()
	agent.setAgentClient(recorder)
	s := &session{agent: agent, id: "session"}
	stream := lifecycle.NewStream("stream", negotiated)
	_, err := stream.Emit(lifecycle.SnapshotEvent("cycle", lifecycle.QuiescenceFact{}))
	require.NoError(t, err)
	_, err = stream.Emit(lifecycle.TransitionEventWithCause(
		lifecycle.ForegroundRunning, "cycle", "turn", lifecycle.CauseActivity,
	))
	require.NoError(t, err)
	_, err = stream.Emit(lifecycle.ActionEvent(lifecycle.PendingAction(
		"action", lifecycle.ActionPermission, lifecycle.Owner{Type: lifecycle.OwnerTurn, ID: "turn"}, true,
	)))
	require.NoError(t, err)
	s.lifecycleStream = stream
	in := &promptIncarnation{
		session: s, stream: stream, cycleID: "cycle", turnID: "turn", accepted: true,
		events: make(chan codex.Event, 1),
	}
	s.incarnation = in
	agent.setAgentClient(&errorAgentClient{recordingAgentClient: recorder, updateErr: errors.New("resolution failed")})
	require.ErrorContains(t, in.settle(t.Context(), acp.StopReasonEndTurn, lifecycle.OutcomeSuccess), "resolution failed")

	agent = NewAgent()
	agent.setAgentClient(newRecordingAgentClient())
	s = &session{agent: agent, id: "session", codexThreadID: "thread", nativeEventOpened: true}
	require.NoError(t, s.openLifecycleStream(t.Context(), negotiated))
	require.NoError(t, s.routeNativeEvent(codex.Event{
		Kind: codex.EventCompleted, Scope: codex.EventScopeThread, ThreadID: "thread", TurnID: "error-stop",
		StopReason: codex.StopReasonError,
	}))
	require.Contains(t, s.terminalNativeTurns, "error-stop")

	agent = NewAgent()
	s = &session{agent: agent, id: "session"}
	stream = lifecycle.NewStream("stream", negotiated)
	_, err = stream.Emit(lifecycle.SnapshotEvent("cycle", lifecycle.QuiescenceFact{}))
	require.NoError(t, err)
	s.lifecycleStream = stream
	agent.setAgentClient(&errorAgentClient{recordingAgentClient: newRecordingAgentClient(), updateErr: errors.New("running failed")})
	s.lifecycleMu.Lock()
	_, err = s.openAutonomousTurnLocked(t.Context(), "native")
	s.lifecycleMu.Unlock()
	require.ErrorContains(t, err, "running failed")
	require.Nil(t, s.agentIncarnation)

	agent = NewAgent()
	s = &session{agent: agent, id: "session"}
	stream = lifecycle.NewStream("stream", negotiated)
	_, err = stream.Emit(lifecycle.SnapshotEvent("cycle", lifecycle.QuiescenceFact{}))
	require.NoError(t, err)
	s.lifecycleStream = stream
	mutating := &callbackUpdateClient{recordingAgentClient: newRecordingAgentClient()}
	mutating.callback = func() {
		s.lifecycleMu.Lock()
		s.lifecycleClosing = true
		s.lifecycleMu.Unlock()
	}
	agent.setAgentClient(mutating)
	s.lifecycleMu.Lock()
	_, err = s.openAutonomousTurnLocked(t.Context(), "native")
	s.lifecycleMu.Unlock()
	require.ErrorIs(t, err, context.Canceled)

	agent = NewAgent()
	agent.setAgentClient(newRecordingAgentClient())
	s = &session{agent: agent, id: "session", codexThreadID: "thread", nativeEventOpened: true}
	require.NoError(t, s.openLifecycleStream(t.Context(), negotiated))
	prompt := &promptIncarnation{session: s}
	s.incarnation = prompt
	claimCtx, cancelClaim := context.WithTimeout(t.Context(), 10*time.Millisecond)
	defer cancelClaim()
	owner, claimed, err := s.claimLifecycleTurn(claimCtx, "native")
	require.Nil(t, owner)
	require.True(t, claimed)
	require.ErrorIs(t, err, context.DeadlineExceeded)

	hooks := newEstablishmentHooks(NewAgent().log)
	obligation, err := hooks.reserve("blocked")
	require.NoError(t, err)
	obligation.once.Do(func() {})
	s = &session{establishment: obligation}
	ctx, cancel := context.WithDeadline(t.Context(), time.Now().Add(-time.Second))
	cancel()
	require.ErrorIs(t, s.beginLifecycleClose(ctx), context.DeadlineExceeded)

	s = &session{}
	s.lifecycleRouteMu.Lock()
	ctx, cancel = context.WithDeadline(t.Context(), time.Now().Add(-time.Second))
	cancel()
	require.ErrorIs(t, s.beginLifecycleClose(ctx), context.DeadlineExceeded)
	s.lifecycleRouteMu.Unlock()

	fenced := &session{
		agent: NewAgent(), lifecycleStream: lifecycle.NewStream("stream", negotiated),
		incarnation:      &promptIncarnation{events: make(chan codex.Event)},
		agentIncarnation: &promptIncarnation{},
	}
	fenced.lifecycleRouteMu.Lock()
	started = time.Now()
	fenced.fenceSession()
	require.Less(t, time.Since(started), closeTimeout+time.Second)
	fenced.lifecycleRouteMu.Unlock()
	require.True(t, fenced.lifecycleStream.Fenced())
	require.Nil(t, fenced.incarnation)
	require.Nil(t, fenced.agentIncarnation)
}

// The advertisement is the whole reachability condition for the version-1
// stream, so it is pinned as the exact bytes an initialize response carries and
// then followed through to the envelopes one real prompt delivers.
func TestLifecycleAdvertisementAnswersOfferAndOpensForegroundStream(t *testing.T) {
	ctx := context.Background()
	agent := NewAgent(withClientFactory(func(context.Context, codex.Options) (codex.Client, error) {
		return newSpyCodexClient(), nil
	}))
	cleanupSessionNativePumps(t, agent)
	recorder := newRecordingAgentClient()
	agent.setAgentClient(recorder)

	initialized, err := agent.Initialize(ctx, acp.InitializeRequest{
		Meta: map[string]any{lifecycle.MetaKey: map[string]any{"versions": []any{1.0}}},
	})
	require.NoError(t, err)
	require.JSONEq(
		t,
		`{"acp-go.dev/lifecycle":{"versions":[1],"updatesOutsidePrompt":true,`+
			`"authoritativeQuiescence":false,"activityKinds":[]}}`,
		requireJSON(t, initialized.Meta),
	)

	// The negotiated answer lives on the response's own `_meta` and nowhere else.
	require.NotContains(t, initialized.AgentCapabilities.Meta, lifecycle.MetaKey)

	created, err := agent.NewSession(ctx, NewSessionRequest("/tmp/project"))
	require.NoError(t, err)

	promptRequest := TextPromptRequest(created.SessionId, "turn-nonce", "hello")
	promptRequest.Meta[lifecycle.MetaKey] = map[string]any{
		"version":    1,
		"submission": map[string]any{"submissionId": "submission", "clientNonce": "nonce"},
	}
	response, err := agent.Prompt(ctx, promptRequest)
	require.NoError(t, err)
	require.Equal(t, acp.StopReasonEndTurn, response.StopReason)

	var events []string

	streams := map[string]struct{}{}

	for _, update := range recorder.updates {
		envelope, carried := update.Meta[lifecycle.MetaKey].(map[string]any)
		if !carried {
			continue
		}

		require.NotNil(t, update.Update.SessionInfoUpdate, "the identity-only carrier is the only legal one")

		streamID, ok := envelope["streamId"].(string)
		require.True(t, ok)

		event, ok := envelope["event"].(map[string]any)
		require.True(t, ok)

		eventType, ok := event["type"].(string)
		require.True(t, ok)

		streams[streamID] = struct{}{}
		events = append(events, eventType)
	}

	require.Equal(t, []string{"lifecycle_snapshot", "prompt_accepted", "state_update", "state_update"}, events)
	require.Len(t, streams, 1, "the session opens exactly one native incarnation")
}

// A host's real `session/new` is not a string map. The mandatory `mcpServers`
// array and an object-valued `_meta` sit beside the string members, and any
// reader that decodes the whole params blob into a narrower shape fails on
// exactly those two — silently, on every real request, while a suite that only
// ever builds minimal params keeps passing. So the wire bytes are driven through
// the connection's own decoder on a negotiated connection, and the prompt that
// follows still has to open its stream with the snapshot first.
func TestLifecycleOpensForegroundStreamForFullWireSessionParams(t *testing.T) {
	ctx := context.Background()
	agent := NewAgent(withClientFactory(func(context.Context, codex.Options) (codex.Client, error) {
		return newSpyCodexClient(), nil
	}))
	cleanupSessionNativePumps(t, agent)
	recorder := newRecordingAgentClient()
	agent.setAgentClient(recorder)
	conn := &localAgentConnection{agent: agent}

	initializeResult, reqErr := conn.handle(ctx, acp.AgentMethodInitialize, json.RawMessage(
		`{"protocolVersion":1,"_meta":{"acp-go.dev/lifecycle":{"versions":[1]}}}`,
	))
	require.Nil(t, reqErr)

	initialized, ok := initializeResult.(acp.InitializeResponse)
	require.True(t, ok)
	require.Contains(t, initialized.Meta, lifecycle.MetaKey, "version 1 is negotiated for this connection")

	createdResult, reqErr := conn.handle(ctx, acp.AgentMethodSessionNew, json.RawMessage(`{
		"cwd": "/tmp/project",
		"mcpServers": [
			{
				"name": "fixture",
				"command": "/usr/bin/true",
				"args": ["--serve"],
				"env": [{"name": "FIXTURE_TOKEN", "value": "token"}]
			}
		],
		"_meta": {"example.test/host": {"nested": {"depth": 2}, "enabled": true}}
	}`))
	require.Nil(t, reqErr)

	created, ok := createdResult.(acp.NewSessionResponse)
	require.True(t, ok)

	// The non-string members reached the session rather than being dropped or
	// refused, which is what makes the stream below an answer about real params.
	servers := agent.sessionMust(created.SessionId).mcpServers
	require.Len(t, servers, 1)
	require.NotNil(t, servers[0].Stdio)
	require.Equal(t, "fixture", servers[0].Stdio.Name)
	require.Equal(t, []string{"--serve"}, servers[0].Stdio.Args)

	promptResult, reqErr := conn.handle(ctx, acp.AgentMethodSessionPrompt, json.RawMessage(`{
		"sessionId": "`+string(created.SessionId)+`",
		"prompt": [{"type": "text", "text": "hello"}],
		"_meta": {
			"acp-go.dev/route": {"version": 1, "turnNonce": "turn-nonce"},
			"acp-go.dev/lifecycle": {
				"version": 1,
				"submission": {"submissionId": "submission", "clientNonce": "nonce"}
			}
		}
	}`))
	require.Nil(t, reqErr)

	prompted, ok := promptResult.(acp.PromptResponse)
	require.True(t, ok)
	require.Equal(t, acp.StopReasonEndTurn, prompted.StopReason)

	var events []string

	for _, update := range recorder.updates {
		envelope, carried := update.Meta[lifecycle.MetaKey].(map[string]any)
		if !carried {
			continue
		}

		event, isEvent := envelope["event"].(map[string]any)
		require.True(t, isEvent)

		eventType, named := event["type"].(string)
		require.True(t, named)

		events = append(events, eventType)
	}

	require.Equal(t, []string{"lifecycle_snapshot", "prompt_accepted", "state_update", "state_update"}, events)
}

func TestLifecycleFailedOutcomeOmitsStopReason(t *testing.T) {
	agent := NewAgent()
	recorder := newRecordingAgentClient()
	agent.setAgentClient(recorder)
	s := &session{agent: agent, id: "failure"}
	in, err := s.openIncarnation(context.Background(), lifecycle.Negotiated{Versions: []int{1}, ActivityKinds: []lifecycle.ActivityKind{}})
	require.NoError(t, err)
	require.NoError(t, in.accept(context.Background(), lifecycle.Submission{SubmissionID: "s", ClientNonce: "n"}))
	require.NoError(t, in.settle(context.Background(), acp.StopReasonEndTurn, lifecycle.OutcomeFailed))
	encoded := recorder.updates[len(recorder.updates)-1].Meta[lifecycle.MetaKey]
	require.NotContains(t, strings.ToLower(requireJSON(t, encoded)), "stopreason")
}

func TestLifecyclePromptCorrelationAndSettlementBranches(t *testing.T) {
	agent := NewAgent()
	agent.lifecycle = lifecycle.Negotiated{Versions: []int{1}, ActivityKinds: []lifecycle.ActivityKind{}}
	s := &session{agent: agent, id: "s"}
	_, err := s.Prompt(context.Background(), acp.PromptRequest{SessionId: "s", Meta: inboundRouteMeta("turn")})
	require.Error(t, err, "negotiated prompts require correlation before dispatch")

	for _, tc := range []struct {
		reason  acp.StopReason
		outcome lifecycle.Outcome
	}{
		{acp.StopReasonCancelled, lifecycle.OutcomeCancelled},
		{acp.StopReasonRefusal, lifecycle.OutcomeRefused},
		{acp.StopReasonMaxTokens, lifecycle.OutcomeLimit},
		{acp.StopReasonMaxTurnRequests, lifecycle.OutcomeLimit},
		{acp.StopReasonEndTurn, lifecycle.OutcomeSuccess},
	} {
		reason, outcome := promptSettlement(promptTurnResult{state: &promptEventState{stopReason: tc.reason}})
		require.Equal(t, tc.reason, reason)
		require.Equal(t, tc.outcome, outcome)
	}
	reason, outcome := promptSettlement(promptTurnResult{failure: errors.New("native failed")})
	require.Empty(t, reason)
	require.Equal(t, lifecycle.OutcomeFailed, outcome)
}

func TestLifecyclePromptDispatchAndSettlementFailures(t *testing.T) {
	ctx := context.Background()
	negotiated := lifecycle.Negotiated{Versions: []int{1}, ActivityKinds: []lifecycle.ActivityKind{}}
	correlatedMeta := inboundRouteMeta("route")
	correlatedMeta[lifecycle.MetaKey] = map[string]any{
		"version":    1,
		"submission": map[string]any{"submissionId": "submission", "clientNonce": "nonce"},
	}
	promptRequest := TextPromptRequest("s", "route", "hello")
	promptRequest.Meta = correlatedMeta

	makeSession := func(agent *Agent) *session {
		agent.lifecycle = negotiated

		s := &session{
			agent: agent, id: "s", cwd: "/tmp", codexThreadID: "thread",
			client: &runEventsClient{events: []codex.Event{{Kind: codex.EventCompleted, ThreadID: "thread", TurnID: "turn"}}},
		}
		t.Cleanup(s.fenceSession)

		return s
	}

	// A failed durability retry stops before stream construction.
	storeFailure := errors.New("store unavailable")
	store := &appendFuncStore{append: func(context.Context, SessionKey, []SessionStoreEntry) error { return storeFailure }}
	agent := NewAgent(WithSessionStore(store))
	s := makeSession(agent)
	s.unsyncedEntries = []SessionStoreEntry{SessionStoreEntry(`{}`)}
	_, err := s.Prompt(ctx, promptRequest)
	require.ErrorIs(t, err, storeFailure)

	// Identity construction failure writes no native frame.
	originalRand := sessionIDRandReader
	t.Cleanup(func() { sessionIDRandReader = originalRand })
	sessionIDRandReader = strings.NewReader("short")
	s = makeSession(NewAgent())
	_, err = s.Prompt(ctx, promptRequest)
	require.Error(t, err)
	sessionIDRandReader = originalRand

	// Dispatch succeeds, but acceptance delivery fails before the turn is driven.
	agent = NewAgent()
	agent.setAgentClient(&nthFailLifecycleClient{recordingAgentClient: newRecordingAgentClient(), failAt: 2})
	s = makeSession(agent)
	_, err = s.Prompt(ctx, promptRequest)
	require.Error(t, err)

	// Final lifecycle settlement is a latch and its delivery failure is returned.
	agent = NewAgent()
	agent.setAgentClient(&nthFailLifecycleClient{recordingAgentClient: newRecordingAgentClient(), failAt: 4})
	s = makeSession(agent)
	_, err = s.Prompt(ctx, promptRequest)
	require.Error(t, err)

	// A native terminal status outside the clean ACP set is a provider failure,
	// never a fabricated stop reason.
	agent = NewAgent()
	agent.setAgentClient(newRecordingAgentClient())
	s = makeSession(agent)
	s.client = &runEventsClient{events: []codex.Event{{Kind: codex.EventCompleted, StopReason: codex.StopReasonError}}}
	_, err = s.Prompt(ctx, promptRequest)
	require.Error(t, err)
}

func TestLifecycleAcceptanceDeliveryFailureContainsExactAcknowledgedTurn(t *testing.T) {
	for _, failAt := range []int{2, 3} {
		t.Run(fmt.Sprintf("lifecycle_delivery_%d", failAt), func(t *testing.T) {
			agent := NewAgent()
			agent.lifecycle = lifecycle.Negotiated{Versions: []int{1}, ActivityKinds: []lifecycle.ActivityKind{}}
			agent.setAgentClient(&nthFailLifecycleClient{
				recordingAgentClient: newRecordingAgentClient(), failAt: failAt,
			})
			client := &exactCancelRunClient{
				runEventsClient: &runEventsClient{events: []codex.Event{{
					Kind: codex.EventCompleted, ThreadID: "thread", TurnID: "accepted-turn",
				}}},
				cancelStarted: make(chan [2]string, 1), cancelRelease: make(chan struct{}),
			}
			s := &session{
				agent: agent, id: "session", cwd: t.TempDir(), codexThreadID: "thread", client: client,
			}
			t.Cleanup(s.fenceSession)

			request := TextPromptRequest(s.id, "nonce", "hello")
			request.Meta = inboundRouteMeta("nonce")
			request.Meta[lifecycle.MetaKey] = map[string]any{
				"version": 1,
				"submission": map[string]any{
					"submissionId": "submission", "clientNonce": "nonce",
				},
			}
			promptDone := make(chan error, 1)
			go func() {
				_, err := s.Prompt(t.Context(), request)
				promptDone <- err
			}()
			require.Equal(t, [2]string{"thread", "accepted-turn"}, <-client.cancelStarted)
			select {
			case err := <-promptDone:
				t.Fatalf("prompt returned before exact turn containment completed: %v", err)
			default:
			}
			close(client.cancelRelease)
			err := <-promptDone
			require.Error(t, err)
			require.NotContains(t, err.Error(), "stream failed")
			require.Equal(t, [][2]string{{"thread", "accepted-turn"}}, client.cancellationTargets())
		})
	}
}

func TestPostRunningTypedDeliveryFailuresContainBeforeSettlement(t *testing.T) {
	for _, test := range []struct {
		name   string
		failAt int
	}{
		{name: "first ordinary update", failAt: 4},
		{name: "terminal native identity", failAt: 5},
	} {
		t.Run(test.name, func(t *testing.T) {
			recorder := newRecordingAgentClient()
			agent := NewAgent()
			agent.lifecycle = lifecycle.Negotiated{Versions: []int{1}}
			agent.setAgentClient(&nthFailSessionUpdateClient{recordingAgentClient: recorder, failAt: test.failAt})
			client := &exactCancelRunClient{
				runEventsClient: &runEventsClient{events: []codex.Event{
					{Kind: codex.EventAgentMessageDelta, ThreadID: "thread", TurnID: "accepted-turn", ItemID: "message", Text: "hello"},
					{Kind: codex.EventCompleted, ThreadID: "thread", TurnID: "accepted-turn", StopReason: codex.StopReasonEndTurn},
				}},
				cancelStarted: make(chan [2]string, 1), cancelRelease: make(chan struct{}),
			}
			s := &session{agent: agent, id: "session", cwd: t.TempDir(), codexThreadID: "thread", client: client}
			t.Cleanup(s.fenceSession)
			request := TextPromptRequest(s.id, "nonce", "hello")
			request.Meta[lifecycle.MetaKey] = map[string]any{
				"version":    1,
				"submission": map[string]any{"submissionId": "submission", "clientNonce": "nonce"},
			}

			promptDone := make(chan error, 1)
			go func() {
				_, err := s.Prompt(t.Context(), request)
				promptDone <- err
			}()
			require.Equal(t, [2]string{"thread", "accepted-turn"}, <-client.cancelStarted)
			select {
			case err := <-promptDone:
				t.Fatalf("prompt returned before exact delivery containment: %v", err)
			default:
			}
			close(client.cancelRelease)
			require.Error(t, <-promptDone)
			require.Equal(t, [][2]string{{"thread", "accepted-turn"}}, client.cancellationTargets())
			require.Equal(t, "state_update", lifecycleEventType(recorder.updates[len(recorder.updates)-1]),
				"failed delivery did not settle after containment")
		})
	}
}

func TestForegroundCommitFailureFencesTurnAndPreventsRedispatch(t *testing.T) {
	commitErr := errors.New("foreground commit failed")
	store := &appendFuncStore{append: func(context.Context, SessionKey, []SessionStoreEntry) error {
		return commitErr
	}}
	agent := NewAgent(WithSessionStore(store))
	agent.lifecycle = lifecycle.Negotiated{Versions: []int{1}, ActivityKinds: []lifecycle.ActivityKind{}}
	agent.setAgentClient(newRecordingAgentClient())
	rollout := filepath.Join(t.TempDir(), "rollout.jsonl")
	require.NoError(t, os.WriteFile(rollout, nil, 0o600))
	client := &rolloutWritingRunClient{
		runEventsClient: runEventsClient{events: []codex.Event{{
			Kind: codex.EventCompleted, ThreadID: "thread", TurnID: "committed-turn",
		}}},
		path: rollout,
		entries: []SessionStoreEntry{
			SessionStoreEntry(`{"type":"event_msg","payload":{"type":"task_complete","turn_id":"committed-turn"}}`),
		},
	}
	s := &session{
		agent: agent, id: "session", cwd: t.TempDir(), codexThreadID: "thread", rolloutPath: rollout, client: client,
	}
	t.Cleanup(s.fenceSession)

	request := TextPromptRequest(s.id, "nonce", "hello")
	request.Meta = inboundRouteMeta("nonce")
	request.Meta[lifecycle.MetaKey] = map[string]any{
		"version": 1,
		"submission": map[string]any{
			"submissionId": "submission", "clientNonce": "nonce",
		},
	}
	_, err := s.Prompt(t.Context(), request)
	require.ErrorIs(t, err, commitErr)
	require.Equal(t, 1, client.runCalls)
	s.lifecycleMu.Lock()
	require.ErrorIs(t, s.lifecycleFailure, commitErr)
	require.NotNil(t, s.incarnation)
	require.True(t, s.incarnation.accepted)
	require.False(t, s.incarnation.settled)
	require.True(t, s.lifecycleStream.Fenced())
	s.lifecycleMu.Unlock()

	_, err = s.Prompt(t.Context(), request)
	require.Error(t, err)
	require.Equal(t, 1, client.runCalls)
}

func TestLifecycleActionRegistryFailsClosedAtFixedBound(t *testing.T) {
	agent := NewAgent()
	agent.setAgentClient(newRecordingAgentClient())
	s := &session{agent: agent, id: "session"}
	negotiated := lifecycle.Negotiated{Versions: []int{1}, ActivityKinds: []lifecycle.ActivityKind{}}
	require.NoError(t, s.openLifecycleStream(t.Context(), negotiated))
	in, err := s.openIncarnation(t.Context(), negotiated)
	require.NoError(t, err)
	require.NoError(t, in.accept(t.Context(), lifecycle.Submission{SubmissionID: "submission", ClientNonce: "nonce"}))

	for index := range lifecycleActionLimit {
		actionID := fmt.Sprintf("action-%d", index)
		require.NoError(t, in.announceAction(t.Context(), actionID, lifecycle.ActionElicitation, false))
		require.NoError(t, in.resolveAction(t.Context(), actionID, lifecycle.ActionAccepted))
	}
	require.Len(t, in.stream.State().Actions, lifecycleActionLimit)
	err = in.announceAction(t.Context(), "overflow", lifecycle.ActionElicitation, false)
	require.ErrorIs(t, err, codex.ErrTurnEventOverflow)
	s.lifecycleMu.Lock()
	require.ErrorIs(t, s.lifecycleFailure, codex.ErrTurnEventOverflow)
	require.True(t, s.lifecycleStream.Fenced())
	s.lifecycleMu.Unlock()
}

func TestLifecyclePreBindFailedOrMalformedAckIsContainedExactly(t *testing.T) {
	for _, test := range []struct {
		name      string
		malformed bool
	}{
		{name: "failed", malformed: false},
		{name: "malformed", malformed: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			client := newFailedAckThreadFeedClient(test.malformed)
			agent := NewAgent()
			agent.lifecycle = lifecycle.Negotiated{Versions: []int{1}}
			updates := newRecordingAgentClient()
			agent.setAgentClient(updates)
			s := &session{
				agent: agent, id: "session", cwd: "/tmp", codexThreadID: "thread", client: client,
			}
			t.Cleanup(s.fenceSession)

			request := TextPromptRequest("session", "nonce", "hello")
			request.Meta = inboundRouteMeta("nonce")
			request.Meta[lifecycle.MetaKey] = map[string]any{
				"version":    1,
				"submission": map[string]any{"submissionId": "submission", "clientNonce": "nonce"},
			}
			_, err := s.Prompt(context.Background(), request)
			require.Error(t, err)
			require.Equal(t, [2]string{"thread", "pre-ack-turn"}, <-client.cancelled)

			joined := fmt.Sprint(updates.updates)
			require.Contains(t, joined, "accepted")
			require.Contains(t, joined, "failed")
		})
	}
}

func TestInternalSessionClosePublishesExpectedStopBeforeBrokerEOF(t *testing.T) {
	client := newCloseOrderThreadFeedClient()
	agent := NewAgent()
	s := &session{agent: agent, id: "session", codexThreadID: "thread", client: client}
	client.session = s
	require.NoError(t, s.attachNativeEvents())
	require.NoError(t, s.Close(context.Background()))
	require.NoError(t, <-client.order)
	s.mu.Lock()
	closing := s.closing
	s.mu.Unlock()
	s.lifecycleMu.Lock()
	stopping := s.nativeEventStopping
	failure := s.lifecycleFailure
	s.lifecycleMu.Unlock()
	require.True(t, closing)
	require.True(t, stopping)
	require.NoError(t, failure)
}

func TestLifecycleReservedCancelRejectedBeforeNativeInterrupt(t *testing.T) {
	agent := NewAgent()
	client := newSpyCodexClient()
	s := &session{agent: agent, id: "s", client: client, codexThreadID: "thread", turnID: "turn", turnNonce: "nonce", turnDone: make(chan struct{})}
	agent.sessions["s"] = s
	meta := inboundRouteMeta("nonce")
	meta[lifecycle.MetaKey] = map[string]any{}
	require.Error(t, agent.Cancel(context.Background(), acp.CancelNotification{SessionId: "s", Meta: meta}))
	require.False(t, s.turnCancelled)
}

// TestCancelRoutesBeforeItRefusesTheReservedKey pins the one order a surface
// carrying both reserved objects may reach a verdict in. The route nonce is the
// anti-stale authenticator, so it is validated first and its verdict is the one
// reported; a request that is wrong in both ways is never an implementation's
// choice between two answers. Both refusals precede the native interrupt.
func TestCancelRoutesBeforeItRefusesTheReservedKey(t *testing.T) {
	for _, tc := range []struct {
		name  string
		meta  map[string]any
		field string
	}{
		{
			name:  "a stale route beside the reserved key reports the route",
			meta:  inboundRouteMeta("stale"),
			field: "_meta." + routeMetaKey,
		},
		{
			name:  "a missing route beside the reserved key reports the route",
			meta:  map[string]any{},
			field: "_meta." + routeMetaKey,
		},
		{
			name:  "a valid route beside the reserved key reports the key",
			meta:  inboundRouteMeta("nonce"),
			field: lifecycle.MetaPath,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			agent := NewAgent()
			client := newSpyCodexClient()
			s := &session{
				agent: agent, id: "s", client: client, codexThreadID: "thread",
				turnID: "turn", turnNonce: "nonce", turnDone: make(chan struct{}),
			}
			agent.sessions["s"] = s

			meta := tc.meta
			meta[lifecycle.MetaKey] = map[string]any{}

			var requestErr *acp.RequestError

			require.ErrorAs(t, agent.Cancel(context.Background(), acp.CancelNotification{SessionId: "s", Meta: meta}),
				&requestErr)

			data, ok := requestErr.Data.(map[string]any)
			require.True(t, ok)
			require.Equal(t, tc.field, data[jsonFieldField])
			require.False(t, s.turnCancelled)
		})
	}
}

// TestPromptRoutesBeforeItReadsTheLifecycleValue proves the same precedence on
// the other surface carrying both reserved objects. A prompt whose route is
// stale and whose correlation value is malformed reports the route, and neither
// refusal reaches a native frame.
func TestPromptRoutesBeforeItReadsTheLifecycleValue(t *testing.T) {
	agent := NewAgent()
	agent.lifecycle = lifecycle.Negotiated{Versions: []int{1}, ActivityKinds: []lifecycle.ActivityKind{}}
	client := newSpyCodexClient()
	s := &session{agent: agent, id: "s", client: client, codexThreadID: "thread"}
	t.Cleanup(s.fenceSession)

	meta := map[string]any{lifecycle.MetaKey: map[string]any{"version": "one"}}

	var requestErr *acp.RequestError

	_, err := s.Prompt(context.Background(), acp.PromptRequest{SessionId: "s", Meta: meta})
	require.ErrorAs(t, err, &requestErr)

	data, ok := requestErr.Data.(map[string]any)
	require.True(t, ok)
	require.Equal(t, "_meta."+routeMetaKey, data[jsonFieldField])
	require.Empty(t, client.lastTurn.ThreadID, "a refused prompt writes no native frame")
}

func TestLifecycleSettlementPersistenceContainmentEdges(t *testing.T) {
	ctx := context.Background()
	store := &appendFuncStore{}
	agent := NewAgent(WithSessionStore(store))
	s := &session{agent: agent, id: "s"}
	require.NoError(t, s.mirrorAndEmitRollout(ctx))

	s.persistenceFenced = true
	require.NoError(t, s.commitRolloutEntries(ctx, store, []SessionStoreEntry{SessionStoreEntry(`{}`)}, 1))
	s.persistenceFenced = false
	s.unsyncedEntries = []SessionStoreEntry{SessionStoreEntry(`{}`)}
	s.unsyncedRow = 1
	require.NoError(t, s.ensureMirrorSynced(ctx))
	require.Empty(t, s.unsyncedEntries)
	require.Equal(t, 1, s.mirroredRows)

	containFailure := errors.New("terminal sweep failed")
	client := &failingBackgroundTerminalClient{spyCodexClient: newSpyCodexClient(), err: containFailure}
	s.client = client
	s.codexThreadID = "thread"
	err := s.containSession(ctx)
	require.Error(t, err)
	require.ErrorIs(t, err, containFailure)

	containmentErr := errors.New("turn containment failed")
	done := make(chan struct{})
	close(done)
	s.turnContainment = &turnContainment{done: done, err: containmentErr, started: true}
	_, err = s.settlePrompt(ctx, ctx, nil, promptTurnResult{
		state: &promptEventState{stopReason: acp.StopReasonEndTurn},
	}, nil)
	require.ErrorIs(t, err, containmentErr)
}

func requireJSON(t *testing.T, value any) string {
	t.Helper()
	raw, err := json.Marshal(value)
	require.NoError(t, err)

	return string(raw)
}

// closeBoundaryFixture builds one negotiated session on a recording connection.
// The close-boundary cases all need the same three things: an answer that makes
// an incarnation possible, a connection that records every notification, and a
// registered session the ACP close method can address.
func closeBoundaryFixture(t *testing.T, opts ...Option) (*Agent, *session, *recordingAgentClient) {
	t.Helper()

	client := newSpyCodexClient()
	agent := NewAgent(append(opts, withClientFactory(
		func(context.Context, codex.Options) (codex.Client, error) { return client, nil },
	))...)
	cleanupSessionNativePumps(t, agent)
	agent.lifecycle = lifecycle.Negotiated{Versions: []int{1}, ActivityKinds: []lifecycle.ActivityKind{}}

	conn := newRecordingAgentClient()
	agent.setAgentClient(conn)

	created, err := agent.NewSession(context.Background(), NewSessionRequest(t.TempDir()))
	require.NoError(t, err)

	return agent, agent.activeSession(created.SessionId), conn
}

func cleanupSessionNativePumps(t *testing.T, agent *Agent) {
	t.Helper()
	t.Cleanup(func() {
		agent.mu.Lock()
		sessions := make([]*session, 0, len(agent.sessions))
		for _, session := range agent.sessions {
			sessions = append(sessions, session)
		}
		agent.mu.Unlock()
		for _, session := range sessions {
			session.fenceSession()
		}
	})
}

// lifecycleNotifications counts the notifications carrying the reserved envelope.
func lifecycleNotifications(conn *recordingAgentClient) int {
	count := 0

	for _, update := range conn.updates {
		if _, carried := update.Meta[lifecycle.MetaKey]; carried {
			count++
		}
	}

	return count
}

// TestCloseOnADeadStreamEmitsNothing pins the close ladder's emission rungs to a
// live incarnation. A session that never opened a stream and a stream already
// fenced by containment both emit nothing at close; an event bearing a fenced
// streamId is exactly what a conforming reducer refuses.
func TestCloseOnADeadStreamEmitsNothing(t *testing.T) {
	ctx := context.Background()

	t.Run("an incarnation that never opened", func(t *testing.T) {
		agent, session, conn := closeBoundaryFixture(t)
		require.Nil(t, session.liveIncarnation())

		_, err := agent.CloseSession(ctx, acp.CloseSessionRequest{SessionId: session.id})
		require.NoError(t, err)
		require.Zero(t, lifecycleNotifications(conn))
	})

	t.Run("an incarnation a cancel already ended", func(t *testing.T) {
		agent, session, conn := closeBoundaryFixture(t)

		incarnation, err := session.openIncarnation(ctx, agent.negotiatedLifecycle())
		require.NoError(t, err)
		require.NoError(t, incarnation.accept(ctx, lifecycle.Submission{SubmissionID: "sub", ClientNonce: "non"}))
		require.NoError(t, incarnation.settle(ctx, acp.StopReasonCancelled, lifecycle.OutcomeCancelled))
		session.fenceSession()

		emitted := lifecycleNotifications(conn)
		require.Positive(t, emitted)

		_, err = agent.CloseSession(ctx, acp.CloseSessionRequest{SessionId: session.id})
		require.NoError(t, err)
		require.Equal(t, emitted, lifecycleNotifications(conn))

		// The rung is skipped rather than optional: the stream itself refuses
		// what a close that terminalized on it would have had to send. The event
		// offered here is the well-formed one that close would send, so the
		// refusal is the fence's and not the payload's.
		require.ErrorIs(t,
			incarnation.emit(ctx, lifecycle.IdleEvent(incarnation.cycleID, incarnation.turnID,
				lifecycle.StopReasonCancelled, lifecycle.OutcomeCancelled)),
			&lifecycle.ViolationError{Kind: lifecycle.ViolationStaleStream})
	})
}

// TestCloseCommitsTheCapturedPrefixAcrossTheFence pins the non-emission half of
// the ladder. The durable rung belongs to the session rather than to the
// incarnation, so a prefix a settlement captured and could not place is still
// committed by a close that has already fenced the stream.
func TestCloseCommitsTheCapturedPrefixAcrossTheFence(t *testing.T) {
	var appended []SessionStoreEntry

	store := &appendFuncStore{append: func(_ context.Context, _ SessionKey, entries []SessionStoreEntry) error {
		appended = append(appended, entries...)

		return nil
	}}

	agent, session, _ := closeBoundaryFixture(t, WithSessionStore(store))
	captured := SessionStoreEntry(`{"type":"turn_context"}`)
	session.unsyncedEntries = []SessionStoreEntry{captured}
	session.unsyncedRow = 1

	_, err := agent.CloseSession(context.Background(), acp.CloseSessionRequest{SessionId: session.id})
	require.NoError(t, err)
	require.Nil(t, session.liveIncarnation(), "the stream is fenced before the commit runs")
	require.Contains(t, appended, captured)
	require.Empty(t, session.unsyncedEntries)
}

// TestAgentCloseCommitsTheDurableRungAWireCloseOwes pins the ladder travelling
// with the boundary. An embedded host shuts its agent down instead of sending
// session/close, and the prefix a settlement captured and could not place is
// the same resumable state either way: dropping it with the wrapper loses a
// turn the host was already shown.
func TestAgentCloseCommitsTheDurableRungAWireCloseOwes(t *testing.T) {
	var appended []SessionStoreEntry

	store := &appendFuncStore{append: func(_ context.Context, _ SessionKey, entries []SessionStoreEntry) error {
		appended = append(appended, entries...)

		return nil
	}}

	agent, session, _ := closeBoundaryFixture(t, WithSessionStore(store))
	captured := SessionStoreEntry(`{"type":"turn_context"}`)
	session.unsyncedEntries = []SessionStoreEntry{captured}
	session.unsyncedRow = 1

	require.NoError(t, agent.Close())
	require.Contains(t, appended, captured)
	require.Empty(t, session.unsyncedEntries)
}

// TestAgentCloseKeepsTheMaterialWhileTheCommitIsOwed pins the other half. The
// materialized rollout is the only remaining copy of what the commit must
// place, so a store that refuses fails the close and leaves the material where
// it stands; releasing it would destroy the state and the evidence together.
func TestAgentCloseKeepsTheMaterialWhileTheCommitIsOwed(t *testing.T) {
	storeFailure := errors.New("store unavailable")

	store := &appendFuncStore{append: func(context.Context, SessionKey, []SessionStoreEntry) error {
		return storeFailure
	}}

	agent, session, _ := closeBoundaryFixture(t, WithSessionStore(store))

	material := filepath.Join(t.TempDir(), "rollout.jsonl")
	writeRolloutLines(t, material, `{"type":"turn_context"}`)

	session.materializedPath = material
	session.unsyncedEntries = []SessionStoreEntry{SessionStoreEntry(`{"type":"turn_context"}`)}
	session.unsyncedRow = 1

	require.ErrorIs(t, agent.Close(), storeFailure)
	require.FileExists(t, material, "the material a still-owed commit needs was destroyed")
	require.NotEmpty(t, session.unsyncedEntries, "the refused prefix is retained, not dropped")
}

// attemptCountingStore answers with real tombstone semantics and counts every
// write the adapter offers it. Counting attempts rather than effects is the
// point: a store that swallows a post-delete write still proves the adapter
// tried to make one.
type attemptCountingStore struct {
	*InMemorySessionStore

	mu      sync.Mutex
	appends int
	refuse  error
}

func (s *attemptCountingStore) Append(ctx context.Context, key SessionKey, entries []SessionStoreEntry) error {
	s.mu.Lock()
	s.appends++
	refuse := s.refuse
	s.mu.Unlock()

	if refuse != nil {
		return refuse
	}

	return s.InMemorySessionStore.Append(ctx, key, entries)
}

func (s *attemptCountingStore) attempts() int {
	s.mu.Lock()
	defer s.mu.Unlock()

	return s.appends
}

// TestDeleteFencesEveryLaterCommit pins the post-delete persistence fence on the
// paths that actually reach the store. A delete is final, and the session
// wrapper it retires can outlive it by a beat: a close rung can still be
// entered, and the harness can still be appending rows to the file. Every one
// of those paths must write nothing, and no write may recreate the row the
// delete removed.
func TestDeleteFencesEveryLaterCommit(t *testing.T) {
	ctx := context.Background()
	storeFailure := errors.New("store unavailable")
	store := &attemptCountingStore{InMemorySessionStore: NewInMemorySessionStore(), refuse: storeFailure}

	agent, session, _ := closeBoundaryFixture(t, WithSessionStore(store))
	path := rolloutFixture(t, session, `{"type":"turn_context"}`)

	// A settlement pass that captured a prefix and could not place it: the
	// commit this delete must never make is now owed.
	require.Error(t, session.mirrorAndEmitRollout(ctx))
	require.NotEmpty(t, session.unsyncedEntries)

	store.mu.Lock()
	store.refuse = nil
	store.mu.Unlock()

	_, err := agent.UnstableDeleteSession(ctx, DeleteSessionRequest(session.id))
	require.NoError(t, err)

	// The harness finished a row behind the delete, so a late tail pass has
	// something new to place and is refused by the fence rather than by an
	// empty read.
	writeRolloutLines(t, path, `{"type":"turn_context"}`, `{"type":"event_msg","payload":{"type":"agent_message","message":"late"}}`)

	fenced := store.attempts()

	require.NoError(t, session.mirrorAndEmitRollout(ctx), "a late durability pass writes nothing")
	require.NoError(t, session.ensureMirrorSynced(ctx), "the owed prefix went with the delete")
	require.NoError(t, session.commitResumableSnapshot(ctx), "a late close rung writes nothing")

	require.Equal(t, fenced, store.attempts(), "a fenced session offered the store a write")

	entries, err := store.Load(ctx, SessionKey{SessionID: string(session.id)})
	require.NoError(t, err)
	require.Empty(t, entries, "a post-delete write recreated the row")
}

// TestCloseFailsWhenTheCapturedPrefixCannotBeCommitted pins the other half of the
// same rung: durability outranks the response, so a capture the store refuses
// fails the close instead of being dropped with the session wrapper. The
// terminal pending-commit owner stays addressable only to retry the owed commit
// and removal; native containment must never run a second time.
func TestCloseFailsWhenTheCapturedPrefixCannotBeCommitted(t *testing.T) {
	storeFailure := errors.New("store unavailable")
	refuse := false
	appendAttempts := 0

	store := &appendFuncStore{append: func(context.Context, SessionKey, []SessionStoreEntry) error {
		appendAttempts++
		if refuse {
			return storeFailure
		}

		return nil
	}}

	agent, session, _ := closeBoundaryFixture(t, WithSessionStore(store))
	session.unsyncedEntries = []SessionStoreEntry{SessionStoreEntry(`{"type":"turn_context"}`)}
	session.unsyncedRow = 1
	refuse = true

	_, err := agent.CloseSession(context.Background(), acp.CloseSessionRequest{SessionId: session.id})
	require.ErrorIs(t, err, storeFailure)
	require.Same(t, session, agent.activeSession(session.id))
	client, ok := session.client.(*spyCodexClient)
	require.True(t, ok)
	require.Len(t, client.unsubscribedSnapshot(), 1)
	firstAppendAttempts := appendAttempts
	require.Positive(t, firstAppendAttempts)

	session.mu.Lock()
	require.True(t, session.closing)
	require.True(t, session.closeContained)
	require.True(t, session.closeCommitPending)
	session.mu.Unlock()
	session.lifecycleMu.Lock()
	require.Nil(t, session.lifecycleStream, "a failed durable rung must still fence the terminal stream")
	session.lifecycleMu.Unlock()

	refuse = false
	_, err = agent.CloseSession(context.Background(), acp.CloseSessionRequest{SessionId: session.id})
	require.NoError(t, err)
	require.Empty(t, session.unsyncedEntries)
	require.Len(t, client.unsubscribedSnapshot(), 1, "pending-commit retry re-contained a terminal owner")
	require.Greater(t, appendAttempts, firstAppendAttempts, "pending commit was not retried")
	require.Nil(t, agent.activeSession(session.id))
}

// TestPendingCommitCloseKeepsProviderAuthTerminal pins the two surfaces of a
// terminal pending-commit owner against each other. The lifecycle wrapper stays
// addressable for the exact close retry, but no provider-auth leg or prompt may
// re-enter an already-contained session.
func TestPendingCommitCloseKeepsProviderAuthTerminal(t *testing.T) {
	storeFailure := errors.New("store unavailable")
	refuse := true

	store := &appendFuncStore{append: func(context.Context, SessionKey, []SessionStoreEntry) error {
		if refuse {
			return storeFailure
		}

		return nil
	}}

	agent, session, _ := closeBoundaryFixture(t, WithSessionStore(store), WithProviderAuthRoot(t.TempDir()))
	require.NotNil(t, agent.providerAuth, "the broker is built from the configured ledger root")
	require.False(t, agent.providerAuth.sessionClosed(session.id))

	session.unsyncedEntries = []SessionStoreEntry{SessionStoreEntry(`{"type":"turn_context"}`)}
	session.unsyncedRow = 1

	_, err := agent.CloseSession(context.Background(), acp.CloseSessionRequest{SessionId: session.id})
	require.ErrorIs(t, err, storeFailure)
	require.Same(t, session, agent.activeSession(session.id))

	require.True(t, agent.providerAuth.sessionClosed(session.id), "commit failure re-admitted the terminal auth owner")
	_, err = agent.providerAuth.authSession(string(session.id))
	require.Error(t, err)
	_, err = session.Prompt(context.Background(), acp.PromptRequest{SessionId: session.id})
	require.Error(t, err)

	// A boundary that completes leaves the same mark standing.
	refuse = false

	_, err = agent.CloseSession(context.Background(), acp.CloseSessionRequest{SessionId: session.id})
	require.NoError(t, err)
	require.True(t, agent.providerAuth.sessionClosed(session.id))
}

// rolloutFixture points a session at a rollout file holding exactly the given
// lines and returns the path, so a test can change what the file says between the
// settlement pass and the close.
func rolloutFixture(t *testing.T, session *session, lines ...string) string {
	t.Helper()

	path := filepath.Join(t.TempDir(), "rollout.jsonl")
	writeRolloutLines(t, path, lines...)
	session.rolloutPath = path

	return path
}

func writeRolloutLines(t *testing.T, path string, lines ...string) {
	t.Helper()

	require.NoError(t, os.WriteFile(path, []byte(strings.Join(lines, "\n")), 0o600))
}

// TestCloseRecapturesAPrefixNoSettlementCaptured pins the close rung against the
// failure that retains nothing. A mirror pass has a capture half and a commit
// half, and only the commit half retains what it could not place: a pass that
// died reading the file — here on a row the harness had written only half of —
// leaves no capture behind at all. The rung as it stood placed captures and made
// none, so the close committed nothing and reported success owing a row.
func TestCloseRecapturesAPrefixNoSettlementCaptured(t *testing.T) {
	const torn = `{"type":"turn_c`

	const whole = `{"type":"turn_context"}`

	t.Run("the recaptured prefix is committed", func(t *testing.T) {
		var appended []SessionStoreEntry

		store := &appendFuncStore{append: func(_ context.Context, _ SessionKey, entries []SessionStoreEntry) error {
			appended = append(appended, entries...)

			return nil
		}}

		agent, session, _ := closeBoundaryFixture(t, WithSessionStore(store))
		path := rolloutFixture(t, session, torn)

		require.Error(t, session.mirrorAndEmitRollout(context.Background()), "the settlement's mirror pass failed")
		require.Empty(t, session.unsyncedEntries, "a pass that failed before its commit captured nothing")

		// The harness finished the row the settlement read half of.
		writeRolloutLines(t, path, whole)

		_, err := agent.CloseSession(context.Background(), acp.CloseSessionRequest{SessionId: session.id})
		require.NoError(t, err)
		require.Equal(t, []SessionStoreEntry{SessionStoreEntry(whole)}, appended)
		require.Empty(t, session.unsyncedEntries)
	})

	t.Run("a recapture the store refuses fails the close", func(t *testing.T) {
		storeFailure := errors.New("store unavailable")

		store := &appendFuncStore{append: func(context.Context, SessionKey, []SessionStoreEntry) error {
			return storeFailure
		}}

		agent, session, _ := closeBoundaryFixture(t, WithSessionStore(store))
		path := rolloutFixture(t, session, torn)

		require.Error(t, session.mirrorAndEmitRollout(context.Background()))
		writeRolloutLines(t, path, whole)

		_, err := agent.CloseSession(context.Background(), acp.CloseSessionRequest{SessionId: session.id})
		require.ErrorIs(t, err, storeFailure, "durability outranks the response on the recaptured prefix too")
		require.Equal(t, []SessionStoreEntry{SessionStoreEntry(whole)}, session.unsyncedEntries)
		require.Same(t, session, agent.activeSession(session.id))
	})
}

// TestCloseReadsNothingNewWithoutACaptureFailure pins the other side of the same
// latch. The close boundary is not a mirror pass: it places what a settlement
// captured and could not commit, and reads nothing off the rollout file on its
// own. Only the failure that captured nothing sends it back to the file, and a
// failure a later pass repaired does not.
func TestCloseReadsNothingNewWithoutACaptureFailure(t *testing.T) {
	const first = `{"type":"turn_context"}`

	const second = `{"type":"event_msg","payload":{"type":"agent_message","message":"hi"}}`

	t.Run("an unmirrored row no settlement pass ever read", func(t *testing.T) {
		var appended []SessionStoreEntry

		store := &appendFuncStore{append: func(_ context.Context, _ SessionKey, entries []SessionStoreEntry) error {
			appended = append(appended, entries...)

			return nil
		}}

		agent, session, _ := closeBoundaryFixture(t, WithSessionStore(store))
		rolloutFixture(t, session, first)

		_, err := agent.CloseSession(context.Background(), acp.CloseSessionRequest{SessionId: session.id})
		require.NoError(t, err)
		require.Empty(t, appended, "the close boundary read nothing off the rollout file")
	})

	t.Run("a capture failure a later pass repaired", func(t *testing.T) {
		var appended []SessionStoreEntry

		store := &appendFuncStore{append: func(_ context.Context, _ SessionKey, entries []SessionStoreEntry) error {
			appended = append(appended, entries...)

			return nil
		}}

		agent, session, _ := closeBoundaryFixture(t, WithSessionStore(store))
		path := rolloutFixture(t, session, `{"type":"turn_c`)

		require.Error(t, session.mirrorAndEmitRollout(context.Background()))

		writeRolloutLines(t, path, first)
		require.NoError(t, session.mirrorAndEmitRollout(context.Background()), "a later pass captured and placed the row")
		require.Equal(t, []SessionStoreEntry{SessionStoreEntry(first)}, appended)

		// A row the harness wrote after that pass belongs to no capture at all,
		// and the close boundary is not the thing that reads it.
		writeRolloutLines(t, path, first, second)

		_, err := agent.CloseSession(context.Background(), acp.CloseSessionRequest{SessionId: session.id})
		require.NoError(t, err)
		require.Equal(t, []SessionStoreEntry{SessionStoreEntry(first)}, appended)
	})
}

// TestLatchAndReducerRefuseToRestateALossTerminalizedFailure pins what actually
// holds a loss-terminalized turn in place. Incarnation loss terminalizes as
// `failed` and close as `cancelled`, and an entity the loss already finished
// stays finished the way the loss finished it — but nothing at the close boundary
// is what enforces that. This configuration's close emits nothing on a stream a
// settlement already ended, and this adapter holds no durable lifecycle entity
// state at all, so there is no record for a later boundary to rewrite.
//
// Two things enforce the outcome, and both are asserted here: the incarnation's
// settled latch, which makes a second settlement a no-op, and the shared reducer,
// which refuses any idle naming a terminal turn under post_terminal_mutation
// whether or not the latch was passed. Beside them is the one fact the boundary
// itself owns: close adds no event to the stream.
func TestLatchAndReducerRefuseToRestateALossTerminalizedFailure(t *testing.T) {
	ctx := context.Background()
	agent, session, conn := closeBoundaryFixture(t)

	incarnation, err := session.openIncarnation(ctx, agent.negotiatedLifecycle())
	require.NoError(t, err)
	require.NoError(t, incarnation.accept(ctx, lifecycle.Submission{SubmissionID: "sub", ClientNonce: "non"}))
	require.NoError(t, incarnation.settle(ctx, "", lifecycle.OutcomeFailed))

	settled := lifecycleNotifications(conn)

	// The latch, on the live stream: a second settlement is the shape a close that
	// terminalized unconditionally would take, and it neither emits nor reduces.
	require.NoError(t, incarnation.settle(ctx, acp.StopReasonCancelled, lifecycle.OutcomeCancelled))
	require.Equal(t, settled, lifecycleNotifications(conn), "the settled latch emitted nothing")

	turn, known := incarnation.stream.State().Turn(incarnation.turnID)
	require.True(t, known)
	require.True(t, turn.Terminal)
	require.Equal(t, lifecycle.OutcomeFailed, turn.Outcome)

	// The reducer, reached past the latch: a cancelled idle naming the same turn
	// is not a weaker restatement, it is a post-terminal mutation, and the
	// sequence it consumed stays consumed.
	_, err = incarnation.stream.Emit(
		lifecycle.IdleEvent(incarnation.cycleID, incarnation.turnID, string(acp.StopReasonCancelled), lifecycle.OutcomeCancelled),
	)
	require.ErrorIs(t, err, &lifecycle.ViolationError{Kind: lifecycle.ViolationPostTerminalMutation})

	// The boundary's own fact: close ends the stream and states nothing on it.
	_, err = agent.CloseSession(ctx, acp.CloseSessionRequest{SessionId: session.id})
	require.NoError(t, err)
	require.Equal(t, settled, lifecycleNotifications(conn), "the close boundary emitted nothing")
	require.Nil(t, session.liveIncarnation(), "the close boundary fenced the stream")

	turn, known = incarnation.stream.State().Turn(incarnation.turnID)
	require.True(t, known)
	require.Equal(t, lifecycle.OutcomeFailed, turn.Outcome)
}

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
