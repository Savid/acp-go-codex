package codex

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"sync"
	"sync/atomic"
	"time"
)

const jsonRPCVersion = "2.0"

// maxNativeLineBytes bounds one app-server stdout line so a single oversize
// frame cannot force an unbounded buffer allocation before the JSON is parsed.
const maxNativeLineBytes = 10 * 1024 * 1024

// errSkippableLine marks a native stdout line that is malformed or empty but
// does not indicate a dead connection. The read loop skips and counts these
// lines rather than tearing down the whole session.
var errSkippableLine = errors.New("skippable JSON-RPC line")

type rpcTransport interface {
	Send(context.Context, rpcMessage) error
	Recv() (rpcMessage, string, error)
	Close() error
}

type rpcMessage struct {
	JSONRPC string          `json:"jsonrpc,omitempty"`
	ID      json.RawMessage `json:"id,omitempty"`
	Method  string          `json:"method,omitempty"`
	Params  json.RawMessage `json:"params,omitempty"`
	Result  json.RawMessage `json:"result,omitempty"`
	Error   *rpcError       `json:"error,omitempty"`
}

type rpcError struct {
	Code    int             `json:"code"`
	Message string          `json:"message"`
	Data    json.RawMessage `json:"data,omitempty"`
}

func (e *rpcError) Error() string {
	if e == nil {
		return ""
	}

	if len(e.Data) == 0 {
		return e.Message
	}

	return fmt.Sprintf("%s: %s", e.Message, string(e.Data))
}

type lineTransport struct {
	s    *bufio.Scanner
	w    io.Writer
	proc *process
	mu   sync.Mutex

	// grace bounds how long readError waits for the process to be reaped
	// before classifying a read failure as a live-transport fault. Captured at
	// construction so concurrent readers never touch shared mutable state.
	grace time.Duration
}

func newLineTransport(r io.Reader, w io.Writer, proc *process) *lineTransport {
	scanner := bufio.NewScanner(r)
	scanner.Buffer(make([]byte, 0, 64*1024), maxNativeLineBytes)

	return &lineTransport{
		s:     scanner,
		w:     w,
		proc:  proc,
		grace: processExitGrace,
	}
}

func (t *lineTransport) Send(ctx context.Context, msg rpcMessage) error {
	if err := ctx.Err(); err != nil {
		return err
	}

	msg.JSONRPC = jsonRPCVersion

	payload, err := json.Marshal(msg)
	if err != nil {
		return err
	}

	payload = append(payload, '\n')

	t.mu.Lock()
	defer t.mu.Unlock()

	_, err = t.w.Write(payload)

	return err
}

func (t *lineTransport) Recv() (rpcMessage, string, error) {
	if !t.s.Scan() {
		err := t.s.Err()
		if err == nil {
			err = io.EOF
		}

		return rpcMessage{}, "", t.readError(err)
	}

	raw := string(t.s.Bytes())
	if raw == "" {
		return rpcMessage{}, raw, fmt.Errorf("%w: empty line", errSkippableLine)
	}

	var msg rpcMessage
	if err := json.Unmarshal(t.s.Bytes(), &msg); err != nil {
		return rpcMessage{}, raw, fmt.Errorf("%w: %w", errSkippableLine, err)
	}

	return msg, raw, nil
}

// readError classifies a stdout read failure. When the stream ends because the
// app-server process exited, it returns a *ProcessExitError carrying the real
// exit status and stderr tail so the failure surfaces as cause:"process_exit"
// instead of a bare transport EOF. A read failure with the process still alive
// is returned unchanged and stays cause:"transport".
func (t *lineTransport) readError(err error) error {
	if t.proc == nil {
		return err
	}

	status, stderrTail, ok := t.proc.exited(t.grace)
	if !ok {
		return err
	}

	return &ProcessExitError{Status: status, StderrTail: stderrTail, Err: err}
}

func (t *lineTransport) Close() error {
	if t.proc == nil {
		return nil
	}

	return t.proc.Close()
}

type pendingCall struct {
	result chan rpcMessage
}

type pendingRequest struct {
	cancel context.CancelFunc
	done   chan struct{}
}

type rpcConn struct {
	transport rpcTransport
	handler   RequestHandler
	events    chan rpcEvent
	done      chan struct{}
	doneOnce  sync.Once

	nextID         atomic.Int64
	malformedLines atomic.Int64

	mu       sync.Mutex
	pending  map[string]pendingCall
	requests map[string]*pendingRequest
	closed   bool
	closeErr error
}

type rpcEvent struct {
	Method string
	Params json.RawMessage
	Raw    string
}

func newRPCConn(transport rpcTransport, handler RequestHandler) *rpcConn {
	conn := &rpcConn{
		transport: transport,
		handler:   handler,
		events:    make(chan rpcEvent, 128),
		done:      make(chan struct{}),
		pending:   make(map[string]pendingCall),
		requests:  make(map[string]*pendingRequest),
	}

	go func() {
		defer recoverCodexGoroutine(context.Background(), "Codex JSON-RPC read loop")

		conn.readLoop()
	}()

	return conn
}

func (c *rpcConn) Events() <-chan rpcEvent { return c.events }

// MalformedLines reports how many malformed or empty native lines were skipped
// without tearing down the connection.
func (c *rpcConn) MalformedLines() int64 { return c.malformedLines.Load() }

func (c *rpcConn) Call(ctx context.Context, method string, params any, result any) error {
	if err := ctx.Err(); err != nil {
		return err
	}

	id := c.nextID.Add(1)
	idRaw := json.RawMessage(fmt.Sprintf("%d", id))
	key := string(idRaw)
	call := pendingCall{result: make(chan rpcMessage, 1)}

	c.mu.Lock()
	if c.closed {
		err := c.closeErr
		if err == nil {
			err = ErrConnectionClosed
		}
		c.mu.Unlock()

		return err
	}

	c.pending[key] = call
	c.mu.Unlock()

	cleanup := func() {
		c.mu.Lock()
		delete(c.pending, key)
		c.mu.Unlock()
	}

	paramsRaw, err := marshalRaw(params)
	if err != nil {
		cleanup()

		return err
	}

	if err := c.transport.Send(ctx, rpcMessage{ID: idRaw, Method: method, Params: paramsRaw}); err != nil {
		cleanup()

		return err
	}

	select {
	case <-ctx.Done():
		cleanup()

		return ctx.Err()
	case <-c.done:
		cleanup()

		return c.err()
	case msg, ok := <-call.result:
		if !ok {
			return c.err()
		}

		if msg.Error != nil {
			return msg.Error
		}

		if result == nil || len(msg.Result) == 0 {
			return nil
		}

		return json.Unmarshal(msg.Result, result)
	}
}

func (c *rpcConn) Notify(ctx context.Context, method string, params any) error {
	paramsRaw, err := marshalRaw(params)
	if err != nil {
		return err
	}

	return c.transport.Send(ctx, rpcMessage{Method: method, Params: paramsRaw})
}

func (c *rpcConn) Respond(ctx context.Context, id json.RawMessage, result any, reqErr *rpcError) error {
	resultRaw, err := marshalRaw(result)
	if err != nil {
		return err
	}

	return c.transport.Send(ctx, rpcMessage{ID: id, Result: resultRaw, Error: reqErr})
}

func (c *rpcConn) Close() error {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	return c.closeContext(ctx, false)
}

func (c *rpcConn) CloseContext(ctx context.Context) error {
	return c.closeContext(ctx, true)
}

func (c *rpcConn) closeContext(ctx context.Context, reportWaitErr bool) error {
	c.mu.Lock()
	if c.closed {
		c.mu.Unlock()

		return nil
	}

	c.closed = true
	for key, call := range c.pending {
		delete(c.pending, key)
		close(call.result)
	}

	requestDone := make([]<-chan struct{}, 0, len(c.requests))
	for _, request := range c.requests {
		requestDone = append(requestDone, request.done)
	}
	c.mu.Unlock()

	var waitErr error

	for _, done := range requestDone {
		select {
		case <-done:
		case <-ctx.Done():
			waitErr = ctx.Err()
		}
	}

	if waitErr != nil {
		c.mu.Lock()
		for _, request := range c.requests {
			request.cancel()
		}
		c.mu.Unlock()
	}

	if !reportWaitErr {
		waitErr = nil
	}

	err := errors.Join(waitErr, c.transport.Close())
	c.closeDone()

	return err
}

func (c *rpcConn) readLoop() {
	defer func() {
		c.mu.Lock()
		if !c.closed {
			c.closed = true
			for key, call := range c.pending {
				delete(c.pending, key)
				close(call.result)
			}

			for key, request := range c.requests {
				delete(c.requests, key)
				request.cancel()
			}
		}
		c.mu.Unlock()
		c.closeDone()
	}()

	for {
		msg, raw, err := c.transport.Recv()
		if err != nil {
			if errors.Is(err, errSkippableLine) {
				c.malformedLines.Add(1)

				continue
			}

			c.recordCloseError(err)

			return
		}

		switch {
		case len(msg.ID) > 0 && msg.Method == "":
			c.deliverResponse(msg)
		case len(msg.ID) > 0 && msg.Method != "":
			go func() {
				defer recoverCodexGoroutine(context.Background(), "Codex server request")

				c.handleRequest(msg)
			}()
		case msg.Method != "":
			c.deliverNotification(msg, raw)
		}
	}
}

func (c *rpcConn) recordCloseError(err error) {
	if err == nil {
		return
	}

	c.mu.Lock()
	defer c.mu.Unlock()

	if c.closed || c.closeErr != nil {
		return
	}

	c.closeErr = fmt.Errorf("%w: %w", ErrConnectionClosed, err)
}

func (c *rpcConn) err() error {
	c.mu.Lock()
	defer c.mu.Unlock()

	if c.closeErr != nil {
		return c.closeErr
	}

	return ErrConnectionClosed
}

func (c *rpcConn) closeError() error {
	c.mu.Lock()
	defer c.mu.Unlock()

	return c.closeErr
}

func (c *rpcConn) closeDone() {
	c.doneOnce.Do(func() {
		close(c.done)
		close(c.events)
	})
}

func (c *rpcConn) deliverResponse(msg rpcMessage) {
	c.mu.Lock()

	call, ok := c.pending[string(msg.ID)]
	if ok {
		delete(c.pending, string(msg.ID))
	}
	c.mu.Unlock()

	if ok {
		call.result <- msg
	}
}

func (c *rpcConn) deliverNotification(msg rpcMessage, raw string) {
	event := rpcEvent{Method: msg.Method, Params: msg.Params, Raw: raw}
	select {
	case c.events <- event:
	case <-c.done:
	}
}

func (c *rpcConn) handleRequest(msg rpcMessage) {
	if c.handler == nil {
		_ = c.Respond(context.Background(), msg.ID, nil, &rpcError{Code: -32601, Message: "method not found"})

		return
	}

	ctx, finish, ok := c.beginRequest(string(msg.ID))
	if !ok {
		return
	}
	defer finish()

	result, err := c.handler(ctx, ServerRequest{
		ID:     append(json.RawMessage(nil), msg.ID...),
		Method: msg.Method,
		Params: append(json.RawMessage(nil), msg.Params...),
	})
	if ctx.Err() != nil || errors.Is(err, context.Canceled) {
		return
	}

	if err != nil {
		_ = c.Respond(ctx, msg.ID, nil, &rpcError{Code: -32000, Message: err.Error()})

		return
	}

	_ = c.Respond(ctx, msg.ID, result, nil)
}

func (c *rpcConn) beginRequest(key string) (context.Context, func(), bool) {
	ctx, cancel := context.WithCancel(context.Background())
	request := &pendingRequest{cancel: cancel, done: make(chan struct{})}

	c.mu.Lock()
	if c.closed {
		c.mu.Unlock()
		cancel()

		return nil, nil, false
	}

	c.requests[key] = request
	c.mu.Unlock()

	finish := func() {
		c.mu.Lock()
		if c.requests[key] == request {
			delete(c.requests, key)
		}
		c.mu.Unlock()
		cancel()
		close(request.done)
	}

	return ctx, finish, true
}

func marshalRaw(value any) (json.RawMessage, error) {
	if value == nil {
		return nil, nil
	}

	if raw, ok := value.(json.RawMessage); ok {
		return raw, nil
	}

	payload, err := json.Marshal(value)
	if err != nil {
		return nil, err
	}

	return payload, nil
}
