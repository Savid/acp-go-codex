package codexacp

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
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

// The advertisement is the whole reachability condition for the version-1
// stream, so it is pinned as the exact bytes an initialize response carries and
// then followed through to the envelopes one real prompt delivers.
func TestLifecycleAdvertisementAnswersOfferAndOpensForegroundStream(t *testing.T) {
	ctx := context.Background()
	agent := NewAgent(withClientFactory(func(context.Context, codex.Options) (codex.Client, error) {
		return newSpyCodexClient(), nil
	}))
	recorder := newRecordingAgentClient()
	agent.setAgentClient(recorder)

	initialized, err := agent.Initialize(ctx, acp.InitializeRequest{
		Meta: map[string]any{lifecycle.MetaKey: map[string]any{"versions": []any{1.0}}},
	})
	require.NoError(t, err)
	require.JSONEq(
		t,
		`{"acp-go.dev/lifecycle":{"versions":[1],"updatesOutsidePrompt":false,`+
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
	require.Len(t, streams, 1, "one prompt opens exactly one incarnation")
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
	s := &session{agent: agent, id: "s", rolloutLiveFenced: true}
	require.NoError(t, s.mirrorAndEmitRolloutLive(ctx, make(chan codex.Event, 1)))

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
	agent.lifecycle = lifecycle.Negotiated{Versions: []int{1}, ActivityKinds: []lifecycle.ActivityKind{}}

	conn := newRecordingAgentClient()
	agent.setAgentClient(conn)

	created, err := agent.NewSession(context.Background(), NewSessionRequest(t.TempDir()))
	require.NoError(t, err)

	return agent, agent.activeSession(created.SessionId), conn
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
// live incarnation. This configuration opens one incarnation per prompt, so a
// close between prompts has no stream at all, and a close after a cancel has one
// the cancel already ended; either way the boundary emits nothing, because an
// event bearing a fenced streamId is exactly what a conforming reducer refuses.
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
		// what a close that terminalized on it would have had to send.
		require.ErrorIs(t,
			incarnation.emit(ctx, lifecycle.TransitionEvent(lifecycle.ForegroundIdle, incarnation.cycleID, incarnation.turnID)),
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

// TestCloseFailsWhenTheCapturedPrefixCannotBeCommitted pins the other half of the
// same rung: durability outranks the response, so a capture the store refuses
// fails the close instead of being dropped with the session wrapper. The session
// stays addressable so the host can drive the boundary again.
func TestCloseFailsWhenTheCapturedPrefixCannotBeCommitted(t *testing.T) {
	storeFailure := errors.New("store unavailable")
	refuse := false

	store := &appendFuncStore{append: func(context.Context, SessionKey, []SessionStoreEntry) error {
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

	session.mu.Lock()
	require.False(t, session.closing)
	session.mu.Unlock()

	refuse = false
	_, err = agent.CloseSession(context.Background(), acp.CloseSessionRequest{SessionId: session.id})
	require.NoError(t, err)
	require.Empty(t, session.unsyncedEntries)
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
