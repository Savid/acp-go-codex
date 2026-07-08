package codexacp

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/coder/acp-go-sdk"
	"github.com/savid/acp-go-codex/internal/codex"
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

			promptResp, promptErr := agent.Prompt(ctx, TextPromptRequest(resp.SessionId, "hi"))
			if promptResp.StopReason == acp.StopReasonEndTurn {
				t.Fatal("failed turn reported end_turn")
			}

			var reqErr *acp.RequestError
			if !errors.As(promptErr, &reqErr) || reqErr.Code != tc.wantCode {
				t.Fatalf("prompt error = %v, want code %d", promptErr, tc.wantCode)
			}

			if tc.wantCode != -32603 {
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

	_, promptErr := agent.Prompt(ctx, TextPromptRequest(resp.SessionId, "hi"))
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

// T3 — a process death mid-turn leaves the session addressable; the follow-up
// prompt relaunches the app-server lazily.
func TestTurnFailureProcessDeathThenRelaunch(t *testing.T) {
	ctx := context.Background()

	dead := &runEventsClient{spyCodexClient: newSpyCodexClient(), events: []codex.Event{{
		Kind:     codex.EventError,
		ThreadID: "thread-1",
		TurnID:   "turn-1",
		Err:      &codex.TurnFailedError{Cause: codex.CauseProcessExit, Message: "codex app-server exited: exit status 1: killed (OOM)"},
	}}}
	relaunched := newSpyCodexClient()

	agent := NewAgent(withClientFactory(sequencedClientFactory(dead, relaunched)))
	agent.setAgentClient(newRecordingAgentClient())

	resp, err := agent.NewSession(ctx, NewSessionRequest("/tmp/project"))
	if err != nil {
		t.Fatalf("NewSession returned error: %v", err)
	}

	_, promptErr := agent.Prompt(ctx, TextPromptRequest(resp.SessionId, "hi"))
	if !isTurnFailure(promptErr, codex.CauseProcessExit) {
		t.Fatalf("prompt error = %v, want process_exit failure", promptErr)
	}

	data := turnFailureData(t, promptErr)
	if msg, _ := data[jsonFieldMessage].(string); !strings.Contains(msg, "OOM") {
		t.Fatalf("message = %v, want exit/stderr cause", data[jsonFieldMessage])
	}

	if _, sessionErr := agent.session(resp.SessionId); sessionErr != nil {
		t.Fatalf("session not addressable after process death: %v", sessionErr)
	}

	followResp, followErr := agent.Prompt(ctx, TextPromptRequest(resp.SessionId, "again"))
	if followErr != nil {
		t.Fatalf("follow-up prompt after relaunch returned error: %v", followErr)
	}
	if followResp.StopReason != acp.StopReasonEndTurn {
		t.Fatalf("relaunched prompt stop reason = %v", followResp.StopReason)
	}
	if relaunched.resume.ThreadID == "" {
		t.Fatal("relaunch did not resume the native thread")
	}
}

// T5 — a native error observed while cancelled maps to cancelled, not a failure.
func TestTurnFailureCancelNotConflated(t *testing.T) {
	cancelSession := &session{agent: NewAgent(), id: "cancel", cwd: "/tmp/project", codexThreadID: "thread"}
	cancelSession.agent.setAgentClient(newRecordingAgentClient())
	cancelSession.client = &cancelDuringRunClient{spyCodexClient: newSpyCodexClient(), session: cancelSession}

	resp, err := cancelSession.Prompt(context.Background(), TextPromptRequest("cancel", "hi"))
	if err != nil {
		t.Fatalf("cancelled turn returned error: %v", err)
	}
	if resp.StopReason != acp.StopReasonCancelled {
		t.Fatalf("stop reason = %v, want cancelled", resp.StopReason)
	}
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

	resp, err := timeoutSession.Prompt(context.Background(), TextPromptRequest("timeout", "hi"))
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

// recordingCancelClient hangs the turn until its context is cancelled and
// records whether CancelTurn (turn/interrupt) was invoked.
type recordingCancelClient struct {
	*spyCodexClient
	cancelled bool
}

func (c *recordingCancelClient) RunTurn(ctx context.Context, _ codex.TurnStartRequest) (<-chan codex.Event, error) {
	out := make(chan codex.Event)
	go func() {
		defer close(out)
		<-ctx.Done()
	}()

	return out, nil
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

func (c *recordingCancelClient) UnsubscribeThread(context.Context, string) error { return nil }
func (c *recordingCancelClient) DeleteThread(context.Context, codex.ThreadDeleteRequest) error {
	return nil
}
func (c *recordingCancelClient) Close(context.Context) error { return nil }
