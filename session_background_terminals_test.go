package codexacp

import (
	"context"
	"errors"
	"io"
	"sort"
	"sync"
	"testing"
	"time"

	"github.com/coder/acp-go-sdk"
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
	require.NotErrorIs(t, err, codex.ErrContainmentIncomplete,
		"an unoffered sweep selects no boundary, so the caller's fence classifies the result")
}

// soleFailedSweepAgent builds one logical session whose thread-scoped sweep is
// offered and always fails, on a generation no peer owns. The fence therefore
// succeeds, and the only thing left to report the boundary incomplete is the
// sweep's own result.
func soleFailedSweepAgent(t *testing.T, sweepFailure error) (*Agent, *failingBackgroundTerminalClient, *session) {
	t.Helper()

	client := &failingBackgroundTerminalClient{spyCodexClient: newSpyCodexClient(), err: sweepFailure}
	agent := NewAgent()
	agent.setAgentClient(newRecordingAgentClient())
	agent.runtimeClient = client

	target := newSession(agent, "target", t.TempDir(), nil, codex.Thread{ID: "target-thread"}, client, sessionMeta{}, nil)
	agent.sessions[target.id] = target

	return agent, client, target
}

// TestFailedSweepReportsIncompleteContainmentOnEverySurface pins the family's
// stable discriminator to the boundary that did not complete. The offered sweep
// failed, so this session's descendants were never proved gone — and the
// generation fence behind it succeeding says nothing about them, because it
// retires the source rather than the processes. session/close, Agent.Close, and
// Serve each owe the host an error matching ErrContainmentIncomplete.
func TestFailedSweepReportsIncompleteContainmentOnEverySurface(t *testing.T) {
	sweepFailure := errors.New("thread-scoped containment failed")

	t.Run("session/close", func(t *testing.T) {
		agent, client, target := soleFailedSweepAgent(t, sweepFailure)

		_, err := agent.CloseSession(context.Background(), acp.CloseSessionRequest{SessionId: target.id})
		require.ErrorIs(t, err, sweepFailure)
		require.ErrorIs(t, err, codex.ErrContainmentIncomplete)

		client.mu.Lock()
		require.True(t, client.closed, "the sole owner's generation fence completed")
		client.mu.Unlock()
	})

	t.Run("Agent.Close", func(t *testing.T) {
		agent, client, _ := soleFailedSweepAgent(t, sweepFailure)

		err := agent.Close()
		require.ErrorIs(t, err, sweepFailure)
		require.ErrorIs(t, err, codex.ErrContainmentIncomplete)

		client.mu.Lock()
		require.True(t, client.closed)
		client.mu.Unlock()
	})

	t.Run("Serve", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()

		clientToAgentReader, clientToAgentWriter := io.Pipe()
		agentToClientReader, agentToClientWriter := io.Pipe()

		t.Cleanup(func() {
			_ = clientToAgentReader.Close()
			_ = clientToAgentWriter.Close()
			_ = agentToClientReader.Close()
			_ = agentToClientWriter.Close()
		})

		served := make(chan error, 1)

		go func() {
			served <- Serve(ctx, clientToAgentReader, agentToClientWriter,
				withClientFactory(func(context.Context, codex.Options) (codex.Client, error) {
					return &failingBackgroundTerminalClient{spyCodexClient: newSpyCodexClient(), err: sweepFailure}, nil
				}))
		}()

		peer := acp.NewClientSideConnection(&recordingClient{}, clientToAgentWriter, agentToClientReader)
		_, err := peer.Initialize(ctx, acp.InitializeRequest{})
		require.NoError(t, err)
		_, err = peer.NewSession(ctx, acp.NewSessionRequest{Cwd: t.TempDir(), McpServers: []acp.McpServer{}})
		require.NoError(t, err)

		// The loop stops on context cancellation, and the close error still wins:
		// a host classifying the terminal result must see the boundary's verdict
		// rather than the reason the loop ended.
		cancel()

		select {
		case err := <-served:
			require.ErrorIs(t, err, sweepFailure)
			require.ErrorIs(t, err, codex.ErrContainmentIncomplete)
		case <-time.After(30 * time.Second):
			t.Fatal("Serve did not return after cancellation")
		}
	})
}

func TestCloseRefusesUnsupportedContainmentWithoutDestroyingPeer(t *testing.T) {
	client := &unsupportedBackgroundTerminalClient{spyCodexClient: newSpyCodexClient()}
	agent := NewAgent()
	agent.runtimeClient = client
	target := newSession(agent, "target", t.TempDir(), nil, codex.Thread{ID: "target-thread"}, client, sessionMeta{}, nil)
	peer := newSession(agent, "peer", t.TempDir(), nil, codex.Thread{ID: "peer-thread"}, client, sessionMeta{}, nil)
	agent.sessions[target.id] = target
	agent.sessions[peer.id] = peer

	targetTurn := target.beginTurn(context.Background(), "target-nonce")
	target.setTurnID("target-turn")
	peerTurn := peer.beginTurn(context.Background(), "peer-nonce")
	peer.setTurnID("peer-turn")

	_, err := agent.CloseSession(context.Background(), acp.CloseSessionRequest{SessionId: target.id})
	require.ErrorIs(t, err, codex.ErrBackgroundTerminalsUnsupported)
	require.ErrorIs(t, err, codex.ErrContainmentIncomplete)
	require.Same(t, client, agent.runtimeClient)
	require.True(t, target.clientDead)
	require.False(t, peer.clientDead)
	require.ErrorIs(t, targetTurn.Err(), context.Canceled)
	require.NoError(t, peerTurn.Err(), "target close must leave the peer turn alive")
	require.Same(t, target, agent.activeSession(target.id), "failed close must retain target ownership")

	client.mu.Lock()
	require.False(t, client.closed, "failed target close must not close the shared runtime")
	client.mu.Unlock()

	target.finishTurn()
	_, err = agent.UnstableDeleteSession(context.Background(), DeleteSessionRequest(target.id))
	require.Error(t, err)
	require.True(t, agent.isDeleted(target.id),
		"the tombstone precedes teardown, so a failed containment reports with the id already hidden")
	require.Same(t, target, agent.activeSession(target.id),
		"a hidden id keeps its native scope so a later delete can finish the teardown")

	peer.finishTurn()
	require.NoError(t, agent.Close())
}

func TestDeleteRetainedThreadRefusesUnsupportedContainmentWithActivePeer(t *testing.T) {
	client := &unsupportedBackgroundTerminalClient{spyCodexClient: newSpyCodexClient()}
	agent := NewAgent()
	agent.runtimeClient = client
	agent.retainedThreads["target"] = &retainedRuntimeThread{
		sessionID: "target",
		threadID:  "target-thread",
		client:    client,
	}
	peer := newSession(agent, "peer", t.TempDir(), nil, codex.Thread{ID: "peer-thread"}, client, sessionMeta{}, nil)
	agent.sessions[peer.id] = peer

	_, err := agent.UnstableDeleteSession(context.Background(), DeleteSessionRequest("target"))
	require.ErrorIs(t, err, codex.ErrBackgroundTerminalsUnsupported)
	require.True(t, agent.isDeleted("target"),
		"the tombstone precedes teardown, so a failed containment reports with the id already hidden")
	require.False(t, agent.retainedThreads["target"].claimed)
	delete(agent.retainedThreads, "target")
	require.NoError(t, agent.Close())
}

func TestDeleteSoleRetainedThreadFencesUnsupportedGeneration(t *testing.T) {
	targetID := acp.SessionId("00000000-0000-4000-8000-000000000001")
	replacement := newSpyCodexClient()
	client := &unsupportedClosedTransportClient{
		unsupportedBackgroundTerminalClient: &unsupportedBackgroundTerminalClient{
			spyCodexClient: newSpyCodexClient(),
		},
	}
	agent := NewAgent(withClientFactory(func(context.Context, codex.Options) (codex.Client, error) {
		return replacement, nil
	}))
	agent.runtimeClient = client
	agent.retainedThreads[targetID] = &retainedRuntimeThread{
		sessionID: targetID,
		threadID:  string(targetID),
		client:    client,
	}

	_, err := agent.UnstableDeleteSession(context.Background(), DeleteSessionRequest(targetID))
	require.NoError(t, err)
	require.True(t, agent.isDeleted(targetID))
	require.Same(t, replacement, agent.runtimeClient)
	require.NotContains(t, agent.retainedThreads, targetID)
	client.mu.Lock()
	require.True(t, client.closed)
	client.mu.Unlock()
	require.NoError(t, agent.Close())
}

func TestAgentCloseAcceptsConnectionCloseAfterRuntimeFence(t *testing.T) {
	agent, _, _ := soleFailedSweepAgent(t, codex.ErrConnectionClosed)
	require.NoError(t, agent.Close())
}

type unsupportedBackgroundTerminalClient struct {
	*spyCodexClient
}

type unsupportedClosedTransportClient struct {
	*unsupportedBackgroundTerminalClient
}

func (*unsupportedBackgroundTerminalClient) ListBackgroundTerminals(
	context.Context,
	codex.BackgroundTerminalListRequest,
) (codex.BackgroundTerminalListResponse, error) {
	return codex.BackgroundTerminalListResponse{}, codex.ErrBackgroundTerminalsUnsupported
}

func (c *unsupportedClosedTransportClient) UnsubscribeThread(context.Context, string) error {
	c.mu.Lock()
	defer c.mu.Unlock()

	if c.closed {
		return codex.ErrConnectionClosed
	}

	return nil
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
