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

// The fake app-server reproduces the native behavior observed through Wagie:
// turn/interrupt returns success while a command descendant remains alive.
// Cancellation must synchronously retire the app-server containment, preserve
// the logical ACP session, and resume that thread for the replacement turn.
func TestCancelQuiescesDescendantsBeforeReturnAndRebindsSession(t *testing.T) {
	root := t.TempDir()
	home := filepath.Join(root, "home")
	scratch := filepath.Join(root, "scratch")
	childStarted := filepath.Join(root, "child-started")
	cancelReturned := filepath.Join(root, "cancel-returned")
	childSentinel := filepath.Join(root, "child-sentinel")
	require.NoError(t, os.MkdirAll(home, 0o700))
	require.NoError(t, os.MkdirAll(scratch, 0o700))

	agent := NewAgent(
		WithExecutablePath(os.Args[0]),
		WithHome(home),
		WithScratchDir(scratch),
		WithEnv(map[string]string{
			fakeCodexModeEnv:           fakeCodexCancelTreeMode,
			fakeCodexChildStartedEnv:   childStarted,
			fakeCodexCancelReturnedEnv: cancelReturned,
			fakeCodexChildSentinelEnv:  childSentinel,
		}),
	)
	agent.setAgentClient(newRecordingAgentClient())
	t.Cleanup(func() { require.NoError(t, agent.Close()) })

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	client, err := agent.sharedRuntime(ctx)
	require.NoError(t, err)

	sessionID := acp.SessionId("cancel-tree-session")
	session := newSession(agent, sessionID, root, nil, codex.Thread{ID: fakeCodexThreadID}, client, sessionMeta{}, nil)
	agent.mu.Lock()
	agent.sessions[sessionID] = session
	agent.mu.Unlock()

	blocking := make(chan promptResult, 1)
	go func() {
		response, promptErr := agent.Prompt(ctx, TextPromptRequest(sessionID, "blocking-nonce", fakeCodexBlockingPrompt))
		blocking <- promptResult{response: response, err: promptErr}
	}()

	require.Eventually(t, func() bool {
		_, statErr := os.Stat(childStarted)

		return statErr == nil && session.activeTurnID() == fakeCodexBlockingTurnID
	}, 5*time.Second, 10*time.Millisecond)

	require.NoError(t, agent.Cancel(ctx, CancelRequest(sessionID, "blocking-nonce")))
	// If the descendant is still alive after Cancel returns, this marker makes
	// it publish the violation sentinel immediately. Its independent delayed
	// write catches a survivor that misses the marker race.
	require.NoError(t, os.WriteFile(cancelReturned, []byte("returned"), 0o600))
	time.Sleep(fakeCodexChildObservationWait)
	require.NoFileExists(t, childSentinel)

	result := <-blocking
	require.NoError(t, result.err)
	require.Equal(t, acp.StopReasonCancelled, result.response.StopReason)
	require.True(t, session.clientDead)

	replacement, err := agent.Prompt(ctx, TextPromptRequest(sessionID, "replacement-nonce", fakeCodexReplacementPrompt))
	require.NoError(t, err)
	require.Equal(t, acp.StopReasonEndTurn, replacement.StopReason)
	require.False(t, session.clientDead)
	require.Equal(t, fakeCodexThreadID, session.snapshot().codexThreadID)
}
