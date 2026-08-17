package codexacp

import (
	"context"
	"errors"
	"sort"
	"sync"
	"testing"

	"github.com/savid/acp-go-codex/internal/codex"
	"github.com/stretchr/testify/require"
)

func TestCancelContainsOnlyTargetThreadAndLeavesPeerTurnAlive(t *testing.T) {
	client := &threadScopedTerminalClient{
		spyCodexClient: newSpyCodexClient(),
		terminals: map[string]map[string]struct{}{
			"target-thread": {"target-1": {}, "target-2": {}},
			"peer-thread":   {"peer-1": {}},
		},
	}
	agent := NewAgent()
	agent.setAgentClient(newRecordingAgentClient())
	agent.runtimeClient = client

	target := newSession(agent, "target", t.TempDir(), nil, codex.Thread{ID: "target-thread"}, client, sessionMeta{}, nil)
	peer := newSession(agent, "peer", t.TempDir(), nil, codex.Thread{ID: "peer-thread"}, client, sessionMeta{}, nil)
	agent.sessions[target.id] = target
	agent.sessions[peer.id] = peer

	targetTurn := target.beginTurn(context.Background(), "target-nonce")
	target.setTurnID("target-turn")
	defer target.finishTurn()
	peerTurn := peer.beginTurn(context.Background(), "peer-nonce")
	peer.setTurnID("peer-turn")
	defer peer.finishTurn()

	require.NoError(t, agent.Cancel(context.Background(), CancelRequest(target.id, "target-nonce")))
	require.ErrorIs(t, targetTurn.Err(), context.Canceled)
	require.NoError(t, peerTurn.Err(), "target cancellation must not cancel the peer turn context")
	require.False(t, target.clientDead)
	require.False(t, peer.clientDead)

	client.terminalMu.Lock()
	require.Empty(t, client.terminals["target-thread"])
	require.Equal(t, map[string]struct{}{"peer-1": {}}, client.terminals["peer-thread"])
	require.Equal(t, []string{"target-thread/target-1", "target-thread/target-2"}, client.terminated)
	require.Equal(t, "target-thread", client.cancelThread)
	client.terminalMu.Unlock()

	client.mu.Lock()
	require.False(t, client.closed, "target cancellation must not close the shared runtime")
	client.mu.Unlock()
}

func TestTerminateThreadBackgroundTerminalsRequiresScopedNativeSurface(t *testing.T) {
	client := codex.NewPlaceholderClient(codex.Options{})
	err := terminateThreadBackgroundTerminals(context.Background(), client, "thread")
	require.ErrorContains(t, err, "required thread-scoped background terminal containment")
}

func TestCancelFencesGenerationWhenTargetedContainmentFails(t *testing.T) {
	containErr := errors.New("target containment failed")
	client := &failingThreadScopedTerminalClient{
		threadScopedTerminalClient: threadScopedTerminalClient{
			spyCodexClient: newSpyCodexClient(),
			terminals:      map[string]map[string]struct{}{"target-thread": {"target-1": {}}},
		},
		err: containErr,
	}
	agent := NewAgent()
	agent.setAgentClient(newRecordingAgentClient())
	agent.runtimeClient = client
	target := newSession(agent, "target", t.TempDir(), nil, codex.Thread{ID: "target-thread"}, client, sessionMeta{}, nil)
	peer := newSession(agent, "peer", t.TempDir(), nil, codex.Thread{ID: "peer-thread"}, client, sessionMeta{}, nil)
	agent.sessions[target.id] = target
	agent.sessions[peer.id] = peer
	target.beginTurn(context.Background(), "target-nonce")
	target.setTurnID("target-turn")
	defer target.finishTurn()

	err := agent.Cancel(context.Background(), CancelRequest(target.id, "target-nonce"))
	require.ErrorContains(t, err, containErr.Error())
	require.True(t, target.clientDead)
	require.True(t, peer.clientDead)
	client.mu.Lock()
	require.True(t, client.closed, "failed targeted containment must fence the accepting generation")
	client.mu.Unlock()
}

func TestListThreadBackgroundTerminalIDsRejectsRepeatedCursor(t *testing.T) {
	client := &repeatedBackgroundTerminalCursorClient{}
	_, err := listThreadBackgroundTerminalIDs(context.Background(), client, "thread")
	require.ErrorContains(t, err, "repeated cursor")
}

func TestListThreadBackgroundTerminalIDsRejectsInvalidAndFailedLists(t *testing.T) {
	_, err := listThreadBackgroundTerminalIDs(
		context.Background(),
		&scriptedBackgroundTerminalClient{},
		"",
	)
	require.ErrorContains(t, err, "threadId is required")

	listErr := errors.New("list failed")
	_, err = listThreadBackgroundTerminalIDs(
		context.Background(),
		&scriptedBackgroundTerminalClient{listSteps: []backgroundTerminalListStep{{err: listErr}}},
		"thread",
	)
	require.ErrorIs(t, err, listErr)

	_, err = listThreadBackgroundTerminalIDs(
		context.Background(),
		&scriptedBackgroundTerminalClient{listSteps: []backgroundTerminalListStep{{
			response: codex.BackgroundTerminalListResponse{
				Terminals: []codex.BackgroundTerminal{{ItemID: "missing-process"}},
			},
		}}},
		"thread",
	)
	require.ErrorContains(t, err, "missing processId")
}

func TestTerminateThreadBackgroundTerminalsHandlesVerificationEdges(t *testing.T) {
	listErr := errors.New("verification list failed")

	tests := []struct {
		name        string
		client      *scriptedBackgroundTerminalClient
		cancelFirst bool
		wantErr     error
		wantText    string
	}{
		{
			name: "initial list failure",
			client: &scriptedBackgroundTerminalClient{
				spyCodexClient: newSpyCodexClient(),
				listSteps:      []backgroundTerminalListStep{{err: listErr}},
			},
			wantErr: listErr,
		},
		{
			name: "verification list failure",
			client: &scriptedBackgroundTerminalClient{
				spyCodexClient: newSpyCodexClient(),
				listSteps: []backgroundTerminalListStep{
					{response: backgroundTerminalResponse("process")},
					{err: listErr},
				},
				terminateResults: []bool{false},
			},
			wantErr: listErr,
		},
		{
			name: "terminal exits before terminate",
			client: &scriptedBackgroundTerminalClient{
				spyCodexClient: newSpyCodexClient(),
				listSteps: []backgroundTerminalListStep{
					{response: backgroundTerminalResponse("process")},
					{},
				},
				terminateResults: []bool{false},
			},
		},
		{
			name: "terminal remains uncontained",
			client: &scriptedBackgroundTerminalClient{
				spyCodexClient: newSpyCodexClient(),
				listSteps: []backgroundTerminalListStep{
					{response: backgroundTerminalResponse("process")},
					{response: backgroundTerminalResponse("process")},
				},
				terminateResults: []bool{false},
			},
			wantText: "containment remained unproven",
		},
		{
			name: "context ends before verification wait",
			client: &scriptedBackgroundTerminalClient{
				spyCodexClient:   newSpyCodexClient(),
				listSteps:        []backgroundTerminalListStep{{response: backgroundTerminalResponse("process")}},
				terminateResults: []bool{true},
			},
			cancelFirst: true,
			wantErr:     context.Canceled,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			ctx := context.Background()
			if test.cancelFirst {
				var cancel context.CancelFunc
				ctx, cancel = context.WithCancel(ctx)
				cancel()
			}

			err := terminateThreadBackgroundTerminals(ctx, test.client, "thread")
			if test.wantErr != nil {
				require.ErrorIs(t, err, test.wantErr)
			}
			if test.wantText != "" {
				require.ErrorContains(t, err, test.wantText)
			}
			if test.wantErr == nil && test.wantText == "" {
				require.NoError(t, err)
			}
		})
	}
}

type threadScopedTerminalClient struct {
	*spyCodexClient

	terminalMu   sync.Mutex
	terminals    map[string]map[string]struct{}
	terminated   []string
	cancelThread string
}

func (c *threadScopedTerminalClient) CancelTurn(_ context.Context, threadID string, _ string) error {
	c.terminalMu.Lock()
	c.cancelThread = threadID
	c.terminalMu.Unlock()

	return nil
}

func (c *threadScopedTerminalClient) ListBackgroundTerminals(
	_ context.Context,
	req codex.BackgroundTerminalListRequest,
) (codex.BackgroundTerminalListResponse, error) {
	c.terminalMu.Lock()
	defer c.terminalMu.Unlock()

	processIDs := make([]string, 0, len(c.terminals[req.ThreadID]))
	for processID := range c.terminals[req.ThreadID] {
		processIDs = append(processIDs, processID)
	}
	sort.Strings(processIDs)

	if req.ThreadID == "target-thread" && len(processIDs) == 2 {
		if req.Cursor == "" {
			return codex.BackgroundTerminalListResponse{
				Terminals:  []codex.BackgroundTerminal{{ProcessID: processIDs[0]}},
				NextCursor: "target-page-2",
			}, nil
		}
		if req.Cursor == "target-page-2" {
			return codex.BackgroundTerminalListResponse{
				Terminals: []codex.BackgroundTerminal{{ProcessID: processIDs[1]}},
			}, nil
		}
	}

	result := codex.BackgroundTerminalListResponse{Terminals: make([]codex.BackgroundTerminal, 0, len(processIDs))}
	for _, processID := range processIDs {
		result.Terminals = append(result.Terminals, codex.BackgroundTerminal{ProcessID: processID})
	}

	return result, nil
}

func (c *threadScopedTerminalClient) TerminateBackgroundTerminal(
	_ context.Context,
	req codex.BackgroundTerminalTerminateRequest,
) (bool, error) {
	c.terminalMu.Lock()
	defer c.terminalMu.Unlock()

	if _, ok := c.terminals[req.ThreadID][req.ProcessID]; !ok {
		return false, nil
	}

	delete(c.terminals[req.ThreadID], req.ProcessID)
	c.terminated = append(c.terminated, req.ThreadID+"/"+req.ProcessID)

	return true, nil
}

type repeatedBackgroundTerminalCursorClient struct{}

type backgroundTerminalListStep struct {
	response codex.BackgroundTerminalListResponse
	err      error
}

type scriptedBackgroundTerminalClient struct {
	*spyCodexClient

	listSteps        []backgroundTerminalListStep
	terminateResults []bool
	terminateErrors  []error
}

func backgroundTerminalResponse(processID string) codex.BackgroundTerminalListResponse {
	return codex.BackgroundTerminalListResponse{
		Terminals: []codex.BackgroundTerminal{{ProcessID: processID}},
	}
}

func (c *scriptedBackgroundTerminalClient) ListBackgroundTerminals(
	context.Context,
	codex.BackgroundTerminalListRequest,
) (codex.BackgroundTerminalListResponse, error) {
	if len(c.listSteps) == 0 {
		return codex.BackgroundTerminalListResponse{}, nil
	}

	step := c.listSteps[0]
	c.listSteps = c.listSteps[1:]

	return step.response, step.err
}

func (c *scriptedBackgroundTerminalClient) TerminateBackgroundTerminal(
	context.Context,
	codex.BackgroundTerminalTerminateRequest,
) (bool, error) {
	var result bool
	if len(c.terminateResults) > 0 {
		result = c.terminateResults[0]
		c.terminateResults = c.terminateResults[1:]
	}

	var err error
	if len(c.terminateErrors) > 0 {
		err = c.terminateErrors[0]
		c.terminateErrors = c.terminateErrors[1:]
	}

	return result, err
}

type failingThreadScopedTerminalClient struct {
	threadScopedTerminalClient
	err error
}

func (c *failingThreadScopedTerminalClient) TerminateBackgroundTerminal(
	context.Context,
	codex.BackgroundTerminalTerminateRequest,
) (bool, error) {
	return false, c.err
}

func (*repeatedBackgroundTerminalCursorClient) ListBackgroundTerminals(
	context.Context,
	codex.BackgroundTerminalListRequest,
) (codex.BackgroundTerminalListResponse, error) {
	return codex.BackgroundTerminalListResponse{NextCursor: "same"}, nil
}

func (*repeatedBackgroundTerminalCursorClient) TerminateBackgroundTerminal(
	context.Context,
	codex.BackgroundTerminalTerminateRequest,
) (bool, error) {
	return false, errors.New("unexpected terminate")
}

var _ codex.BackgroundTerminalClient = (*threadScopedTerminalClient)(nil)
var _ codex.BackgroundTerminalClient = (*failingThreadScopedTerminalClient)(nil)
var _ codex.BackgroundTerminalClient = (*repeatedBackgroundTerminalCursorClient)(nil)
var _ codex.BackgroundTerminalClient = (*scriptedBackgroundTerminalClient)(nil)
var _ codex.Client = (*threadScopedTerminalClient)(nil)
var _ codex.Client = (*failingThreadScopedTerminalClient)(nil)
var _ codex.Client = (*scriptedBackgroundTerminalClient)(nil)
