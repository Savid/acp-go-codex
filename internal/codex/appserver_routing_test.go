package codex

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

type routingTransport struct {
	mu     sync.Mutex
	closed bool
	recv   chan rpcMessage
	turns  map[string]string
}

type preResponseThreadTransport struct {
	mu     sync.Mutex
	recv   chan rpcMessage
	closed bool
}

func newPreResponseThreadTransport() *preResponseThreadTransport {
	return &preResponseThreadTransport{recv: make(chan rpcMessage, 16)}
}

func (t *preResponseThreadTransport) Send(_ context.Context, msg rpcMessage) error {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.closed {
		return errors.New("closed")
	}
	if len(msg.ID) == 0 {
		return nil
	}

	threadID := ""
	switch msg.Method {
	case methodThreadStart:
		threadID = "started-thread"
	case methodThreadResume:
		var params map[string]any
		_ = json.Unmarshal(msg.Params, &params)
		threadID = stringValue(params, fieldThreadID)
	case methodThreadFork:
		threadID = "forked-thread"
	}
	if threadID != "" {
		t.recv <- rpcMessage{JSONRPC: jsonRPCVersion, Method: notifyItemAgentMessageDelta, Params: mustRaw(map[string]any{
			"threadId": threadID, "turnId": "agent-turn", "delta": "before-response",
		})}
	}
	t.recv <- rpcMessage{JSONRPC: jsonRPCVersion, ID: msg.ID, Result: mustRaw(map[string]any{
		"thread": map[string]any{"id": threadID, "sessionId": threadID},
	})}

	return nil
}

func (t *preResponseThreadTransport) Recv() (rpcMessage, string, error) {
	msg, ok := <-t.recv
	if !ok {
		return rpcMessage{}, "", errors.New("closed")
	}

	return msg, string(mustRaw(msg)), nil
}

func (t *preResponseThreadTransport) Close() error {
	t.mu.Lock()
	defer t.mu.Unlock()
	if !t.closed {
		t.closed = true
		close(t.recv)
	}

	return nil
}

func newRoutingTransport(turns map[string]string) *routingTransport {
	return &routingTransport{recv: make(chan rpcMessage, 64), turns: turns}
}

func (t *routingTransport) Send(_ context.Context, msg rpcMessage) error {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.closed {
		return errors.New("closed")
	}
	if len(msg.ID) == 0 {
		return nil
	}
	result := map[string]any{}
	if msg.Method == methodTurnStart {
		var params map[string]any
		_ = json.Unmarshal(msg.Params, &params)
		if turnID := t.turns[stringValue(params, fieldThreadID)]; turnID != "" {
			result = map[string]any{"turn": map[string]any{"id": turnID}}
		}
	}
	t.recv <- rpcMessage{JSONRPC: jsonRPCVersion, ID: msg.ID, Result: mustRaw(result)}

	return nil
}

func (t *routingTransport) Recv() (rpcMessage, string, error) {
	msg, ok := <-t.recv
	if !ok {
		return rpcMessage{}, "", errors.New("closed")
	}

	return msg, string(mustRaw(msg)), nil
}

func (t *routingTransport) Close() error {
	t.mu.Lock()
	defer t.mu.Unlock()
	if !t.closed {
		t.closed = true
		close(t.recv)
	}

	return nil
}

func (t *routingTransport) publish(method string, params map[string]any) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if !t.closed {
		t.recv <- rpcMessage{JSONRPC: jsonRPCVersion, Method: method, Params: mustRaw(params)}
	}
}

func claimRoutedThread(t *testing.T, client *AppServerClient, threadID string) ThreadEventStream {
	t.Helper()
	client.ensureEventPump()
	if err := client.registerThread(threadID); err != nil {
		t.Fatalf("registerThread(%s): %v", threadID, err)
	}
	feed, err := client.SubscribeThread(context.Background(), threadID)
	if err != nil {
		t.Fatalf("SubscribeThread(%s): %v", threadID, err)
	}

	return feed
}

func awaitEvent(t *testing.T, events <-chan Event) (Event, bool) {
	t.Helper()
	select {
	case event, ok := <-events:
		return event, ok
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for a thread event")

		return Event{}, false
	}
}

func requireNoRoutedEvent(t *testing.T, events <-chan Event) {
	t.Helper()
	select {
	case event, ok := <-events:
		t.Fatalf("peer stream advanced: event=%#v open=%v", event, ok)
	default:
	}
}

func synchronizeRouting(t *testing.T, client *AppServerClient) {
	t.Helper()
	if _, err := client.ListThreads(context.Background(), ThreadListRequest{}); err != nil {
		t.Fatalf("routing barrier: %v", err)
	}
}

func TestThreadBrokerRetainsNotificationBeforeThreadResponseExactlyOnce(t *testing.T) {
	tests := []struct {
		name string
		call func(context.Context, *AppServerClient) (Thread, error)
	}{
		{name: "start", call: func(ctx context.Context, client *AppServerClient) (Thread, error) {
			return client.StartThread(ctx, ThreadStartRequest{})
		}},
		{name: "resume", call: func(ctx context.Context, client *AppServerClient) (Thread, error) {
			return client.ResumeThread(ctx, ThreadResumeRequest{ThreadID: "resumed-thread"})
		}},
		{name: "fork", call: func(ctx context.Context, client *AppServerClient) (Thread, error) {
			return client.ForkThread(ctx, ThreadForkRequest{ThreadID: "parent-thread"})
		}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			client := &AppServerClient{rpc: newRPCConn(newPreResponseThreadTransport(), nil)}
			t.Cleanup(func() { _ = client.Close(context.Background()) })
			thread, err := test.call(context.Background(), client)
			if err != nil {
				t.Fatal(err)
			}
			feed, err := client.SubscribeThread(context.Background(), thread.ID)
			if err != nil {
				t.Fatal(err)
			}
			event := <-feed.Events
			if event.ThreadID != thread.ID || event.TurnID != "agent-turn" || event.Text != "before-response" {
				t.Fatalf("retained event = %#v", event)
			}
			if buffered := len(feed.Events); buffered != 0 {
				t.Fatalf("retained event delivered %d extra times", buffered)
			}
		})
	}
}

func TestPreRegistrationRouterIsBoundedAndFailsClosed(t *testing.T) {
	client := &AppServerClient{}
	if err := client.beginPendingThread(); err != nil {
		t.Fatal(err)
	}
	for index := 0; index <= pendingThreadEventBuffer; index++ {
		client.dispatchEvent(Event{
			Kind: EventRaw, Scope: EventScopeThread,
			ThreadID: "pending-thread", TurnID: fmt.Sprintf("turn-%d", index),
		})
	}
	client.mu.Lock()
	failure := client.routingFailure
	pending := len(client.pendingThreads)
	client.mu.Unlock()
	if !errors.Is(failure, ErrTurnEventOverflow) || pending != 0 {
		t.Fatalf("bounded router failure=%v pending=%d", failure, pending)
	}
}

func TestFailedThreadRegistrationContainsLateNativeEvents(t *testing.T) { //nolint:gocyclo // One ownership matrix shares the same exact broker fixture.
	tests := []struct {
		name     string
		threadID string
		callErr  error
	}{
		{name: "failed response", callErr: context.Canceled},
		{name: "malformed response"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			client := &AppServerClient{}
			if err := client.beginPendingThread(); err != nil {
				t.Fatal(err)
			}
			if err := client.finishPendingThread(test.threadID, test.callErr); err == nil {
				t.Fatal("thread registration failure was accepted")
			}
			client.dispatchEvent(Event{
				Kind: EventRaw, Scope: EventScopeThread,
				ThreadID: "late-thread", TurnID: "late-turn",
			})
			client.mu.Lock()
			failure := client.routingFailure
			pendingCreates := client.pendingCreates
			pendingThreads := len(client.pendingThreads)
			client.mu.Unlock()
			if failure == nil || pendingCreates != 0 || pendingThreads != 0 {
				t.Fatalf("late event containment failure=%v creates=%d threads=%d", failure, pendingCreates, pendingThreads)
			}
			if err := client.beginPendingThread(); !errors.Is(err, failure) {
				t.Fatalf("routing failure did not latch: got %v want %v", err, failure)
			}
		})
	}

	client := &AppServerClient{}
	stream, created, err := client.preRegisterThread("resumed-thread")
	if err != nil {
		t.Fatal(err)
	}
	resumeErr := errors.New("resume failed")
	if abortErr := client.abortPreRegisteredThread(stream, created, resumeErr); !errors.Is(abortErr, resumeErr) {
		t.Fatalf("resume failure = %v", abortErr)
	}
	terminal, open := <-stream.out
	if !open || terminal.Scope != EventScopeTransportLost || !errors.Is(terminal.Err, resumeErr) {
		t.Fatalf("resume terminal = %#v open=%v", terminal, open)
	}
	if _, open = <-stream.out; open {
		t.Fatal("failed resume stream remained open")
	}

	existingClient := &AppServerClient{}
	existing, _, err := existingClient.preRegisterThread("active-thread")
	if err != nil {
		t.Fatal(err)
	}
	if !existing.claim() {
		t.Fatal("existing broker could not be claimed")
	}
	reused, reusedCreated, err := existingClient.preRegisterThread("active-thread")
	if err != nil || reused != existing || reusedCreated {
		t.Fatalf("existing registration reuse stream=%p created=%v err=%v", reused, reusedCreated, err)
	}
	if abortErr := existingClient.abortPreRegisteredThread(reused, reusedCreated, resumeErr); !errors.Is(abortErr, resumeErr) {
		t.Fatalf("active resume failure = %v", abortErr)
	}
	existingClient.mu.Lock()
	routingFailure := existingClient.routingFailure
	stillRegistered := existingClient.threads["active-thread"] == existing && existing.live()
	existingClient.mu.Unlock()
	if routingFailure != nil || !stillRegistered {
		t.Fatalf("active broker was fenced on failed rebind: failure=%v registered=%v", routingFailure, stillRegistered)
	}

	unclaimedClient := &AppServerClient{}
	unclaimed, _, err := unclaimedClient.preRegisterThread("fork-child")
	if err != nil {
		t.Fatal(err)
	}
	reused, reusedCreated, err = unclaimedClient.preRegisterThread("fork-child")
	if err != nil || reused != unclaimed || reusedCreated {
		t.Fatalf("unclaimed registration reuse stream=%p created=%v err=%v", reused, reusedCreated, err)
	}
	if err := unclaimedClient.abortPreRegisteredThread(reused, reusedCreated, resumeErr); !errors.Is(err, resumeErr) {
		t.Fatalf("failed fork resume = %v", err)
	}
	unclaimedClient.mu.Lock()
	routingFailure = unclaimedClient.routingFailure
	stillRegistered = unclaimedClient.threads["fork-child"] == unclaimed && unclaimed.live()
	unclaimedClient.mu.Unlock()
	if routingFailure != nil || stillRegistered || unclaimed.live() {
		t.Fatalf("unclaimed child broker survived failed resume: failure=%v registered=%v", routingFailure, stillRegistered)
	}
}

func TestUnknownNativeThreadEventFailsRouting(t *testing.T) {
	client := &AppServerClient{}
	client.dispatchEvent(Event{
		Kind: EventRaw, Scope: EventScopeThread, ThreadID: "unknown-thread", TurnID: "turn",
	})
	client.mu.Lock()
	failure := client.routingFailure
	client.mu.Unlock()
	if failure == nil {
		t.Fatal("unknown native thread event was dropped")
	}
	if err := client.beginPendingThread(); !errors.Is(err, failure) {
		t.Fatalf("unknown-thread routing failure did not fence later admission: got %v want %v", err, failure)
	}
}

func TestThreadBrokerPersistsAcrossTurnsAndRoutesExactly(t *testing.T) {
	transport := newRoutingTransport(map[string]string{"thread-a": "turn-a", "thread-b": "turn-b"})
	client := &AppServerClient{rpc: newRPCConn(transport, nil)}
	defer client.Close(context.Background())
	first := claimRoutedThread(t, client, "thread-a")
	second := claimRoutedThread(t, client, "thread-b")
	if _, err := client.RunTurn(context.Background(), TurnStartRequest{ThreadID: "thread-a"}); err != nil {
		t.Fatal(err)
	}
	if _, err := client.RunTurn(context.Background(), TurnStartRequest{ThreadID: "thread-b"}); err != nil {
		t.Fatal(err)
	}

	transport.publish(notifyTurnCompleted, map[string]any{"threadId": "thread-a", "turnId": "turn-a", "turn": map[string]any{"status": statusDone}})
	completed, open := awaitEvent(t, first.Events)
	if !open || completed.Kind != EventCompleted || completed.TurnID != "turn-a" {
		t.Fatalf("prompt terminal = %#v open=%v", completed, open)
	}
	synchronizeRouting(t, client)
	requireNoRoutedEvent(t, second.Events)

	transport.publish(notifyItemAgentMessageDelta, map[string]any{"threadId": "thread-a", "turnId": "agent-turn", "delta": "between prompts"})
	agent, open := awaitEvent(t, first.Events)
	if !open || agent.TurnID != "agent-turn" || agent.Text != "between prompts" {
		t.Fatalf("between-prompt event = %#v open=%v", agent, open)
	}
	synchronizeRouting(t, client)
	requireNoRoutedEvent(t, second.Events)
}

func TestGenerationWarningIsNotBroadcastButTransportLossIs(t *testing.T) {
	transport := newRoutingTransport(nil)
	client := &AppServerClient{rpc: newRPCConn(transport, nil)}
	defer client.Close(context.Background())
	first := claimRoutedThread(t, client, "thread-a")
	second := claimRoutedThread(t, client, "thread-b")
	transport.publish("error", map[string]any{"message": "generation warning"})
	synchronizeRouting(t, client)
	requireNoRoutedEvent(t, first.Events)
	requireNoRoutedEvent(t, second.Events)
	_ = transport.Close()
	for name, events := range map[string]<-chan Event{"first": first.Events, "second": second.Events} {
		lost, open := awaitEvent(t, events)
		if open && (lost.Scope != EventScopeTransportLost || lost.Err == nil) {
			t.Fatalf("%s transport terminal = %#v", name, lost)
		}
		if open {
			if _, open = awaitEvent(t, events); open {
				t.Fatalf("%s broker stayed open after transport loss", name)
			}
		}
	}
}

func TestThreadBrokerRequiresRegistrationAndOneClaim(t *testing.T) {
	client := &AppServerClient{rpc: newRPCConn(newRoutingTransport(nil), nil)}
	defer client.Close(context.Background())
	if err := client.registerThread(""); err == nil {
		t.Fatal("registerThread accepted empty thread id")
	}
	if _, err := client.SubscribeThread(context.Background(), "missing"); err == nil {
		t.Fatal("SubscribeThread accepted unknown thread")
	}
	feed := claimRoutedThread(t, client, "thread")
	if _, err := client.SubscribeThread(context.Background(), "thread"); err == nil {
		t.Fatal("SubscribeThread accepted second claimant")
	}
	feed.Release()
	if _, open := awaitEvent(t, feed.Events); open {
		t.Fatal("released broker stayed open")
	}
}

func TestThreadBrokerOverflowIsBoundedAndFailClosed(t *testing.T) {
	client := &AppServerClient{rpc: newRPCConn(newRoutingTransport(nil), nil)}
	defer client.Close(context.Background())
	feed := claimRoutedThread(t, client, "thread")
	for range threadEventBuffer + 1 {
		client.dispatchEvent(Event{Kind: EventRaw, Scope: EventScopeThread, ThreadID: "thread", TurnID: "turn"})
	}
	for range threadEventBuffer {
		if event := <-feed.Events; event.Kind != EventRaw {
			t.Fatalf("buffered event = %#v", event)
		}
	}
	overflow, open := <-feed.Events
	if !open || !errors.Is(overflow.Err, ErrTurnEventOverflow) {
		t.Fatalf("overflow = %#v open=%v", overflow, open)
	}
	if _, open = <-feed.Events; open {
		t.Fatal("overflowed broker stayed open")
	}
}

func TestTurnStartRequiresClaimAndNativeIdentity(t *testing.T) {
	transport := newRoutingTransport(map[string]string{})
	client := &AppServerClient{rpc: newRPCConn(transport, nil)}
	defer client.Close(context.Background())
	if _, err := client.RunTurn(context.Background(), TurnStartRequest{ThreadID: "thread"}); err == nil {
		t.Fatal("RunTurn accepted unclaimed thread")
	}
	claimRoutedThread(t, client, "thread")
	if _, err := client.RunTurn(context.Background(), TurnStartRequest{ThreadID: "thread"}); err == nil {
		t.Fatal("RunTurn accepted ack without native turn id")
	}
}

type methodNotFoundTransport struct {
	routingTransport
	absent string
}

func (t *methodNotFoundTransport) Send(_ context.Context, msg rpcMessage) error {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.closed || len(msg.ID) == 0 {
		return nil
	}
	if msg.Method == t.absent {
		t.recv <- rpcMessage{JSONRPC: jsonRPCVersion, ID: msg.ID, Error: &rpcError{Code: jsonRPCMethodNotFound, Message: methodNotFoundMessage}}

		return nil
	}
	t.recv <- rpcMessage{JSONRPC: jsonRPCVersion, ID: msg.ID, Result: mustRaw(map[string]any{"data": []any{}})}

	return nil
}

func TestBackgroundTerminalCapabilityComesFromTheAppServer(t *testing.T) {
	transport := &methodNotFoundTransport{routingTransport: routingTransport{recv: make(chan rpcMessage, 64)}, absent: methodBackgroundTerminalList}
	client := &AppServerClient{rpc: newRPCConn(transport, nil)}
	defer client.Close(context.Background())
	_, err := client.ListBackgroundTerminals(context.Background(), BackgroundTerminalListRequest{ThreadID: "thread-a"})
	if !errors.Is(err, ErrBackgroundTerminalsUnsupported) {
		t.Fatalf("ListBackgroundTerminals error = %v", err)
	}
}

func TestBackgroundTerminalSupportIsLatchedFromAnAnsweredCall(t *testing.T) {
	transport := &methodNotFoundTransport{routingTransport: routingTransport{recv: make(chan rpcMessage, 64)}, absent: methodBackgroundTerminalTerminate}
	client := &AppServerClient{rpc: newRPCConn(transport, nil)}
	defer client.Close(context.Background())
	if _, err := client.ListBackgroundTerminals(context.Background(), BackgroundTerminalListRequest{ThreadID: "thread-a"}); err != nil {
		t.Fatal(err)
	}
	if supported, known := client.BackgroundTerminalsSupported(); !supported || !known {
		t.Fatalf("answered capability supported=%v known=%v", supported, known)
	}
	_, err := client.TerminateBackgroundTerminal(context.Background(), BackgroundTerminalTerminateRequest{ThreadID: "thread-a", ProcessID: "p1"})
	if !errors.Is(err, ErrBackgroundTerminalsUnsupported) {
		t.Fatalf("TerminateBackgroundTerminal error = %v", err)
	}
}

func TestThreadRouterRejectsClosedDuplicateAndDisplacedOwnership(t *testing.T) {
	stream := newThreadStream("thread")
	require.True(t, stream.claim())
	require.False(t, stream.claim())
	stream.stop()
	require.False(t, stream.claim())
	require.False(t, stream.send(Event{}))
	stream.fail(Event{Kind: EventError})

	require.NoError(t, discardOwnedCloseCancellation(nil))
	require.NoError(t, discardOwnedCloseCancellation(context.Canceled))
	nonCancellation := errors.New("close failed")
	joined := errors.Join(context.Canceled, nonCancellation)
	require.ErrorIs(t, discardOwnedCloseCancellation(joined), nonCancellation)
	require.NotErrorIs(t, discardOwnedCloseCancellation(joined), context.Canceled)

	client := &AppServerClient{closed: true}
	require.ErrorContains(t, client.beginPendingThread(), "closed")
	_, _, err := client.preRegisterThread("thread")
	require.ErrorContains(t, err, "closed")

	client = &AppServerClient{routingFailure: errors.New("routing failed")}
	require.ErrorContains(t, client.beginPendingThread(), "routing failed")
	_, _, err = client.preRegisterThread("thread")
	require.ErrorContains(t, err, "routing failed")

	client = &AppServerClient{threads: map[string]*threadStream{}, pendingThreads: map[string]*threadStream{}, pendingCreates: 1}
	pending := newThreadStream("thread")
	pending.count = 2
	client.pendingThreads["thread"] = pending
	client.pendingEventCount = 2
	require.NoError(t, client.finishPendingThread("thread", nil))
	require.Same(t, pending, client.threads["thread"])
	require.Zero(t, client.pendingEventCount)

	client = &AppServerClient{threads: map[string]*threadStream{}, pendingThreads: map[string]*threadStream{}, pendingCreates: 1}
	existing := newThreadStream("thread")
	client.threads["thread"] = existing
	other := newThreadStream("thread")
	client.pendingThreads["thread"] = other
	err = client.finishPendingThread("thread", nil)
	require.ErrorContains(t, err, "already exists")
	require.Error(t, client.routingFailure)

	client = &AppServerClient{threads: map[string]*threadStream{}, pendingThreads: map[string]*threadStream{"orphan": newThreadStream("orphan")}, pendingCreates: 1}
	err = client.finishPendingThread("acknowledged", nil)
	require.ErrorContains(t, err, "without an acknowledged owner")

	client = &AppServerClient{threads: map[string]*threadStream{}}
	registration, created, err := client.preRegisterThread("thread")
	require.NoError(t, err)
	require.True(t, created)
	client.routingFailure = errors.New("fenced")
	require.ErrorContains(t, client.completePreRegisteredThread(registration), "fenced")
	client.routingFailure = nil
	delete(client.threads, "thread")
	require.ErrorContains(t, client.completePreRegisteredThread(registration), "was lost")
	require.ErrorIs(t, client.abortPreRegisteredThread(nil, true, nonCancellation), nonCancellation)

	client = &AppServerClient{threads: map[string]*threadStream{}}
	existing = newThreadStream("thread")
	client.threads["thread"] = existing
	require.ErrorIs(t, client.abortPreRegisteredThread(existing, false, nonCancellation), nonCancellation)
	require.Nil(t, client.threads["thread"])

	client = &AppServerClient{threads: map[string]*threadStream{}}
	createdStream := newThreadStream("created")
	require.True(t, createdStream.send(Event{ThreadID: "created"}))
	client.threads["created"] = newThreadStream("created")
	err = client.abortPreRegisteredThread(createdStream, true, nonCancellation)
	require.ErrorContains(t, err, "events before a failed acknowledgement")
	require.Error(t, client.routingFailure)
}

func TestThreadRouterBoundsPendingStreamsAndClosesAllOwners(t *testing.T) {
	cancelled := false
	client := &AppServerClient{
		threads:        map[string]*threadStream{},
		pendingThreads: make(map[string]*threadStream, pendingThreadLimit),
		pendingCreates: 1,
		procCancel:     func() { cancelled = true },
	}
	for index := range pendingThreadLimit {
		id := fmt.Sprintf("pending-%d", index)
		client.pendingThreads[id] = newThreadStream(id)
	}
	client.dispatchThreadEvent(Event{Scope: EventScopeThread, ThreadID: "overflow"})
	require.ErrorIs(t, client.routingFailure, ErrTurnEventOverflow)
	require.True(t, cancelled)

	active := newThreadStream("active")
	pending := newThreadStream("pending")
	client = &AppServerClient{
		threads:        map[string]*threadStream{"active": active},
		pendingThreads: map[string]*threadStream{"pending": pending},
	}
	client.dispatchThreadEvent(Event{Scope: EventScopeTransportLost, Err: errors.New("lost")})
	require.False(t, active.live())
	require.False(t, pending.live())
	client.closeAllThreads()
	require.Empty(t, client.threads)
	require.Empty(t, client.pendingThreads)

	client = &AppServerClient{closed: true}
	require.ErrorContains(t, client.registerThread("thread"), "closed")
	client = &AppServerClient{}
	require.NoError(t, client.registerThread("thread"))
	require.NoError(t, client.registerThread("thread"))
	client.threads["thread"].stop()
	require.NoError(t, client.registerThread("thread"))
	require.ErrorContains(t, client.requireClaimedThread("thread"), "live claimed")
}
