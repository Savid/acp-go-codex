package codexacp

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/coder/acp-go-sdk"
	"github.com/savid/acp-go-codex/internal/codex"
	"github.com/stretchr/testify/require"
)

// turnFailureData returns the data map of a codex_turn_failed JSON-RPC error, or
// fails the test when err is not the uniform turn-failure error.
func turnFailureData(t *testing.T, err error) map[string]any {
	t.Helper()

	var reqErr *acp.RequestError
	if !errors.As(err, &reqErr) {
		t.Fatalf("error %v is not an acp.RequestError", err)
	}

	data, ok := reqErr.Data.(map[string]any)
	if !ok {
		t.Fatalf("error data %#v is not a map", reqErr.Data)
	}

	if data[jsonFieldError] != valueTurnFailed {
		t.Fatalf("error tag = %v, want %s", data[jsonFieldError], valueTurnFailed)
	}

	return data
}

// isTurnFailure reports whether err is the uniform codex_turn_failed error with
// the given cause and JSON-RPC code -32603.
func isTurnFailure(err error, cause string) bool {
	var reqErr *acp.RequestError
	if !errors.As(err, &reqErr) || reqErr.Code != -32603 {
		return false
	}

	data, ok := reqErr.Data.(map[string]any)
	if !ok {
		return false
	}

	return data[jsonFieldError] == valueTurnFailed && data[jsonFieldCause] == cause
}

// sequencedClientFactory hands out pre-built clients in order, so a test can
// simulate a native app-server that dies and is relaunched lazily.
func sequencedClientFactory(clients ...codex.Client) func(context.Context, codex.Options) (codex.Client, error) {
	index := 0

	return func(context.Context, codex.Options) (codex.Client, error) {
		if index >= len(clients) {
			return nil, errors.New("no more sequenced clients")
		}

		client := clients[index]
		index++

		return client, nil
	}
}

// T1 — provider error surfaces as a structured failure, never end_turn.
func TestTurnFailureProviderError(t *testing.T) {
	ctx := context.Background()

	for _, tc := range []struct {
		name       string
		failure    *codex.TurnFailedError
		wantCode   int
		wantCause  string
		wantStatus int
	}{
		{
			name:       "rate limit",
			failure:    &codex.TurnFailedError{Cause: codex.CauseProvider, Message: "rate limited by upstream", StatusCode: 429, ProviderCode: "rate_limit"},
			wantCode:   -32603,
			wantCause:  codex.CauseProvider,
			wantStatus: 429,
		},
		{
			name:      "auth",
			failure:   &codex.TurnFailedError{Cause: codex.CauseProvider, Message: "unauthorized: 401 from provider", StatusCode: 401},
			wantCode:  -32000,
			wantCause: "",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			client := &runEventsClient{spyCodexClient: newSpyCodexClient(), events: []codex.Event{{
				Kind:       codex.EventCompleted,
				ThreadID:   "thread-1",
				TurnID:     "turn-1",
				StopReason: codex.StopReasonError,
				Err:        tc.failure,
			}}}
			agent := NewAgent(withClientFactory(sequencedClientFactory(client)))
			agent.setAgentClient(newRecordingAgentClient())

			resp, err := agent.NewSession(ctx, NewSessionRequest("/tmp/project"))
			if err != nil {
				t.Fatalf("NewSession returned error: %v", err)
			}

			promptResp, promptErr := agent.Prompt(ctx, TextPromptRequest(resp.SessionId, "test-turn", "hi"))
			if promptResp.StopReason == acp.StopReasonEndTurn {
				t.Fatal("failed turn reported end_turn")
			}

			var reqErr *acp.RequestError
			if !errors.As(promptErr, &reqErr) || reqErr.Code != tc.wantCode {
				t.Fatalf("prompt error = %v, want code %d", promptErr, tc.wantCode)
			}

			if tc.wantCode == -32000 {
				// The auth carve-out still carries the uniform turn-failure
				// envelope alongside the additive _meta.codex.auth block.
				authData, isMap := reqErr.Data.(map[string]any)
				if !isMap {
					t.Fatalf("auth data = %#v", reqErr.Data)
				}
				if authData[jsonFieldError] != valueTurnFailed {
					t.Fatalf("auth error tag = %v, want %s", authData[jsonFieldError], valueTurnFailed)
				}
				if authData[jsonFieldCause] != codex.CauseProvider {
					t.Fatalf("auth cause = %v, want provider", authData[jsonFieldCause])
				}
				if msg, _ := authData[jsonFieldMessage].(string); !strings.Contains(msg, "unauthorized") {
					t.Fatalf("auth message = %v, want native cause", authData[jsonFieldMessage])
				}
				if asType[map[string]any](t, authData[codexMetaKey])[authMetaAuthKey] == nil {
					t.Fatalf("auth data missing _meta.codex.auth: %#v", authData)
				}

				return
			}

			data := turnFailureData(t, promptErr)
			if data[jsonFieldCause] != tc.wantCause {
				t.Fatalf("cause = %v, want %s", data[jsonFieldCause], tc.wantCause)
			}
			if msg, _ := data[jsonFieldMessage].(string); !strings.Contains(msg, "rate limited") {
				t.Fatalf("message = %v, want injected cause", data[jsonFieldMessage])
			}
			if data[jsonFieldStatusCode] != tc.wantStatus {
				t.Fatalf("statusCode = %v, want %d", data[jsonFieldStatusCode], tc.wantStatus)
			}
			if data[jsonFieldProviderCode] != "rate_limit" {
				t.Fatalf("providerCode = %v", data[jsonFieldProviderCode])
			}
		})
	}
}

// T2 — a transport error mid-turn surfaces the real cause, never a bare EOF.
func TestTurnFailureTransportRecoversCause(t *testing.T) {
	ctx := context.Background()
	client := &runEventsClient{spyCodexClient: newSpyCodexClient(), events: []codex.Event{{
		Kind:     codex.EventError,
		ThreadID: "thread-1",
		TurnID:   "turn-1",
		Err:      fmt.Errorf("%w: upstream reset the stream", codex.ErrConnectionClosed),
	}}}
	agent := NewAgent(withClientFactory(sequencedClientFactory(client)))
	agent.setAgentClient(newRecordingAgentClient())

	resp, err := agent.NewSession(ctx, NewSessionRequest("/tmp/project"))
	if err != nil {
		t.Fatalf("NewSession returned error: %v", err)
	}

	_, promptErr := agent.Prompt(ctx, TextPromptRequest(resp.SessionId, "test-turn", "hi"))
	if !isTurnFailure(promptErr, codex.CauseTransport) {
		t.Fatalf("prompt error = %v, want transport failure", promptErr)
	}

	data := turnFailureData(t, promptErr)
	msg, _ := data[jsonFieldMessage].(string)
	if !strings.Contains(msg, "upstream reset the stream") {
		t.Fatalf("message = %q, want real transport cause", msg)
	}
	if msg == "EOF" {
		t.Fatal("message is a bare EOF")
	}
}

// The process-exit mapping itself is platform-independent: a ProcessExitError
// reported mid-turn must surface cause:"process_exit" with the real exit status
// and stderr tail, and must mark the session for lazy relaunch. T3 proves the
// same contract against a real dying app-server where that fixture can run.
func TestTurnFailureProcessExitMapping(t *testing.T) {
	ctx := context.Background()

	client := &runEventsClient{spyCodexClient: newSpyCodexClient(), events: []codex.Event{{
		Kind:     codex.EventError,
		ThreadID: "thread-1",
		TurnID:   "turn-1",
		Err: &codex.ProcessExitError{
			Status:     "exit status 1",
			StderrTail: "out of memory",
			Err:        errors.New("EOF"),
		},
	}}}
	agent := NewAgent(withClientFactory(sequencedClientFactory(client)))
	agent.setAgentClient(newRecordingAgentClient())

	resp, err := agent.NewSession(ctx, NewSessionRequest("/tmp/project"))
	require.NoError(t, err)

	promptResp, promptErr := agent.Prompt(ctx, TextPromptRequest(resp.SessionId, "test-turn", "hi"))
	require.NotEqual(t, acp.StopReasonEndTurn, promptResp.StopReason)
	require.True(t, isTurnFailure(promptErr, codex.CauseProcessExit), "prompt error = %v", promptErr)

	data := turnFailureData(t, promptErr)
	message, _ := data[jsonFieldMessage].(string)
	require.Contains(t, message, "exit status 1")
	require.Contains(t, message, "out of memory")
	require.NotEqual(t, "EOF", message)

	session := agent.activeSession(resp.SessionId)
	require.NotNil(t, session)
	_, _, clientDead := session.closeState()
	require.True(t, clientDead, "process death did not mark the session for lazy relaunch")
}

// T3 — a real app-server process death mid-turn surfaces cause:"process_exit"
// with the true exit status and stderr tail (never a bare EOF) and marks the
// session for lazy relaunch. The fake app-server is this test binary re-execed
// via TestMain; it dies on turn/start.
func TestTurnFailureProcessDeath(t *testing.T) {
	skipUnprivilegedDarwinIsolation(t)
	ctx := context.Background()

	isolation := testProcessIsolation()
	client, err := codex.NewAppServerClient(ctx, codex.Options{
		CLIPath:          testReachableExecutable(t),
		CodexHome:        testNativeOwnedTempDir(t),
		SupervisorRoot:   testTraversableTempDir(t),
		SupervisorParent: testTraversableTempDir(t),
		DarwinBestEffort: runtime.GOOS == "darwin",
		NativeVersion:    "0.144.1",
		ProcessIsolation: &codex.ProcessIsolation{
			UID: isolation.UID, GID: isolation.GID, BaseEnvironment: isolation.BaseEnvironment,
			StandaloneOwnerID: isolation.StandaloneOwnerID, StandaloneStateRoot: isolation.StandaloneStateRoot,
		},
	})
	if err != nil {
		t.Fatalf("launch fake app-server: %v", err)
	}

	s := &session{agent: NewAgent(), id: "death", cwd: "/tmp/project", codexThreadID: "thread-1"}
	s.agent.setAgentClient(newRecordingAgentClient())
	s.client = client
	t.Cleanup(func() { _ = client.Close(context.Background()) })

	resp, promptErr := s.Prompt(ctx, TextPromptRequest("death", "test-turn", "hi"))
	if resp.StopReason == acp.StopReasonEndTurn {
		t.Fatal("process death reported end_turn")
	}
	if !isTurnFailure(promptErr, codex.CauseProcessExit) {
		t.Fatalf("prompt error = %v, want -32603 process_exit failure", promptErr)
	}

	data := turnFailureData(t, promptErr)
	msg, _ := data[jsonFieldMessage].(string)
	if !strings.Contains(msg, "exit status 1") {
		t.Fatalf("message = %q, want the real exit status", msg)
	}
	if !strings.Contains(msg, "out of memory") {
		t.Fatalf("message = %q, want the stderr tail", msg)
	}
	if msg == "EOF" {
		t.Fatal("process-death message is a bare EOF")
	}

	if !s.clientDead {
		t.Fatal("process death did not mark the session for lazy relaunch")
	}
}

// A dead session relaunches the app-server lazily on the next prompt and resumes
// the native thread, so the turn succeeds.
func TestPromptRelaunchesDeadClient(t *testing.T) {
	ctx := context.Background()

	relaunched := newSpyCodexClient()
	agent := NewAgent(withClientFactory(func(context.Context, codex.Options) (codex.Client, error) {
		return relaunched, nil
	}))
	old := newSpyCodexClient()
	s := &session{
		agent:         agent,
		id:            "relaunch-ok",
		cwd:           "/tmp/project",
		codexThreadID: "thread-1",
		clientDead:    true,
		client:        old,
	}
	s.agent.setAgentClient(newRecordingAgentClient())
	agent.sessions[s.id] = s
	agent.runtimeClient = old
	agent.runtimeDead = true
	agent.runtimeNativeRelease = func() {}

	resp, err := s.Prompt(ctx, TextPromptRequest("relaunch-ok", "test-turn", "again"))
	if err != nil {
		t.Fatalf("relaunched prompt returned error: %v", err)
	}
	if resp.StopReason != acp.StopReasonEndTurn {
		t.Fatalf("relaunched prompt stop reason = %v, want end_turn", resp.StopReason)
	}
	if relaunched.resume.ThreadID != "thread-1" {
		t.Fatalf("relaunch resumed thread %q, want thread-1", relaunched.resume.ThreadID)
	}
	if s.clientDead {
		t.Fatal("successful relaunch left the session marked dead")
	}
}

// T5 — a native error observed while cancelled maps to cancelled, not a failure.
func TestTurnFailureCancelNotConflated(t *testing.T) {
	cancelSession := &session{agent: NewAgent(), id: "cancel", cwd: "/tmp/project", codexThreadID: "thread"}
	cancelSession.agent.setAgentClient(newRecordingAgentClient())
	cancelSession.client = &cancelDuringRunClient{spyCodexClient: newSpyCodexClient(), session: cancelSession}

	resp, err := cancelSession.Prompt(context.Background(), TextPromptRequest("cancel", "test-turn", "hi"))
	if err != nil {
		t.Fatalf("cancelled turn returned error: %v", err)
	}
	if resp.StopReason != acp.StopReasonCancelled {
		t.Fatalf("stop reason = %v, want cancelled", resp.StopReason)
	}
}

// R5-2 — a user cancel coincident with turn-timeout expiry resolves to
// cancelled, never a timeout failure. The cancel guard runs before all failure
// mapping, and the timeout abort never fires (no double-send).
func TestTurnCancelWinsOnTimeoutCoincidence(t *testing.T) {
	coincideSession := &session{
		agent:         NewAgent(WithTurnTimeout(time.Nanosecond)),
		id:            "coincide",
		cwd:           "/tmp/project",
		codexThreadID: "thread",
	}
	coincideSession.agent.setAgentClient(newRecordingAgentClient())
	client := &cancelAtTurnStartClient{spyCodexClient: newSpyCodexClient(), session: coincideSession}
	coincideSession.client = client

	resp, err := coincideSession.Prompt(context.Background(), TextPromptRequest("coincide", "test-turn", "hi"))
	if err != nil {
		t.Fatalf("coincident cancel+timeout returned error: %v", err)
	}
	if resp.StopReason != acp.StopReasonCancelled {
		t.Fatalf("stop reason = %v, want cancelled", resp.StopReason)
	}
	if client.interrupted() {
		t.Fatal("timeout abort fired even though the cancel won (double-send)")
	}
}

// cancelAtTurnStartClient fires a user cancel as the turn begins, coincident with
// an expiring turn timeout, and records whether the timeout abort (turn/interrupt)
// was subsequently invoked.
type cancelAtTurnStartClient struct {
	*spyCodexClient
	session   *session
	cancelled bool
}

func (c *cancelAtTurnStartClient) RunTurn(context.Context, codex.TurnStartRequest) (codex.Turn, error) {
	c.session.cancelTurn()

	return codex.Turn{ID: "turn", Events: make(chan codex.Event)}, nil
}

func (c *cancelAtTurnStartClient) CancelTurn(context.Context, string, string) error {
	c.mu.Lock()
	c.cancelled = true
	c.mu.Unlock()

	return nil
}

func (c *cancelAtTurnStartClient) interrupted() bool {
	c.mu.Lock()
	defer c.mu.Unlock()

	return c.cancelled
}

// T6 — a turn that exceeds WithTurnTimeout fails with cause timeout, not cancel.
func TestTurnFailureTimeout(t *testing.T) {
	interrupt := &recordingCancelClient{spyCodexClient: newSpyCodexClient()}
	timeoutSession := &session{
		agent:         NewAgent(WithTurnTimeout(40 * time.Millisecond)),
		id:            "timeout",
		cwd:           "/tmp/project",
		codexThreadID: "thread",
		client:        interrupt,
	}
	timeoutSession.agent.setAgentClient(newRecordingAgentClient())

	resp, err := timeoutSession.Prompt(context.Background(), TextPromptRequest("timeout", "test-turn", "hi"))
	if resp.StopReason == acp.StopReasonCancelled {
		t.Fatal("timeout reported as cancelled")
	}
	if !isTurnFailure(err, codex.CauseTimeout) {
		t.Fatalf("timeout error = %v, want timeout failure", err)
	}
	if !interrupt.interrupted() {
		t.Fatal("timeout did not abort the native turn")
	}
}

func TestTurnFailureTimeoutIncludesTerminalContainmentFailure(t *testing.T) {
	containErr := errors.New("terminal containment failed")
	interrupt := &terminalCleanupErrorClient{
		recordingCancelClient: recordingCancelClient{spyCodexClient: newSpyCodexClient()},
		err:                   containErr,
	}
	agent := NewAgent(WithTurnTimeout(time.Nanosecond))
	timeoutSession := &session{
		agent:         agent,
		id:            "timeout-close-error",
		cwd:           "/tmp/project",
		codexThreadID: "thread",
		client:        interrupt,
	}
	agent.setAgentClient(newRecordingAgentClient())
	agent.sessions[timeoutSession.id] = timeoutSession
	agent.runtimeClient = interrupt

	_, err := timeoutSession.Prompt(
		context.Background(),
		TextPromptRequest(timeoutSession.id, "test-turn", "hi"),
	)
	require.True(t, isTurnFailure(err, codex.CauseTimeout))
	require.ErrorContains(t, err, containErr.Error())
	require.True(t, timeoutSession.clientDead)
	interrupt.mu.Lock()
	require.True(t, interrupt.closed, "failed targeted containment must fence the shared runtime")
	interrupt.mu.Unlock()
}

func TestTurnTimeoutMirrorsRowsWrittenDuringRuntimeQuiescence(t *testing.T) {
	root := t.TempDir()
	rolloutPath := filepath.Join(root, "rollout.jsonl")
	require.NoError(t, appendFakeCodexRolloutRow(
		rolloutPath,
		`{"type":"session_meta","payload":{"id":"thread"}}`,
	))

	store := NewInMemorySessionStore()
	interrupt := &lateRolloutCloseClient{
		recordingCancelClient: recordingCancelClient{spyCodexClient: newSpyCodexClient()},
		rolloutPath:           rolloutPath,
	}
	agent := NewAgent(WithTurnTimeout(time.Nanosecond), WithSessionStore(store))
	timeoutSession := &session{
		agent: agent, id: "timeout-late-rollout", cwd: root,
		codexThreadID: "thread", rolloutPath: rolloutPath, client: interrupt,
	}
	agent.setAgentClient(newRecordingAgentClient())
	agent.sessions[timeoutSession.id] = timeoutSession
	agent.runtimeClient = interrupt

	_, err := timeoutSession.Prompt(
		context.Background(),
		TextPromptRequest(timeoutSession.id, "test-turn", "hi"),
	)
	require.True(t, isTurnFailure(err, codex.CauseTimeout))
	entries, loadErr := store.Load(context.Background(), SessionKey{SessionID: string(timeoutSession.id)})
	require.NoError(t, loadErr)
	require.Contains(t, string(joinSessionStoreEntries(entries)), fakeCodexLateAbortRolloutRow)
}

// recordingCancelClient hangs the turn until its context is cancelled and
// records whether CancelTurn (turn/interrupt) was invoked.
type recordingCancelClient struct {
	*spyCodexClient
	cancelled bool
	closeErr  error
}

type lateRolloutCloseClient struct {
	recordingCancelClient
	rolloutPath string
	terminated  bool
}

func (c *lateRolloutCloseClient) ListBackgroundTerminals(
	context.Context,
	codex.BackgroundTerminalListRequest,
) (codex.BackgroundTerminalListResponse, error) {
	if c.terminated {
		return codex.BackgroundTerminalListResponse{}, nil
	}

	return codex.BackgroundTerminalListResponse{
		Terminals: []codex.BackgroundTerminal{{ProcessID: "late-rollout-terminal"}},
	}, nil
}

func (c *lateRolloutCloseClient) TerminateBackgroundTerminal(
	context.Context,
	codex.BackgroundTerminalTerminateRequest,
) (bool, error) {
	c.terminated = true

	return true, appendFakeCodexRolloutRow(c.rolloutPath, fakeCodexLateAbortRolloutRow)
}

type terminalCleanupErrorClient struct {
	recordingCancelClient
	err error
}

func (c *terminalCleanupErrorClient) ListBackgroundTerminals(
	context.Context,
	codex.BackgroundTerminalListRequest,
) (codex.BackgroundTerminalListResponse, error) {
	return codex.BackgroundTerminalListResponse{
		Terminals: []codex.BackgroundTerminal{{ProcessID: "target-process"}},
	}, nil
}

func (c *terminalCleanupErrorClient) TerminateBackgroundTerminal(
	context.Context,
	codex.BackgroundTerminalTerminateRequest,
) (bool, error) {
	return false, c.err
}

func (c *recordingCancelClient) RunTurn(ctx context.Context, _ codex.TurnStartRequest) (codex.Turn, error) {
	out := make(chan codex.Event)
	go func() {
		defer close(out)
		<-ctx.Done()
	}()

	return codex.Turn{ID: "turn", Events: out}, nil
}

func (c *recordingCancelClient) CancelTurn(context.Context, string, string) error {
	c.mu.Lock()
	c.cancelled = true
	c.mu.Unlock()

	return nil
}

func (c *recordingCancelClient) interrupted() bool {
	c.mu.Lock()
	defer c.mu.Unlock()

	return c.cancelled
}

func (c *recordingCancelClient) Close(context.Context) error {
	c.mu.Lock()
	c.closed = true
	c.mu.Unlock()

	return c.closeErr
}

func (c *recordingCancelClient) UnsubscribeThread(context.Context, string) error { return nil }
func (c *recordingCancelClient) DeleteThread(context.Context, codex.ThreadDeleteRequest) error {
	return nil
}

func TestMapTurnFailureBranches(t *testing.T) {
	failSession := &session{agent: NewAgent(), id: "map"}

	unknown := failSession.mapTurnFailure(errors.Join(codex.ErrThreadNotFound, errors.New("drift")))

	var reqErr *acp.RequestError
	if !errors.As(unknown, &reqErr) || reqErr.Code != -32602 {
		t.Fatalf("thread-not-found mapped to %v, want -32602 unknown session", unknown)
	}

	auth := failSession.mapTurnFailure(errors.New("unauthorized: 401"))
	if !errors.As(auth, &reqErr) || reqErr.Code != -32000 {
		t.Fatalf("auth failure mapped to %v, want -32000", auth)
	}

	provider := failSession.mapTurnFailure(errors.New("weird provider glitch"))
	data := turnFailureData(t, provider)
	if data[jsonFieldCause] != codex.CauseProvider {
		t.Fatalf("generic failure cause = %v, want provider", data[jsonFieldCause])
	}
	if failSession.clientDead {
		t.Fatal("provider failure must not mark the client dead")
	}
}
