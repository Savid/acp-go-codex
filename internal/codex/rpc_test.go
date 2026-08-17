package codex

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestLineTransportSendRecvAndClose(t *testing.T) {
	var out bytes.Buffer
	closer := &recordingCloser{}
	transport := newLineTransport(strings.NewReader(`{"jsonrpc":"2.0","method":"note","params":{"x":1}}`+"\n"), &out, &process{stdin: closer, stdout: closer})

	if err := transport.Send(context.Background(), rpcMessage{ID: json.RawMessage("1"), Method: "call", Params: json.RawMessage(`{"a":true}`)}); err != nil {
		t.Fatalf("Send returned error: %v", err)
	}
	if !strings.Contains(out.String(), `"jsonrpc":"2.0"`) || !strings.Contains(out.String(), `"method":"call"`) {
		t.Fatalf("sent payload = %s", out.String())
	}
	msg, raw, err := transport.Recv()
	if err != nil {
		t.Fatalf("Recv returned error: %v", err)
	}
	if msg.Method != "note" || raw == "" {
		t.Fatalf("recv msg=%#v raw=%q", msg, raw)
	}
	if err := transport.Close(); err != nil || !closer.closed {
		t.Fatalf("Close err=%v closed=%v", err, closer.closed)
	}
}

func TestLineTransportErrors(t *testing.T) {
	canceled, cancel := context.WithCancel(context.Background())
	cancel()
	if err := newLineTransport(strings.NewReader(""), io.Discard, nil).Send(canceled, rpcMessage{}); err == nil {
		t.Fatal("Send with canceled context succeeded")
	}
	if _, _, err := newLineTransport(strings.NewReader("\n"), io.Discard, nil).Recv(); err == nil {
		t.Fatal("empty recv succeeded")
	}
	if _, _, err := newLineTransport(strings.NewReader("{bad}\n"), io.Discard, nil).Recv(); err == nil {
		t.Fatal("bad JSON recv succeeded")
	}
	if err := newLineTransport(strings.NewReader(""), io.Discard, nil).Close(); err != nil {
		t.Fatalf("nil closer returned error: %v", err)
	}
}

func TestLineTransportRejectsOversizeLine(t *testing.T) {
	oversize := strings.Repeat("a", maxNativeLineBytes+1)
	if _, _, err := newLineTransport(strings.NewReader(oversize), io.Discard, nil).Recv(); err == nil {
		t.Fatal("oversize native line accepted")
	}
}

func TestRPCConnErrorBranches(t *testing.T) {
	transport := newScriptTransport()
	conn := newRPCConn(transport, nil)
	defer conn.Close()

	if err := conn.Notify(context.Background(), "notify", map[string]any{"ok": true}); err != nil {
		t.Fatalf("Notify returned error: %v", err)
	}
	canceled, cancel := context.WithCancel(context.Background())
	cancel()
	if err := conn.Call(canceled, "x", nil, nil); err == nil {
		t.Fatal("Call with canceled context succeeded")
	}
	if err := conn.Respond(context.Background(), json.RawMessage("1"), func() {}, nil); err == nil {
		t.Fatal("Respond with unmarshalable value succeeded")
	}
	if _, err := marshalRaw(func() {}); err == nil {
		t.Fatal("marshalRaw with function succeeded")
	}
	if (&rpcError{Message: "bad", Data: json.RawMessage(`{"x":1}`)}).Error() == "" || ((*rpcError)(nil)).Error() != "" {
		t.Fatal("rpcError Error returned unexpected text")
	}
}

func TestRPCConnResponseErrorAndClosePending(t *testing.T) {
	transport := &manualTransport{recv: make(chan rpcMessage, 1)}
	conn := newRPCConn(transport, nil)
	defer conn.Close()

	errCh := make(chan error, 1)
	go func() {
		errCh <- conn.Call(context.Background(), "wait", nil, nil)
	}()
	waitUntil(t, func() bool { return len(transport.sentMessages()) == 1 })
	transport.recv <- rpcMessage{JSONRPC: jsonRPCVersion, ID: json.RawMessage("1"), Error: &rpcError{Code: -1, Message: "boom"}}
	if err := <-errCh; err == nil || !strings.Contains(err.Error(), "boom") {
		t.Fatalf("Call error = %v", err)
	}

	go func() {
		errCh <- conn.Call(context.Background(), "pending", nil, nil)
	}()
	waitUntil(t, func() bool { return len(transport.sentMessages()) == 2 })
	if err := conn.Close(); err != nil {
		t.Fatalf("Close returned error: %v", err)
	}
	if err := <-errCh; err == nil || !strings.Contains(err.Error(), "closed") {
		t.Fatalf("pending call error = %v", err)
	}
	if err := conn.Call(context.Background(), "after-close", nil, nil); err == nil {
		t.Fatal("Call after close succeeded")
	}
}

func TestRPCConnServerRequestErrors(t *testing.T) {
	transport := &manualTransport{recv: make(chan rpcMessage, 4)}
	conn := newRPCConn(transport, func(context.Context, ServerRequest) (any, error) {
		return nil, errors.New("handler failed")
	})
	defer conn.Close()
	transport.recv <- rpcMessage{JSONRPC: jsonRPCVersion, ID: json.RawMessage("99"), Method: "server/request", Params: json.RawMessage(`{}`)}
	waitUntil(t, func() bool { return len(transport.sentMessages()) == 1 })
	sent := transport.sentMessages()[0]
	if sent.Error == nil || !strings.Contains(sent.Error.Message, "handler failed") {
		t.Fatalf("handler error response = %#v", sent)
	}

	noHandler := &manualTransport{recv: make(chan rpcMessage, 2)}
	connNoHandler := newRPCConn(noHandler, nil)
	defer connNoHandler.Close()
	noHandler.recv <- rpcMessage{JSONRPC: jsonRPCVersion, ID: json.RawMessage("100"), Method: "server/request", Params: json.RawMessage(`{}`)}
	waitUntil(t, func() bool { return len(noHandler.sentMessages()) == 1 })
	if noHandler.sentMessages()[0].Error == nil || noHandler.sentMessages()[0].Error.Code != -32601 {
		t.Fatalf("no handler response = %#v", noHandler.sentMessages()[0])
	}
}

func TestRPCConnServerRequestCancellationSuppressesResponse(t *testing.T) {
	transport := &manualTransport{recv: make(chan rpcMessage, 2)}
	started := make(chan context.Context, 1)
	done := make(chan struct{})
	conn := newRPCConn(transport, func(ctx context.Context, _ ServerRequest) (any, error) {
		started <- ctx
		<-ctx.Done()
		close(done)

		return map[string]any{"late": true}, nil
	})

	transport.recv <- rpcMessage{JSONRPC: jsonRPCVersion, ID: json.RawMessage("101"), Method: "server/request", Params: json.RawMessage(`{}`)}
	reqCtx := <-started
	if reqCtx.Err() != nil {
		t.Fatal("server request context started canceled")
	}
	if err := conn.Close(); err != nil {
		t.Fatalf("Close returned error: %v", err)
	}
	<-done
	time.Sleep(10 * time.Millisecond)
	if sent := transport.sentMessages(); len(sent) != 0 {
		t.Fatalf("canceled request sent response: %#v", sent)
	}
	if ctx, finish, ok := conn.beginRequest("after-close"); ok || ctx != nil || finish != nil {
		t.Fatalf("beginRequest after close ctx=%v finishNil=%t ok=%v", ctx, finish == nil, ok)
	}
}

func TestRPCConnServerRequestContextCanceledErrorSuppressesResponse(t *testing.T) {
	transport := &manualTransport{recv: make(chan rpcMessage, 2)}
	conn := newRPCConn(transport, func(context.Context, ServerRequest) (any, error) {
		return nil, context.Canceled
	})
	defer conn.Close()

	transport.recv <- rpcMessage{JSONRPC: jsonRPCVersion, ID: json.RawMessage("102"), Method: "server/request", Params: json.RawMessage(`{}`)}
	time.Sleep(10 * time.Millisecond)
	if sent := transport.sentMessages(); len(sent) != 0 {
		t.Fatalf("context canceled request sent response: %#v", sent)
	}
}

func TestRPCConnGracefulCloseDrainsServerResponse(t *testing.T) {
	transport := &manualTransport{recv: make(chan rpcMessage, 2)}
	started := make(chan struct{})
	release := make(chan struct{})
	conn := newRPCConn(transport, func(context.Context, ServerRequest) (any, error) {
		close(started)
		<-release

		return map[string]any{"decision": "cancel"}, nil
	})

	transport.recv <- rpcMessage{JSONRPC: jsonRPCVersion, ID: json.RawMessage("103"), Method: RequestCommandApproval, Params: json.RawMessage(`{}`)}
	<-started
	closeErr := make(chan error, 1)
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()

		closeErr <- conn.CloseContext(ctx)
	}()

	select {
	case err := <-closeErr:
		t.Fatalf("CloseContext returned before request completion: %v", err)
	default:
	}
	close(release)
	if err := <-closeErr; err != nil {
		t.Fatalf("CloseContext returned error: %v", err)
	}

	sent := transport.sentMessages()
	if len(sent) != 1 || string(sent[0].ID) != "103" || sent[0].Error != nil {
		t.Fatalf("graceful close responses = %#v", sent)
	}
}

func TestRPCConnReadLoopSkipsMalformedLines(t *testing.T) {
	transport := &rawManualTransport{recv: make(chan recvItem, 4)}
	conn := newRPCConn(transport, nil)
	defer conn.Close()

	transport.recv <- recvItem{err: fmt.Errorf("%w: empty line", errSkippableLine)}
	transport.recv <- recvItem{err: fmt.Errorf("%w: invalid character", errSkippableLine)}
	transport.recv <- recvItem{msg: rpcMessage{JSONRPC: jsonRPCVersion, Method: "note", Params: json.RawMessage(`{"ok":true}`)}, raw: `{"method":"note"}`}

	select {
	case event := <-conn.Events():
		if event.Method != "note" {
			t.Fatalf("event after malformed lines = %#v", event)
		}
	case <-conn.done:
		t.Fatal("connection closed on malformed line")
	case <-time.After(time.Second):
		t.Fatal("valid notification not delivered after malformed lines")
	}

	if got := conn.MalformedLines(); got != 2 {
		t.Fatalf("MalformedLines = %d, want 2", got)
	}

	select {
	case <-conn.done:
		t.Fatal("connection closed despite only malformed lines")
	default:
	}
}

func TestRPCConnReadLoopCancelsRequestsOnRecvError(t *testing.T) {
	transport := &manualTransport{recv: make(chan rpcMessage)}
	conn := newRPCConn(transport, nil)
	ctx, _, ok := conn.beginRequest("orphan")
	if !ok {
		t.Fatal("beginRequest before close failed")
	}
	close(transport.recv)
	<-conn.done
	if ctx.Err() == nil {
		t.Fatal("readLoop recv error did not cancel active request")
	}
}

func TestRPCConnNotificationDoneAndClosedRequestBranches(t *testing.T) {
	done := make(chan struct{})
	close(done)
	conn := &rpcConn{events: make(chan rpcEvent), done: done}
	conn.deliverNotification(rpcMessage{Method: "note"}, "")

	transport := &manualTransport{recv: make(chan rpcMessage, 1)}
	closed := newRPCConn(transport, func(context.Context, ServerRequest) (any, error) {
		return map[string]any{"ok": true}, nil
	})
	if err := closed.Close(); err != nil {
		t.Fatalf("Close returned error: %v", err)
	}
	closed.handleRequest(rpcMessage{ID: json.RawMessage("103"), Method: "server/request"})
	if sent := transport.sentMessages(); len(sent) != 0 {
		t.Fatalf("closed request sent response: %#v", sent)
	}
}

type recordingCloser struct{ closed bool }

func (c *recordingCloser) Close() error {
	c.closed = true

	return nil
}

func (c *recordingCloser) Read([]byte) (int, error) { return 0, io.EOF }

func (c *recordingCloser) Write(p []byte) (int, error) { return len(p), nil }

type manualTransport struct {
	mu   sync.Mutex
	sent []rpcMessage
	recv chan rpcMessage
	done bool
}

func (t *manualTransport) Send(_ context.Context, msg rpcMessage) error {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.sent = append(t.sent, msg)

	return nil
}

func (t *manualTransport) Recv() (rpcMessage, string, error) {
	msg, ok := <-t.recv
	if !ok {
		return rpcMessage{}, "", errors.New("closed")
	}

	return msg, string(mustRaw(msg)), nil
}

func (t *manualTransport) Close() error {
	t.mu.Lock()
	defer t.mu.Unlock()
	if !t.done {
		t.done = true
		close(t.recv)
	}

	return nil
}

func (t *manualTransport) sentMessages() []rpcMessage {
	t.mu.Lock()
	defer t.mu.Unlock()

	return append([]rpcMessage(nil), t.sent...)
}

func waitUntil(t *testing.T, ready func() bool) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if ready() {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("timed out waiting for condition")
}

func TestRPCTransportAndPendingCallErrors(t *testing.T) {
	var written bytes.Buffer
	transport := newLineTransport(strings.NewReader(""), &written, nil)
	if err := transport.Send(context.Background(), rpcMessage{Params: json.RawMessage(`{bad}`)}); err == nil {
		t.Fatal("line transport accepted invalid raw JSON")
	}
	if err := transport.Send(canceledContextForCodexTest(), rpcMessage{}); err == nil {
		t.Fatal("line transport ignored canceled context")
	}
	if _, _, err := newLineTransport(strings.NewReader(""), io.Discard, nil).Recv(); err == nil {
		t.Fatal("Recv accepted EOF")
	}

	notifyConn := newRPCConn(&recordingTransport{sendErr: errors.New("send failed")}, nil)
	if err := notifyConn.Notify(context.Background(), "bad", func() {}); err == nil {
		t.Fatal("Notify accepted unmarshalable params")
	}
	_ = notifyConn.Close()

	callConn := newRPCConn(&recordingTransport{}, nil)
	if err := callConn.Call(context.Background(), "bad", func() {}, nil); err == nil {
		t.Fatal("Call accepted unmarshalable params")
	}
	_ = callConn.Close()

	ctx, cancel := context.WithCancel(context.Background())
	blocking := &recordingTransport{}
	pendingConn := newRPCConn(blocking, nil)
	errCh := make(chan error, 1)
	go func() { errCh <- pendingConn.Call(ctx, "wait", map[string]any{}, nil) }()
	for blocking.sentCount() == 0 {
		time.Sleep(time.Millisecond)
	}
	cancel()
	if err := <-errCh; err == nil {
		t.Fatal("Call ignored context cancellation")
	}
	_ = pendingConn.Close()

	doneTransport := &recordingTransport{}
	doneConn := newRPCConn(doneTransport, nil)
	errCh = make(chan error, 1)
	go func() { errCh <- doneConn.Call(context.Background(), "wait", map[string]any{}, nil) }()
	for doneTransport.sentCount() == 0 {
		time.Sleep(time.Millisecond)
	}
	_ = doneConn.Close()
	if err := <-errCh; err == nil {
		t.Fatal("Call ignored connection close")
	}

	responseTransport := &rawManualTransport{recv: make(chan recvItem, 4)}
	responseConn := newRPCConn(responseTransport, nil)
	resultErr := make(chan error, 1)
	go func() {
		var result map[string]any
		resultErr <- responseConn.Call(context.Background(), "bad-result", map[string]any{}, &result)
	}()
	for responseTransport.sentCount() == 0 {
		time.Sleep(time.Millisecond)
	}
	responseTransport.respondFirst(json.RawMessage(`{bad}`), nil)
	if err := <-resultErr; err == nil {
		t.Fatal("Call accepted invalid response JSON")
	}
	_ = responseConn.Close()

	requestConn := newRPCConn(&rawManualTransport{recv: make(chan recvItem, 4)}, func(context.Context, ServerRequest) (any, error) {
		return nil, errors.New("handler failed")
	})
	requestTransport, ok := requestConn.transport.(*rawManualTransport)
	if !ok {
		t.Fatalf("requestConn transport type = %T", requestConn.transport)
	}
	requestTransport.recv <- recvItem{msg: rpcMessage{JSONRPC: jsonRPCVersion, ID: json.RawMessage("7"), Method: "server/request"}}
	time.Sleep(10 * time.Millisecond)
	if requestTransport.sentCount() == 0 {
		t.Fatal("handler error did not send response")
	}
	_ = requestConn.Close()

	raw, err := marshalRaw(json.RawMessage(`{"ok":true}`))
	if err != nil || string(raw) != `{"ok":true}` {
		t.Fatalf("marshal raw = %s err=%v", raw, err)
	}
	if err := processCloseError(errors.New("close failed")); err == nil {
		t.Fatal("processCloseError ignored plain error")
	}
	if err := processCloseError(ErrProcessContainmentIncomplete); !errors.Is(err, ErrProcessContainmentIncomplete) {
		t.Fatalf("processCloseError lost process-tree proof sentinel: %v", err)
	}
}

func TestRPCDoneAndPlaceholderCancelEdges(t *testing.T) {
	doneTransport := &manualTransport{recv: make(chan rpcMessage, 1)}
	doneConn := newRPCConn(doneTransport, nil)
	errCh := make(chan error, 1)
	go func() { errCh <- doneConn.Call(context.Background(), "wait", nil, nil) }()
	waitUntil(t, func() bool { return len(doneTransport.sentMessages()) == 1 })
	doneConn.closeDone()
	if err := <-errCh; err == nil {
		t.Fatal("Call ignored done channel close")
	}
	_ = doneConn.Close()

	readTransport := &manualTransport{recv: make(chan rpcMessage, 1)}
	readConn := newRPCConn(readTransport, nil)
	readConn.recordCloseError(nil)
	errCh = make(chan error, 1)
	go func() { errCh <- readConn.Call(context.Background(), "wait", nil, nil) }()
	waitUntil(t, func() bool { return len(readTransport.sentMessages()) == 1 })
	close(readTransport.recv)
	if err := <-errCh; !errors.Is(err, ErrConnectionClosed) {
		t.Fatalf("Call err=%v, want connection closed", err)
	}
	if !errors.Is(readConn.closeError(), ErrConnectionClosed) {
		t.Fatalf("closeError = %v, want connection closed", readConn.closeError())
	}
	_ = readConn.Close()

	client := NewPlaceholderClient(Options{})
	thread, err := client.StartThread(context.Background(), ThreadStartRequest{})
	if err != nil {
		t.Fatalf("StartThread returned error: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	events, err := client.RunTurn(ctx, TurnStartRequest{ThreadID: thread.ID})
	if err != nil {
		t.Fatalf("RunTurn returned error: %v", err)
	}
	cancel()
	time.Sleep(10 * time.Millisecond)
	for range events {
	}

	ctx, cancel = context.WithCancel(context.Background())
	events, err = client.RunTurn(ctx, TurnStartRequest{ThreadID: thread.ID})
	if err != nil {
		t.Fatalf("RunTurn third returned error: %v", err)
	}
	<-events
	cancel()
	time.Sleep(10 * time.Millisecond)
	for range events {
	}

	ctx, cancel = context.WithCancel(context.Background())
	events, err = client.RunTurn(ctx, TurnStartRequest{ThreadID: thread.ID})
	if err != nil {
		t.Fatalf("RunTurn second returned error: %v", err)
	}
	<-events
	<-events
	cancel()
	time.Sleep(10 * time.Millisecond)
	for range events {
	}
}

type responseTransport struct {
	responses map[string]any
	recv      chan rpcMessage
	mu        sync.Mutex
	sent      []rpcMessage
	closed    bool
}

func (t *responseTransport) Send(_ context.Context, msg rpcMessage) error {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.recv == nil {
		t.recv = make(chan rpcMessage, 16)
	}
	t.sent = append(t.sent, msg)
	if len(msg.ID) > 0 {
		t.recv <- rpcMessage{JSONRPC: jsonRPCVersion, ID: msg.ID, Result: mustRaw(t.responses[msg.Method])}
	}

	return nil
}

func (t *responseTransport) Recv() (rpcMessage, string, error) {
	t.mu.Lock()
	if t.recv == nil {
		t.recv = make(chan rpcMessage, 16)
	}
	recv := t.recv
	t.mu.Unlock()
	msg, ok := <-recv
	if !ok {
		return rpcMessage{}, "", errors.New("closed")
	}

	return msg, string(mustRaw(msg)), nil
}

func (t *responseTransport) Close() error {
	t.mu.Lock()
	defer t.mu.Unlock()
	if !t.closed {
		t.closed = true
		if t.recv != nil {
			close(t.recv)
		}
	}

	return nil
}

type recordingTransport struct {
	mu      sync.Mutex
	sent    []rpcMessage
	sendErr error
	done    chan struct{}
	closed  bool
}

func (t *recordingTransport) Send(_ context.Context, msg rpcMessage) error {
	if t.sendErr != nil {
		return t.sendErr
	}
	t.mu.Lock()
	t.sent = append(t.sent, msg)
	t.mu.Unlock()

	return nil
}

func (t *recordingTransport) Recv() (rpcMessage, string, error) {
	t.mu.Lock()
	if t.done == nil {
		t.done = make(chan struct{})
	}
	done := t.done
	t.mu.Unlock()

	<-done

	return rpcMessage{}, "", errors.New("closed")
}

func (t *recordingTransport) Close() error {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.done == nil {
		t.done = make(chan struct{})
	}
	if !t.closed {
		t.closed = true
		close(t.done)
	}

	return nil
}

func (t *recordingTransport) sentCount() int {
	t.mu.Lock()
	defer t.mu.Unlock()

	return len(t.sent)
}

type recvItem struct {
	msg rpcMessage
	raw string
	err error
}

type rawManualTransport struct {
	mu   sync.Mutex
	sent []rpcMessage
	recv chan recvItem
}

func (t *rawManualTransport) Send(_ context.Context, msg rpcMessage) error {
	t.mu.Lock()
	t.sent = append(t.sent, msg)
	t.mu.Unlock()

	return nil
}

func (t *rawManualTransport) Recv() (rpcMessage, string, error) {
	item, ok := <-t.recv
	if !ok {
		return rpcMessage{}, "", errors.New("closed")
	}
	if item.err != nil {
		return rpcMessage{}, "", item.err
	}

	return item.msg, item.raw, nil
}

func (t *rawManualTransport) Close() error {
	close(t.recv)

	return nil
}

func (t *rawManualTransport) sentCount() int {
	t.mu.Lock()
	defer t.mu.Unlock()

	return len(t.sent)
}

func (t *rawManualTransport) respondFirst(result json.RawMessage, reqErr *rpcError) {
	t.mu.Lock()
	id := append(json.RawMessage(nil), t.sent[0].ID...)
	t.mu.Unlock()
	t.recv <- recvItem{msg: rpcMessage{JSONRPC: jsonRPCVersion, ID: id, Result: result, Error: reqErr}}
}

func canceledContextForCodexTest() context.Context {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	return ctx
}
