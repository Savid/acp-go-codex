package codexacp

import (
	"context"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/coder/acp-go-sdk"
	acpcore "github.com/savid/acp-go-core"
	"github.com/savid/acp-go-core/lifecycle"
	"github.com/savid/acp-go-core/wire"
	"github.com/stretchr/testify/require"

	"github.com/savid/acp-go-codex/internal/codex"
)

func queueTestAgent(t *testing.T, extra ...Option) (*Agent, *recorder, *blockedCommitStore, func()) {
	t.Helper()
	store := &blockedCommitStore{SessionStore: acpcore.NewInMemorySessionStore(), entered: make(chan struct{}, 1), release: make(chan struct{})}
	a := NewAgent(testOptions(t, append(extra, WithSessionStore(store))...)...)
	rec := newRecorder()
	a.conn = rec
	request := acp.InitializeRequest{ProtocolVersion: acp.ProtocolVersionNumber}
	withLifecycle()(&request)
	_, err := a.Initialize(t.Context(), request)
	require.NoError(t, err)
	release := sync.OnceFunc(func() { close(store.release) })
	t.Cleanup(func() {
		release()
		require.NoError(t, a.Close())
	})

	return a, rec, store, release
}

func TestBackgroundApprovalWaitsForPrecedingCommit(t *testing.T) {
	t.Parallel()
	sent := filepath.Join(t.TempDir(), "request-sent")
	a, rec, store, release := queueTestAgent(t, WithEnv(map[string]string{fakeCodexEnv: "1", fakeCodexEnvRequestSent: sent}))
	first, err := a.NewSession(t.Context(), wire.NewSessionRequest(t.TempDir()))
	require.NoError(t, err)
	peer, err := a.NewSession(t.Context(), wire.NewSessionRequest(t.TempDir()))
	require.NoError(t, err)
	store.blockID.Store(string(first.SessionId))
	request := wire.TextPromptRequest(first.SessionId, "AGENTTOOL")
	request.Meta = promptMeta(1)
	done := make(chan error, 1)
	go func() {
		_, err := a.Prompt(t.Context(), request)
		done <- err
	}()
	select {
	case <-store.entered:
	case <-time.After(testTimeout):
		t.Fatal("prompt did not reach its mirror commit")
	}
	require.Eventually(t, func() bool {
		_, err := os.Stat(sent)

		return err == nil
	}, testTimeout, time.Millisecond)

	// The callback is already on native stdout, so the peer's response also
	// proves the shared reader accepted it while the first commit is held.
	peerRequest := wire.TextPromptRequest(peer.SessionId, "HELLO")
	peerRequest.Meta = promptMeta(2)
	peerDone := make(chan error, 1)
	go func() {
		_, promptErr := a.Prompt(t.Context(), peerRequest)
		peerDone <- promptErr
	}()
	select {
	case err := <-peerDone:
		require.NoError(t, err)
	case <-time.After(3 * time.Second):
		t.Fatal("peer prompt waited for another session's commit")
	}
	rec.mu.Lock()
	early := len(rec.permissions)
	rec.mu.Unlock()
	require.Zero(t, early, "the next cycle's approval bypassed the preceding commit")

	release()
	require.NoError(t, <-done)
	rec.waitFor(t, func(updates []acp.SessionNotification) bool {
		for _, event := range lifecycleEvents(updates) {
			if event["cause"] == string(lifecycle.CauseActivity) && event["state"] == string(lifecycle.ForegroundIdle) {
				return true
			}
		}

		return false
	})
	var owner any
	for _, event := range lifecycleEvents(rec.snapshot()) {
		if event["cause"] == string(lifecycle.CauseActivity) && event["state"] == string(lifecycle.ForegroundRunning) {
			owner = event["turnId"]

			break
		}
	}
	require.NotNil(t, owner)
	rec.mu.Lock()
	permissions := append([]acp.RequestPermissionRequest(nil), rec.permissions...)
	rec.mu.Unlock()
	require.Len(t, permissions, 1)
	envelope, ok := permissions[0].Meta[wire.LifecycleKey].(map[string]any)
	require.True(t, ok)
	action, ok := envelope["action"].(map[string]any)
	require.True(t, ok)
	actionOwner, ok := action["owner"].(map[string]any)
	require.True(t, ok)
	require.Equal(t, owner, actionOwner["id"])
	reduceAll(t, first.SessionId, rec.snapshot())
}

func TestProcessExitDoesNotPublishQueuedTerminalTail(t *testing.T) {
	t.Parallel()
	a, rec, store, release := queueTestAgent(t)
	created, err := a.NewSession(t.Context(), wire.NewSessionRequest(t.TempDir()))
	require.NoError(t, err)
	store.blockID.Store(string(created.SessionId))
	a.mu.Lock()
	s := a.sessions[created.SessionId]
	a.mu.Unlock()
	s.mu.Lock()
	rt := s.rt
	s.mu.Unlock()
	request := wire.TextPromptRequest(created.SessionId, "TAIL")
	request.Meta = promptMeta(1)
	type promptResult struct {
		response acp.PromptResponse
		err      error
	}
	done := make(chan promptResult, 1)
	go func() {
		response, promptErr := a.Prompt(t.Context(), request)
		done <- promptResult{response: response, err: promptErr}
	}()
	select {
	case <-store.entered:
	case <-time.After(testTimeout):
		t.Fatal("prompt did not reach its mirror commit")
	}
	require.Eventually(t, func() bool {
		rt.mu.Lock()
		defer rt.mu.Unlock()
		queue := rt.events[s]

		return queue != nil && len(queue.messages) > 0
	}, testTimeout, time.Millisecond)
	require.NoError(t, rt.proc.Kill())
	select {
	case <-rt.done:
	case <-time.After(testTimeout):
		t.Fatal("exited generation did not finish while the commit was held")
	}
	require.Equal(t, "Hello world", agentText(rec.snapshot()))
	release()
	result := <-done
	require.NoError(t, result.err)
	require.Equal(t, acp.StopReasonEndTurn, result.response.StopReason)
}

// gateClient blocks SessionUpdate for one session id until released, so a test
// can stall one session's pump without touching any peer.
type gateClient struct {
	*recorder
	target acp.SessionId
	block  chan struct{}
}

func (g *gateClient) SessionUpdate(ctx context.Context, params acp.SessionNotification) error {
	if params.SessionId == g.target {
		select {
		case <-g.block:
		case <-ctx.Done():
			return ctx.Err()
		}
	}

	return g.recorder.SessionUpdate(ctx, params)
}

// TestSessionOverflowContainsWithoutKillingPeers proves a stalled session whose
// event queue overflows is contained alone: the shared app-server survives and
// a peer session still completes a turn.
func TestSessionOverflowContainsWithoutKillingPeers(t *testing.T) {
	gate := &gateClient{recorder: newRecorder(), block: make(chan struct{})}

	a := NewAgent(testOptions(t)...)
	t.Cleanup(func() { _ = a.Close() })
	a.attach(gate, nil)

	_, err := a.Initialize(t.Context(), acp.InitializeRequest{ProtocolVersion: acp.ProtocolVersionNumber, Meta: map[string]any{wire.LifecycleKey: map[string]any{"version": 1}}})
	require.NoError(t, err)

	stalled, err := a.NewSession(t.Context(), wire.NewSessionRequest(t.TempDir()))
	require.NoError(t, err)
	peer, err := a.NewSession(t.Context(), wire.NewSessionRequest(t.TempDir()))
	require.NoError(t, err)

	gate.target = stalled.SessionId

	sa, err := a.session(t.Context(), stalled.SessionId)
	require.NoError(t, err)
	sa.mu.Lock()
	rt := sa.rt
	sa.mu.Unlock()
	require.NotNil(t, rt)

	// Flood the stalled session past its 256-slot queue. Its pump blocks on the
	// first publish to the gated client, so the rest overflow and contain it.
	for range 400 {
		a.routeMessage(t.Context(), rt, sa, sessionMessage{event: codex.Event{Kind: codex.EventTurnStarted, ThreadID: sa.nativeID, TurnID: "background"}})
	}

	require.True(t, rt.alive(), "one session's overflow must not kill the shared app-server")

	// The peer still completes a turn on the same app-server.
	prompt := wire.TextPromptRequest(peer.SessionId, "hi")
	prompt.Meta = promptMeta(1)
	response, err := a.Prompt(t.Context(), prompt)
	require.NoError(t, err)
	require.Equal(t, acp.StopReasonEndTurn, response.StopReason)

	// Releasing the stalled client lets containment finish and detach it.
	close(gate.block)
	require.Eventually(t, func() bool {
		sa.mu.Lock()
		defer sa.mu.Unlock()

		return sa.rt == nil
	}, 5*time.Second, 5*time.Millisecond, "the contained session detaches from the live generation")
}
