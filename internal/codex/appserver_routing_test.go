package codex

import (
	"context"
	"encoding/json"
	"errors"
	"sync"
	"testing"
	"time"
)

// routingTransport answers turn/start for any thread and lets a test inject one
// notification at a time, so two concurrent sessions can be driven against one
// shared app-server the way production multiplexes them.
type routingTransport struct {
	mu     sync.Mutex
	closed bool
	recv   chan rpcMessage
	turns  map[string]string
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

	if t.closed {
		return
	}

	t.recv <- rpcMessage{JSONRPC: jsonRPCVersion, Method: method, Params: mustRaw(params)}
}

func startRoutedTurn(t *testing.T, client *AppServerClient, threadID string) <-chan Event {
	t.Helper()

	events, err := client.RunTurn(context.Background(), TurnStartRequest{ThreadID: threadID})
	if err != nil {
		t.Fatalf("RunTurn(%s) returned error: %v", threadID, err)
	}

	return events
}

func awaitEvent(t *testing.T, events <-chan Event) (Event, bool) {
	t.Helper()

	select {
	case event, ok := <-events:
		return event, ok
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for a turn event")

		return Event{}, false
	}
}

func requireQuiet(t *testing.T, events <-chan Event) {
	t.Helper()

	select {
	case event, ok := <-events:
		t.Fatalf("peer stream advanced: event=%#v open=%v", event, ok)
	case <-time.After(100 * time.Millisecond):
	}
}

// TestGenerationEventNeverTerminatesAPeerTurn drives two concurrent sessions
// through one shared app-server and proves the three ways one session's traffic
// could have ended the other's incarnation cannot: a thread-less error, one
// thread's completion, and one thread's cancel.
func TestGenerationEventNeverTerminatesAPeerTurn(t *testing.T) {
	transport := newRoutingTransport(map[string]string{"thread-a": "turn-a", "thread-b": "turn-b"})
	client := &AppServerClient{rpc: newRPCConn(transport, nil)}

	defer client.Close(context.Background())

	first := startRoutedTurn(t, client, "thread-a")
	second := startRoutedTurn(t, client, "thread-b")

	// A thread-less error is a fact about the generation, so it reaches no turn.
	transport.publish("error", map[string]any{"message": "generation warning"})
	requireQuiet(t, first)
	requireQuiet(t, second)

	// One thread's completion terminates only that thread's stream.
	transport.publish("turn/completed", map[string]any{
		"threadId": "thread-a",
		"turnId":   "turn-a",
		"turn":     map[string]any{"status": statusDone},
	})

	completed, ok := awaitEvent(t, first)
	if !ok || completed.Kind != EventCompleted {
		t.Fatalf("owning stream event = %#v open=%v", completed, ok)
	}

	if _, open := awaitEvent(t, first); open {
		t.Fatal("a completed turn stream stayed open")
	}

	requireQuiet(t, second)

	// One thread's error terminates only that thread's stream.
	transport.publish("error", map[string]any{"threadId": "thread-b", "message": "thread failed"})

	failed, ok := awaitEvent(t, second)
	if !ok || failed.Kind != EventError {
		t.Fatalf("owning stream error = %#v open=%v", failed, ok)
	}
}

// TestOneThreadsEventsNeverReachAPeersStream proves attribution is by stated
// ownership rather than by an absent identifier: a thread-scoped event reaches
// exactly the stream of that thread.
func TestOneThreadsEventsNeverReachAPeersStream(t *testing.T) {
	transport := newRoutingTransport(map[string]string{"thread-a": "turn-a", "thread-b": "turn-b"})
	client := &AppServerClient{rpc: newRPCConn(transport, nil)}

	defer client.Close(context.Background())

	first := startRoutedTurn(t, client, "thread-a")
	second := startRoutedTurn(t, client, "thread-b")

	transport.publish("item/agentMessage/delta", map[string]any{
		"threadId": "thread-a",
		"turnId":   "turn-a",
		"delta":    "owned",
	})

	owned, ok := awaitEvent(t, first)
	if !ok || owned.Text != "owned" || owned.Scope != EventScopeThread {
		t.Fatalf("owning stream event = %#v open=%v", owned, ok)
	}

	requireQuiet(t, second)

	// A turn the thread no longer runs is not this stream's turn.
	transport.publish("item/agentMessage/delta", map[string]any{
		"threadId": "thread-a",
		"turnId":   "turn-stale",
		"delta":    "stale",
	})
	requireQuiet(t, first)
	requireQuiet(t, second)
}

// TestTransportLossEndsEveryIncarnation pins the one broadcast that is truthful:
// when the shared transport dies, no live incarnation can still receive the
// terminal it was waiting for.
func TestTransportLossEndsEveryIncarnation(t *testing.T) {
	transport := newRoutingTransport(map[string]string{"thread-a": "turn-a", "thread-b": "turn-b"})
	client := &AppServerClient{rpc: newRPCConn(transport, nil)}

	defer client.Close(context.Background())

	first := startRoutedTurn(t, client, "thread-a")
	second := startRoutedTurn(t, client, "thread-b")

	// The app-server dies underneath both sessions rather than being shut down.
	_ = transport.Close()

	for name, events := range map[string]<-chan Event{"first": first, "second": second} {
		drained := false

		for event := range events {
			if event.Kind == EventError {
				drained = true
			}
		}

		if !drained {
			t.Fatalf("%s stream never saw the transport loss", name)
		}
	}
}

// TestTurnStartWithoutATurnIdentityFailsClosed proves the missing-turn-id case is
// handled at the ack rather than becoming a stream that matches every turn on its
// thread.
func TestTurnStartWithoutATurnIdentityFailsClosed(t *testing.T) {
	transport := newRoutingTransport(map[string]string{})
	client := &AppServerClient{rpc: newRPCConn(transport, nil)}

	defer client.Close(context.Background())

	if _, err := client.RunTurn(context.Background(), TurnStartRequest{ThreadID: "thread-a"}); err == nil {
		t.Fatal("RunTurn accepted an ack that named no turn")
	}

	client.mu.Lock()
	live := len(client.turns)
	client.mu.Unlock()

	if live != 0 {
		t.Fatalf("live turn streams after a refused ack = %d", live)
	}
}

func TestTurnStreamRequiresTheThreadItRoutesFor(t *testing.T) {
	transport := newRoutingTransport(map[string]string{})
	client := &AppServerClient{rpc: newRPCConn(transport, nil)}

	defer client.Close(context.Background())

	if _, err := client.registerTurn(context.Background(), ""); err == nil {
		t.Fatal("registerTurn accepted a stream with no thread")
	}
}

// methodNotFoundTransport answers one named method the way an app-server that
// does not implement it does, so the capability comes from the protocol rather
// than from a version compare.
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
		t.recv <- rpcMessage{
			JSONRPC: jsonRPCVersion,
			ID:      msg.ID,
			Error:   &rpcError{Code: jsonRPCMethodNotFound, Message: methodNotFoundMessage},
		}

		return nil
	}

	t.recv <- rpcMessage{JSONRPC: jsonRPCVersion, ID: msg.ID, Result: mustRaw(map[string]any{"data": []any{}})}

	return nil
}

func TestBackgroundTerminalCapabilityComesFromTheAppServer(t *testing.T) {
	transport := &methodNotFoundTransport{
		routingTransport: routingTransport{recv: make(chan rpcMessage, 64)},
		absent:           methodBackgroundTerminalList,
	}
	client := &AppServerClient{rpc: newRPCConn(transport, nil)}

	defer client.Close(context.Background())

	if supported, known := client.BackgroundTerminalsSupported(); supported || known {
		t.Fatalf("an unprobed capability reported supported=%v known=%v", supported, known)
	}

	_, err := client.ListBackgroundTerminals(context.Background(), BackgroundTerminalListRequest{ThreadID: "thread-a"})
	if !errors.Is(err, ErrBackgroundTerminalsUnsupported) {
		t.Fatalf("ListBackgroundTerminals error = %v, want the unsupported sentinel", err)
	}

	if supported, known := client.BackgroundTerminalsSupported(); supported || !known {
		t.Fatalf("after a method-not-found answer supported=%v known=%v", supported, known)
	}
}

func TestBackgroundTerminalSupportIsLatchedFromAnAnsweredCall(t *testing.T) {
	transport := &methodNotFoundTransport{
		routingTransport: routingTransport{recv: make(chan rpcMessage, 64)},
		absent:           methodBackgroundTerminalTerminate,
	}
	client := &AppServerClient{rpc: newRPCConn(transport, nil)}

	defer client.Close(context.Background())

	if _, err := client.ListBackgroundTerminals(
		context.Background(),
		BackgroundTerminalListRequest{ThreadID: "thread-a", Cursor: "c", Limit: 10},
	); err != nil {
		t.Fatalf("ListBackgroundTerminals returned error: %v", err)
	}

	if supported, known := client.BackgroundTerminalsSupported(); !supported || !known {
		t.Fatalf("after an answered call supported=%v known=%v", supported, known)
	}

	_, err := client.TerminateBackgroundTerminal(
		context.Background(),
		BackgroundTerminalTerminateRequest{ThreadID: "thread-a", ProcessID: "p1"},
	)
	if !errors.Is(err, ErrBackgroundTerminalsUnsupported) {
		t.Fatalf("TerminateBackgroundTerminal error = %v, want the unsupported sentinel", err)
	}

	if supported, known := client.BackgroundTerminalsSupported(); supported || !known {
		t.Fatalf("after a method-not-found answer supported=%v known=%v", supported, known)
	}
}
