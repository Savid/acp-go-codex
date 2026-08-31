package codexacp

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync"

	"github.com/coder/acp-go-sdk"
	"github.com/savid/acp-go-codex/internal/codex"
)

func jsonString(value any) string {
	data, _ := json.Marshal(value)

	return string(data)
}

func containsAll(value string, needles ...string) bool {
	for _, needle := range needles {
		if !strings.Contains(value, needle) {
			return false
		}
	}

	return true
}

type runtimeFailureClient struct {
	*runtimeRecordingClient
	resumeErr error
	runErr    error
	events    []codex.Event
}

type threadScopedTerminalClient struct {
	*spyCodexClient
	terminalMu sync.Mutex
	terminals  map[string]map[string]struct{}
	terminated []string
}

func (c *threadScopedTerminalClient) ListBackgroundTerminals(_ context.Context, req codex.BackgroundTerminalListRequest) (codex.BackgroundTerminalListResponse, error) {
	c.terminalMu.Lock()
	defer c.terminalMu.Unlock()
	result := codex.BackgroundTerminalListResponse{}
	for processID := range c.terminals[req.ThreadID] {
		result.Terminals = append(result.Terminals, codex.BackgroundTerminal{ProcessID: processID})
	}

	return result, nil
}

func (c *threadScopedTerminalClient) TerminateBackgroundTerminal(_ context.Context, req codex.BackgroundTerminalTerminateRequest) (bool, error) {
	c.terminalMu.Lock()
	defer c.terminalMu.Unlock()
	if _, ok := c.terminals[req.ThreadID][req.ProcessID]; !ok {
		return false, nil
	}
	delete(c.terminals[req.ThreadID], req.ProcessID)
	c.terminated = append(c.terminated, req.ThreadID+"/"+req.ProcessID)

	return true, nil
}

func (c *runtimeFailureClient) ResumeThread(ctx context.Context, req codex.ThreadResumeRequest) (codex.Thread, error) {
	if c.resumeErr != nil {
		return codex.Thread{}, c.resumeErr
	}

	return c.runtimeRecordingClient.ResumeThread(ctx, req)
}

func (c *runtimeFailureClient) RunTurn(_ context.Context, req codex.TurnStartRequest) (codex.Turn, error) {
	if c.runErr != nil {
		return codex.Turn{}, c.runErr
	}
	if err := c.publishTurn(req.ThreadID, "turn", c.events); err != nil {
		return codex.Turn{}, err
	}

	return codex.Turn{ID: "turn"}, nil
}

type runtimeRecordingClient struct {
	*spyCodexClient

	mu         sync.Mutex
	starts     []codex.ThreadStartRequest
	resumes    []codex.ThreadResumeRequest
	turns      []codex.TurnStartRequest
	order      []string
	closeCount int
}

func newRuntimeRecordingClient() *runtimeRecordingClient {
	return &runtimeRecordingClient{spyCodexClient: newSpyCodexClient()}
}

func (c *runtimeRecordingClient) StartThread(_ context.Context, req codex.ThreadStartRequest) (codex.Thread, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.starts = append(c.starts, req)
	id := "thread-" + string(rune('1'+len(c.starts)-1))

	return codex.Thread{ID: id, Cwd: req.Cwd, Model: req.Model}, nil
}

func (c *runtimeRecordingClient) ResumeThread(_ context.Context, req codex.ThreadResumeRequest) (codex.Thread, error) {
	c.mu.Lock()
	c.resumes = append(c.resumes, req)
	c.order = append(c.order, "resume:"+req.ThreadID)
	c.mu.Unlock()

	return codex.Thread{ID: req.ThreadID, Cwd: req.Cwd}, nil
}

func (c *runtimeRecordingClient) RunTurn(_ context.Context, req codex.TurnStartRequest) (codex.Turn, error) {
	text, _ := req.Prompt[0]["text"].(string)
	c.mu.Lock()
	c.turns = append(c.turns, req)
	c.mu.Unlock()
	events := make([]codex.Event, 0, 2)
	if strings.Contains(text, "runtime_ready") {
		nonce := strings.SplitN(strings.SplitN(text, "nonce ", 2)[1], ".", 2)[0]
		c.mu.Lock()
		c.order = append(c.order, "canary:"+req.ThreadID)
		c.mu.Unlock()
		events = append(events, codex.Event{Kind: codex.EventToolCompleted, ThreadID: req.ThreadID, TurnID: "turn", Tool: codex.ToolEvent{Title: "runtime_ready", Content: nonce}})
	}
	events = append(events, codex.Event{Kind: codex.EventCompleted, ThreadID: req.ThreadID, TurnID: "turn", StopReason: codex.StopReasonEndTurn})
	if err := c.publishTurn(req.ThreadID, "turn", events); err != nil {
		return codex.Turn{}, err
	}

	return codex.Turn{ID: "turn"}, nil
}

func (c *runtimeRecordingClient) Close(context.Context) error {
	c.mu.Lock()
	c.closeCount++
	c.mu.Unlock()

	return nil
}

func isTurnFailure(err error, cause string) bool {
	var reqErr *acp.RequestError
	if !errors.As(err, &reqErr) || reqErr.Code != -32603 {
		return false
	}
	data, ok := reqErr.Data.(map[string]any)

	return ok && data[jsonFieldError] == valueTurnFailed && data[jsonFieldCause] == cause
}
