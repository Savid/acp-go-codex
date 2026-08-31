package codexacp

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/coder/acp-go-sdk"
	"github.com/savid/acp-go-codex/internal/codex"
	"github.com/savid/acp-go-codex/internal/lifecycle"
	"github.com/stretchr/testify/require"
)

func requireRequestError(t *testing.T, err error, code int, message string) {
	t.Helper()
	reqErr, ok := err.(*acp.RequestError)
	if !ok {
		t.Fatalf("error = %T %v, want ACP request error", err, err)
	}
	if reqErr.Code != code || reqErr.Message != message {
		t.Fatalf("request error = %#v, want code=%d message=%q", reqErr, code, message)
	}
}

func requireUnknownSession(t *testing.T, err error) {
	t.Helper()
	reqErr, ok := err.(*acp.RequestError)
	if !ok {
		t.Fatalf("error = %T %v, want ACP request error", err, err)
	}
	if reqErr.Code != -32602 {
		t.Fatalf("request error = %#v, want code=-32602", reqErr)
	}
	data, ok := reqErr.Data.(map[string]any)
	if !ok {
		t.Fatalf("request error data = %#v, want map", reqErr.Data)
	}
	if data["error"] != "unknown session" || data["field"] != "sessionId" {
		t.Fatalf("request error data = %#v, want {error:unknown session, field:sessionId}", data)
	}
}

// requireSessionDeleteInProgress asserts the retriable conflict an uncommitted
// delete earns, and — because that is the whole point of the distinction —
// asserts it is not the permanent unknown-session verdict.
func requireSessionDeleteInProgress(t *testing.T, err error) {
	t.Helper()

	var reqErr *acp.RequestError

	require.ErrorAs(t, err, &reqErr)
	require.Equal(t, -32600, reqErr.Code, "an in-flight delete is a conflict, not a terminal verdict")
	require.Equal(t,
		map[string]any{jsonFieldError: "session delete lifecycle is already in progress"},
		reqErr.Data,
	)
}

func TestForkIsExtensionOnly(t *testing.T) {
	ctx := context.Background()
	client := newSpyCodexClient()
	agent := NewAgent(withClientFactory(func(context.Context, codex.Options) (codex.Client, error) {
		return client, nil
	}))

	parent, err := agent.NewSession(ctx, NewSessionRequest("/tmp/project"))
	if err != nil {
		t.Fatalf("NewSession returned error: %v", err)
	}
	if _, ok := any(agent).(interface {
		UnstableForkSession(context.Context, acp.UnstableForkSessionRequest) (acp.UnstableForkSessionResponse, error)
	}); ok {
		t.Fatal("Agent exposes UnstableForkSession")
	}

	raw, err := json.Marshal(ForkSessionRequest(parent.SessionId, "/tmp/project"))
	if err != nil {
		t.Fatalf("marshal fork request: %v", err)
	}
	resp, err := agent.HandleExtensionMethod(ctx, ForkSessionMethod, raw)
	if err != nil {
		t.Fatalf("HandleExtensionMethod fork returned error: %v", err)
	}
	fork, ok := resp.(acp.UnstableForkSessionResponse)
	if !ok || fork.SessionId == "" {
		t.Fatalf("fork response = %#v", resp)
	}

	if _, err := agent.HandleExtensionMethod(ctx, acp.AgentMethodSessionFork, raw); err == nil {
		t.Fatal("stable session/fork extension route succeeded")
	} else {
		requireRequestError(t, err, -32601, "Method not found")
	}
}

func TestLocalDispatcherDoesNotRouteDeletedSessionMethods(t *testing.T) {
	agent := NewAgent()
	conn := &localAgentConnection{agent: agent}
	conn.initialized.Store(true)
	ctx := context.Background()

	raw, err := json.Marshal(ForkSessionRequest("parent", "/tmp/project"))
	if err != nil {
		t.Fatalf("marshal fork request: %v", err)
	}
	if _, reqErr := conn.handle(ctx, acp.AgentMethodSessionFork, raw); reqErr == nil || reqErr.Code != -32601 {
		t.Fatalf("session/fork request error = %#v, want method-not-found", reqErr)
	}

	modeRaw, err := json.Marshal(acp.SetSessionModeRequest{SessionId: "s", ModeId: modePlan})
	if err != nil {
		t.Fatalf("marshal mode request: %v", err)
	}
	if _, reqErr := conn.handle(ctx, acp.AgentMethodSessionSetMode, modeRaw); reqErr == nil || reqErr.Code != -32601 {
		t.Fatalf("session/set_mode request error = %#v, want method-not-found", reqErr)
	}
}

func TestDeleteSessionTombstonesStoreAndBlocksLoadResume(t *testing.T) {
	ctx := context.Background()
	store := NewInMemorySessionStore()
	client := newSpyCodexClient()
	agent := NewAgent(
		WithSessionStore(store),
		withClientFactory(func(context.Context, codex.Options) (codex.Client, error) { return client, nil }),
	)

	resp, err := agent.NewSession(ctx, NewSessionRequest("/tmp/project"))
	if err != nil {
		t.Fatalf("NewSession returned error: %v", err)
	}
	if seedErr := store.Replace(ctx, SessionKey{SessionID: string(resp.SessionId)}, []SessionStoreReplacement{{
		Key:     SessionKey{SessionID: string(resp.SessionId)},
		Entries: []SessionStoreEntry{SessionStoreEntry(`{"type":"event_msg","payload":{"type":"agent_message","message":"hi"}}`)},
	}}); seedErr != nil {
		t.Fatalf("seed store: %v", seedErr)
	}

	if _, delErr := agent.UnstableDeleteSession(ctx, acp.UnstableDeleteSessionRequest{SessionId: resp.SessionId}); delErr != nil {
		t.Fatalf("DeleteSession returned error: %v", delErr)
	}
	if !containsString(client.deletedThreadSnapshot(), "thread-1") {
		t.Fatalf("native thread was not deleted: %#v", client.deletedThreadSnapshot())
	}
	entries, err := store.Load(ctx, SessionKey{SessionID: string(resp.SessionId)})
	if err != nil {
		t.Fatalf("Load tombstoned store returned error: %v", err)
	}
	if len(entries) != 0 {
		t.Fatalf("tombstoned entries = %#v", entries)
	}
	if _, err := agent.ResumeSession(ctx, ResumeSessionRequest(resp.SessionId, "/tmp/project")); err == nil {
		t.Fatal("ResumeSession after delete succeeded")
	} else {
		requireUnknownSession(t, err)
	}
	if _, err := agent.LoadSession(ctx, LoadSessionRequest(resp.SessionId, "/tmp/project")); err == nil {
		t.Fatal("LoadSession after delete succeeded")
	} else {
		requireUnknownSession(t, err)
	}
}

// A store-only delete has no active wrapper whose close flag could fence the id,
// so a load already blocked inside its native resume must be refused where it
// would become reachable rather than where it started.
func TestDeleteRefusesLoadResumingAcrossTheTombstone(t *testing.T) {
	ctx := context.Background()
	id := acp.SessionId("00000000-0000-4000-8000-0000000000aa")
	store := NewInMemorySessionStore()
	require.NoError(t, store.Append(ctx, SessionKey{SessionID: string(id)}, []SessionStoreEntry{
		SessionStoreEntry(`{"type":"session_meta","payload":{"id":"thread-stored"}}`),
	}))

	client := &blockingLifecycleCodexClient{
		spyCodexClient: newSpyCodexClient(),
		resumeStarted:  make(chan codex.ThreadResumeRequest, 1),
		resumeRelease:  make(chan struct{}),
	}
	agent := NewAgent(
		WithSessionStore(store),
		withClientFactory(func(context.Context, codex.Options) (codex.Client, error) { return client, nil }),
	)
	agent.setAgentClient(newRecordingAgentClient())

	loadResult := make(chan error, 1)

	go func() {
		_, loadErr := agent.LoadSession(ctx, LoadSessionRequest(id, "/tmp/project"))
		loadResult <- loadErr
	}()
	<-client.resumeStarted

	_, err := agent.UnstableDeleteSession(ctx, DeleteSessionRequest(id))
	require.NoError(t, err)
	require.True(t, agent.isDeleted(id))

	close(client.resumeRelease)
	requireUnknownSession(t, <-loadResult)
	require.Nil(t, agent.activeSession(id), "a tombstoned id may not come back as a live wrapper")
	require.Equal(t, []string{"thread-stored"}, client.unsubscribedSnapshot(),
		"the native thread the refused resume produced is contained exactly once, by its caller")

	_, err = agent.Prompt(ctx, TextPromptRequest(id, "turn-nonce", "hello"))
	requireUnknownSession(t, err)
	require.NoError(t, agent.Close())
}

// A refused registration hands the candidate back to its caller unclosed, so one
// session's containment boundary runs exactly once over the native thread it
// produced. A second pass sweeps a thread the first pass already unsubscribed,
// and an app-server that refuses that sweep turns the redundant pass into a
// generation fence: the shared app-server is retired for the sake of a thread
// that was already contained and that nobody could address anyway.
func TestRefusedRegistrationContainsOnceAndSparesTheRuntime(t *testing.T) {
	ctx := context.Background()
	id := acp.SessionId("00000000-0000-4000-8000-0000000000ab")
	store := NewInMemorySessionStore()
	require.NoError(t, store.Append(ctx, SessionKey{SessionID: string(id)}, []SessionStoreEntry{
		SessionStoreEntry(`{"type":"session_meta","payload":{"id":"thread-stored"}}`),
	}))

	client := &postContainmentStrictClient{
		blockingLifecycleCodexClient: &blockingLifecycleCodexClient{
			spyCodexClient: newSpyCodexClient(),
			resumeStarted:  make(chan codex.ThreadResumeRequest, 1),
			resumeRelease:  make(chan struct{}),
		},
	}
	agent := NewAgent(
		WithSessionStore(store),
		withClientFactory(func(context.Context, codex.Options) (codex.Client, error) { return client, nil }),
	)
	agent.setAgentClient(newRecordingAgentClient())

	loadResult := make(chan error, 1)

	go func() {
		_, loadErr := agent.LoadSession(ctx, LoadSessionRequest(id, "/tmp/project"))
		loadResult <- loadErr
	}()
	<-client.resumeStarted

	generation := agent.runtimeGenerationSnapshot()

	_, err := agent.UnstableDeleteSession(ctx, DeleteSessionRequest(id))
	require.NoError(t, err)

	close(client.resumeRelease)
	requireUnknownSession(t, <-loadResult)

	require.Equal(t, []string{"thread-stored"}, client.unsubscribedSnapshot(),
		"the refused candidate's thread is contained exactly once, by the caller that owns it")
	require.Zero(t, client.closeCount(),
		"a refused registration may not close the shared app-server the runtime is still serving from")

	surviving := agent.runtimeGenerationSnapshot()
	require.False(t, surviving.dead, "a refused registration may not fence the shared runtime generation")
	require.Equal(t, generation.epoch, surviving.epoch, "the generation that served the refused resume is unchanged")

	// The generation is alive in more than name: a session opened next serves its
	// prompt on the very generation the redundant containment would have retired.
	peer, err := agent.NewSession(ctx, NewSessionRequest("/tmp/project"))
	require.NoError(t, err)
	promptResp, err := agent.Prompt(ctx, TextPromptRequest(peer.SessionId, "turn-nonce", "hello"))
	require.NoError(t, err)
	require.Equal(t, acp.StopReasonEndTurn, promptResp.StopReason)
	require.Equal(t, generation.epoch, agent.runtimeGenerationSnapshot().epoch,
		"the new session joined the surviving generation rather than a replacement for a fenced one")

	require.NoError(t, agent.Close())
}

// postContainmentStrictClient models an app-server that will not list a thread's
// background terminals once that thread has been unsubscribed — exactly the
// state a first containment pass leaves behind — so a redundant second pass
// fails its sweep and escalates to the generation fence.
type postContainmentStrictClient struct {
	*blockingLifecycleCodexClient

	closeMu sync.Mutex
	closes  int
}

func (c *postContainmentStrictClient) ListBackgroundTerminals(
	ctx context.Context,
	req codex.BackgroundTerminalListRequest,
) (codex.BackgroundTerminalListResponse, error) {
	if containsString(c.unsubscribedSnapshot(), req.ThreadID) {
		return codex.BackgroundTerminalListResponse{}, errors.New("thread is not subscribed")
	}

	return c.blockingLifecycleCodexClient.ListBackgroundTerminals(ctx, req)
}

func (c *postContainmentStrictClient) Close(ctx context.Context) error {
	c.closeMu.Lock()
	c.closes++
	c.closeMu.Unlock()

	return c.blockingLifecycleCodexClient.Close(ctx)
}

func (c *postContainmentStrictClient) closeCount() int {
	c.closeMu.Lock()
	defer c.closeMu.Unlock()

	return c.closes
}

// The claim covers the whole delete, so an id whose delete is still running is
// refused at load and resume admission before any native work is dispatched. The
// refusal is a retriable conflict, not the unknown-session verdict: the delete
// has not committed a tombstone yet and may still fail.
func TestRunningDeleteFencesLoadAndResumeAdmission(t *testing.T) {
	ctx := context.Background()
	store := &blockingDeleteSessionStore{
		configurableStore: &configurableStore{
			entries: []SessionStoreEntry{SessionStoreEntry(`{"type":"session_meta","payload":{"id":"thread-stored"}}`)},
		},
		started: make(chan struct{}),
		release: make(chan struct{}),
	}
	client := &blockingLifecycleCodexClient{spyCodexClient: newSpyCodexClient()}
	agent := NewAgent(
		WithSessionStore(store),
		withClientFactory(func(context.Context, codex.Options) (codex.Client, error) { return client, nil }),
	)
	agent.setAgentClient(newRecordingAgentClient())

	deleteResult := make(chan error, 1)

	go func() {
		_, deleteErr := agent.UnstableDeleteSession(ctx, DeleteSessionRequest("session"))
		deleteResult <- deleteErr
	}()
	<-store.started

	_, err := agent.LoadSession(ctx, LoadSessionRequest("session", "/tmp/project"))
	requireSessionDeleteInProgress(t, err)
	_, err = agent.ResumeSession(ctx, ResumeSessionRequest("session", "/tmp/project"))
	requireSessionDeleteInProgress(t, err)
	require.Zero(t, client.resumeCallCount(), "a fenced id never reaches the native harness")

	close(store.release)
	require.NoError(t, <-deleteResult)

	// Once the tombstone is committed the same id earns the permanent verdict.
	_, err = agent.LoadSession(ctx, LoadSessionRequest("session", "/tmp/project"))
	requireUnknownSession(t, err)
	require.NoError(t, agent.Close())
}

// A delete that fails never deleted anything, so an id it fenced along the way
// must still load. The in-flight refusal is therefore retriable: answering the
// racing load with the unknown-session verdict would tell the host an id is gone
// that the failed delete left perfectly intact.
func TestFailedDeleteLeavesRacingLoadRetriable(t *testing.T) {
	ctx := context.Background()
	store := &blockingDeleteSessionStore{
		configurableStore: &configurableStore{
			entries:   []SessionStoreEntry{SessionStoreEntry(`{"type":"session_meta","payload":{"id":"thread-stored"}}`)},
			deleteErr: errors.New("store delete failed"),
		},
		started: make(chan struct{}),
		release: make(chan struct{}),
	}
	client := &blockingLifecycleCodexClient{spyCodexClient: newSpyCodexClient()}
	agent := NewAgent(
		WithSessionStore(store),
		withClientFactory(func(context.Context, codex.Options) (codex.Client, error) { return client, nil }),
	)
	agent.setAgentClient(newRecordingAgentClient())

	deleteResult := make(chan error, 1)

	go func() {
		_, deleteErr := agent.UnstableDeleteSession(ctx, DeleteSessionRequest("session"))
		deleteResult <- deleteErr
	}()
	<-store.started

	_, err := agent.LoadSession(ctx, LoadSessionRequest("session", "/tmp/project"))
	requireSessionDeleteInProgress(t, err)

	close(store.release)
	require.ErrorContains(t, <-deleteResult, "store delete failed")
	require.False(t, agent.isDeleted("session"), "a failed delete commits no tombstone")

	// The retry the conflict invited now succeeds, which is the fact the
	// unknown-session verdict would have contradicted.
	loaded, err := agent.LoadSession(ctx, LoadSessionRequest("session", "/tmp/project"))
	require.NoError(t, err)
	require.NotNil(t, agent.activeSession("session"))
	require.NotNil(t, loaded.Meta)
	require.NoError(t, agent.Close())
}

func TestDeleteFenceAdmissionBranches(t *testing.T) {
	agent := NewAgent()
	client := newSpyCodexClient()
	active := newSession(agent, "session", "/tmp/project", nil, codex.Thread{ID: "thread"}, client, sessionMeta{}, nil)
	agent.sessions[active.id] = active

	// Two concurrent deletes are both legal, and the fence outlives the later.
	// Neither has committed a tombstone, so both answer with the retriable
	// conflict rather than a verdict either one may yet fail to earn.
	first := agent.claimSessionDelete(active.id)
	second := agent.claimSessionDelete(active.id)
	requireSessionDeleteInProgress(t, agent.validateSessionLifecycle(active.id, active))
	_, err := agent.acquireSessionLifecycle(active.id)
	requireSessionDeleteInProgress(t, err)
	requireSessionDeleteInProgress(t, agent.storeStartedSession(active))

	first()
	requireSessionDeleteInProgress(t, agent.validateSessionLifecycle(active.id, active))
	second()
	require.NoError(t, agent.validateSessionLifecycle(active.id, active))

	release, err := agent.acquireSessionLifecycle(active.id)
	require.NoError(t, err)
	release()

	// A committed tombstone is the terminal verdict, at every reader of the
	// fence, including one reached while a second delete of the id still runs.
	agent.deleted[active.id] = struct{}{}
	requireUnknownSession(t, agent.validateSessionLifecycle(active.id, active))
	_, err = agent.acquireSessionLifecycle(active.id)
	requireUnknownSession(t, err)

	third := agent.claimSessionDelete(active.id)
	requireUnknownSession(t, agent.validateSessionLifecycle(active.id, active))
	third()

	delete(agent.deleted, active.id)

	// Registration refuses a tombstoned id outright, and leaves the candidate's
	// native thread to the caller that owns it until registration succeeds.
	resumed := newSession(agent, "resumed", "/tmp/project", nil, codex.Thread{ID: "thread-resumed"}, client, sessionMeta{}, nil)
	agent.deleted[resumed.id] = struct{}{}
	requireUnknownSession(t, agent.storeStartedSession(resumed))
	require.NotContains(t, agent.sessions, resumed.id)
	require.Empty(t, client.unsubscribedSnapshot(),
		"a refusal decides reachability only; containment belongs to the caller")
}

func TestCloseAndDeleteContainActiveTurnBeforeSettlement(t *testing.T) {
	for _, operation := range []string{"close", "delete"} {
		t.Run(operation, func(t *testing.T) {
			client := &blockingInterruptClient{
				spyCodexClient:   newSpyCodexClient(),
				runStarted:       make(chan struct{}),
				interruptStarted: make(chan struct{}),
				interruptRelease: make(chan struct{}),
			}
			agent := NewAgent(withClientFactory(func(context.Context, codex.Options) (codex.Client, error) {
				return client, nil
			}))
			agent.setAgentClient(newRecordingAgentClient())

			created, err := agent.NewSession(context.Background(), NewSessionRequest("/tmp/project"))
			require.NoError(t, err)
			active := agent.activeSession(created.SessionId)

			type promptResult struct {
				response acp.PromptResponse
				err      error
			}
			promptDone := make(chan promptResult, 1)
			go func() {
				response, promptErr := agent.Prompt(
					context.Background(),
					TextPromptRequest(created.SessionId, "turn-nonce", "wait"),
				)
				promptDone <- promptResult{response: response, err: promptErr}
			}()
			<-client.runStarted

			operationDone := make(chan error, 1)
			go func() {
				if operation == "close" {
					_, closeErr := agent.CloseSession(context.Background(), acp.CloseSessionRequest{SessionId: created.SessionId})
					operationDone <- closeErr

					return
				}

				_, deleteErr := agent.UnstableDeleteSession(context.Background(), DeleteSessionRequest(created.SessionId))
				operationDone <- deleteErr
			}()
			<-client.interruptStarted
			interactionCtx, finishInteraction := active.beginInteraction(context.Background(), "during-containment")

			select {
			case result := <-promptDone:
				t.Fatalf("prompt settled before native interrupt completed: %#v", result)
			default:
			}
			select {
			case err := <-operationDone:
				t.Fatalf("%s returned before native interrupt completed: %v", operation, err)
			default:
			}
			if operation == "delete" {
				require.True(t, agent.isDeleted(created.SessionId),
					"the tombstone is the delete's first act, so the id is already hidden while containment runs")
			}

			close(client.interruptRelease)
			require.Eventually(t, func() bool { return errors.Is(interactionCtx.Err(), context.Canceled) }, time.Second, time.Millisecond)
			finishInteraction()

			result := <-promptDone
			require.NoError(t, result.err)
			require.Equal(t, acp.StopReasonCancelled, result.response.StopReason)
			require.NoError(t, <-operationDone)
			require.Nil(t, agent.activeSession(created.SessionId))
			if operation == "delete" {
				require.True(t, agent.isDeleted(created.SessionId))
			}

			require.NoError(t, agent.Close())
		})
	}
}

type blockingInterruptClient struct {
	*spyCodexClient
	runStarted       chan struct{}
	runOnce          sync.Once
	interruptStarted chan struct{}
	interruptOnce    sync.Once
	interruptRelease chan struct{}
}

func TestCloseFencesUnknownTurnDispatchOutcome(t *testing.T) {
	client := &unknownDispatchClient{
		spyCodexClient: newSpyCodexClient(),
		started:        make(chan struct{}),
	}
	agent := NewAgent(withClientFactory(func(context.Context, codex.Options) (codex.Client, error) {
		return client, nil
	}))
	created, err := agent.NewSession(context.Background(), NewSessionRequest("/tmp/project"))
	require.NoError(t, err)

	promptDone := make(chan error, 1)
	go func() {
		_, promptErr := agent.Prompt(
			context.Background(),
			TextPromptRequest(created.SessionId, "turn-nonce", "wait"),
		)
		promptDone <- promptErr
	}()
	<-client.started

	_, err = agent.CloseSession(context.Background(), acp.CloseSessionRequest{SessionId: created.SessionId})
	require.ErrorContains(t, err, "dispatch outcome is unknown")
	require.ErrorContains(t, <-promptDone, "dispatch outcome is unknown")
	client.mu.Lock()
	require.False(t, client.closed, "session close must not retire the shared runtime")
	client.mu.Unlock()
	require.NoError(t, agent.Close())
}

type unknownDispatchClient struct {
	*spyCodexClient
	started chan struct{}
}

func (c *unknownDispatchClient) RunTurn(ctx context.Context, _ codex.TurnStartRequest) (codex.Turn, error) {
	close(c.started)
	<-ctx.Done()

	return codex.Turn{}, ctx.Err()
}

func (c *blockingInterruptClient) RunTurn(ctx context.Context, _ codex.TurnStartRequest) (codex.Turn, error) {
	c.runOnce.Do(func() { close(c.runStarted) })

	return codex.Turn{ID: "native-turn"}, nil
}

func (c *blockingInterruptClient) CancelTurn(context.Context, string, string) error {
	c.interruptOnce.Do(func() { close(c.interruptStarted) })
	<-c.interruptRelease

	return nil
}

func TestDeleteAdmissionPreventsLateNativeTurn(t *testing.T) {
	client := &blockingContainmentClient{
		spyCodexClient: newSpyCodexClient(),
		containStarted: make(chan struct{}),
		containRelease: make(chan struct{}),
	}
	agent := NewAgent(withClientFactory(func(context.Context, codex.Options) (codex.Client, error) {
		return client, nil
	}))
	agent.setAgentClient(newRecordingAgentClient())

	created, err := agent.NewSession(context.Background(), NewSessionRequest("/tmp/project"))
	require.NoError(t, err)

	deleteDone := make(chan error, 1)
	go func() {
		_, deleteErr := agent.UnstableDeleteSession(context.Background(), DeleteSessionRequest(created.SessionId))
		deleteDone <- deleteErr
	}()
	<-client.containStarted

	// The tombstone is already durable, so the wire answer is the uniform
	// unknown-session verdict a host may treat as final; the wrapper behind it is
	// still owned, and refuses on its own closed mark.
	_, promptErr := agent.Prompt(
		context.Background(),
		TextPromptRequest(created.SessionId, "late-turn", "must not start"),
	)
	requireUnknownSession(t, promptErr)

	active := agent.activeSession(created.SessionId)
	_, promptErr = active.Prompt(
		context.Background(),
		TextPromptRequest(created.SessionId, "direct-late-turn", "must not start"),
	)
	require.ErrorContains(t, promptErr, "session close in progress")
	client.mu.Lock()
	require.Empty(t, client.lastTurn.ThreadID, "prompt admitted native work after delete admission")
	client.mu.Unlock()

	close(client.containRelease)
	require.NoError(t, <-deleteDone)
	require.True(t, agent.isDeleted(created.SessionId))
	require.NoError(t, agent.Close())
}

// TestDeleteStoreFailureLeavesTheSessionUntouched pins the tombstone as the
// delete's first act. A delete the store refuses has torn nothing down, so the
// session it addressed is exactly as the host left it: not hidden, not closed to
// prompts, and still holding its live native thread.
func TestDeleteStoreFailureLeavesTheSessionUntouched(t *testing.T) {
	deleteErr := errors.New("store delete failed")
	store := &configurableStore{deleteErr: deleteErr}
	client := newSpyCodexClient()
	agent := NewAgent(
		WithSessionStore(store),
		withClientFactory(func(context.Context, codex.Options) (codex.Client, error) { return client, nil }),
	)
	created, err := agent.NewSession(context.Background(), NewSessionRequest("/tmp/project"))
	require.NoError(t, err)
	active := agent.activeSession(created.SessionId)

	_, err = agent.UnstableDeleteSession(context.Background(), DeleteSessionRequest(created.SessionId))
	require.ErrorIs(t, err, deleteErr)
	require.Same(t, active, agent.activeSession(created.SessionId))
	require.False(t, active.clientDead, "a refused tombstone must not have unsubscribed the thread")
	require.False(t, active.closing, "a refused tombstone must leave prompt admission open")
	require.False(t, agent.isDeleted(created.SessionId))

	listed, err := agent.ListSessions(context.Background(), acp.ListSessionsRequest{})
	require.NoError(t, err)
	require.Len(t, listed.Sessions, 1)
	require.Equal(t, created.SessionId, listed.Sessions[0].SessionId)

	require.NoError(t, agent.Close())
}

type blockingContainmentClient struct {
	*spyCodexClient
	containStarted chan struct{}
	containOnce    sync.Once
	containRelease chan struct{}
}

func (c *blockingContainmentClient) ListBackgroundTerminals(
	context.Context,
	codex.BackgroundTerminalListRequest,
) (codex.BackgroundTerminalListResponse, error) {
	c.containOnce.Do(func() { close(c.containStarted) })
	<-c.containRelease

	return codex.BackgroundTerminalListResponse{}, nil
}

func TestDeleteSessionSurfacesNativeCleanupErrorAfterTombstone(t *testing.T) {
	ctx := context.Background()
	store := NewInMemorySessionStore()
	cleanupErr := errors.New("delete native failed")
	client := &errorCodexClient{spyCodexClient: newSpyCodexClient(), deleteErr: cleanupErr}
	agent := NewAgent(
		WithSessionStore(store),
		withClientFactory(func(context.Context, codex.Options) (codex.Client, error) { return client, nil }),
	)

	resp, err := agent.NewSession(ctx, NewSessionRequest("/tmp/project"))
	if err != nil {
		t.Fatalf("NewSession returned error: %v", err)
	}
	if seedErr := store.Replace(ctx, SessionKey{SessionID: string(resp.SessionId)}, []SessionStoreReplacement{{
		Key:     SessionKey{SessionID: string(resp.SessionId)},
		Entries: []SessionStoreEntry{SessionStoreEntry(`{"type":"event_msg","payload":{"type":"agent_message","message":"hi"}}`)},
	}}); seedErr != nil {
		t.Fatalf("seed store: %v", seedErr)
	}

	if _, delErr := agent.UnstableDeleteSession(ctx, DeleteSessionRequest(resp.SessionId)); !errors.Is(delErr, cleanupErr) {
		t.Fatalf("DeleteSession error = %v, want cleanup error", delErr)
	}
	entries, err := store.Load(ctx, SessionKey{SessionID: string(resp.SessionId)})
	if err != nil {
		t.Fatalf("Load tombstoned store returned error: %v", err)
	}
	if len(entries) != 0 {
		t.Fatalf("tombstoned entries = %#v", entries)
	}
	if _, err := agent.ResumeSession(ctx, ResumeSessionRequest(resp.SessionId, "/tmp/project")); err == nil {
		t.Fatal("ResumeSession after failed cleanup succeeded")
	} else {
		requireUnknownSession(t, err)
	}
}

// TestDeleteHidesTheIdBeforeAFailedTeardownAndRetriesIt pins the delete's
// tombstone-first order. The durable tombstone is written before any teardown
// runs, so a boundary that cannot prove containment surfaces its error with the
// id already hidden from list, load, resume, and prompt — while the agent goes
// on owning the native scope, and the next delete runs the same ladder again.
func TestDeleteHidesTheIdBeforeAFailedTeardownAndRetriesIt(t *testing.T) {
	ctx := context.Background()
	sweepFailure := errors.New("thread-scoped containment failed")
	client := &recoverableBackgroundTerminalClient{
		spyCodexClient: newSpyCodexClient(),
		err:            sweepFailure,
	}
	agent := NewAgent(
		WithSessionStore(NewInMemorySessionStore()),
		withClientFactory(func(context.Context, codex.Options) (codex.Client, error) { return client, nil }),
	)
	agent.setAgentClient(newRecordingAgentClient())

	created, err := agent.NewSession(ctx, NewSessionRequest("/tmp/project"))
	require.NoError(t, err)
	active := agent.activeSession(created.SessionId)
	require.NotNil(t, active)

	_, err = agent.UnstableDeleteSession(ctx, DeleteSessionRequest(created.SessionId))
	require.ErrorIs(t, err, sweepFailure, "an unproved boundary fails the delete")
	require.True(t, agent.isDeleted(created.SessionId), "the tombstone lands ahead of the teardown that failed")
	require.Same(t, active, agent.activeSession(created.SessionId),
		"a hidden session keeps its native scope so the retry can reach it")

	_, promptErr := agent.Prompt(ctx, TextPromptRequest(created.SessionId, "late", "must not start"))
	requireUnknownSession(t, promptErr)

	_, loadErr := agent.LoadSession(ctx, LoadSessionRequest(created.SessionId, "/tmp/project"))
	requireUnknownSession(t, loadErr)

	_, resumeErr := agent.ResumeSession(ctx, ResumeSessionRequest(created.SessionId, "/tmp/project"))
	requireUnknownSession(t, resumeErr)

	_, closeErr := agent.CloseSession(ctx, acp.CloseSessionRequest{SessionId: created.SessionId})
	requireUnknownSession(t, closeErr)

	listed, err := agent.ListSessions(ctx, acp.ListSessionsRequest{})
	require.NoError(t, err)
	require.Empty(t, listed.Sessions, "a tombstoned id is hidden even while its wrapper is retained")

	// The sweep now answers, so the second delete completes the teardown the
	// first one left owed.
	client.recover()

	_, err = agent.UnstableDeleteSession(ctx, DeleteSessionRequest(created.SessionId))
	require.NoError(t, err)
	require.Nil(t, agent.activeSession(created.SessionId))
	require.NoError(t, agent.Close())
}

// TestDeleteRacingACloseHidesTheIdAndDefersTheLadder pins the delete that finds
// the close boundary already taken. The tombstone is written before the ladder
// is even attempted, so the id is hidden and the conflict is what the delete
// reports; the teardown is the next delete's, once the boundary is free.
func TestDeleteRacingACloseHidesTheIdAndDefersTheLadder(t *testing.T) {
	ctx := context.Background()
	client := newSpyCodexClient()
	agent := NewAgent(
		WithSessionStore(NewInMemorySessionStore()),
		withClientFactory(func(context.Context, codex.Options) (codex.Client, error) { return client, nil }),
	)

	created, err := agent.NewSession(ctx, NewSessionRequest("/tmp/project"))
	require.NoError(t, err)
	active := agent.activeSession(created.SessionId)
	require.NotNil(t, active)

	active.mu.Lock()
	active.closing = true
	active.mu.Unlock()

	_, err = agent.UnstableDeleteSession(ctx, DeleteSessionRequest(created.SessionId))
	require.ErrorContains(t, err, "session close in progress")
	require.True(t, agent.isDeleted(created.SessionId), "the tombstone lands before the ladder it could not run")

	active.mu.Lock()
	active.closing = false
	active.mu.Unlock()

	_, err = agent.UnstableDeleteSession(ctx, DeleteSessionRequest(created.SessionId))
	require.NoError(t, err)
	require.Nil(t, agent.activeSession(created.SessionId))
	require.NoError(t, agent.Close())
}

// recoverableBackgroundTerminalClient fails the thread-scoped sweep until it is
// told to stop, so one agent can observe a failed containment boundary and the
// delete that retries it.
type recoverableBackgroundTerminalClient struct {
	*spyCodexClient
	sweepMu sync.Mutex
	err     error
}

func (c *recoverableBackgroundTerminalClient) recover() {
	c.sweepMu.Lock()
	defer c.sweepMu.Unlock()

	c.err = nil
}

func (c *recoverableBackgroundTerminalClient) ListBackgroundTerminals(
	context.Context,
	codex.BackgroundTerminalListRequest,
) (codex.BackgroundTerminalListResponse, error) {
	c.sweepMu.Lock()
	defer c.sweepMu.Unlock()

	return codex.BackgroundTerminalListResponse{}, c.err
}

func TestDeleteSessionTombstoneSurvivesRestartAndRetriesNativeCleanup(t *testing.T) {
	ctx := context.Background()
	store := NewInMemorySessionStore()
	sessionID := acp.SessionId("11111111-1111-4111-8111-111111111111")
	if err := store.Delete(ctx, SessionKey{SessionID: string(sessionID)}); err != nil {
		t.Fatalf("seed tombstone: %v", err)
	}

	nativeClient := &errorCodexClient{
		spyCodexClient: newSpyCodexClient(),
		listThreads: []codex.Thread{{
			ID:        "thread-native",
			SessionID: string(sessionID),
			Cwd:       "/tmp/project",
			Title:     "Native",
		}},
	}
	agent := NewAgent(
		WithSessionStore(store),
		withClientFactory(func(context.Context, codex.Options) (codex.Client, error) { return nativeClient, nil }),
	)

	listResp, err := agent.ListSessions(ctx, ListSessionsRequest(WithListSessionsCwd("/tmp/project")))
	if err != nil {
		t.Fatalf("ListSessions returned error: %v", err)
	}
	if len(listResp.Sessions) != 0 {
		t.Fatalf("ListSessions returned tombstoned native sessions: %#v", listResp.Sessions)
	}
	if len(nativeClient.deletedThreadSnapshot()) != 0 {
		t.Fatalf("ListSessions should not need native cleanup without an in-process deleted set: %#v", nativeClient.deletedThreadSnapshot())
	}

	if _, err := agent.LoadSession(ctx, LoadSessionRequest(sessionID, "/tmp/project")); err == nil {
		t.Fatal("LoadSession returned nil error")
	} else {
		requireUnknownSession(t, err)
	}
	if !containsString(nativeClient.deletedThreadSnapshot(), "thread-native") {
		t.Fatalf("LoadSession did not retry native cleanup: %#v", nativeClient.deletedThreadSnapshot())
	}

	if _, err := agent.ResumeSession(ctx, ResumeSessionRequest(sessionID, "/tmp/project")); err == nil {
		t.Fatal("ResumeSession returned nil error")
	} else {
		requireUnknownSession(t, err)
	}
}

func TestDeleteRetainedRuntimeThreadReleasesOwnership(t *testing.T) {
	ctx := context.Background()
	store := NewInMemorySessionStore()
	client := newSpyCodexClient()
	agent := NewAgent(
		WithSessionStore(store),
		withClientFactory(func(context.Context, codex.Options) (codex.Client, error) { return client, nil }),
	)

	created, err := agent.NewSession(ctx, NewSessionRequest("/tmp/project"))
	require.NoError(t, err)
	active := agent.activeSession(created.SessionId)
	require.NotNil(t, active)
	threadID := active.codexThreadID
	_, err = agent.CloseSession(ctx, acp.CloseSessionRequest{SessionId: created.SessionId})
	require.NoError(t, err)
	require.NotNil(t, agent.retainedThreads[created.SessionId])

	_, err = agent.UnstableDeleteSession(ctx, acp.UnstableDeleteSessionRequest{SessionId: created.SessionId})
	require.NoError(t, err)
	require.Nil(t, agent.retainedThreads[created.SessionId])
	require.Contains(t, client.deletedThreadSnapshot(), threadID)
}

func TestRetainedDeleteAndRetryCleanupErrors(t *testing.T) {
	ctx := context.Background()
	entries := []SessionStoreEntry{SessionStoreEntry(`{"type":"session_meta","payload":{"id":"thread"}}`)}
	oldRemove := removeMaterializedRolloutFile
	t.Cleanup(func() { removeMaterializedRolloutFile = oldRemove })

	fixture := func(id acp.SessionId) (*Agent, *retainedRuntimeThread) {
		client := newSpyCodexClient()
		agent := NewAgent(
			WithSessionStore(NewInMemorySessionStore()),
			withClientFactory(func(context.Context, codex.Options) (codex.Client, error) { return client, nil }),
		)
		agent.runtimeClient = client
		agent.runtimeEpoch = 4
		path, err := materializeRollout(t.TempDir(), entries)
		require.NoError(t, err)
		retained := &retainedRuntimeThread{
			sessionID:        id,
			threadID:         "thread",
			path:             path,
			client:           client,
			epoch:            4,
			materializedPath: path,
		}
		agent.retainedThreads[id] = retained

		return agent, retained
	}

	agent, retained := fixture("delete")
	removeMaterializedRolloutFile = func(string) error { return errors.New("retained delete cleanup failed") }
	_, err := agent.UnstableDeleteSession(ctx, acp.UnstableDeleteSessionRequest{SessionId: retained.sessionID})
	require.ErrorContains(t, err, "retained delete cleanup failed")
	require.True(t, retained.nativeEnded)
	require.Same(t, retained, agent.retainedThreads[retained.sessionID])
	removeMaterializedRolloutFile = oldRemove
	require.NoError(t, agent.releaseRetainedRuntimeThreads(retained.client, retained.epoch))

	agent, retained = fixture("retry")
	removeMaterializedRolloutFile = func(string) error { return errors.New("retained retry cleanup failed") }
	agent.retryDeleteNativeCodexSession(ctx, retained.sessionID, "")
	require.True(t, retained.nativeEnded)
	require.Same(t, retained, agent.retainedThreads[retained.sessionID])
	removeMaterializedRolloutFile = oldRemove
	require.NoError(t, agent.releaseRetainedRuntimeThreads(retained.client, retained.epoch))
}

func containsString(values []string, want string) bool {
	for _, value := range values {
		if value == want {
			return true
		}
	}

	return false
}

func TestLifecycleMetaRejectsDeletedNamespaces(t *testing.T) {
	ctx := context.Background()
	agent := NewAgent(withClientFactory(func(context.Context, codex.Options) (codex.Client, error) {
		return newSpyCodexClient(), nil
	}))
	t.Cleanup(func() { require.NoError(t, agent.Close()) })

	cases := []struct {
		name  string
		meta  map[string]any
		field string
	}{
		{
			name:  "mode option",
			meta:  map[string]any{codexMetaKey: map[string]any{"options": map[string]any{"mode": "plan"}}},
			field: "_meta.codex.options.mode",
		},
		{
			name:  "goals",
			meta:  map[string]any{codexMetaKey: map[string]any{"goal": map[string]any{"objective": "ship"}}},
			field: "_meta.codex.goal",
		},
		{
			name:  "sdk message",
			meta:  map[string]any{codexMetaKey: map[string]any{"emitRawSDKMessages": true}},
			field: "_meta.codex.emitRawSDKMessages",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := agent.NewSession(ctx, NewSessionRequest("/tmp/project", WithSessionMeta(tc.meta))); err == nil {
				t.Fatal("NewSession accepted deleted metadata")
			} else {
				requireUnsupportedFieldError(t, err, tc.field)
			}
		})
	}

	t.Run("foreign module path ignored", func(t *testing.T) {
		meta := map[string]any{"github.com/savid/acp-go-codex": map[string]any{"anything": true}}
		if _, err := agent.NewSession(ctx, NewSessionRequest("/tmp/project", WithSessionMeta(meta))); err != nil {
			t.Fatalf("NewSession rejected foreign module-path _meta namespace instead of ignoring it: %v", err)
		}
	})
}

func TestAgentLifecycleErrorBranches(t *testing.T) {
	ctx := context.Background()

	closed := NewAgent()
	if err := closed.Close(); err != nil {
		t.Fatalf("Close returned error: %v", err)
	}
	if _, err := closed.NewSession(ctx, NewSessionRequest("/tmp/project")); err == nil {
		t.Fatal("closed NewSession succeeded")
	}
	if _, err := closed.ResumeSession(ctx, ResumeSessionRequest("s", "/tmp/project")); err == nil {
		t.Fatal("closed ResumeSession succeeded")
	}
	if _, err := closed.LoadSession(ctx, LoadSessionRequest("s", "/tmp/project")); err == nil {
		t.Fatal("closed LoadSession succeeded")
	}
	if _, err := closed.ListSessions(ctx, acp.ListSessionsRequest{}); err == nil {
		t.Fatal("closed ListSessions succeeded")
	}

	client := newSpyCodexClient()
	agent := NewAgent(withClientFactory(func(context.Context, codex.Options) (codex.Client, error) { return client, nil }))
	if _, err := agent.NewSession(ctx, NewSessionRequest("relative")); err == nil {
		t.Fatal("NewSession accepted relative cwd")
	}
	if _, err := agent.NewSession(ctx, NewSessionRequest("/tmp/project", WithSessionMeta(map[string]any{codexMetaKey: "bad"}))); err == nil {
		t.Fatal("NewSession accepted invalid meta")
	}
	if _, err := agent.NewSession(ctx, NewSessionRequest("/tmp/project", WithSessionMCPServers(acp.McpServer{Sse: &acp.McpServerSseInline{Name: "sse"}}))); err == nil {
		t.Fatal("NewSession accepted unsupported MCP server")
	}

	factoryErr := errors.New("factory failed")
	factoryAgent := NewAgent(withClientFactory(func(context.Context, codex.Options) (codex.Client, error) { return nil, factoryErr }))
	if _, err := factoryAgent.NewSession(ctx, NewSessionRequest("/tmp/project")); !errors.Is(err, factoryErr) {
		t.Fatalf("NewSession factory err=%v", err)
	}

	startErr := errors.New("start failed")
	startClient := &errorCodexClient{spyCodexClient: newSpyCodexClient(), startErr: startErr}
	startAgent := NewAgent(withClientFactory(func(context.Context, codex.Options) (codex.Client, error) { return startClient, nil }))
	if _, err := startAgent.NewSession(ctx, NewSessionRequest("/tmp/project")); !errors.Is(err, startErr) {
		t.Fatalf("NewSession start err=%v", err)
	}
	if startClient.closed {
		t.Fatal("NewSession start error closed the Agent-owned shared runtime")
	}

	limitAgent := NewAgent(
		WithConcurrencyLimits(ConcurrencyLimits{MaxActiveSessions: 1, MaxConcurrentClientCalls: 1}),
		withClientFactory(func(context.Context, codex.Options) (codex.Client, error) { return newSpyCodexClient(), nil }),
	)
	if _, err := limitAgent.NewSession(ctx, NewSessionRequest("/tmp/project")); err != nil {
		t.Fatalf("first limited NewSession returned error: %v", err)
	}
	if _, err := limitAgent.NewSession(ctx, NewSessionRequest("/tmp/other")); err == nil {
		t.Fatal("NewSession ignored active session limit")
	}
}

func TestUnknownSessionUniformInvalidParams(t *testing.T) {
	ctx := context.Background()
	agent := NewAgent(
		WithSessionStore(NewInMemorySessionStore()),
		withClientFactory(func(context.Context, codex.Options) (codex.Client, error) {
			return newSpyCodexClient(), nil
		}),
	)

	_, promptErr := agent.Prompt(ctx, TextPromptRequest("missing", "test-turn", "hello"))
	requireUnknownSession(t, promptErr)
	requireUnknownSession(t, agent.Cancel(ctx, acp.CancelNotification{SessionId: "missing"}))
	_, configErr := agent.SetSessionConfigOption(ctx, SetConfigOptionRequest("missing", configModel, "gpt"))
	requireUnknownSession(t, configErr)
	_, closeErr := agent.CloseSession(ctx, acp.CloseSessionRequest{SessionId: "missing"})
	requireUnknownSession(t, closeErr)
	_, resumeErr := agent.ResumeSession(ctx, ResumeSessionRequest("missing", "/tmp/project"))
	requireUnknownSession(t, resumeErr)
	_, loadErr := agent.LoadSession(ctx, LoadSessionRequest("missing", "/tmp/project"))
	requireUnknownSession(t, loadErr)
}

func TestAgentSessionOperationErrorBranches(t *testing.T) {
	ctx := context.Background()
	client := newSpyCodexClient()
	agent := NewAgent(withClientFactory(func(context.Context, codex.Options) (codex.Client, error) { return client, nil }))
	resp, err := agent.NewSession(ctx, NewSessionRequest("/tmp/project"))
	if err != nil {
		t.Fatalf("NewSession returned error: %v", err)
	}

	if _, promptErr := agent.Prompt(ctx, TextPromptRequest("missing", "test-turn", "hello")); promptErr == nil {
		t.Fatal("Prompt missing session succeeded")
	}
	if cancelErr := agent.Cancel(ctx, acp.CancelNotification{SessionId: "missing"}); cancelErr == nil {
		t.Fatal("Cancel missing session succeeded")
	}
	if _, closeMissingErr := agent.CloseSession(ctx, acp.CloseSessionRequest{SessionId: "missing"}); closeMissingErr == nil {
		t.Fatal("CloseSession missing session succeeded")
	}
	if _, deleteMissingErr := agent.UnstableDeleteSession(ctx, acp.UnstableDeleteSessionRequest{}); deleteMissingErr == nil {
		t.Fatal("DeleteSession missing id succeeded")
	}
	deleteStoreErr := errors.New("store delete failed")
	deleteStoreAgent := NewAgent(WithSessionStore(&configurableStore{deleteErr: deleteStoreErr}))
	if _, storeDelErr := deleteStoreAgent.UnstableDeleteSession(ctx, acp.UnstableDeleteSessionRequest{SessionId: "session-1"}); !errors.Is(storeDelErr, deleteStoreErr) {
		t.Fatalf("DeleteSession store error = %v", storeDelErr)
	}
	deleteCloseErr := errors.New("delete close failed")
	deleteCloseClient := &errorCodexClient{spyCodexClient: newSpyCodexClient(), closeErr: deleteCloseErr}
	deleteCloseAgent := NewAgent(
		WithSessionStore(NewInMemorySessionStore()),
		withClientFactory(func(context.Context, codex.Options) (codex.Client, error) { return deleteCloseClient, nil }),
	)
	deleteCloseResp, err := deleteCloseAgent.NewSession(ctx, NewSessionRequest("/tmp/project"))
	if err != nil {
		t.Fatalf("delete close NewSession returned error: %v", err)
	}
	if _, closeDelErr := deleteCloseAgent.UnstableDeleteSession(ctx, DeleteSessionRequest(deleteCloseResp.SessionId)); closeDelErr != nil {
		t.Fatalf("DeleteSession released thread with error = %v", closeDelErr)
	}
	if closeErr := deleteCloseAgent.Close(); !errors.Is(closeErr, deleteCloseErr) {
		t.Fatalf("Agent close error = %v", closeErr)
	}

	cancelErr := codex.ErrThreadNotFound
	cancelClient := &cancelErrorClient{spyCodexClient: newSpyCodexClient(), cancelErr: cancelErr}
	cancelAgent := NewAgent(withClientFactory(func(context.Context, codex.Options) (codex.Client, error) { return cancelClient, nil }))
	cancelResp, err := cancelAgent.NewSession(ctx, NewSessionRequest("/tmp/project"))
	if err != nil {
		t.Fatalf("cancel NewSession returned error: %v", err)
	}
	if cancelCodexErr := cancelAgent.Cancel(ctx, acp.CancelNotification{SessionId: cancelResp.SessionId}); cancelCodexErr == nil {
		t.Fatal("Cancel ignored Codex error")
	}

	fatalClient := &runEventsClient{spyCodexClient: newSpyCodexClient(), runErr: codex.ErrConnectionClosed}
	fatalAgent := NewAgent(withClientFactory(func(context.Context, codex.Options) (codex.Client, error) { return fatalClient, nil }))
	fatalResp, err := fatalAgent.NewSession(ctx, NewSessionRequest("/tmp/project"))
	if err != nil {
		t.Fatalf("fatal NewSession returned error: %v", err)
	}
	if _, fatalPromptErr := fatalAgent.Prompt(ctx, TextPromptRequest(fatalResp.SessionId, "test-turn", "hello")); !isTurnFailure(fatalPromptErr, codex.CauseTransport) {
		t.Fatalf("Prompt fatal process error = %v, want codex_turn_failed transport", fatalPromptErr)
	}
	if _, sessionErr := fatalAgent.session(fatalResp.SessionId); sessionErr != nil {
		t.Fatalf("fatal Prompt must leave session addressable: %v", sessionErr)
	}

	closeErr := errors.New("close failed")
	closeClient := &errorCodexClient{spyCodexClient: newSpyCodexClient(), closeErr: closeErr}
	closeAgent := NewAgent(withClientFactory(func(context.Context, codex.Options) (codex.Client, error) { return closeClient, nil }))
	closeResp, err := closeAgent.NewSession(ctx, NewSessionRequest("/tmp/project"))
	if err != nil {
		t.Fatalf("close NewSession returned error: %v", err)
	}
	if _, err := closeAgent.CloseSession(ctx, acp.CloseSessionRequest{SessionId: closeResp.SessionId}); err != nil {
		t.Fatalf("CloseSession logical release err=%v", err)
	}
	if _, err := closeAgent.session(closeResp.SessionId); err == nil {
		t.Fatal("CloseSession did not remove session")
	}
	if err := closeAgent.Close(); !errors.Is(err, closeErr) {
		t.Fatalf("Agent runtime close err=%v", err)
	}

	if session := agent.removeSession(resp.SessionId); session == nil {
		t.Fatal("removeSession returned nil")
	}
}

func TestListSessionsUsesActiveAndStoreAuthority(t *testing.T) {
	ctx := context.Background()
	store := &configurableStore{summaries: []SessionSummary{
		{SessionID: "active", Cwd: "/tmp/project", UpdatedAtUnixMilli: 100, Title: "Active"},
		{SessionID: "stored", Cwd: "/tmp/project", UpdatedAtUnixMilli: 200, Title: "Stored", Meta: map[string]any{"origin": "test"}},
		{SessionID: "other", Cwd: "/tmp/other", UpdatedAtUnixMilli: 300},
	}}
	client := &errorCodexClient{spyCodexClient: newSpyCodexClient(), listErr: errors.New("native list must not be lifecycle authority")}
	agent := NewAgent(
		WithSessionStore(store),
		withClientFactory(func(context.Context, codex.Options) (codex.Client, error) { return client, nil }),
	)
	active := newSession(agent, "active", "/tmp/project", nil, codex.Thread{ID: "active-thread", Title: "Active"}, client, sessionMeta{}, nil)
	if err := agent.storeStartedSession(active); err != nil {
		t.Fatalf("store active: %v", err)
	}
	activeOther := newSession(agent, "active-other", "/tmp/other", nil, codex.Thread{ID: "active-other-thread", Title: "Other"}, client, sessionMeta{}, nil)
	if err := agent.storeStartedSession(activeOther); err != nil {
		t.Fatalf("store other active: %v", err)
	}
	cwd := "/tmp/project"
	list, err := agent.ListSessions(ctx, acp.ListSessionsRequest{Cwd: &cwd})
	if err != nil {
		t.Fatalf("ListSessions returned error: %v", err)
	}
	if len(list.Sessions) != 2 {
		t.Fatalf("ListSessions active/store sessions = %#v", list.Sessions)
	}

	residualClient := &errorCodexClient{spyCodexClient: newSpyCodexClient(), listThreads: []codex.Thread{{
		ID: "residual-thread", SessionID: "residual", Cwd: "/tmp/project", Title: "Residual native thread",
	}}}
	nativeAgent := NewAgent(withClientFactory(func(context.Context, codex.Options) (codex.Client, error) { return residualClient, nil }))
	native, err := nativeAgent.ListSessions(ctx, acp.ListSessionsRequest{Cwd: &cwd})
	if err != nil {
		t.Fatalf("ListSessions with residual native thread returned error: %v", err)
	}
	if len(native.Sessions) != 0 {
		t.Fatalf("ListSessions adopted residual native threads: %#v", native.Sessions)
	}
	badCwd := "relative"
	if _, badCwdErr := nativeAgent.ListSessions(ctx, acp.ListSessionsRequest{Cwd: &badCwd}); badCwdErr == nil {
		t.Fatal("ListSessions accepted relative cwd")
	}
	badCursor := "%%%"
	if _, badCursorErr := nativeAgent.ListSessions(ctx, acp.ListSessionsRequest{Cursor: &badCursor}); badCursorErr == nil {
		t.Fatal("ListSessions accepted invalid cursor")
	}
	storeErrAgent := NewAgent(WithSessionStore(&configurableStore{listErr: errors.New("store list")}))
	if _, storeListErr := storeErrAgent.ListSessions(ctx, acp.ListSessionsRequest{Cwd: &cwd}); storeListErr == nil {
		t.Fatal("ListSessions ignored store list error")
	}

	sessions := make([]acp.SessionInfo, listSessionsPageSize+1)
	for i := range sessions {
		sessions[i].SessionId = acp.SessionId(fmt.Sprintf("s-%02d", i))
	}
	page, next, err := paginateSessionInfos(sessions, nil)
	if err != nil || len(page) != listSessionsPageSize || next == nil {
		t.Fatalf("paginate first page len=%d next=%v err=%v", len(page), next, err)
	}
	if decoded, err := decodeListCursor(next); err != nil || decoded != listSessionsPageSize {
		t.Fatalf("decode cursor = %d err=%v", decoded, err)
	}
	if _, _, err := paginateSessionInfos(sessions, &badCursor); err == nil {
		t.Fatal("paginate accepted invalid cursor")
	}
	past := encodeListCursor(len(sessions) + 1)
	if _, _, err := paginateSessionInfos(sessions, &past); err == nil {
		t.Fatal("paginate accepted past-end cursor")
	}
	negative := base64.RawURLEncoding.EncodeToString([]byte("-1"))
	if _, err := decodeListCursor(&negative); err == nil {
		t.Fatal("decodeListCursor accepted negative cursor")
	}
}

func TestResumeLoadAndMaterializedBranches(t *testing.T) {
	ctx := context.Background()
	store := &configurableStore{}
	agent := NewAgent(WithSessionStore(store), withClientFactory(func(context.Context, codex.Options) (codex.Client, error) {
		return newSpyCodexClient(), nil
	}))
	if _, err := agent.ResumeSession(ctx, ResumeSessionRequest("s", "relative")); err == nil {
		t.Fatal("ResumeSession accepted relative cwd")
	}
	if _, err := agent.LoadSession(ctx, LoadSessionRequest("s", "relative")); err == nil {
		t.Fatal("LoadSession accepted relative cwd")
	}
	agent.deleted["deleted"] = struct{}{}
	if _, err := agent.ResumeSession(ctx, ResumeSessionRequest("deleted", "/tmp/project")); err == nil {
		t.Fatal("ResumeSession deleted id succeeded")
	}
	if _, err := agent.LoadSession(ctx, LoadSessionRequest("deleted", "/tmp/project")); err == nil {
		t.Fatal("LoadSession deleted id succeeded")
	}
	if _, err := agent.ResumeSession(ctx, ResumeSessionRequest("s", "/tmp/project", WithSessionMeta(map[string]any{codexMetaKey: "bad"}))); err == nil {
		t.Fatal("ResumeSession accepted invalid meta")
	}
	if _, err := agent.LoadSession(ctx, LoadSessionRequest("s", "/tmp/project", WithSessionMeta(map[string]any{codexMetaKey: "bad"}))); err == nil {
		t.Fatal("LoadSession accepted invalid meta")
	}
	store.loadErr = errors.New("load failed")
	if _, err := agent.ResumeSession(ctx, ResumeSessionRequest("s", "/tmp/project")); err == nil {
		t.Fatal("ResumeSession ignored store load error")
	}
	if _, err := agent.LoadSession(ctx, LoadSessionRequest("s", "/tmp/project")); err == nil {
		t.Fatal("LoadSession ignored store load error")
	}
	store.loadErr = nil
	if _, err := agent.ResumeSession(ctx, ResumeSessionRequest("s", "/tmp/project")); err == nil {
		t.Fatal("ResumeSession explicit empty store succeeded")
	}
	if _, err := agent.LoadSession(ctx, LoadSessionRequest("s", "/tmp/project")); err == nil {
		t.Fatal("LoadSession explicit empty store succeeded")
	}

	resumeClient := &errorCodexClient{spyCodexClient: newSpyCodexClient(), listThreads: []codex.Thread{{ID: "native", SessionID: "native"}}}
	resumeAgent := NewAgent(withClientFactory(func(context.Context, codex.Options) (codex.Client, error) { return resumeClient, nil }))
	if resumeAgent.options.SessionStore == nil {
		t.Fatal("NewAgent did not install the default in-memory session store")
	}
	if _, err := resumeAgent.ResumeSession(ctx, ResumeSessionRequest("native", "/tmp/project")); err == nil {
		t.Fatal("ResumeSession adopted a residual native thread absent from the default store")
	}
	if resumeAgent.activeSession("native") != nil {
		t.Fatal("ResumeSession installed a residual native thread")
	}
	resumeClient.mu.Lock()
	resumeReq := resumeClient.resume
	resumeClient.mu.Unlock()
	if resumeReq.ThreadID != "" {
		t.Fatalf("ResumeSession called native resume for an unknown store session: %#v", resumeReq)
	}

	loadClient := &errorCodexClient{spyCodexClient: newSpyCodexClient(), listThreads: []codex.Thread{{ID: "native-load", SessionID: "native-load"}}}
	loadAgent := NewAgent(withClientFactory(func(context.Context, codex.Options) (codex.Client, error) { return loadClient, nil }))
	if _, err := loadAgent.LoadSession(ctx, LoadSessionRequest("native-load", "/tmp/project")); err == nil {
		t.Fatal("LoadSession adopted native history absent from the default store")
	}
	if loadAgent.activeSession("native-load") != nil {
		t.Fatal("LoadSession installed a residual native thread")
	}
	loadClient.mu.Lock()
	loadResumeReq := loadClient.resume
	loadClient.mu.Unlock()
	if loadResumeReq.ThreadID != "" {
		t.Fatalf("LoadSession called native resume for an unknown store session: %#v", loadResumeReq)
	}
}

func TestResumeLoadActiveSessionBranches(t *testing.T) {
	ctx := context.Background()

	activeClient := newSpyCodexClient()
	activeAgent := NewAgent(withClientFactory(func(context.Context, codex.Options) (codex.Client, error) { return activeClient, nil }))
	activeID := acp.SessionId("active")
	activeStart := codexSessionStart{Cwd: "/tmp/project", ResumeID: string(activeID)}
	activeSession := newSession(activeAgent, activeID, "/tmp/project", nil, codex.Thread{ID: string(activeID), SessionID: string(activeID)}, activeClient, sessionMeta{}, nil)
	activeSession.fingerprint = codexSessionStartFingerprint(activeStart)
	if err := activeAgent.storeStartedSession(activeSession); err != nil {
		t.Fatalf("store active session: %v", err)
	}
	t.Cleanup(activeSession.fenceSession)
	if _, err := activeAgent.ResumeSession(ctx, ResumeSessionRequest(activeID, "/tmp/project")); err != nil {
		t.Fatalf("ResumeSession active returned error: %v", err)
	}
	if _, err := activeAgent.ResumeSession(ctx, ResumeSessionRequest(activeID, "/tmp/project", WithSessionAdditionalDirectories("/tmp/different"))); err == nil {
		t.Fatal("ResumeSession changed active binding without store rows succeeded")
	}
	if activeAgent.activeSession(activeID) != activeSession {
		t.Fatal("ResumeSession changed active binding removed the usable session")
	}
	if _, err := activeAgent.LoadSession(ctx, LoadSessionRequest(activeID, "/tmp/project")); err == nil {
		t.Fatal("LoadSession active session without store rows succeeded")
	}
	if _, err := activeAgent.LoadSession(ctx, LoadSessionRequest(activeID, "/tmp/project", WithSessionAdditionalDirectories("/tmp/different"))); err == nil {
		t.Fatal("LoadSession changed active binding without store rows succeeded")
	}
	if activeAgent.activeSession(activeID) != activeSession {
		t.Fatal("LoadSession changed active binding removed the usable session")
	}
	if deleted := activeClient.deletedThreadSnapshot(); len(deleted) != 0 {
		t.Fatalf("store-missing active lifecycle deleted native threads: %#v", deleted)
	}
	defaultStore, ok := activeAgent.options.SessionStore.(*InMemorySessionStore)
	if !ok {
		t.Fatalf("default session store = %T, want *InMemorySessionStore", activeAgent.options.SessionStore)
	}
	if err := defaultStore.Replace(ctx, SessionKey{SessionID: string(activeID)}, []SessionStoreReplacement{{
		Key: SessionKey{SessionID: string(activeID)},
		Entries: []SessionStoreEntry{SessionStoreEntry(
			`{"type":"event_msg","payload":{"type":"agent_message","message":"stored history"}}`,
		)},
	}}); err != nil {
		t.Fatalf("seed active store: %v", err)
	}
	if _, err := activeAgent.LoadSession(ctx, LoadSessionRequest(activeID, "/tmp/project")); err != nil {
		t.Fatalf("LoadSession active with store rows returned error: %v", err)
	}

	activeLoadErrAgent := NewAgent(WithSessionStore(&configurableStore{loadErr: errors.New("active load failed")}))
	activeLoadSession := newSession(activeLoadErrAgent, activeID, "/tmp/project", nil, codex.Thread{ID: string(activeID), SessionID: string(activeID)}, newSpyCodexClient(), sessionMeta{}, nil)
	activeLoadSession.fingerprint = codexSessionStartFingerprint(activeStart)
	if err := activeLoadErrAgent.storeStartedSession(activeLoadSession); err != nil {
		t.Fatalf("store active load err session: %v", err)
	}
	t.Cleanup(activeLoadSession.fenceSession)
	if _, err := activeLoadErrAgent.LoadSession(ctx, LoadSessionRequest(activeID, "/tmp/project")); err == nil {
		t.Fatal("LoadSession active ignored store load error")
	}

	activeReplayErrStore := &configurableStore{entries: []SessionStoreEntry{SessionStoreEntry(`{"type":"event_msg","payload":{"type":"agent_message","message":"hi"}}`)}}
	activeReplayErrAgent := NewAgent(WithSessionStore(activeReplayErrStore))
	activeReplayErrAgent.setAgentClient(&errorAgentClient{recordingAgentClient: newRecordingAgentClient(), updateErr: errors.New("update failed")})
	activeReplayErrSession := newSession(activeReplayErrAgent, activeID, "/tmp/project", nil, codex.Thread{ID: string(activeID), SessionID: string(activeID)}, newSpyCodexClient(), sessionMeta{}, nil)
	activeReplayErrSession.fingerprint = codexSessionStartFingerprint(activeStart)
	if err := activeReplayErrAgent.storeStartedSession(activeReplayErrSession); err != nil {
		t.Fatalf("store active replay err session: %v", err)
	}
	t.Cleanup(activeReplayErrSession.fenceSession)
	if _, err := activeReplayErrAgent.LoadSession(ctx, LoadSessionRequest(activeID, "/tmp/project")); err == nil {
		t.Fatal("LoadSession active ignored rollout replay error")
	}
}

func TestMCPToolApprovalModeChangeForcesActiveNativeRebind(t *testing.T) {
	for _, lifecycle := range []struct {
		name string
		run  func(context.Context, *Agent, acp.SessionId, ...SessionRequestOption) error
	}{
		{
			name: "resume",
			run: func(ctx context.Context, agent *Agent, id acp.SessionId, opts ...SessionRequestOption) error {
				_, err := agent.ResumeSession(ctx, ResumeSessionRequest(id, "/tmp/project", opts...))

				return err
			},
		},
		{
			name: "load",
			run: func(ctx context.Context, agent *Agent, id acp.SessionId, opts ...SessionRequestOption) error {
				_, err := agent.LoadSession(ctx, LoadSessionRequest(id, "/tmp/project", opts...))

				return err
			},
		},
	} {
		t.Run(lifecycle.name, func(t *testing.T) {
			ctx := context.Background()
			store := NewInMemorySessionStore()
			client := newRuntimeRecordingClient()
			agent := NewAgent(
				WithSessionStore(store),
				withClientFactory(func(context.Context, codex.Options) (codex.Client, error) { return client, nil }),
			)
			t.Cleanup(func() { require.NoError(t, agent.Close()) })
			server := HTTPMCPServer("marker", "https://marker.example.test/mcp", map[string]string{
				"Authorization": "Bearer marker",
			})

			created, err := agent.NewSession(ctx, NewSessionRequest(
				"/tmp/project",
				WithSessionMCPServers(server),
				WithSessionCodexOptions(NewCodexOptions(WithCodexMCPToolApprovalMode("auto"))),
			))
			require.NoError(t, err)
			require.NoError(t, store.Replace(ctx, SessionKey{SessionID: string(created.SessionId)}, []SessionStoreReplacement{{
				Key: SessionKey{SessionID: string(created.SessionId)},
				Entries: []SessionStoreEntry{SessionStoreEntry(
					`{"type":"session_meta","payload":{"id":"thread-1","cwd":"/tmp/project"}}`,
				)},
			}}))

			client.mu.Lock()
			client.resumes = nil
			client.mu.Unlock()

			err = lifecycle.run(
				ctx,
				agent,
				created.SessionId,
				WithSessionMCPServers(server),
				WithSessionCodexOptions(NewCodexOptions(WithCodexMCPToolApprovalMode("prompt"))),
			)
			require.NoError(t, err)

			client.mu.Lock()
			resumes := append([]codex.ThreadResumeRequest(nil), client.resumes...)
			client.mu.Unlock()
			require.Len(t, resumes, 1, "mode-only lifecycle change skipped thread/resume")
			require.Equal(t, "thread-1", resumes[0].ThreadID)
			servers, ok := resumes[0].Config["mcp_servers"].(map[string]any)
			require.True(t, ok)
			marker, ok := servers["marker"].(map[string]any)
			require.True(t, ok)
			require.Equal(t, "prompt", marker["default_tools_approval_mode"])
			active := agent.activeSession(created.SessionId)
			require.NotNil(t, active)
			active.mu.Lock()
			approvalMode := active.mcpApprovalMode
			active.mu.Unlock()
			require.Equal(t, "prompt", approvalMode)
		})
	}
}

func TestResumeLoadMaterializedSessionBranches(t *testing.T) {
	ctx := context.Background()
	store := &configurableStore{}
	agent := NewAgent(WithSessionStore(store), withClientFactory(func(context.Context, codex.Options) (codex.Client, error) {
		return newSpyCodexClient(), nil
	}))

	materializedEntries := []SessionStoreEntry{SessionStoreEntry(`{"type":"response_item","payload":{"type":"message","role":"assistant","content":[{"type":"output_text","text":"agent"}]}}`)}
	store.entries = materializedEntries
	resumeResp, err := agent.ResumeSession(ctx, ResumeSessionRequest("stored", "/tmp/project"))
	if err != nil {
		t.Fatalf("ResumeSession materialized returned error: %v", err)
	}
	if resumeResp.Meta == nil {
		t.Fatal("ResumeSession materialized returned nil meta")
	}

	loadAgent := NewAgent(WithSessionStore(&configurableStore{entries: materializedEntries}), withClientFactory(func(context.Context, codex.Options) (codex.Client, error) {
		return newSpyCodexClient(), nil
	}))
	if _, err := loadAgent.LoadSession(ctx, LoadSessionRequest("stored", "/tmp/project")); err != nil {
		t.Fatalf("LoadSession materialized returned error: %v", err)
	}

	resumeErrAgent := NewAgent(WithSessionStore(&configurableStore{entries: materializedEntries}), withClientFactory(func(context.Context, codex.Options) (codex.Client, error) {
		return &errorCodexClient{spyCodexClient: newSpyCodexClient(), resumeErr: codex.ErrThreadNotFound}, nil
	}))
	if _, err := resumeErrAgent.ResumeSession(ctx, ResumeSessionRequest("stored", "/tmp/project")); err == nil {
		t.Fatal("ResumeSession materialized ignored resume error")
	}
	if _, err := resumeErrAgent.LoadSession(ctx, LoadSessionRequest("stored", "/tmp/project")); err == nil {
		t.Fatal("LoadSession materialized ignored resume error")
	}

	closedMaterialized := NewAgent()
	if err := closedMaterialized.Close(); err != nil {
		t.Fatalf("Close returned error: %v", err)
	}
	if _, err := closedMaterialized.resumeMaterializedSession(ctx, ResumeSessionRequest("stored", "/tmp/project"), materializedEntries); err == nil {
		t.Fatal("resumeMaterializedSession on closed agent succeeded")
	}
	if _, err := closedMaterialized.loadMaterializedSession(ctx, LoadSessionRequest("stored", "/tmp/project"), materializedEntries); err == nil {
		t.Fatal("loadMaterializedSession on closed agent succeeded")
	}
	if _, err := agent.resumeMaterializedSession(ctx, ResumeSessionRequest("bad-meta", "/tmp/project", WithSessionMeta(map[string]any{codexMetaKey: "bad"})), materializedEntries); err == nil {
		t.Fatal("resumeMaterializedSession accepted invalid meta")
	}
	if _, err := agent.loadMaterializedSession(ctx, LoadSessionRequest("bad-meta", "/tmp/project", WithSessionMeta(map[string]any{codexMetaKey: "bad"})), materializedEntries); err == nil {
		t.Fatal("loadMaterializedSession accepted invalid meta")
	}
	origCreateRollout := createMaterializedRolloutTemp
	t.Cleanup(func() { createMaterializedRolloutTemp = origCreateRollout })
	createMaterializedRolloutTemp = func(string) (materializedRolloutFile, error) {
		return nil, errors.New("create rollout failed")
	}
	if _, err := agent.resumeMaterializedSession(ctx, ResumeSessionRequest("bad-rollout", "/tmp/project"), materializedEntries); err == nil {
		t.Fatal("resumeMaterializedSession ignored materialize error")
	}
	if _, err := agent.loadMaterializedSession(ctx, LoadSessionRequest("bad-rollout", "/tmp/project"), materializedEntries); err == nil {
		t.Fatal("loadMaterializedSession ignored materialize error")
	}
	createMaterializedRolloutTemp = origCreateRollout
	if _, err := agent.resumeMaterializedSession(ctx, ResumeSessionRequest("bad-mcp", "/tmp/project", WithSessionMCPServers(acp.McpServer{Sse: &acp.McpServerSseInline{Name: "sse"}})), materializedEntries); err == nil {
		t.Fatal("resumeMaterializedSession accepted unsupported MCP")
	}
	if _, err := agent.loadMaterializedSession(ctx, LoadSessionRequest("bad-mcp", "/tmp/project", WithSessionMCPServers(acp.McpServer{Sse: &acp.McpServerSseInline{Name: "sse"}})), materializedEntries); err == nil {
		t.Fatal("loadMaterializedSession accepted unsupported MCP")
	}
	materializedFactoryAgent := NewAgent(withClientFactory(func(context.Context, codex.Options) (codex.Client, error) {
		return nil, errors.New("materialized factory failed")
	}))
	if _, err := materializedFactoryAgent.resumeMaterializedSession(ctx, ResumeSessionRequest("stored", "/tmp/project"), materializedEntries); err == nil {
		t.Fatal("resumeMaterializedSession ignored factory error")
	}
	if _, err := materializedFactoryAgent.loadMaterializedSession(ctx, LoadSessionRequest("stored", "/tmp/project"), materializedEntries); err == nil {
		t.Fatal("loadMaterializedSession ignored factory error")
	}
	materializedLimitAgent := NewAgent(
		WithConcurrencyLimits(ConcurrencyLimits{MaxActiveSessions: 1}),
		withClientFactory(func(context.Context, codex.Options) (codex.Client, error) { return newSpyCodexClient(), nil }),
	)
	limitSession := newSession(materializedLimitAgent, "limit", "/tmp/project", nil, codex.Thread{ID: "limit"}, newSpyCodexClient(), sessionMeta{}, nil)
	if err := materializedLimitAgent.storeStartedSession(limitSession); err != nil {
		t.Fatalf("store materialized limit session: %v", err)
	}
	if _, err := materializedLimitAgent.resumeMaterializedSession(ctx, ResumeSessionRequest("stored", "/tmp/project"), materializedEntries); err == nil {
		t.Fatal("resumeMaterializedSession ignored store backpressure")
	}
	if _, err := materializedLimitAgent.loadMaterializedSession(ctx, LoadSessionRequest("stored", "/tmp/project"), materializedEntries); err == nil {
		t.Fatal("loadMaterializedSession ignored store backpressure")
	}
	replayErrorAgent := NewAgent(withClientFactory(func(context.Context, codex.Options) (codex.Client, error) {
		return newSpyCodexClient(), nil
	}))
	replayErrorAgent.setAgentClient(&errorAgentClient{
		recordingAgentClient: newRecordingAgentClient(),
		updateErr:            errors.New("replay update failed"),
	})
	if _, err := replayErrorAgent.loadMaterializedSession(
		ctx,
		LoadSessionRequest("replay-error", "/tmp/project"),
		materializedEntries,
	); err == nil {
		t.Fatal("loadMaterializedSession ignored post-start replay error")
	}
	if _, err := agent.loadMaterializedSession(ctx, LoadSessionRequest("bad-replay", "/tmp/project"), []SessionStoreEntry{SessionStoreEntry(`not-json`)}); err == nil {
		t.Fatal("loadMaterializedSession ignored replay error")
	}
}

type activeRolloutPathClient struct {
	*spyCodexClient

	nativeThreadID string
	nativePath     string
	turnStarted    chan struct{}
	startOnce      sync.Once
	resumeCount    int
}

func (c *activeRolloutPathClient) StartThread(ctx context.Context, req codex.ThreadStartRequest) (codex.Thread, error) {
	thread, err := c.spyCodexClient.StartThread(ctx, req)
	if err != nil {
		return codex.Thread{}, err
	}

	thread.ID = c.nativeThreadID
	thread.SessionID = c.nativeThreadID
	thread.Path = c.nativePath

	return thread, nil
}

func (c *activeRolloutPathClient) ResumeThread(ctx context.Context, req codex.ThreadResumeRequest) (codex.Thread, error) {
	c.mu.Lock()
	defer c.mu.Unlock()

	c.resume = req
	c.resumeCount++
	if req.ThreadID != c.nativeThreadID {
		return codex.Thread{}, fmt.Errorf("wrong active thread: %s", req.ThreadID)
	}
	if req.Path != c.nativePath {
		return codex.Thread{}, fmt.Errorf("cannot resume running thread %s with stale path", req.ThreadID)
	}

	thread := c.thread
	thread.ID = c.nativeThreadID
	thread.SessionID = c.nativeThreadID
	thread.Path = c.nativePath
	thread.Cwd = req.Cwd
	thread.Title = ""
	thread.UpdatedAt = ""

	return thread, nil
}

func (c *activeRolloutPathClient) RunTurn(ctx context.Context, req codex.TurnStartRequest) (codex.Turn, error) {
	c.mu.Lock()
	c.lastTurn = req
	c.mu.Unlock()
	c.startOnce.Do(func() { close(c.turnStarted) })

	return codex.Turn{ID: "turn"}, nil
}

// A steering interrupt followed by session/close removes the wrapper session
// but leaves the native Codex thread owned by the shared app-server. A store
// resume must retain that runtime ownership, use its canonical rollout path,
// and reject store identities belonging to another logical session.
func TestResumeInterruptedActiveThreadUsesOwnedRolloutPath(t *testing.T) {
	ctx := context.Background()
	nativeThreadID := "thread-active"
	nativePath := filepath.Join(t.TempDir(), "native-rollout.jsonl")
	entries := []SessionStoreEntry{
		SessionStoreEntry(`{"type":"session_meta","payload":{"id":"thread-active","cwd":"/tmp/project"}}`),
		SessionStoreEntry(`{"type":"event_msg","payload":{"type":"user_message","message":"before interrupt"}}`),
	}
	var rollout bytes.Buffer
	for _, entry := range entries {
		rollout.Write(entry)
		rollout.WriteByte('\n')
	}
	require.NoError(t, os.WriteFile(nativePath, rollout.Bytes(), 0o600))

	store := NewInMemorySessionStore()
	client := &activeRolloutPathClient{
		spyCodexClient: newSpyCodexClient(),
		nativeThreadID: nativeThreadID,
		nativePath:     nativePath,
		turnStarted:    make(chan struct{}),
	}
	agent := NewAgent(
		WithSessionStore(store),
		withClientFactory(func(context.Context, codex.Options) (codex.Client, error) { return client, nil }),
	)
	agent.setAgentClient(newRecordingAgentClient())
	t.Cleanup(func() { require.NoError(t, agent.Close()) })

	created, err := agent.NewSession(ctx, NewSessionRequest("/tmp/project"))
	require.NoError(t, err)
	active := agent.activeSession(created.SessionId)
	require.NotNil(t, active)

	type promptResult struct {
		resp acp.PromptResponse
		err  error
	}
	promptDone := make(chan promptResult, 1)
	go func() {
		resp, promptErr := agent.Prompt(ctx, TextPromptRequest(created.SessionId, "interrupted-turn", "wait"))
		promptDone <- promptResult{resp: resp, err: promptErr}
	}()
	<-client.turnStarted
	active.cancelTurn()
	result := <-promptDone
	require.NoError(t, result.err)
	require.Equal(t, acp.StopReasonCancelled, result.resp.StopReason)

	mirrored, err := store.Load(ctx, SessionKey{SessionID: string(created.SessionId)})
	require.NoError(t, err)
	require.Equal(t, entries, mirrored)
	_, err = agent.CloseSession(ctx, acp.CloseSessionRequest{SessionId: created.SessionId})
	require.NoError(t, err)
	require.Nil(t, agent.activeSession(created.SessionId))

	client.mu.Lock()
	require.Equal(t, []string{nativeThreadID}, client.unsubscribed)
	require.Zero(t, client.resumeCount)
	client.mu.Unlock()

	hijackID := acp.SessionId("other-logical-session")
	require.NoError(t, store.Replace(ctx, SessionKey{SessionID: string(hijackID)}, []SessionStoreReplacement{{
		Key:     SessionKey{SessionID: string(hijackID)},
		Entries: entries,
	}}))
	_, err = agent.ResumeSession(ctx, ResumeSessionRequest(hijackID, "/tmp/project"))
	require.ErrorContains(t, err, "retained by another session")

	missingIdentity := []SessionStoreEntry{SessionStoreEntry(`{"type":"event_msg","payload":{"type":"user_message","message":"missing identity"}}`)}
	require.NoError(t, store.Replace(ctx, SessionKey{SessionID: string(created.SessionId)}, []SessionStoreReplacement{{
		Key:     SessionKey{SessionID: string(created.SessionId)},
		Entries: missingIdentity,
	}}))
	_, err = agent.ResumeSession(ctx, ResumeSessionRequest(created.SessionId, "/tmp/project"))
	require.ErrorContains(t, err, "thread identity is required")

	wrongRetainedIdentity := []SessionStoreEntry{SessionStoreEntry(`{"type":"session_meta","payload":{"id":"thread-other"}}`)}
	require.NoError(t, store.Replace(ctx, SessionKey{SessionID: string(created.SessionId)}, []SessionStoreReplacement{{
		Key:     SessionKey{SessionID: string(created.SessionId)},
		Entries: wrongRetainedIdentity,
	}}))
	_, err = agent.ResumeSession(ctx, ResumeSessionRequest(created.SessionId, "/tmp/project"))
	require.ErrorContains(t, err, "does not match the retained session")
	require.NoError(t, store.Replace(ctx, SessionKey{SessionID: string(created.SessionId)}, []SessionStoreReplacement{{
		Key:     SessionKey{SessionID: string(created.SessionId)},
		Entries: entries,
	}}))

	resumed, err := agent.ResumeSession(ctx, ResumeSessionRequest(
		created.SessionId,
		"/tmp/project",
		WithSessionAdditionalDirectories("/tmp/additional"),
	))
	require.NoError(t, err)
	require.NotNil(t, resumed.Meta)
	resumedActive := agent.activeSession(created.SessionId)
	require.NotNil(t, resumedActive)
	require.NotSame(t, active, resumedActive)

	client.mu.Lock()
	require.Equal(t, 1, client.resumeCount)
	require.Equal(t, nativeThreadID, client.resume.ThreadID)
	require.Equal(t, nativePath, client.resume.Path)
	require.Equal(t, []string{nativeThreadID}, client.unsubscribed)
	client.mu.Unlock()

	_, err = agent.LoadSession(ctx, LoadSessionRequest(
		created.SessionId,
		"/tmp/project",
		WithSessionAdditionalDirectories("/tmp/load"),
	))
	require.NoError(t, err)
	require.Same(t, resumedActive, agent.activeSession(created.SessionId))

	client.mu.Lock()
	require.Equal(t, 2, client.resumeCount)
	require.Equal(t, nativePath, client.resume.Path)
	require.Equal(t, []string{nativeThreadID}, client.unsubscribed)
	client.mu.Unlock()

	agent.setAgentClient(&errorAgentClient{
		recordingAgentClient: newRecordingAgentClient(),
		updateErr:            errors.New("replay update failed"),
	})
	_, err = agent.LoadSession(ctx, LoadSessionRequest(
		created.SessionId,
		"/tmp/project",
		WithSessionAdditionalDirectories("/tmp/replay-error"),
	))
	require.ErrorContains(t, err, "replay update failed")
	agent.setAgentClient(newRecordingAgentClient())

	wrongEntries := []SessionStoreEntry{
		SessionStoreEntry(`{"type":"session_meta","payload":{"id":"thread-other","cwd":"/tmp/project"}}`),
	}
	require.NoError(t, store.Replace(ctx, SessionKey{SessionID: string(created.SessionId)}, []SessionStoreReplacement{{
		Key:     SessionKey{SessionID: string(created.SessionId)},
		Entries: wrongEntries,
	}}))
	_, err = agent.LoadSession(ctx, LoadSessionRequest(
		created.SessionId,
		"/tmp/project",
		WithSessionAdditionalDirectories("/tmp/different"),
	))
	require.ErrorContains(t, err, "stored Codex thread does not match the active session")

	client.mu.Lock()
	require.Equal(t, 3, client.resumeCount, "wrong-thread snapshot reached Codex")
	client.mu.Unlock()
}

type activeRebindEdgeClient struct {
	*spyCodexClient
	resume  func(context.Context, codex.ThreadResumeRequest) (codex.Thread, error)
	account func(context.Context) (codex.Account, error)
}

func (c *activeRebindEdgeClient) ResumeThread(ctx context.Context, req codex.ThreadResumeRequest) (codex.Thread, error) {
	return c.resume(ctx, req)
}

func (c *activeRebindEdgeClient) AccountRead(ctx context.Context) (codex.Account, error) {
	if c.account != nil {
		return c.account(ctx)
	}

	return c.spyCodexClient.AccountRead(ctx)
}

type loadedThreadCarrierClient struct {
	*spyCodexClient

	mu            sync.Mutex
	environment   map[string]string
	extraPathDirs []string
	calls         []string
}

type joinedRebindClient struct {
	*spyCodexClient
	active  *session
	checked chan error
}

func (c *joinedRebindClient) ResumeThread(ctx context.Context, req codex.ThreadResumeRequest) (codex.Thread, error) {
	c.active.lifecycleMu.Lock()
	pumping := c.active.nativeEventPumping
	stopping := c.active.nativeEventStopping
	c.active.lifecycleMu.Unlock()
	if !pumping || stopping {
		c.checked <- fmt.Errorf("old pump state pumping=%v stopping=%v", pumping, stopping)
	} else {
		c.checked <- nil
	}

	thread, err := c.spyCodexClient.ResumeThread(ctx, req)
	thread.Path = req.Path

	return thread, err
}

func (c *loadedThreadCarrierClient) ResumeThread(ctx context.Context, req codex.ThreadResumeRequest) (codex.Thread, error) {
	c.mu.Lock()
	c.environment = cloneStringMap(req.Environment)
	c.extraPathDirs = cloneStrings(req.ExtraPathDirs)
	c.calls = append(c.calls, "resume")
	c.mu.Unlock()

	thread, err := c.spyCodexClient.ResumeThread(ctx, req)
	if thread.Path == "" {
		thread.Path = req.Path
	}

	return thread, err
}

func TestActiveStoredRebindFailureBranches(t *testing.T) {
	ctx := context.Background()
	entries := []SessionStoreEntry{SessionStoreEntry(`{"type":"session_meta","payload":{"id":"thread-active"}}`)}
	params := ResumeSessionRequest("session", "/tmp/project")

	bind := func(agent *Agent, client codex.Client) *session {
		active := newSession(agent, "session", "/tmp/project", nil, codex.Thread{
			ID:   "thread-active",
			Path: "/native/rollout.jsonl",
		}, client, sessionMeta{}, nil)
		agent.sessions[active.id] = active
		agent.runtimeClient = client
		agent.runtimeDead = false
		t.Cleanup(active.fenceSession)

		return active
	}

	t.Run("turn admission failure", func(t *testing.T) {
		client := newSpyCodexClient()
		agent := NewAgent()
		active := bind(agent, client)
		releaseTurn, err := active.acquireTurn(ctx)
		require.NoError(t, err)
		defer releaseTurn()

		_, err = agent.rebindActiveStoredSession(ctx, params, entries, sessionMeta{}, active)
		require.ErrorContains(t, err, valueBackpressure)
		require.Empty(t, client.unsubscribed)
		require.Empty(t, client.resume.ThreadID)
	})

	t.Run("materialized dispatcher keeps active ownership", func(t *testing.T) {
		client := newSpyCodexClient()
		agent := NewAgent()
		bind(agent, client)
		_, err := agent.resumeMaterializedSession(ctx, params, entries)
		require.NoError(t, err)
	})

	t.Run("relaunch failure", func(t *testing.T) {
		agent := NewAgent(withClientFactory(func(context.Context, codex.Options) (codex.Client, error) {
			return nil, errors.New("relaunch failed")
		}))
		active := bind(agent, newSpyCodexClient())
		active.setClientDead(true)
		agent.runtimeDead = true
		_, err := agent.rebindActiveStoredSession(ctx, params, entries, sessionMeta{}, active)
		require.ErrorContains(t, err, "relaunch failed")
	})

	t.Run("ownership unavailable", func(t *testing.T) {
		agent := NewAgent()
		active := bind(agent, nil)
		_, err := agent.rebindActiveStoredSession(ctx, params, entries, sessionMeta{}, active)
		require.ErrorContains(t, err, "active Codex thread ownership is unavailable")
	})

	t.Run("MCP validation", func(t *testing.T) {
		client := newSpyCodexClient()
		agent := NewAgent()
		active := bind(agent, client)
		invalid := ResumeSessionRequest("session", "/tmp/project", WithSessionMCPServers(acp.McpServer{
			Sse: &acp.McpServerSseInline{Name: "removed"},
		}))
		_, err := agent.rebindActiveStoredSession(ctx, invalid, entries, sessionMeta{}, active)
		require.Error(t, err)
	})

	t.Run("native resume failure", func(t *testing.T) {
		client := &errorCodexClient{spyCodexClient: newSpyCodexClient(), resumeErr: errors.New("resume failed")}
		agent := NewAgent()
		active := bind(agent, client)
		_, err := agent.rebindActiveStoredSession(ctx, params, entries, sessionMeta{}, active)
		require.ErrorContains(t, err, "resume failed")
	})

	for _, tc := range []struct {
		name   string
		thread codex.Thread
		want   string
	}{
		{
			name:   "wrong returned thread",
			thread: codex.Thread{ID: "thread-other", Path: "/native/rollout.jsonl"},
			want:   "different native thread",
		},
		{
			name:   "wrong returned path",
			thread: codex.Thread{ID: "thread-active", Path: "/other/rollout.jsonl"},
			want:   "different rollout path",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			client := &activeRebindEdgeClient{
				spyCodexClient: newSpyCodexClient(),
				resume: func(context.Context, codex.ThreadResumeRequest) (codex.Thread, error) {
					return tc.thread, nil
				},
			}
			agent := NewAgent()
			active := bind(agent, client)
			_, err := agent.rebindActiveStoredSession(ctx, params, entries, sessionMeta{}, active)
			require.ErrorContains(t, err, tc.want)
		})
	}

	t.Run("empty returned path keeps ownership", func(t *testing.T) {
		client := &activeRebindEdgeClient{
			spyCodexClient: newSpyCodexClient(),
			resume: func(context.Context, codex.ThreadResumeRequest) (codex.Thread, error) {
				return codex.Thread{ID: "thread-active"}, nil
			},
		}
		agent := NewAgent()
		active := bind(agent, client)
		_, err := agent.rebindActiveStoredSession(ctx, params, entries, sessionMeta{}, active)
		require.NoError(t, err)
		require.Equal(t, "/native/rollout.jsonl", active.rolloutPath)
	})

	t.Run("canary failure", func(t *testing.T) {
		client := &runtimeFailureClient{
			runtimeRecordingClient: newRuntimeRecordingClient(),
			events:                 []codex.Event{{Kind: codex.EventCompleted}},
		}
		agent := NewAgent()
		active := bind(agent, client)
		withMCP := ResumeSessionRequest("session", "/tmp/project", WithSessionMCPServers(
			HTTPMCPServer("marker", "https://example.test/mcp", nil),
		))
		_, err := agent.rebindActiveStoredSession(ctx, withMCP, entries, sessionMeta{}, active)
		require.ErrorContains(t, err, "runtime_ready")
	})

	t.Run("ownership changes before commit", func(t *testing.T) {
		agent := NewAgent()
		client := &activeRebindEdgeClient{spyCodexClient: newSpyCodexClient()}
		client.resume = func(_ context.Context, req codex.ThreadResumeRequest) (codex.Thread, error) {
			return codex.Thread{ID: req.ThreadID, Path: req.Path}, nil
		}
		active := bind(agent, client)
		client.account = func(context.Context) (codex.Account, error) {
			agent.mu.Lock()
			agent.sessions[active.id] = &session{id: active.id}
			agent.mu.Unlock()

			return codex.Account{}, nil
		}
		_, err := agent.rebindActiveStoredSession(ctx, params, entries, sessionMeta{}, active)
		require.ErrorContains(t, err, "ownership changed during resume")
	})
}

func TestSameFingerprintResumeAndLoadShareActiveTurnAdmission(t *testing.T) {
	for _, method := range []string{"resume", "load"} {
		for _, turnKind := range []string{"foreground", "autonomous"} {
			t.Run(method+"_"+turnKind, func(t *testing.T) {
				client := newSpyCodexClient()
				agent := NewAgent()
				recorder := newRecordingAgentClient()
				agent.setAgentClient(recorder)
				cwd := t.TempDir()
				s := newSession(agent, "session", cwd, nil, codex.Thread{ID: "thread", Path: "/rollout"}, client, sessionMeta{}, nil)
				s.fingerprint = codexSessionStartFingerprint(codexSessionStart{
					Cwd: cwd, ResumeID: "session", Meta: sessionMeta{},
				})
				agent.sessions[s.id] = s
				agent.runtimeClient = client
				t.Cleanup(s.fenceSession)

				var (
					foregroundCleanup func()
					autonomous        *promptIncarnation
				)
				if turnKind == "foreground" {
					foregroundCleanup = beginTestPromptTurn(t, s, "live-foreground", "foreground-turn")
					defer foregroundCleanup()
				} else {
					agent.lifecycle = lifecycle.Negotiated{Version: 1}
					require.NoError(t, s.openLifecycleStream(t.Context(), agent.lifecycle))
					s.lifecycleMu.Lock()
					var err error
					autonomous, err = s.openAutonomousTurnLocked(t.Context(), "autonomous-turn")
					s.lifecycleMu.Unlock()
					require.NoError(t, err)
				}

				baseline := len(recorder.updates)
				var err error
				switch method {
				case "resume":
					_, err = agent.ResumeSession(t.Context(), ResumeSessionRequest(s.id, cwd))
				case "load":
					_, err = agent.LoadSession(t.Context(), LoadSessionRequest(s.id, cwd))
				}
				require.Error(t, err)
				require.Len(t, recorder.updates, baseline, "refused path injected history or replaced lifecycle state")
				client.mu.Lock()
				require.Empty(t, client.resume.ThreadID, "refused path reached native resume")
				client.mu.Unlock()

				if autonomous != nil {
					s.lifecycleMu.Lock()
					require.Same(t, autonomous, s.agentIncarnation)
					require.False(t, autonomous.settled)
					s.lifecycleMu.Unlock()
				} else {
					require.Equal(t, "live-foreground", s.activeTurnNonce())
				}
			})
		}
	}
}

func TestActiveRebindRotatesThreadCarrier(t *testing.T) {
	oldPath := filepath.Join(t.TempDir(), "old-bin")
	newPath := filepath.Join(t.TempDir(), "new-bin")
	var resumed codex.ThreadResumeRequest
	client := &activeRebindEdgeClient{spyCodexClient: newSpyCodexClient()}
	client.resume = func(_ context.Context, request codex.ThreadResumeRequest) (codex.Thread, error) {
		resumed = request

		return codex.Thread{ID: request.ThreadID, Path: request.Path}, nil
	}

	agent := NewAgent()
	active := newSession(agent, "session", "/tmp/project", nil, codex.Thread{
		ID: "thread-active", Path: "/native/rollout.jsonl",
	}, client, sessionMeta{
		Env:           map[string]string{"WAGIE_API_TOKEN": "old-token"},
		ExtraPathDirs: []string{oldPath},
	}, nil)
	agent.sessions[active.id] = active
	agent.runtimeClient = client
	t.Cleanup(active.fenceSession)

	meta := sessionMeta{
		Env:           map[string]string{"WAGIE_API_TOKEN": "new-token", "WAGIE_OPERATION_ID": "operation-new"},
		ExtraPathDirs: []string{newPath},
	}
	entries := []SessionStoreEntry{SessionStoreEntry(`{"type":"session_meta","payload":{"id":"thread-active"}}`)}
	_, err := agent.rebindActiveStoredSession(
		context.Background(),
		ResumeSessionRequest("session", "/tmp/project"),
		entries,
		meta,
		active,
	)
	require.NoError(t, err)
	require.Equal(t, "new-token", resumed.Environment["WAGIE_API_TOKEN"])
	require.Equal(t, "operation-new", resumed.Environment["WAGIE_OPERATION_ID"])
	for _, value := range resumed.Environment {
		require.NotEqual(t, "old-token", value)
	}
	require.Equal(t, []string{newPath}, resumed.ExtraPathDirs)

	snapshot := active.snapshot()
	require.Equal(t, meta.Env, snapshot.env)
	require.Equal(t, meta.ExtraPathDirs, snapshot.extraPathDirs)
	meta.Env["WAGIE_API_TOKEN"] = "caller-mutated"
	meta.ExtraPathDirs[0] = "caller-mutated"
	require.Equal(t, "new-token", active.snapshot().env["WAGIE_API_TOKEN"])
	require.Equal(t, newPath, active.snapshot().extraPathDirs[0])
}

func TestActiveRebindAppliesCarrierWithoutDetachingBroker(t *testing.T) {
	oldPath := filepath.Join(t.TempDir(), "old-bin")
	newPath := filepath.Join(t.TempDir(), "new-bin")
	client := &loadedThreadCarrierClient{
		spyCodexClient: newSpyCodexClient(),
		environment:    map[string]string{"WAGIE_OPERATION_ID": "operation-old"},
		extraPathDirs:  []string{oldPath},
	}
	client.thread.Path = "/native/rollout.jsonl"

	agent := NewAgent()
	active := newSession(agent, "session", "/tmp/project", nil, codex.Thread{
		ID: "thread-active", Path: client.thread.Path,
	}, client, sessionMeta{
		Env:           cloneStringMap(client.environment),
		ExtraPathDirs: cloneStrings(client.extraPathDirs),
	}, nil)
	agent.sessions[active.id] = active
	agent.runtimeClient = client
	t.Cleanup(active.fenceSession)

	entries := []SessionStoreEntry{SessionStoreEntry(`{"type":"session_meta","payload":{"id":"thread-active"}}`)}
	_, err := agent.rebindActiveStoredSession(
		context.Background(),
		ResumeSessionRequest("session", "/tmp/project"),
		entries,
		sessionMeta{
			Env:           map[string]string{"WAGIE_OPERATION_ID": "operation-new"},
			ExtraPathDirs: []string{newPath},
		},
		active,
	)
	require.NoError(t, err)

	client.mu.Lock()
	require.Equal(t, []string{"resume"}, client.calls)
	require.Equal(t, "operation-new", client.environment["WAGIE_OPERATION_ID"])
	require.Equal(t, []string{newPath}, client.extraPathDirs)
	client.mu.Unlock()
}

func TestActiveRebindMaintainsExactBrokerDuringResume(t *testing.T) {
	client := &joinedRebindClient{spyCodexClient: newSpyCodexClient(), checked: make(chan error, 1)}
	agent := NewAgent()
	active := newSession(agent, "session", "/tmp/project", nil, codex.Thread{
		ID: "thread-active", Path: "/native/rollout.jsonl",
	}, client, sessionMeta{}, nil)
	client.active = active
	agent.sessions[active.id] = active
	agent.runtimeClient = client
	require.NoError(t, active.attachNativeEvents())
	t.Cleanup(active.fenceSession)

	_, err := agent.rebindActiveStoredSession(
		context.Background(),
		ResumeSessionRequest("session", "/tmp/project"),
		[]SessionStoreEntry{SessionStoreEntry(`{"type":"session_meta","payload":{"id":"thread-active"}}`)},
		sessionMeta{}, active,
	)
	require.NoError(t, err)
	require.NoError(t, <-client.checked)
	active.lifecycleMu.Lock()
	require.True(t, active.nativeEventSource)
	require.True(t, active.nativeEventPumping)
	require.False(t, active.nativeEventStopping)
	require.NoError(t, active.lifecycleFailure)
	active.lifecycleMu.Unlock()
}

func TestActiveRebindLinearizesRacingAgentOriginWork(t *testing.T) {
	t.Run("pending establishment and pre-open work refuse rebind", func(t *testing.T) {
		for _, prepare := range []func(*session){
			func(s *session) {
				s.establishment = &establishmentObligation{responseID: "pending", done: make(chan struct{})}
			},
			func(s *session) {
				s.preOpenEvents = []codex.Event{{
					Kind: codex.EventAgentMessageDelta, Scope: codex.EventScopeThread,
					ThreadID: "thread-active", TurnID: "pre-open",
				}}
			},
		} {
			agent := NewAgent()
			s := newSession(agent, "session", t.TempDir(), nil, codex.Thread{ID: "thread-active"}, newSpyCodexClient(), sessionMeta{}, nil)
			s.lifecycleMu.Lock()
			prepare(s)
			s.lifecycleMu.Unlock()
			require.Error(t, s.beginActiveNativeRebind(t.Context()))
		}

		agent := NewAgent()
		agent.lifecycle = lifecycle.Negotiated{Version: 1, ActivityKinds: []lifecycle.ActivityKind{}}
		s := newSession(agent, "session", t.TempDir(), nil, codex.Thread{ID: "thread-active"}, newSpyCodexClient(), sessionMeta{}, nil)
		require.Error(t, s.beginActiveNativeRebind(t.Context()), "an unopened negotiated lifecycle is pending establishment work")
	})

	t.Run("agent event wins", func(t *testing.T) {
		agent := NewAgent()
		agent.options.SessionStore = nil
		agent.lifecycle = lifecycle.Negotiated{Version: 1, ActivityKinds: []lifecycle.ActivityKind{}}
		agent.setAgentClient(newRecordingAgentClient())
		client := newSpyCodexClient()
		s := newSession(agent, "session", t.TempDir(), nil, codex.Thread{ID: "thread-active"}, client, sessionMeta{}, nil)
		require.NoError(t, s.openLifecycleStream(t.Context(), agent.lifecycle))
		require.NoError(t, s.routeNativeEvent(codex.Event{
			Kind: codex.EventAgentMessageDelta, Scope: codex.EventScopeThread,
			ThreadID: "thread-active", TurnID: "event-winner", ItemID: "message", Text: "winner",
		}))
		require.Error(t, s.beginActiveNativeRebind(t.Context()))
		s.lifecycleMu.Lock()
		require.Equal(t, "event-winner", s.agentIncarnation.nativeTurnID)
		s.lifecycleMu.Unlock()
		s.fenceSession()
	})

	t.Run("rebind wins", func(t *testing.T) {
		started := make(chan struct{})
		release := make(chan struct{})
		client := &activeRebindEdgeClient{spyCodexClient: newSpyCodexClient()}
		client.thread.ID = "thread-active"
		client.thread.Path = "/native/rollout.jsonl"
		client.resume = func(ctx context.Context, req codex.ThreadResumeRequest) (codex.Thread, error) {
			close(started)
			select {
			case <-release:
			case <-ctx.Done():
				return codex.Thread{}, ctx.Err()
			}
			if err := client.publishTurn(req.ThreadID, "buffered-event", []codex.Event{
				{Kind: codex.EventAgentMessageDelta, ItemID: "message", Text: "buffered"},
				{Kind: codex.EventCompleted, StopReason: codex.StopReasonEndTurn},
			}); err != nil {
				return codex.Thread{}, err
			}

			return codex.Thread{ID: req.ThreadID, Path: req.Path}, nil
		}

		agent := NewAgent()
		agent.options.SessionStore = nil
		agent.lifecycle = lifecycle.Negotiated{Version: 1, ActivityKinds: []lifecycle.ActivityKind{}}
		recorder := newRecordingAgentClient()
		agent.setAgentClient(recorder)
		s := newSession(agent, "session", t.TempDir(), nil, client.thread, client, sessionMeta{}, nil)
		agent.sessions[s.id] = s
		agent.runtimeClient = client
		require.NoError(t, s.attachNativeEvents())
		require.NoError(t, s.openLifecycleStream(t.Context(), agent.lifecycle))
		t.Cleanup(s.fenceSession)

		rebound := make(chan error, 1)
		go func() {
			_, err := agent.rebindActiveStoredSession(
				t.Context(), ResumeSessionRequest(s.id, s.cwd),
				[]SessionStoreEntry{SessionStoreEntry(`{"type":"session_meta","payload":{"id":"thread-active"}}`)},
				sessionMeta{}, s,
			)
			rebound <- err
		}()
		<-started

		type claimResult struct {
			in  *promptIncarnation
			err error
		}
		probeCtx, cancelProbe := context.WithCancel(t.Context())
		enteredProbe := make(chan struct{})
		claimed := make(chan claimResult, 1)
		go func() {
			close(enteredProbe)
			in, _, err := s.claimLifecycleTurn(probeCtx, "server-request-after-rebind")
			claimed <- claimResult{in: in, err: err}
		}()
		<-enteredProbe
		cancelProbe()
		probe := <-claimed
		require.ErrorIs(t, probe.err, context.Canceled)
		require.Nil(t, probe.in)

		close(release)
		require.NoError(t, <-rebound)
		go func() {
			in, _, err := s.claimLifecycleTurn(t.Context(), "server-request-after-rebind")
			claimed <- claimResult{in: in, err: err}
		}()
		result := <-claimed
		require.NoError(t, result.err)
		require.NotNil(t, result.in)
		require.Equal(t, "server-request-after-rebind", result.in.nativeTurnID)
		require.True(t, recorderHasAgentText(recorder, "buffered"), "buffered broker event was dropped during rebind")
		require.NoError(t, agent.Cancel(t.Context(), CancelRequest(s.id, result.in.turnNonce)))
	})
}

func recorderHasAgentText(recorder *recordingAgentClient, text string) bool {
	for _, update := range recorder.updates {
		if update.Update.AgentMessageChunk != nil && update.Update.AgentMessageChunk.Content.Text != nil &&
			update.Update.AgentMessageChunk.Content.Text.Text == text {
			return true
		}
	}

	return false
}

func TestRuntimeCrashRecoveryPreservesRotatedThreadCarrier(t *testing.T) {
	rotatedPath := filepath.Join(t.TempDir(), "rotated-bin")
	agent := NewAgent()
	session := newSession(agent, "session", "/tmp/project", nil, codex.Thread{
		ID: "thread-active", Path: "/native/rollout.jsonl",
	}, newSpyCodexClient(), sessionMeta{
		Env: map[string]string{
			"WAGIE_API_TOKEN":    "rotated-token",
			"WAGIE_OPERATION_ID": "rotated-operation",
		},
		ExtraPathDirs: []string{rotatedPath},
	}, nil)
	recovery := newRuntimeRecordingClient()

	thread, err := agent.resumeRuntimeSession(context.Background(), recovery, session)
	require.NoError(t, err)
	require.Equal(t, "thread-active", thread.ID)

	recovery.mu.Lock()
	require.Len(t, recovery.resumes, 1)
	request := recovery.resumes[0]
	recovery.mu.Unlock()
	require.Equal(t, "rotated-token", request.Environment["WAGIE_API_TOKEN"])
	require.Equal(t, "rotated-operation", request.Environment["WAGIE_OPERATION_ID"])
	require.Equal(t, []string{rotatedPath}, request.ExtraPathDirs)
	for _, value := range request.Environment {
		require.NotEqual(t, "old-token", value)
	}
}

func TestForkAppliesChildThreadCarrierToForkAndResume(t *testing.T) {
	client := newSpyCodexClient()
	agent := NewAgent(withClientFactory(func(context.Context, codex.Options) (codex.Client, error) {
		return client, nil
	}))
	t.Cleanup(func() { require.NoError(t, agent.Close()) })

	parent, err := agent.NewSession(context.Background(), NewSessionRequest(t.TempDir()))
	require.NoError(t, err)
	childPath := filepath.Join(t.TempDir(), "child-bin")
	_, err = agent.forkSession(context.Background(), ForkSessionRequest(
		parent.SessionId,
		t.TempDir(),
		WithSessionCodexOptions(NewCodexOptions(
			WithCodexEnv(map[string]string{
				"WAGIE_API_TOKEN":    "child-token",
				"WAGIE_OPERATION_ID": "child-operation",
			}),
			WithCodexExtraPathDirs(childPath),
		)),
	))
	require.NoError(t, err)

	client.mu.Lock()
	fork := client.fork
	resume := client.resume
	client.mu.Unlock()
	for _, carrier := range []struct {
		env  map[string]string
		path []string
	}{
		{env: fork.Environment, path: fork.ExtraPathDirs},
		{env: resume.Environment, path: resume.ExtraPathDirs},
	} {
		require.Equal(t, "child-token", carrier.env["WAGIE_API_TOKEN"])
		require.Equal(t, "child-operation", carrier.env["WAGIE_OPERATION_ID"])
		require.Equal(t, []string{childPath}, carrier.path)
	}
}

func TestRetainedRuntimeResumeFailureBranches(t *testing.T) {
	ctx := context.Background()
	entries := []SessionStoreEntry{SessionStoreEntry(`{"type":"session_meta","payload":{"id":"thread-active"}}`)}
	params := ResumeSessionRequest("session", "/tmp/project")

	fixture := func(client codex.Client) (*Agent, *retainedRuntimeThread) {
		agent := NewAgent()
		agent.runtimeClient = client
		agent.runtimeEpoch = 9
		retained := &retainedRuntimeThread{
			sessionID: "session",
			threadID:  "thread-active",
			path:      "/native/rollout.jsonl",
			client:    client,
			epoch:     9,
			claimed:   true,
		}
		agent.retainedThreads[retained.sessionID] = retained

		return agent, retained
	}

	t.Run("MCP validation", func(t *testing.T) {
		agent, retained := fixture(newSpyCodexClient())
		invalid := ResumeSessionRequest("session", "/tmp/project", WithSessionMCPServers(acp.McpServer{
			Sse: &acp.McpServerSseInline{Name: "removed"},
		}))
		_, _, err := agent.resumeRetainedRuntimeSession(ctx, invalid, sessionMeta{}, retained)
		require.Error(t, err)
	})

	t.Run("native resume failure", func(t *testing.T) {
		client := &errorCodexClient{spyCodexClient: newSpyCodexClient(), resumeErr: errors.New("retained resume failed")}
		agent, retained := fixture(client)
		_, _, err := agent.resumeRetainedRuntimeSession(ctx, params, sessionMeta{}, retained)
		require.ErrorContains(t, err, "retained resume failed")
	})

	for _, tc := range []struct {
		name   string
		thread codex.Thread
		want   string
	}{
		{
			name:   "wrong returned thread",
			thread: codex.Thread{ID: "thread-other", Path: "/native/rollout.jsonl"},
			want:   "different retained native thread",
		},
		{
			name:   "wrong returned path",
			thread: codex.Thread{ID: "thread-active", Path: "/other/rollout.jsonl"},
			want:   "different rollout path",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			client := &activeRebindEdgeClient{
				spyCodexClient: newSpyCodexClient(),
				resume: func(context.Context, codex.ThreadResumeRequest) (codex.Thread, error) {
					return tc.thread, nil
				},
			}
			agent, retained := fixture(client)
			_, _, err := agent.resumeRetainedRuntimeSession(ctx, params, sessionMeta{}, retained)
			require.ErrorContains(t, err, tc.want)
			require.False(t, retained.claimed)
			require.Len(t, client.unsubscribed, 1)

			client.resume = func(_ context.Context, req codex.ThreadResumeRequest) (codex.Thread, error) {
				return codex.Thread{ID: req.ThreadID, Path: req.Path}, nil
			}
			claimed, claimErr := agent.claimRetainedRuntimeThreadForStore("session", "thread-active")
			require.NoError(t, claimErr)
			require.Same(t, retained, claimed)
			_, retried, retryErr := agent.resumeRetainedRuntimeSession(ctx, params, sessionMeta{}, claimed)
			require.NoError(t, retryErr)
			require.NotNil(t, retried)
			require.NoError(t, agent.Close())
		})
	}

	t.Run("empty returned path keeps retained ownership", func(t *testing.T) {
		client := &activeRebindEdgeClient{
			spyCodexClient: newSpyCodexClient(),
			resume: func(_ context.Context, req codex.ThreadResumeRequest) (codex.Thread, error) {
				return codex.Thread{ID: req.ThreadID}, nil
			},
		}
		agent, retained := fixture(client)
		_, session, err := agent.resumeRetainedRuntimeSession(ctx, params, sessionMeta{}, retained)
		require.NoError(t, err)
		require.Equal(t, "/native/rollout.jsonl", session.rolloutPath)
		require.NoError(t, agent.Close())
	})

	t.Run("canary failure", func(t *testing.T) {
		client := &runtimeFailureClient{
			runtimeRecordingClient: newRuntimeRecordingClient(),
			events:                 []codex.Event{{Kind: codex.EventCompleted}},
		}
		agent, retained := fixture(client)
		withMCP := ResumeSessionRequest("session", "/tmp/project", WithSessionMCPServers(
			HTTPMCPServer("marker", "https://example.test/mcp", nil),
		))
		_, _, err := agent.resumeRetainedRuntimeSession(ctx, withMCP, sessionMeta{}, retained)
		require.ErrorContains(t, err, "runtime_ready")
		require.False(t, retained.claimed)
		require.Len(t, client.unsubscribed, 1)
		claimed, claimErr := agent.claimRetainedRuntimeThreadForStore("session", "thread-active")
		require.NoError(t, claimErr)
		_, retried, retryErr := agent.resumeRetainedRuntimeSession(ctx, params, sessionMeta{}, claimed)
		require.NoError(t, retryErr)
		require.NotNil(t, retried)
		require.NoError(t, agent.Close())
	})

	t.Run("store commit failure rolls back and retries", func(t *testing.T) {
		client := &activeRebindEdgeClient{
			spyCodexClient: newSpyCodexClient(),
			resume: func(_ context.Context, req codex.ThreadResumeRequest) (codex.Thread, error) {
				return codex.Thread{ID: req.ThreadID, Path: req.Path}, nil
			},
		}
		agent, retained := fixture(client)
		agent.options.ConcurrencyLimits.MaxActiveSessions = 1
		agent.sessions["occupied"] = &session{id: "occupied"}
		_, _, err := agent.resumeRetainedRuntimeSession(ctx, params, sessionMeta{}, retained)
		require.ErrorContains(t, err, valueBackpressure)
		require.False(t, retained.claimed)
		require.Len(t, client.unsubscribed, 1)

		delete(agent.sessions, "occupied")
		claimed, claimErr := agent.claimRetainedRuntimeThreadForStore("session", "thread-active")
		require.NoError(t, claimErr)
		_, retried, retryErr := agent.resumeRetainedRuntimeSession(ctx, params, sessionMeta{}, claimed)
		require.NoError(t, retryErr)
		require.NotNil(t, retried)
		require.NoError(t, agent.Close())
	})

	t.Run("ownership changes before commit", func(t *testing.T) {
		client := &activeRebindEdgeClient{spyCodexClient: newSpyCodexClient()}
		client.resume = func(_ context.Context, req codex.ThreadResumeRequest) (codex.Thread, error) {
			return codex.Thread{ID: req.ThreadID, Path: req.Path}, nil
		}
		agent, retained := fixture(client)
		client.account = func(context.Context) (codex.Account, error) {
			agent.mu.Lock()
			agent.retainedThreads[retained.sessionID] = &retainedRuntimeThread{sessionID: retained.sessionID}
			agent.mu.Unlock()

			return codex.Account{}, nil
		}
		_, _, err := agent.resumeRetainedRuntimeSession(ctx, params, sessionMeta{}, retained)
		require.ErrorContains(t, err, "ownership changed")
	})

	t.Run("load retained replay error and success", func(t *testing.T) {
		loadEntries := []SessionStoreEntry{
			SessionStoreEntry(`{"type":"session_meta","payload":{"id":"thread-active"}}`),
			SessionStoreEntry(`{"type":"event_msg","payload":{"type":"agent_message","message":"history"}}`),
		}

		client := &activeRebindEdgeClient{
			spyCodexClient: newSpyCodexClient(),
			resume: func(_ context.Context, req codex.ThreadResumeRequest) (codex.Thread, error) {
				return codex.Thread{ID: req.ThreadID, Path: req.Path}, nil
			},
		}
		agent, retained := fixture(client)
		retained.claimed = false
		agent.setAgentClient(&errorAgentClient{
			recordingAgentClient: newRecordingAgentClient(),
			updateErr:            errors.New("retained replay failed"),
		})
		_, err := agent.loadMaterializedSession(ctx, LoadSessionRequest("session", "/tmp/project"), loadEntries)
		require.ErrorContains(t, err, "retained replay failed")

		client = &activeRebindEdgeClient{
			spyCodexClient: newSpyCodexClient(),
			resume: func(_ context.Context, req codex.ThreadResumeRequest) (codex.Thread, error) {
				return codex.Thread{ID: req.ThreadID, Path: req.Path}, nil
			},
		}
		agent, retained = fixture(client)
		retained.claimed = false
		agent.setAgentClient(newRecordingAgentClient())
		_, err = agent.loadMaterializedSession(ctx, LoadSessionRequest("session", "/tmp/project"), loadEntries)
		require.NoError(t, err)
	})

	t.Run("load retained lookup and resume errors", func(t *testing.T) {
		client := newSpyCodexClient()
		agent, retained := fixture(client)
		retained.claimed = false
		wrong := []SessionStoreEntry{SessionStoreEntry(`{"type":"session_meta","payload":{"id":"wrong"}}`)}
		_, err := agent.loadMaterializedSession(ctx, LoadSessionRequest("session", "/tmp/project"), wrong)
		require.ErrorContains(t, err, "does not match")

		agent, retained = fixture(client)
		retained.claimed = false
		_, err = agent.loadMaterializedSession(ctx, LoadSessionRequest(
			"session",
			"/tmp/project",
			WithSessionMCPServers(acp.McpServer{Sse: &acp.McpServerSseInline{Name: "removed"}}),
		), entries)
		require.Error(t, err)
	})
}

type blockingLifecycleCodexClient struct {
	*spyCodexClient

	unsubscribeStarted chan struct{}
	unsubscribeRelease chan struct{}
	unsubscribeOnce    sync.Once
	resumeStarted      chan codex.ThreadResumeRequest
	resumeRelease      chan struct{}
	resumeMu           sync.Mutex
	resumeCalls        int
}

func (c *blockingLifecycleCodexClient) UnsubscribeThread(ctx context.Context, threadID string) error {
	if c.unsubscribeStarted != nil {
		c.unsubscribeOnce.Do(func() { close(c.unsubscribeStarted) })
		select {
		case <-c.unsubscribeRelease:
		case <-ctx.Done():
			return ctx.Err()
		}
	}

	return c.spyCodexClient.UnsubscribeThread(ctx, threadID)
}

func (c *blockingLifecycleCodexClient) ResumeThread(ctx context.Context, req codex.ThreadResumeRequest) (codex.Thread, error) {
	c.resumeMu.Lock()
	c.resumeCalls++
	c.resumeMu.Unlock()

	if c.resumeStarted != nil {
		select {
		case c.resumeStarted <- req:
		default:
		}
		select {
		case <-c.resumeRelease:
		case <-ctx.Done():
			return codex.Thread{}, ctx.Err()
		}
	}

	thread, err := c.spyCodexClient.ResumeThread(ctx, req)
	if thread.Path == "" {
		thread.Path = req.Path
	}

	return thread, err
}

func (c *blockingLifecycleCodexClient) resumeCallCount() int {
	c.resumeMu.Lock()
	defer c.resumeMu.Unlock()

	return c.resumeCalls
}

func TestCloseSessionSerializesRetainedResume(t *testing.T) {
	ctx := context.Background()
	store := &configurableStore{}
	client := &blockingLifecycleCodexClient{
		spyCodexClient:     newSpyCodexClient(),
		unsubscribeStarted: make(chan struct{}),
		unsubscribeRelease: make(chan struct{}),
	}
	client.thread.Path = "/native/rollout.jsonl"
	agent := NewAgent(
		WithSessionStore(store),
		withClientFactory(func(context.Context, codex.Options) (codex.Client, error) { return client, nil }),
	)
	created, err := agent.NewSession(ctx, NewSessionRequest("/tmp/project"))
	require.NoError(t, err)
	store.entries = []SessionStoreEntry{SessionStoreEntry(fmt.Sprintf(
		`{"type":"session_meta","payload":{"id":%q}}`, client.thread.ID,
	))}

	closeResult := make(chan error, 1)
	go func() {
		_, closeErr := agent.CloseSession(ctx, acp.CloseSessionRequest{SessionId: created.SessionId})
		closeResult <- closeErr
	}()
	<-client.unsubscribeStarted

	_, err = agent.ResumeSession(ctx, ResumeSessionRequest(created.SessionId, "/tmp/project"))
	require.ErrorContains(t, err, "session close in progress")
	require.Zero(t, client.resumeCallCount())

	close(client.unsubscribeRelease)
	require.NoError(t, <-closeResult)
	_, err = agent.ResumeSession(ctx, ResumeSessionRequest(created.SessionId, "/tmp/project"))
	require.NoError(t, err)
	require.Equal(t, 1, client.resumeCallCount())
	client.mu.Lock()
	require.Equal(t, "/native/rollout.jsonl", client.resume.Path)
	client.mu.Unlock()
	require.NoError(t, agent.Close())
}

func TestCloseSessionUnsubscribeFailureKeepsOwnership(t *testing.T) {
	ctx := context.Background()
	client := &errorCodexClient{spyCodexClient: newSpyCodexClient(), unsubscribeErr: errors.New("unsubscribe failed")}
	client.thread.Path = "/native/rollout.jsonl"
	agent := NewAgent(withClientFactory(func(context.Context, codex.Options) (codex.Client, error) { return client, nil }))
	created, err := agent.NewSession(ctx, NewSessionRequest("/tmp/project"))
	require.NoError(t, err)
	active := agent.activeSession(created.SessionId)
	materialized, err := materializeRollout(t.TempDir(), []SessionStoreEntry{SessionStoreEntry(`{"type":"session_meta"}`)})
	require.NoError(t, err)
	released := false
	active.materializedPath = materialized
	active.materializedRelease = func() { released = true }

	_, err = agent.CloseSession(ctx, acp.CloseSessionRequest{SessionId: created.SessionId})
	require.ErrorContains(t, err, "unsubscribe failed")
	require.Same(t, active, agent.activeSession(created.SessionId))
	require.Empty(t, agent.retainedThreads)
	require.FileExists(t, materialized)
	require.False(t, released)
	active.mu.Lock()
	require.False(t, active.closing)
	require.Equal(t, materialized, active.materializedPath)
	active.mu.Unlock()

	client.unsubscribeErr = nil
	_, err = agent.CloseSession(ctx, acp.CloseSessionRequest{SessionId: created.SessionId})
	require.NoError(t, err)
	require.Nil(t, agent.activeSession(created.SessionId))
	require.Equal(t, materialized, agent.retainedThreads[created.SessionId].materializedPath)
	require.False(t, released)
	require.NoError(t, agent.Close())
	require.True(t, released)
}

func retainedConcurrencyFixture(store SessionStore, client codex.Client) (*Agent, *retainedRuntimeThread) {
	agent := NewAgent(WithSessionStore(store))
	agent.runtimeClient = client
	agent.runtimeEpoch = 11
	retained := &retainedRuntimeThread{
		sessionID: "session",
		threadID:  "thread-active",
		path:      "/native/rollout.jsonl",
		client:    client,
		epoch:     11,
	}
	agent.retainedThreads[retained.sessionID] = retained

	return agent, retained
}

func TestRetainedRuntimeClaimSerializesResumeAndDelete(t *testing.T) {
	ctx := context.Background()
	entries := []SessionStoreEntry{SessionStoreEntry(`{"type":"session_meta","payload":{"id":"thread-active"}}`)}
	params := ResumeSessionRequest("session", "/tmp/project")

	t.Run("double resume and delete lose to claimed resume", func(t *testing.T) {
		store := &configurableStore{entries: entries}
		client := &blockingLifecycleCodexClient{
			spyCodexClient: newSpyCodexClient(),
			resumeStarted:  make(chan codex.ThreadResumeRequest, 1),
			resumeRelease:  make(chan struct{}),
		}
		agent, _ := retainedConcurrencyFixture(store, client)

		firstResult := make(chan error, 1)
		go func() {
			_, resumeErr := agent.ResumeSession(ctx, params)
			firstResult <- resumeErr
		}()
		<-client.resumeStarted

		_, err := agent.ResumeSession(ctx, params)
		require.ErrorContains(t, err, "lifecycle is already in progress")

		// The delete tombstones before it reaches the retained-thread claim, so it
		// reports the claim conflict with the id already retired. The resume that
		// holds the claim therefore has nowhere to register its thread: it wins the
		// claim and still loses the id.
		_, err = agent.UnstableDeleteSession(ctx, DeleteSessionRequest("session"))
		require.ErrorContains(t, err, "lifecycle is already in progress")
		require.True(t, agent.isDeleted("session"))
		require.Equal(t, 1, client.resumeCallCount())
		require.Empty(t, client.deletedThreadSnapshot())

		close(client.resumeRelease)
		requireUnknownSession(t, <-firstResult)
		require.Equal(t, 1, client.resumeCallCount())
		require.NoError(t, agent.Close())
	})

	t.Run("resume loses to claimed delete", func(t *testing.T) {
		store := &blockingDeleteSessionStore{
			configurableStore: &configurableStore{entries: entries},
			started:           make(chan struct{}),
			release:           make(chan struct{}),
		}
		client := &blockingLifecycleCodexClient{spyCodexClient: newSpyCodexClient()}
		agent, _ := retainedConcurrencyFixture(store, client)

		deleteResult := make(chan error, 1)
		go func() {
			_, deleteErr := agent.UnstableDeleteSession(ctx, DeleteSessionRequest("session"))
			deleteResult <- deleteErr
		}()
		<-store.started

		// The delete's own fence is reached before the retained-thread claim, but
		// it has committed no tombstone yet, so the resume is answered with the
		// same retriable conflict the claim itself would have raised.
		_, err := agent.ResumeSession(ctx, params)
		requireSessionDeleteInProgress(t, err)
		require.Zero(t, client.resumeCallCount())

		close(store.release)
		require.NoError(t, <-deleteResult)
		deletedThreads := client.deletedThreadSnapshot()
		require.NotEmpty(t, deletedThreads)
		require.Contains(t, deletedThreads, "thread-active")
		require.NoError(t, agent.Close())
	})
}

// TestDeleteBeatsALoadAlreadyPastItsEntryCheck races the two operations the
// tombstone check cannot be done once for. A load of a stored session holds no
// claim over an id nothing has made active yet, so a delete may commit while
// that load sits inside its native resume. The load must then install nothing,
// tear the replacement it fully prepared back down, and leave the deletion
// marker exactly as the delete set it — no in-memory wrapper, no material on
// disk, and no durable row.
func TestDeleteBeatsALoadAlreadyPastItsEntryCheck(t *testing.T) {
	ctx := context.Background()
	store := NewInMemorySessionStore()
	key := SessionKey{SessionID: "session"}
	require.NoError(t, store.Append(ctx, key, []SessionStoreEntry{
		SessionStoreEntry(`{"type":"session_meta","payload":{"id":"session"}}`),
	}))

	client := &blockingLifecycleCodexClient{
		spyCodexClient: newSpyCodexClient(),
		resumeStarted:  make(chan codex.ThreadResumeRequest, 1),
		resumeRelease:  make(chan struct{}),
	}
	agent := NewAgent(WithSessionStore(store))
	agent.runtimeClient = client

	loaded := make(chan error, 1)

	go func() {
		_, loadErr := agent.LoadSession(ctx, LoadSessionRequest("session", t.TempDir()))
		loaded <- loadErr
	}()

	prepared := <-client.resumeStarted
	require.NotEmpty(t, prepared.Path, "the load materialized a rollout before its native resume")
	require.FileExists(t, prepared.Path)

	_, err := agent.UnstableDeleteSession(ctx, DeleteSessionRequest("session"))
	require.NoError(t, err)

	close(client.resumeRelease)

	// A committed tombstone is permanent, so the load is answered with the
	// unknown-session verdict rather than a retriable conflict.
	loadErr := <-loaded
	require.Error(t, loadErr)

	var requestErr *acp.RequestError

	require.ErrorAs(t, loadErr, &requestErr)
	require.Equal(t, "unknown session", asType[map[string]any](t, requestErr.Data)[jsonFieldError])

	require.Nil(t, agent.activeSession("session"), "the losing load installed a wrapper anyway")
	require.True(t, agent.isDeleted("session"), "installing cleared the deletion marker")
	require.NoFileExists(t, prepared.Path, "the prepared replacement was never torn down")

	entries, err := store.Load(ctx, key)
	require.NoError(t, err)
	require.Empty(t, entries, "the losing load resurrected the durable row")

	require.NoError(t, agent.Close())
}

func TestSessionLifecycleGuardBranches(t *testing.T) {
	ctx := context.Background()
	agent := NewAgent()
	client := newSpyCodexClient()
	active := newSession(agent, "session", "/tmp/project", nil, codex.Thread{ID: "thread"}, client, sessionMeta{}, nil)
	agent.sessions[active.id] = active

	require.NoError(t, agent.validateSessionLifecycle(active.id, active))
	agent.closed = true
	require.ErrorContains(t, agent.validateSessionLifecycle(active.id, active), "agent is closed")
	_, err := agent.acquireSessionLifecycle(active.id)
	require.ErrorContains(t, err, "agent is closed")
	_, err = agent.beginSessionClose(active.id)
	require.ErrorContains(t, err, "agent is closed")
	_, err = agent.beginSessionDelete(active.id)
	require.ErrorContains(t, err, "agent is closed")
	_, err = agent.UnstableDeleteSession(ctx, DeleteSessionRequest(active.id))
	require.ErrorContains(t, err, "agent is closed")
	agent.closed = false

	active.closing = true
	_, err = agent.session(active.id)
	require.ErrorContains(t, err, "session close in progress")
	_, err = agent.acquireSessionLifecycle(active.id)
	require.ErrorContains(t, err, "session close in progress")
	_, err = agent.beginSessionClose(active.id)
	require.ErrorContains(t, err, "session close in progress")
	_, err = agent.beginSessionDelete(active.id)
	require.ErrorContains(t, err, "session close in progress")
	require.ErrorContains(t, agent.validateSessionLifecycle(active.id, active), "session close in progress")
	_, err = agent.LoadSession(ctx, LoadSessionRequest(active.id, "/tmp/project"))
	require.ErrorContains(t, err, "session close in progress")
	active.closing = false

	delete(agent.sessions, active.id)
	require.ErrorContains(t, agent.validateSessionLifecycle(active.id, active), "unknown session")
	agent.abortSessionClose(active.id, active)
	agent.sessions[active.id] = active

	materialized, err := materializeRollout(t.TempDir(), []SessionStoreEntry{SessionStoreEntry(`{"type":"session_meta"}`)})
	require.NoError(t, err)
	released := false
	active.materializedPath = materialized
	active.materializedRelease = func() { released = true }
	_, err = agent.CloseSession(ctx, acp.CloseSessionRequest{SessionId: active.id})
	require.NoError(t, err)
	require.True(t, released)
	require.NoFileExists(t, materialized)
}

func TestAcquireSessionLifecycleRevalidatesAfterWaiting(t *testing.T) {
	agent := NewAgent()
	active := newSession(agent, "session", "/tmp/project", nil, codex.Thread{ID: "thread"}, newSpyCodexClient(), sessionMeta{}, nil)
	agent.sessions[active.id] = active

	// Hold both locks that follow the initial Agent lookup. Observing Agent.mu
	// held proves the acquisition goroutine passed that lookup and is waiting
	// on session.mu; holding lifecycle keeps it from revalidating too early.
	active.sessionOps.Lock()
	active.mu.Lock()

	result := make(chan error, 1)
	go func() {
		_, err := agent.acquireSessionLifecycle(active.id)
		result <- err
	}()

	require.Eventually(t, func() bool {
		if agent.mu.TryLock() {
			agent.mu.Unlock()

			return false
		}

		return true
	}, time.Second, time.Millisecond)

	active.mu.Unlock()
	require.Eventually(t, func() bool {
		if !agent.mu.TryLock() {
			return false
		}

		delete(agent.sessions, active.id)
		agent.mu.Unlock()

		return true
	}, time.Second, time.Millisecond)

	active.sessionOps.Unlock()
	require.ErrorContains(t, <-result, "unknown session")
}

func TestRetainedResumeRollbackUnsubscribeFailureStaysClaimed(t *testing.T) {
	ctx := context.Background()
	client := &errorCodexClient{
		spyCodexClient: newSpyCodexClient(),
		unsubscribeErr: errors.New("rollback unsubscribe failed"),
	}
	client.thread.Path = "/wrong/rollout.jsonl"
	agent := NewAgent()
	agent.runtimeClient = client
	agent.runtimeEpoch = 4
	retained := &retainedRuntimeThread{
		sessionID: "session",
		threadID:  "thread-active",
		path:      "/native/rollout.jsonl",
		client:    client,
		epoch:     4,
		claimed:   true,
	}
	agent.retainedThreads[retained.sessionID] = retained

	_, _, err := agent.resumeRetainedRuntimeSession(
		ctx,
		ResumeSessionRequest(retained.sessionID, "/tmp/project"),
		sessionMeta{},
		retained,
	)
	require.ErrorContains(t, err, "different rollout path")
	require.ErrorContains(t, err, "rollback unsubscribe failed")
	require.True(t, retained.claimed)
	require.Same(t, retained, agent.retainedThreads[retained.sessionID])

	retained.claimed = false
	client.unsubscribeErr = nil
	require.NoError(t, agent.Close())
}

func TestForkSessionErrorBranches(t *testing.T) {
	ctx := context.Background()
	agent := NewAgent(withClientFactory(func(context.Context, codex.Options) (codex.Client, error) { return newSpyCodexClient(), nil }))
	if _, err := agent.forkSession(ctx, ForkSessionRequest("missing", "/tmp/project")); err == nil {
		t.Fatal("forkSession accepted missing parent")
	}
	parentResp, parentErr := agent.NewSession(ctx, NewSessionRequest("/tmp/project"))
	if parentErr != nil {
		t.Fatalf("NewSession parent returned error: %v", parentErr)
	}
	if _, err := agent.forkSession(ctx, ForkSessionRequest(parentResp.SessionId, "relative")); err == nil {
		t.Fatal("forkSession accepted relative cwd")
	}
	if _, err := agent.forkSession(ctx, ForkSessionRequest(parentResp.SessionId, "/tmp/project", WithSessionMeta(map[string]any{codexMetaKey: "bad"}))); err == nil {
		t.Fatal("forkSession accepted invalid meta")
	}
	if _, err := agent.forkSession(ctx, ForkSessionRequest(parentResp.SessionId, "/tmp/project", WithSessionMCPServers(acp.McpServer{Sse: &acp.McpServerSseInline{Name: "sse"}}))); err == nil {
		t.Fatal("forkSession accepted unsupported MCP")
	}

	parentThread := codex.Thread{ID: "parent-thread", SessionID: "parent"}
	factoryErrAgent := NewAgent(withClientFactory(func(context.Context, codex.Options) (codex.Client, error) {
		return nil, errors.New("fork factory failed")
	}))
	factoryParent := newSession(factoryErrAgent, "parent", "/tmp/project", nil, parentThread, newSpyCodexClient(), sessionMeta{}, nil)
	if err := factoryErrAgent.storeStartedSession(factoryParent); err != nil {
		t.Fatalf("store factory parent: %v", err)
	}
	if _, err := factoryErrAgent.forkSession(ctx, ForkSessionRequest("parent", "/tmp/project")); err == nil {
		t.Fatal("forkSession ignored factory error")
	}

	forkErrAgent := NewAgent(withClientFactory(func(context.Context, codex.Options) (codex.Client, error) {
		return &errorCodexClient{spyCodexClient: newSpyCodexClient(), forkErr: codex.ErrThreadNotFound}, nil
	}))
	forkParent := newSession(forkErrAgent, "parent", "/tmp/project", nil, parentThread, newSpyCodexClient(), sessionMeta{}, nil)
	if err := forkErrAgent.storeStartedSession(forkParent); err != nil {
		t.Fatalf("store fork parent: %v", err)
	}
	if _, err := forkErrAgent.forkSession(ctx, ForkSessionRequest("parent", "/tmp/project")); err == nil {
		t.Fatal("forkSession ignored fork error")
	}

	resumeFailureClient := &errorCodexClient{
		spyCodexClient: newSpyCodexClient(), resumeErr: errors.New("child resume failed"),
	}
	resumeFailureAgent := NewAgent(withClientFactory(func(context.Context, codex.Options) (codex.Client, error) {
		return resumeFailureClient, nil
	}))
	resumeFailureParent := newSession(
		resumeFailureAgent, "parent", "/tmp/project", nil, parentThread, newSpyCodexClient(), sessionMeta{}, nil,
	)
	require.NoError(t, resumeFailureAgent.storeStartedSession(resumeFailureParent))
	_, resumeFailureErr := resumeFailureAgent.forkSession(ctx, ForkSessionRequest("parent", "/tmp/project"))
	require.ErrorContains(t, resumeFailureErr, "child resume failed")
	resumeFailureClient.mu.Lock()
	require.Equal(t, []string{"fork-thread"}, resumeFailureClient.deletedThreads)
	resumeFailureClient.mu.Unlock()

	limitClient := newSpyCodexClient()
	limitAgent := NewAgent(
		WithConcurrencyLimits(ConcurrencyLimits{MaxActiveSessions: 1}),
		withClientFactory(func(context.Context, codex.Options) (codex.Client, error) { return limitClient, nil }),
	)
	limitParent := newSession(limitAgent, "parent", "/tmp/project", nil, parentThread, newSpyCodexClient(), sessionMeta{}, nil)
	if err := limitAgent.storeStartedSession(limitParent); err != nil {
		t.Fatalf("store limit parent: %v", err)
	}
	if _, err := limitAgent.forkSession(ctx, ForkSessionRequest("parent", "/tmp/project")); err == nil {
		t.Fatal("forkSession ignored store backpressure")
	}
	limitClient.mu.Lock()
	require.Equal(t, []string{"fork-thread"}, limitClient.deletedThreads)
	limitClient.mu.Unlock()
}

type forkShapeClient struct {
	*spyCodexClient
	forkThread   codex.Thread
	resumeThread codex.Thread
}

type ambiguousCanaryFailureClient struct {
	*runtimeRecordingClient
	obligation *establishmentObligation
}

func (c *ambiguousCanaryFailureClient) SubscribeThread(ctx context.Context, threadID string) (codex.ThreadEventStream, error) {
	if err := ctx.Err(); err != nil {
		return codex.ThreadEventStream{}, err
	}
	c.spyCodexClient.mu.Lock()
	if c.feeds == nil {
		c.feeds = make(map[string]chan codex.Event)
	}
	events := make(chan codex.Event)
	c.feeds[threadID] = events
	c.spyCodexClient.mu.Unlock()
	var once sync.Once

	return codex.ThreadEventStream{Events: events, Release: func() {
		once.Do(func() {
			c.spyCodexClient.mu.Lock()
			delete(c.feeds, threadID)
			close(events)
			c.spyCodexClient.mu.Unlock()
		})
	}}, nil
}

func (c *ambiguousCanaryFailureClient) RunTurn(_ context.Context, req codex.TurnStartRequest) (codex.Turn, error) {
	c.obligation.mu.Lock()
	active := c.obligation.session
	c.obligation.mu.Unlock()
	active.lifecycleMu.Lock()
	active.lifecycleDeliveryActive = true
	active.lifecycleMu.Unlock()
	if err := c.publishTurn(req.ThreadID, "first", []codex.Event{
		{Kind: codex.EventRaw, TurnID: "first"},
		{Kind: codex.EventRaw, TurnID: "second"},
		{Kind: codex.EventRaw, TurnID: "second"},
	}); err != nil {
		return codex.Turn{}, err
	}

	return codex.Turn{}, errors.New("ambiguous canary failure")
}

func (c *forkShapeClient) ForkThread(context.Context, codex.ThreadForkRequest) (codex.Thread, error) {
	return c.forkThread, nil
}

func (c *forkShapeClient) ResumeThread(context.Context, codex.ThreadResumeRequest) (codex.Thread, error) {
	return c.resumeThread, nil
}

func boundEstablishmentContext(t *testing.T) context.Context {
	t.Helper()
	hooks := newEstablishmentHooks(NewAgent().log)
	obligation, err := hooks.reserve("already-bound")
	require.NoError(t, err)
	require.NoError(t, obligation.bind(&session{}))

	return withEstablishmentObligation(t.Context(), obligation)
}

func TestSessionEstablishmentArmFailuresRemainTransactional(t *testing.T) {
	negotiated := lifecycle.Negotiated{Version: 1, ActivityKinds: []lifecycle.ActivityKind{}}
	entries := []SessionStoreEntry{SessionStoreEntry(`{"type":"response_item","payload":{"type":"message","role":"assistant","content":[]}}`)}

	t.Run("new", func(t *testing.T) {
		client := newSpyCodexClient()
		agent := NewAgent(withClientFactory(func(context.Context, codex.Options) (codex.Client, error) { return client, nil }))
		agent.lifecycle = negotiated
		_, err := agent.NewSession(boundEstablishmentContext(t), NewSessionRequest(t.TempDir()))
		require.ErrorContains(t, err, "changed owner")
	})

	for _, operation := range []struct {
		name string
		run  func(*Agent, context.Context) error
	}{
		{name: "resume materialized", run: func(agent *Agent, ctx context.Context) error {
			_, err := agent.resumeMaterializedSession(ctx, ResumeSessionRequest("stored", t.TempDir()), entries)

			return err
		}},
		{name: "load materialized", run: func(agent *Agent, ctx context.Context) error {
			_, err := agent.loadMaterializedSession(ctx, LoadSessionRequest("stored", t.TempDir()), entries)

			return err
		}},
	} {
		t.Run(operation.name, func(t *testing.T) {
			client := newSpyCodexClient()
			agent := NewAgent(withClientFactory(func(context.Context, codex.Options) (codex.Client, error) { return client, nil }))
			agent.lifecycle = negotiated
			require.ErrorContains(t, operation.run(agent, boundEstablishmentContext(t)), "changed owner")
		})
	}

	t.Run("retained", func(t *testing.T) {
		client := newSpyCodexClient()
		agent := NewAgent()
		agent.lifecycle = negotiated
		agent.runtimeClient = client
		retained := &retainedRuntimeThread{
			sessionID: "stored", threadID: "thread", client: client, claimed: true,
		}
		agent.retainedThreads[retained.sessionID] = retained
		_, _, err := agent.resumeRetainedRuntimeSession(
			boundEstablishmentContext(t), ResumeSessionRequest("stored", t.TempDir()), sessionMeta{}, retained,
		)
		require.ErrorContains(t, err, "changed owner")
	})

	t.Run("active admission", func(t *testing.T) {
		agent := NewAgent()
		agent.lifecycle = negotiated
		active := &session{agent: agent, nativeEventOpened: true}
		_, err := agent.beginActiveLifecycleAdmission(boundEstablishmentContext(t), active)
		require.ErrorContains(t, err, "changed owner")
	})

	t.Run("fork", func(t *testing.T) {
		client := newSpyCodexClient()
		agent := NewAgent(withClientFactory(func(context.Context, codex.Options) (codex.Client, error) { return client, nil }))
		t.Cleanup(func() { require.NoError(t, agent.Close()) })
		agent.lifecycle = negotiated
		parent := newSession(agent, "parent", t.TempDir(), nil, codex.Thread{ID: "parent-thread"}, client, sessionMeta{}, nil)
		require.NoError(t, agent.storeStartedSession(parent))
		_, err := agent.forkSession(boundEstablishmentContext(t), ForkSessionRequest(parent.id, t.TempDir()))
		require.ErrorContains(t, err, "changed owner")
	})
}

func TestRetainedResumeRollbackJoinsLifecycleRebindFailure(t *testing.T) {
	client := &ambiguousCanaryFailureClient{runtimeRecordingClient: newRuntimeRecordingClient()}
	agent := NewAgent()
	agent.lifecycle = lifecycle.Negotiated{Version: 1, ActivityKinds: []lifecycle.ActivityKind{}}
	agent.runtimeClient = client
	retained := &retainedRuntimeThread{sessionID: "stored", threadID: "thread", client: client, claimed: true}
	agent.retainedThreads[retained.sessionID] = retained
	params := ResumeSessionRequest(
		retained.sessionID,
		t.TempDir(),
		WithSessionMCPServers(HTTPMCPServer("marker", "https://example.test/mcp", nil)),
	)

	hooks := newEstablishmentHooks(agent.log)
	obligation, reserveErr := hooks.reserve("retained-resume")
	require.NoError(t, reserveErr)
	client.obligation = obligation
	_, _, err := agent.resumeRetainedRuntimeSession(withEstablishmentObligation(t.Context(), obligation), params, sessionMeta{}, retained)
	require.ErrorContains(t, err, "ambiguous canary failure")
	require.ErrorContains(t, err, "native lifecycle is active")
	require.False(t, retained.claimed)
	obligation.mu.Lock()
	active := obligation.session
	obligation.mu.Unlock()
	active.lifecycleMu.Lock()
	active.lifecycleDeliveryActive = false
	active.lifecycleMu.Unlock()
	active.fenceSession()
}

func TestForkIdentityAndCleanupFailuresAreContained(t *testing.T) {
	makeAgent := func(client codex.Client) (*Agent, *session) {
		agent := NewAgent(withClientFactory(func(context.Context, codex.Options) (codex.Client, error) { return client, nil }))
		t.Cleanup(func() { require.NoError(t, agent.Close()) })
		parent := newSession(agent, "parent", t.TempDir(), nil, codex.Thread{ID: "parent-thread"}, client, sessionMeta{}, nil)
		require.NoError(t, agent.storeStartedSession(parent))

		return agent, parent
	}

	t.Run("pending parent", func(t *testing.T) {
		client := newSpyCodexClient()
		agent, parent := makeAgent(client)
		hooks := newEstablishmentHooks(agent.log)
		obligation, reserveErr := hooks.reserve("pending-parent")
		require.NoError(t, reserveErr)
		require.NoError(t, obligation.bind(parent))
		parent.establishment = obligation
		_, err := agent.forkSession(t.Context(), ForkSessionRequest(parent.id, t.TempDir()))
		require.ErrorContains(t, err, "still outstanding")
	})

	t.Run("missing child identity", func(t *testing.T) {
		client := &forkShapeClient{spyCodexClient: newSpyCodexClient()}
		agent, parent := makeAgent(client)
		_, err := agent.forkSession(t.Context(), ForkSessionRequest(parent.id, t.TempDir()))
		require.ErrorContains(t, err, "omitted its child thread identity")
	})

	t.Run("changed child identity", func(t *testing.T) {
		client := &forkShapeClient{
			spyCodexClient: newSpyCodexClient(), forkThread: codex.Thread{ID: "child"}, resumeThread: codex.Thread{ID: "other"},
		}
		agent, parent := makeAgent(client)
		_, err := agent.forkSession(t.Context(), ForkSessionRequest(parent.id, t.TempDir()))
		require.ErrorContains(t, err, "different child thread")
	})

	t.Run("cleanup outcomes", func(t *testing.T) {
		agent := NewAgent()
		notFound := &errorCodexClient{spyCodexClient: newSpyCodexClient(), deleteErr: codex.ErrThreadNotFound}
		require.NoError(t, agent.cleanupUnregisteredFork(t.Context(), notFound, "child", nil))
		deleteFailure := &errorCodexClient{spyCodexClient: newSpyCodexClient(), deleteErr: errors.New("delete failed")}
		require.ErrorContains(t, agent.cleanupUnregisteredFork(t.Context(), deleteFailure, "child", nil), "delete failed")
	})
}

func TestCloseSessionRetriesRetainedRegistryPublication(t *testing.T) {
	client := newSpyCodexClient()
	agent := NewAgent()
	active := newSession(agent, "session", t.TempDir(), nil, codex.Thread{ID: "thread"}, client, sessionMeta{}, nil)
	active.closeContained = true
	active.closeCommitDone = true
	agent.sessions[active.id] = active
	agent.runtimeClient = client
	for index := range retainedRuntimeThreadLimit {
		id := acp.SessionId(fmt.Sprintf("retained-%d", index))
		agent.retainedThreads[id] = &retainedRuntimeThread{sessionID: id, threadID: string(id), client: client}
	}

	_, err := agent.CloseSession(t.Context(), acp.CloseSessionRequest{SessionId: active.id})
	require.ErrorContains(t, err, "registry is full")
	require.True(t, active.closeRemovalPending)

	agent.retainedThreads = make(map[acp.SessionId]*retainedRuntimeThread)
	_, err = agent.CloseSession(t.Context(), acp.CloseSessionRequest{SessionId: active.id})
	require.NoError(t, err)
	require.Nil(t, agent.activeSession(active.id))
	require.NotNil(t, agent.retainedThreads[active.id])
}

func TestSessionHelperBranches(t *testing.T) {
	agent := NewAgent()
	start := codexSessionStart{
		Cwd: "/tmp/project",
		McpServers: []acp.McpServer{
			HTTPMCPServer("z", "https://z.example", nil),
			StdioMCPServer("a", "cmd", nil, nil),
		},
		Meta: sessionMeta{ApprovalPolicy: map[string]any{"mode": "ask"}},
	}
	if got := codexSessionStartFingerprint(start); got == "" {
		t.Fatal("empty session start fingerprint")
	}
	if agent.activeSessionForStart("missing", start) != nil {
		t.Fatal("activeSessionForStart found missing session")
	}
	if got := jsonFingerprint(make(chan int)); !strings.HasPrefix(got, "marshal-error:") {
		t.Fatalf("jsonFingerprint error = %q", got)
	}
	if got := mcpServerName(acp.McpServer{Http: &acp.McpServerHttpInline{Name: "http"}}); got != "http" {
		t.Fatalf("HTTP mcpServerName = %q", got)
	}
	if got := mcpServerName(acp.McpServer{Acp: &acp.McpServerAcpInline{Name: "acp"}}); got != "acp" {
		t.Fatalf("ACP mcpServerName = %q", got)
	}
	if got := mcpServerName(acp.McpServer{Sse: &acp.McpServerSseInline{Name: "sse"}}); got != "sse" {
		t.Fatalf("SSE mcpServerName = %q", got)
	}
	if got := mcpServerName(acp.McpServer{Stdio: &acp.McpServerStdio{Name: "stdio"}}); got != "stdio" {
		t.Fatalf("stdio mcpServerName = %q", got)
	}
	if got := mcpServerName(acp.McpServer{}); got != "" {
		t.Fatalf("empty mcpServerName = %q", got)
	}
	var sessions []acp.SessionInfo
	seen := map[acp.SessionId]struct{}{}
	addSessionInfo(&sessions, seen, acp.SessionInfo{})
	addSessionInfo(&sessions, seen, acp.SessionInfo{SessionId: "s"})
	addSessionInfo(&sessions, seen, acp.SessionInfo{SessionId: "s"})
	if len(sessions) != 1 {
		t.Fatalf("addSessionInfo sessions = %#v", sessions)
	}
	if ctx, cancel := (&Agent{}).sessionStoreContext(context.Background()); ctx == nil || cancel == nil {
		t.Fatal("sessionStoreContext returned nil values")
	} else {
		cancel()
	}
	requireRequestError(t, cancelACPError(errTurnRouteMismatch, nil), -32602, "Invalid params")
	require.NoError(t, cancelACPError(nil, nil))
}

func TestDeleteRetryAndConfigBranches(t *testing.T) {
	ctx := context.Background()
	deleteClient := &errorCodexClient{
		spyCodexClient: newSpyCodexClient(),
		listThreads: []codex.Thread{
			{ID: "", SessionID: "ignored"},
			{ID: "known", SessionID: "session"},
			{ID: "missing", SessionID: "session"},
		},
	}
	deleteClient.deleteErr = codex.ErrThreadNotFound
	agent := NewAgent(withClientFactory(func(context.Context, codex.Options) (codex.Client, error) { return deleteClient, nil }))
	if err := agent.deleteNativeCodexSession(ctx, "session", "known"); err != nil {
		t.Fatalf("delete native with not-found returned error: %v", err)
	}
	deleteClient.deleteErr = errors.New("delete failed")
	if err := agent.deleteNativeCodexSession(ctx, "session", "known"); err == nil {
		t.Fatal("delete native ignored delete error")
	}
	listErrAgent := NewAgent(withClientFactory(func(context.Context, codex.Options) (codex.Client, error) {
		return &errorCodexClient{spyCodexClient: newSpyCodexClient(), listErr: errors.New("list failed")}, nil
	}))
	if err := listErrAgent.deleteNativeCodexSession(ctx, "session", ""); err == nil {
		t.Fatal("delete native ignored list error")
	}
	factoryErrAgent := NewAgent(withClientFactory(func(context.Context, codex.Options) (codex.Client, error) {
		return nil, errors.New("factory failed")
	}))
	if err := factoryErrAgent.deleteNativeCodexSession(ctx, "session", ""); err == nil {
		t.Fatal("delete native ignored factory error")
	}
	factoryErrAgent.deleted["session"] = struct{}{}
	factoryErrAgent.retryDeletedNativeCodexSessions(ctx)
	factoryErrAgent.retryDeleteNativeCodexSession(ctx, "session", "")

	configClient := newSpyCodexClient()
	configAgent := NewAgent(withClientFactory(func(context.Context, codex.Options) (codex.Client, error) { return configClient, nil }))
	resp, err := configAgent.NewSession(ctx, NewSessionRequest("/tmp/project", WithSessionCodexOptions(CodexOptions{ServiceTier: "flex", Personality: "friendly"})))
	if err != nil {
		t.Fatalf("config NewSession returned error: %v", err)
	}
	for _, tc := range []struct {
		id    acp.SessionConfigId
		value acp.SessionConfigValueId
	}{
		{id: configModel, value: "gpt-other"},
		{id: configServiceTier, value: "priority"},
		{id: configPersonality, value: "pragmatic"},
	} {
		if _, err := configAgent.SetSessionConfigOption(ctx, SetConfigOptionRequest(resp.SessionId, tc.id, tc.value)); err != nil {
			t.Fatalf("SetSessionConfigOption %s returned error: %v", tc.id, err)
		}
	}
	if _, err := configAgent.SetSessionMode(ctx, acp.SetSessionModeRequest{}); err == nil {
		t.Fatal("SetSessionMode succeeded")
	}
	if got := modelList(ctx, nil); got != nil {
		t.Fatalf("modelList nil = %#v", got)
	}
	if got := modelList(ctx, &errorCodexClient{spyCodexClient: newSpyCodexClient(), modelErr: errors.New("models")}); got != nil {
		t.Fatalf("modelList error = %#v", got)
	}
	if got := clientAccountMeta(ctx, nil); got != nil {
		t.Fatalf("clientAccountMeta nil = %#v", got)
	}
	if got := clientAccountMeta(ctx, &errorCodexClient{spyCodexClient: newSpyCodexClient(), accountErr: errors.New("account")}); got != nil {
		t.Fatalf("clientAccountMeta error = %#v", got)
	}
	if err := codexThreadACPError(nil, nil); err != nil {
		t.Fatalf("nil codexThreadACPError = %v", err)
	}
	if got := NewAgent().codexConfig(); got != nil {
		t.Fatalf("codexConfig with no overrides = %#v, want nil", got)
	}
	overrideAgent := NewAgent(WithCodexConfigOverrides(map[string]any{"model_provider": "litellm"}))
	config := overrideAgent.codexConfig()
	if config["model_provider"] != "litellm" {
		t.Fatalf("codexConfig = %#v", config)
	}
	config["model_provider"] = "mutated"
	if overrideAgent.options.Config["model_provider"] != "litellm" {
		t.Fatalf("codexConfig did not return independent clone: %#v", overrideAgent.options.Config)
	}
}

func requireUnsupportedFieldError(t *testing.T, err error, field string) {
	t.Helper()
	var reqErr *acp.RequestError
	if !errors.As(err, &reqErr) {
		t.Fatalf("error = %T %v, want ACP request error", err, err)
	}
	if reqErr.Code != -32602 || reqErr.Message != "Invalid params" {
		t.Fatalf("request error = %#v, want invalid params", reqErr)
	}
	want := map[string]any{"error": "unsupported", "field": field}
	if !mapsEqual(reqErr.Data, want) {
		t.Fatalf("request error data = %#v, want %#v", reqErr.Data, want)
	}
}

func mapsEqual(got any, want map[string]any) bool {
	gotMap, ok := got.(map[string]any)
	if !ok {
		return false
	}
	if len(gotMap) != len(want) {
		return false
	}
	for key, value := range want {
		if gotMap[key] != value {
			return false
		}
	}

	return true
}

type configurableStore struct {
	entries   []SessionStoreEntry
	summaries []SessionSummary
	loadErr   error
	listErr   error
	deleteErr error
}

type blockingDeleteSessionStore struct {
	*configurableStore
	started chan struct{}
	release chan struct{}
	once    sync.Once
}

func (s *blockingDeleteSessionStore) Delete(ctx context.Context, _ SessionKey) error {
	s.once.Do(func() { close(s.started) })
	select {
	case <-s.release:
		return s.deleteErr
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (s *configurableStore) Append(context.Context, SessionKey, []SessionStoreEntry) error {
	return nil
}

func (s *configurableStore) Load(context.Context, SessionKey) ([]SessionStoreEntry, error) {
	if s.loadErr != nil {
		return nil, s.loadErr
	}

	return cloneStoreEntries(s.entries), nil
}

func (s *configurableStore) Replace(context.Context, SessionKey, []SessionStoreReplacement) error {
	return nil
}

func (s *configurableStore) Delete(context.Context, SessionKey) error {
	return s.deleteErr
}

func (s *configurableStore) ListSessions(context.Context) ([]SessionSummary, error) {
	if s.listErr != nil {
		return nil, s.listErr
	}

	return append([]SessionSummary(nil), s.summaries...), nil
}

func (s *configurableStore) ListSubkeys(context.Context, SessionKey) ([]string, error) {
	return nil, nil
}

type cancelErrorClient struct {
	*spyCodexClient
	cancelErr error
}

func (c *cancelErrorClient) CancelTurn(context.Context, string, string) error {
	return c.cancelErr
}

func TestListSessionsListsStoreBackedSessionsWithoutCwd(t *testing.T) {
	agent := NewAgent(WithSessionStore(&configurableStore{summaries: []SessionSummary{
		{SessionID: "stored-a", Cwd: "/tmp/project-a", UpdatedAtUnixMilli: 2},
		{SessionID: "stored-b", Cwd: "", UpdatedAtUnixMilli: 1},
	}}))

	resp, err := agent.ListSessions(context.Background(), acp.ListSessionsRequest{})
	if err != nil {
		t.Fatalf("ListSessions returned error: %v", err)
	}
	if len(resp.Sessions) != 2 || resp.Sessions[0].SessionId != "stored-a" || resp.Sessions[1].SessionId != "stored-b" {
		t.Fatalf("store-backed sessions without cwd = %#v", resp.Sessions)
	}

	cwd := "/tmp/project-a"
	filtered, err := agent.ListSessions(context.Background(), acp.ListSessionsRequest{Cwd: &cwd})
	if err != nil {
		t.Fatalf("ListSessions with cwd returned error: %v", err)
	}
	if len(filtered.Sessions) != 2 {
		t.Fatalf("cwd filter must retain empty-cwd summaries, got %#v", filtered.Sessions)
	}

	other := "/tmp/other"
	otherFiltered, err := agent.ListSessions(context.Background(), acp.ListSessionsRequest{Cwd: &other})
	if err != nil {
		t.Fatalf("ListSessions with other cwd returned error: %v", err)
	}
	if len(otherFiltered.Sessions) != 1 || otherFiltered.Sessions[0].SessionId != "stored-b" {
		t.Fatalf("cwd filter result = %#v", otherFiltered.Sessions)
	}
}

func TestCodexThreadACPErrorBranches(t *testing.T) {
	if err := codexThreadACPError(errors.New("unauthorized"), nil); err == nil {
		t.Fatal("auth error returned nil")
	} else {
		var reqErr *acp.RequestError
		if !errors.As(err, &reqErr) || reqErr.Code != -32000 {
			t.Fatalf("auth error mapped to %v, want -32000", err)
		}
	}

	if err := codexThreadACPError(errors.Join(codex.ErrThreadNotFound, errors.New("gone")), nil); err == nil {
		t.Fatal("thread-not-found returned nil")
	} else {
		var reqErr *acp.RequestError
		if !errors.As(err, &reqErr) || reqErr.Code != -32602 {
			t.Fatalf("thread-not-found mapped to %v, want -32602", err)
		}
	}

	passthrough := errors.New("some other error")
	if err := codexThreadACPError(passthrough, nil); !errors.Is(err, passthrough) {
		t.Fatalf("passthrough error = %v", err)
	}
}
