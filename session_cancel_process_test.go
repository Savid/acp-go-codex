package codexacp

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/coder/acp-go-sdk"
	"github.com/savid/acp-go-codex/internal/codex"
	"github.com/stretchr/testify/require"
)

type promptResult struct {
	response acp.PromptResponse
	err      error
}

// The fake app-server reproduces the native behavior where turn/interrupt
// returns success while a command descendant remains alive.
// Cancellation must synchronously terminate the target thread's descendants,
// preserve the shared app-server, and keep the logical session usable.
func TestCancelTerminatesTargetDescendantsBeforeReturn(t *testing.T) {
	skipUnprivilegedDarwinIsolation(t)
	// The native process runs as the isolated identity, so the fixture tree has
	// to be one that identity can traverse, a home it owns outright, and a
	// directory it can publish its sentinels into.
	root := testTraversableTempDir(t)
	// A standalone isolation fences the durable home to its state root, so the
	// home is the state root rather than a directory beside the fixture tree.
	home := testStandaloneStateRoot()
	scratch := filepath.Join(root, "scratch")
	shared := testNativeSharedDir(t, root)
	childStarted := filepath.Join(shared, "child-started")
	cancelReturned := filepath.Join(shared, "cancel-returned")
	childSentinel := filepath.Join(shared, "child-sentinel")
	rolloutPath := filepath.Join(shared, "rollout.jsonl")
	tailStopped := filepath.Join(shared, "tail-stopped")
	require.NoError(t, os.MkdirAll(scratch, 0o711))

	store := NewInMemorySessionStore()
	agent := NewAgent(nativeContainmentTestOptions(
		WithExecutablePath(testReachableExecutable(t)),
		WithProcessIsolation(testProcessIsolation()),
		WithHome(home),
		WithScratchDir(scratch),
		WithSessionStore(store),
		WithEnv(fakeCodexModeEnvMap(fakeCodexMode{
			Mode:           fakeCodexCancelTreeMode,
			ChildStarted:   childStarted,
			CancelReturned: cancelReturned,
			ChildSentinel:  childSentinel,
			RolloutPath:    rolloutPath,
			TailStopped:    tailStopped,
		})),
	)...)
	agent.setAgentClient(newRecordingAgentClient())
	t.Cleanup(func() { require.NoError(t, agent.Close()) })

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	client, err := agent.sharedRuntime(ctx)
	require.NoError(t, err)
	require.NoError(t, appendFakeCodexRolloutRow(
		rolloutPath,
		`{"type":"session_meta","payload":{"id":"`+fakeCodexThreadID+`"}}`,
	))

	sessionID := acp.SessionId("cancel-tree-session")
	session := newSession(
		agent,
		sessionID,
		root,
		nil,
		codex.Thread{ID: fakeCodexThreadID, Path: rolloutPath},
		client,
		sessionMeta{},
		nil,
	)
	agent.mu.Lock()
	agent.sessions[sessionID] = session
	agent.mu.Unlock()

	blocking := make(chan promptResult, 1)
	go func() {
		response, promptErr := agent.Prompt(ctx, TextPromptRequest(sessionID, "blocking-nonce", fakeCodexBlockingPrompt))
		blocking <- promptResult{response: response, err: promptErr}
	}()

	require.Eventually(t, func() bool {
		return fakeCodexChildIsDetached(childStarted) && session.activeTurnID() == fakeCodexBlockingTurnID
	}, 5*time.Second, 10*time.Millisecond)

	cancelDone := make(chan error, 1)
	go func() { cancelDone <- agent.Cancel(ctx, CancelRequest(sessionID, "blocking-nonce")) }()
	result := <-blocking
	require.NoError(t, result.err)
	require.Equal(t, acp.StopReasonCancelled, result.response.StopReason)
	require.NoError(t, os.WriteFile(tailStopped, []byte("stopped"), 0o600))
	require.NoError(t, <-cancelDone)
	// If the descendant is still alive after Cancel returns, this marker makes
	// it publish the violation sentinel immediately. Its independent delayed
	// write catches a survivor that misses the marker race.
	require.NoError(t, os.WriteFile(cancelReturned, []byte("returned"), 0o600))
	time.Sleep(fakeCodexChildObservationWait)
	require.NoFileExists(t, childSentinel)

	require.False(t, session.clientDead)
	entries, err := store.Load(ctx, SessionKey{SessionID: string(sessionID)})
	require.NoError(t, err)
	require.Contains(t, string(joinSessionStoreEntries(entries)), fakeCodexLateAbortRolloutRow)

	replacement, err := agent.Prompt(ctx, TextPromptRequest(sessionID, "replacement-nonce", fakeCodexReplacementPrompt))
	require.NoError(t, err)
	require.Equal(t, acp.StopReasonEndTurn, replacement.StopReason)
	require.False(t, session.clientDead)
	require.Equal(t, fakeCodexThreadID, session.snapshot().codexThreadID)
}

// Timeout uses the same thread-scoped containment boundary as explicit
// cancellation. The fake app-server acknowledges interrupt while retaining a
// real delayed child, so Prompt must not return its timeout failure until that
// child can no longer publish a side effect.
func TestTimeoutTerminatesTargetDescendantsBeforeReturn(t *testing.T) {
	skipUnprivilegedDarwinIsolation(t)
	// The native process runs as the isolated identity, so the fixture tree has
	// to be one that identity can traverse, a home it owns outright, and a
	// directory it can publish its sentinels into.
	root := testTraversableTempDir(t)
	// A standalone isolation fences the durable home to its state root, so the
	// home is the state root rather than a directory beside the fixture tree.
	home := testStandaloneStateRoot()
	scratch := filepath.Join(root, "scratch")
	shared := testNativeSharedDir(t, root)
	childStarted := filepath.Join(shared, "child-started")
	timeoutReturned := filepath.Join(shared, "timeout-returned")
	childSentinel := filepath.Join(shared, "child-sentinel")
	rolloutPath := filepath.Join(shared, "rollout.jsonl")
	require.NoError(t, os.MkdirAll(scratch, 0o711))

	store := NewInMemorySessionStore()
	agent := NewAgent(nativeContainmentTestOptions(
		WithExecutablePath(testReachableExecutable(t)),
		WithProcessIsolation(testProcessIsolation()),
		WithHome(home),
		WithScratchDir(scratch),
		WithSessionStore(store),
		WithTurnTimeout(250*time.Millisecond),
		WithEnv(fakeCodexModeEnvMap(fakeCodexMode{
			Mode:           fakeCodexCancelTreeMode,
			ChildStarted:   childStarted,
			CancelReturned: timeoutReturned,
			ChildSentinel:  childSentinel,
			RolloutPath:    rolloutPath,
		})),
	)...)
	agent.setAgentClient(newRecordingAgentClient())
	t.Cleanup(func() { require.NoError(t, agent.Close()) })

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	client, err := agent.sharedRuntime(ctx)
	require.NoError(t, err)
	require.NoError(t, appendFakeCodexRolloutRow(
		rolloutPath,
		`{"type":"session_meta","payload":{"id":"`+fakeCodexThreadID+`"}}`,
	))

	sessionID := acp.SessionId("timeout-tree-session")
	session := newSession(
		agent,
		sessionID,
		root,
		nil,
		codex.Thread{ID: fakeCodexThreadID, Path: rolloutPath},
		client,
		sessionMeta{},
		nil,
	)
	agent.mu.Lock()
	agent.sessions[sessionID] = session
	agent.mu.Unlock()

	blocking := make(chan promptResult, 1)
	go func() {
		response, promptErr := agent.Prompt(ctx, TextPromptRequest(sessionID, "timeout-nonce", fakeCodexBlockingPrompt))
		blocking <- promptResult{response: response, err: promptErr}
	}()

	require.Eventually(t, func() bool {
		return fakeCodexChildIsDetached(childStarted) && session.activeTurnID() == fakeCodexBlockingTurnID
	}, 5*time.Second, 10*time.Millisecond)

	result := <-blocking
	require.Empty(t, result.response.StopReason)
	require.True(t, isTurnFailure(result.err, codex.CauseTimeout), "timeout error = %v", result.err)

	// The child observes this marker immediately if Prompt returned before
	// containment, while its own delayed write catches a marker race.
	require.NoError(t, os.WriteFile(timeoutReturned, []byte("returned"), 0o600))
	time.Sleep(fakeCodexChildObservationWait)
	require.NoFileExists(t, childSentinel)

	require.False(t, session.clientDead)
	replacement, err := agent.Prompt(ctx, TextPromptRequest(sessionID, "replacement-nonce", fakeCodexReplacementPrompt))
	require.NoError(t, err)
	require.Equal(t, acp.StopReasonEndTurn, replacement.StopReason)
	require.False(t, session.clientDead)
}

func TestCancelThenCloseAndLoadReconcilesFromExplicitStore(t *testing.T) {
	skipUnprivilegedDarwinIsolation(t)
	// The native process runs as the isolated identity, so the fixture tree has
	// to be one that identity can traverse, a home it owns outright, and a
	// directory it can publish its sentinels into.
	root := testTraversableTempDir(t)
	// A standalone isolation fences the durable home to its state root, so the
	// home is the state root rather than a directory beside the fixture tree.
	home := testStandaloneStateRoot()
	scratch := filepath.Join(root, "scratch")
	shared := testNativeSharedDir(t, root)
	childStarted := filepath.Join(shared, "child-started")
	cancelReturned := filepath.Join(shared, "cancel-returned")
	childSentinel := filepath.Join(shared, "child-sentinel")
	rolloutPath := filepath.Join(shared, "rollout.jsonl")
	tailStopped := filepath.Join(shared, "tail-stopped")
	require.NoError(t, os.MkdirAll(scratch, 0o711))

	store := NewInMemorySessionStore()
	agent := NewAgent(nativeContainmentTestOptions(
		WithExecutablePath(testReachableExecutable(t)),
		WithProcessIsolation(testProcessIsolation()),
		WithHome(home),
		WithScratchDir(scratch),
		WithSessionStore(store),
		WithEnv(fakeCodexModeEnvMap(fakeCodexMode{
			Mode:           fakeCodexCancelTreeMode,
			ChildStarted:   childStarted,
			CancelReturned: cancelReturned,
			ChildSentinel:  childSentinel,
			RolloutPath:    rolloutPath,
			TailStopped:    tailStopped,
		})),
	)...)
	agent.setAgentClient(newRecordingAgentClient())
	t.Cleanup(func() { require.NoError(t, agent.Close()) })

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	created, err := agent.NewSession(ctx, NewSessionRequest(root))
	require.NoError(t, err)
	active := agent.activeSession(created.SessionId)
	require.NotNil(t, active)
	stalePeer := newSession(
		agent,
		acp.SessionId("stale-peer"),
		root,
		nil,
		codex.Thread{ID: fakeCodexStalePeerThreadID},
		active.client,
		sessionMeta{},
		nil,
	)
	agent.mu.Lock()
	agent.sessions[stalePeer.id] = stalePeer
	agent.mu.Unlock()

	blocking := make(chan promptResult, 1)
	go func() {
		response, promptErr := agent.Prompt(ctx, TextPromptRequest(created.SessionId, "blocking-nonce", fakeCodexBlockingPrompt))
		blocking <- promptResult{response: response, err: promptErr}
	}()

	require.Eventually(t, func() bool {
		active := agent.activeSession(created.SessionId)

		return fakeCodexChildIsDetached(childStarted) && active != nil && active.activeTurnID() == fakeCodexBlockingTurnID
	}, 5*time.Second, 10*time.Millisecond)
	cancelDone := make(chan error, 1)
	go func() { cancelDone <- agent.Cancel(ctx, CancelRequest(created.SessionId, "blocking-nonce")) }()
	result := <-blocking
	require.NoError(t, result.err)
	require.Equal(t, acp.StopReasonCancelled, result.response.StopReason)
	require.NoError(t, os.WriteFile(tailStopped, []byte("stopped"), 0o600))
	require.NoError(t, <-cancelDone)
	require.NoError(t, os.WriteFile(cancelReturned, []byte("returned"), 0o600))
	time.Sleep(fakeCodexChildObservationWait)
	require.NoFileExists(t, childSentinel)

	entries := []SessionStoreEntry{
		SessionStoreEntry(`{"type":"session_meta","payload":{"id":"` + fakeCodexThreadID + `","cwd":"` + root + `"}}`),
		SessionStoreEntry(`{"type":"event_msg","payload":{"type":"user_message","message":"before interrupt"}}`),
	}
	require.NoError(t, store.Replace(ctx, SessionKey{SessionID: string(created.SessionId)}, []SessionStoreReplacement{{
		Key:     SessionKey{SessionID: string(created.SessionId)},
		Entries: entries,
	}}))
	stored, err := store.Load(ctx, SessionKey{SessionID: string(created.SessionId)})
	require.NoError(t, err)
	require.Equal(t, entries, stored)

	_, err = agent.CloseSession(ctx, acp.CloseSessionRequest{SessionId: created.SessionId})
	require.NoError(t, err)
	require.Nil(t, agent.activeSession(created.SessionId))
	stored, err = store.Load(ctx, SessionKey{SessionID: string(created.SessionId)})
	require.NoError(t, err)
	require.Equal(t, entries, stored)

	_, err = agent.LoadSession(ctx, LoadSessionRequest(created.SessionId, root))
	require.NoError(t, err)
	replacement, err := agent.Prompt(ctx, TextPromptRequest(created.SessionId, "replacement-nonce", fakeCodexReplacementPrompt))
	require.NoError(t, err)
	require.Equal(t, acp.StopReasonEndTurn, replacement.StopReason)
	require.Equal(t, fakeCodexThreadID, agent.activeSession(created.SessionId).snapshot().codexThreadID)
	require.False(t, stalePeer.clientDead, "target cancellation must not fence an unrelated peer")
}

func joinSessionStoreEntries(entries []SessionStoreEntry) []byte {
	var joined []byte
	for _, entry := range entries {
		joined = append(joined, entry...)
		joined = append(joined, '\n')
	}

	return joined
}
