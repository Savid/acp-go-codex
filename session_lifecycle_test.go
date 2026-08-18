package codexacp

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/coder/acp-go-sdk"
	"github.com/savid/acp-go-codex/internal/codex"
	"github.com/savid/acp-go-codex/internal/lifecycle"
	"github.com/stretchr/testify/require"
)

const lifecycleTestToolID = "exec"

type failingLifecycleClient struct {
	*recordingAgentClient
	err error
}

type nthFailLifecycleClient struct {
	*recordingAgentClient
	failAt int
	calls  int
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
		_, _, err = s.requestPermissionForTool(turnCtx, conn, acp.RequestPermissionRequest{ToolCall: acp.ToolCallUpdate{
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
			turnCtx, conn, acp.UnstableCreateElicitationRequest{}, lifecycleTestToolID, params,
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
	early, correlation, err := s.beginAction(ctx, lifecycle.ActionPermission, true)
	require.NoError(t, err)
	require.NotNil(t, early)
	require.NotNil(t, correlation)
	require.NoError(t, early.resolve(ctx, lifecycle.ActionAccepted))
	require.Len(t, recorder.updates, 1)

	require.NoError(t, incarnation.accept(ctx, lifecycle.Submission{
		SubmissionID: "submission", ClientNonce: "nonce", RunID: "run",
	}))
	require.Len(t, recorder.updates, 3)

	blocking, blockingCorrelation, err := s.beginAction(ctx, lifecycle.ActionPermission, true)
	require.NoError(t, err)
	require.Equal(t, incarnation.stream.ID(), blockingCorrelation["streamId"])
	require.Len(t, recorder.updates, 5)
	require.NoError(t, blocking.resolve(ctx, lifecycle.ActionAccepted))
	require.NoError(t, blocking.resolve(ctx, lifecycle.ActionDeclined)) // terminal finality
	require.Len(t, recorder.updates, 7)

	nonblocking, _, err := s.beginAction(ctx, lifecycle.ActionElicitation, false)
	require.NoError(t, err)
	require.NoError(t, nonblocking.resolve(ctx, lifecycle.ActionDeclined))
	require.Len(t, recorder.updates, 9)

	pending, _, err := s.beginAction(ctx, lifecycle.ActionPermission, true)
	require.NoError(t, err)
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

// provenQuiescenceForTest makes the zero-value assertion readable without
// weakening the production API around process evidence.
func (in *promptIncarnation) provenQuiescenceForTest() lifecycle.QuiescenceFact {
	return in.session.provenQuiescence()
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

	failure := errors.New("delivery failed")
	agent.setAgentClient(&failingLifecycleClient{recordingAgentClient: newRecordingAgentClient(), err: failure})
	active := &promptIncarnation{
		session: s,
		stream: lifecycle.NewStream("stream", lifecycle.Negotiated{
			Versions: []int{lifecycle.Version}, ActivityKinds: []lifecycle.ActivityKind{},
		}),
		cycleID: "cycle", turnID: "turn",
	}
	require.ErrorIs(t, active.emit(ctx, lifecycle.SnapshotEvent("cycle", lifecycle.QuiescenceFact{})), failure)

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

func TestLifecycleConstructionAndDeliveryFailures(t *testing.T) {
	ctx := context.Background()
	negotiated := lifecycle.Negotiated{Versions: []int{1}, ActivityKinds: []lifecycle.ActivityKind{}}
	originalRand := sessionIDRandReader
	t.Cleanup(func() { sessionIDRandReader = originalRand })

	for _, bytes := range []int{0, 16, 32} {
		sessionIDRandReader = strings.NewReader(strings.Repeat("x", bytes))
		_, err := (&session{agent: NewAgent(), id: "s"}).openIncarnation(ctx, negotiated)
		require.Error(t, err)
	}

	// Action identity failure occurs before any request can be exposed.
	sessionIDRandReader = originalRand
	actionSession := &session{agent: NewAgent(), id: "action"}
	actionSession.incarnation, _ = actionSession.openIncarnation(ctx, negotiated)
	sessionIDRandReader = strings.NewReader("short")
	_, _, err := actionSession.beginAction(ctx, lifecycle.ActionPermission, true)
	require.Error(t, err)

	sessionIDRandReader = originalRand
	failure := errors.New("delivery failed")
	agent := NewAgent()
	s := &session{agent: agent, id: "s"}
	agent.setAgentClient(&failingLifecycleClient{recordingAgentClient: newRecordingAgentClient(), err: failure})
	_, err = s.openIncarnation(ctx, negotiated)
	require.ErrorIs(t, err, failure)

	// A valid emission with no attached ACP connection is still reduced, while
	// each later delivery failure is surfaced at its exact ordering boundary.
	agent.setAgentClient(nil)
	in, err := s.openIncarnation(ctx, negotiated)
	require.NoError(t, err)
	require.NotNil(t, in)
	other := &promptIncarnation{session: s, stream: lifecycle.NewStream("other", negotiated)}
	s.clearIncarnation(other)
	require.Same(t, in, s.liveIncarnation(), "an old incarnation cannot clear the active run")

	agent.setAgentClient(&failingLifecycleClient{recordingAgentClient: newRecordingAgentClient(), err: failure})
	require.ErrorIs(t, in.accept(ctx, lifecycle.Submission{SubmissionID: "s", ClientNonce: "n"}), failure)

	// Rebuild accepted streams to reach failures after action registration,
	// action resolution, and settlement cancellation independently.
	newAccepted := func() *promptIncarnation {
		agent.setAgentClient(nil)
		candidate, openErr := s.openIncarnation(ctx, negotiated)
		require.NoError(t, openErr)
		require.NoError(t, candidate.accept(ctx, lifecycle.Submission{SubmissionID: "s", ClientNonce: "n"}))

		return candidate
	}

	_ = newAccepted()
	agent.setAgentClient(&failingLifecycleClient{recordingAgentClient: newRecordingAgentClient(), err: failure})
	_, _, err = s.beginAction(ctx, lifecycle.ActionPermission, true)
	require.ErrorIs(t, err, failure)

	_ = newAccepted()
	agent.setAgentClient(nil)
	action, _, err := s.beginAction(ctx, lifecycle.ActionPermission, false)
	require.NoError(t, err)
	agent.setAgentClient(&failingLifecycleClient{recordingAgentClient: newRecordingAgentClient(), err: failure})
	require.ErrorIs(t, action.resolve(ctx, lifecycle.ActionAccepted), failure)

	in = newAccepted()
	agent.setAgentClient(nil)
	_, _, err = s.beginAction(ctx, lifecycle.ActionPermission, true)
	require.NoError(t, err)
	agent.setAgentClient(&failingLifecycleClient{recordingAgentClient: newRecordingAgentClient(), err: failure})
	require.ErrorIs(t, in.settle(ctx, acp.StopReasonCancelled, lifecycle.OutcomeCancelled), failure)

	agent.setAgentClient(nil)
	unaccepted := &promptIncarnation{session: s, stream: lifecycle.NewStream("unaccepted", negotiated)}
	require.NoError(t, unaccepted.emit(ctx, lifecycle.SnapshotEvent("cycle", lifecycle.QuiescenceFact{})))
	require.NoError(t, unaccepted.settle(ctx, acp.StopReasonCancelled, lifecycle.OutcomeCancelled))
}

func TestLifecycleNegotiationAndReservedExtensionDispatch(t *testing.T) {
	agent := NewAgent()
	answer, err := agent.negotiateLifecycle(map[string]any{lifecycle.MetaKey: map[string]any{"versions": []any{1.0}}})
	require.NoError(t, err)
	require.True(t, answer.Present())
	require.Equal(t, lifecycleAdvertisement(RuntimeContainmentUnavailable).ActivityKinds, answer.ActivityKinds)
	require.NotNil(t, lifecycleResponseMeta(answer))
	require.Nil(t, lifecycleResponseMeta(lifecycle.Negotiated{}))

	answer, err = agent.negotiateLifecycle(nil)
	require.NoError(t, err)
	require.False(t, answer.Present())
	answer, err = agent.negotiateLifecycle(map[string]any{lifecycle.MetaKey: map[string]any{"versions": []any{2.0}}})
	require.NoError(t, err)
	require.False(t, answer.Present())
	_, err = agent.negotiateLifecycle(map[string]any{lifecycle.MetaKey: "bad"})
	require.Error(t, err)
	require.NoError(t, rejectLifecycleKey(nil))
	require.Error(t, rejectLifecycleKey(map[string]any{lifecycle.MetaKey: nil}))
	require.NoError(t, rejectLifecycleKeyInParams(json.RawMessage(`{`)))
	require.NoError(t, rejectLifecycleKeyInParams(json.RawMessage(`{"_meta":{}}`)))
	require.Error(t, rejectLifecycleKeyInParams(json.RawMessage(`{"_meta":{"acp-go.dev/lifecycle":{}}}`)))

	_, err = agent.HandleExtensionMethod(context.Background(), "unknown", json.RawMessage(`{"_meta":{"acp-go.dev/lifecycle":{}}}`))
	require.Error(t, err)
	_, err = agent.HandleExtensionMethod(context.Background(), "unknown", json.RawMessage(`{}`))
	require.Error(t, err)

	// A failed outcome never invents an ACP stop reason.
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

func TestLifecycleReservedKeyRejectedAcrossAgentSurfaces(t *testing.T) {
	ctx := context.Background()
	agent := NewAgent()
	meta := map[string]any{lifecycle.MetaKey: map[string]any{}}

	_, err := agent.Initialize(ctx, acp.InitializeRequest{Meta: meta})
	require.Error(t, err)
	_, err = agent.NewSession(ctx, acp.NewSessionRequest{Meta: meta})
	require.Error(t, err)
	_, err = agent.CloseSession(ctx, acp.CloseSessionRequest{Meta: meta})
	require.Error(t, err)
	_, err = agent.UnstableDeleteSession(ctx, acp.UnstableDeleteSessionRequest{Meta: meta})
	require.Error(t, err)
	_, err = agent.ListSessions(ctx, acp.ListSessionsRequest{Meta: meta})
	require.Error(t, err)
	_, err = agent.ResumeSession(ctx, acp.ResumeSessionRequest{Meta: meta})
	require.Error(t, err)
	_, err = agent.LoadSession(ctx, acp.LoadSessionRequest{Meta: meta})
	require.Error(t, err)
	_, err = agent.SetSessionMode(ctx, acp.SetSessionModeRequest{Meta: meta})
	require.Error(t, err)
	_, err = agent.Authenticate(ctx, acp.AuthenticateRequest{Meta: meta})
	require.Error(t, err)
	_, err = agent.Logout(ctx, acp.LogoutRequest{Meta: meta})
	require.Error(t, err)
	_, err = agent.SetSessionConfigOption(ctx, acp.SetSessionConfigOptionRequest{
		Boolean: &acp.SetSessionConfigOptionBoolean{Meta: meta},
	})
	require.Error(t, err)
	_, err = agent.SetSessionConfigOption(ctx, acp.SetSessionConfigOptionRequest{
		ValueId: &acp.SetSessionConfigOptionValueId{Meta: meta},
	})
	require.Error(t, err)
}

func TestLifecyclePromptCorrelationDrainAndSettlementBranches(t *testing.T) {
	agent := NewAgent()
	agent.lifecycle = lifecycle.Negotiated{Versions: []int{1}, ActivityKinds: []lifecycle.ActivityKind{}}
	s := &session{agent: agent, id: "s"}
	_, err := s.Prompt(context.Background(), acp.PromptRequest{SessionId: "s", Meta: inboundRouteMeta("turn")})
	require.Error(t, err, "negotiated prompts require correlation before dispatch")

	closed := make(chan codex.Event)
	close(closed)
	require.NoError(t, s.drainDeliveredEvents(context.Background(), closed, &promptEventState{}))
	bad := make(chan codex.Event, 1)
	bad <- codex.Event{Kind: codex.EventError, Err: errors.New("native event failed")}
	close(bad)
	require.Error(t, s.drainDeliveredEvents(context.Background(), bad, &promptEventState{}))

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

		return &session{
			agent: agent, id: "s", cwd: "/tmp", codexThreadID: "thread",
			client: &runEventsClient{events: []codex.Event{{Kind: codex.EventCompleted, ThreadID: "thread", TurnID: "turn"}}},
		}
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

func TestLifecycleSettlementPersistenceContainmentEdges(t *testing.T) {
	ctx := context.Background()
	store := &appendFuncStore{}
	agent := NewAgent(WithSessionStore(store))
	s := &session{agent: agent, id: "s", rolloutLiveFenced: true}
	require.NoError(t, s.mirrorAndEmitRolloutWithCompletion(ctx, make(chan struct{}, 1), make(chan codex.Event, 1)))
	require.False(t, rolloutTaskComplete(SessionStoreEntry(`not-json`)))

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
}

func requireJSON(t *testing.T, value any) string {
	t.Helper()
	raw, err := json.Marshal(value)
	require.NoError(t, err)

	return string(raw)
}
