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
)

// ErrTransportClosed reports that the app-server stdout stream ended before a
// call received its response.
var ErrTransportClosed = errors.New("codex app-server transport closed")

const (
	jsonRPCVersion = "2.0"
	// maxLineBytes bounds one app-server stdout line so one oversize frame
	// cannot force an unbounded allocation before it is parsed.
	maxLineBytes = 10 * 1024 * 1024
	// initialLineBytes is the scanner's starting buffer.
	initialLineBytes = 64 * 1024
)

// message is one JSON-RPC frame in either direction.
type message struct {
	JSONRPC string          `json:"jsonrpc,omitempty"`
	ID      json.RawMessage `json:"id,omitempty"`
	Method  string          `json:"method,omitempty"`
	Params  json.RawMessage `json:"params,omitempty"`
	Result  json.RawMessage `json:"result,omitempty"`
	Error   *RPCError       `json:"error,omitempty"`
}

// RPCError is the error member of a failed app-server response.
type RPCError struct {
	Code    int             `json:"code"`
	Message string          `json:"message"`
	Data    json.RawMessage `json:"data,omitempty"`
}

func (e *RPCError) Error() string {
	return fmt.Sprintf("codex app-server request failed (code %d): %s", e.Code, e.Message)
}

// Notification is one app-server notification, undecoded.
type Notification struct {
	Method string
	Params json.RawMessage
}

// ServerRequest is one request the app-server sent to the adapter, answered
// through Client.Respond.
type ServerRequest struct {
	ID     json.RawMessage
	Method string
	Params json.RawMessage
}

// Client speaks the app-server's JSON-RPC protocol over its stdin and stdout.
// Calls are correlated by id; notifications and server requests are delivered
// on channels in stream order. Delivery is synchronous, so the consumer keeps
// draining both channels while calls are in flight.
type Client struct {
	stdin   io.Writer
	scanner *bufio.Scanner

	writeMu sync.Mutex

	pendingMu sync.Mutex
	pending   map[string]chan message
	failed    bool
	failure   error

	nextID  atomic.Int64
	started atomic.Bool

	notifications chan Notification
	requests      chan ServerRequest
	done          chan struct{}
	wg            sync.WaitGroup
}

// NewClient constructs a client over the app-server's stdin writer and stdout
// reader.
func NewClient(stdin io.Writer, stdout io.Reader) *Client {
	scanner := bufio.NewScanner(stdout)
	scanner.Buffer(make([]byte, 0, initialLineBytes), maxLineBytes)

	return &Client{
		stdin:         stdin,
		scanner:       scanner,
		pending:       make(map[string]chan message, 4),
		notifications: make(chan Notification),
		requests:      make(chan ServerRequest),
		done:          make(chan struct{}),
	}
}

// Start launches the stdout read loop. It must be called exactly once before
// any call is issued; the loop stops when the stream ends or ctx is cancelled.
func (c *Client) Start(ctx context.Context) error {
	if !c.started.CompareAndSwap(false, true) {
		return errors.New("codex client already started")
	}

	c.wg.Go(func() { c.readLoop(ctx) })

	return nil
}

// Stop waits for the read loop to exit and returns the transport failure, if
// any. The caller first ends the stream by terminating the process.
func (c *Client) Stop() error {
	if c.started.Load() {
		c.wg.Wait()
	}

	return c.Err()
}

// Notifications returns the notification stream. It is closed when the read
// loop exits.
func (c *Client) Notifications() <-chan Notification { return c.notifications }

// Requests returns the server request stream. It is closed when the read loop
// exits. Every request must be answered through Respond.
func (c *Client) Requests() <-chan ServerRequest { return c.requests }

// Done is closed when the read loop has exited.
func (c *Client) Done() <-chan struct{} { return c.done }

// Err returns the transport failure, or nil after a clean end-of-stream.
func (c *Client) Err() error {
	c.pendingMu.Lock()
	defer c.pendingMu.Unlock()

	return c.failure
}

// Call sends one request and decodes its result into result when non-nil. A
// failed response is returned as an *RPCError.
func (c *Client) Call(ctx context.Context, method string, params any, result any) error {
	if err := ctx.Err(); err != nil {
		return err
	}

	id := json.RawMessage(fmt.Sprintf("%d", c.nextID.Add(1)))

	encodedParams, err := marshalRaw(params)
	if err != nil {
		return fmt.Errorf("encode %s params: %w", method, err)
	}

	waiter := make(chan message, 1)

	if err := c.registerPending(string(id), waiter); err != nil {
		return err
	}

	if err := c.write(message{ID: id, Method: method, Params: encodedParams}); err != nil {
		c.unregisterPending(string(id))

		return fmt.Errorf("write %s request: %w", method, err)
	}

	select {
	case response, ok := <-waiter:
		if !ok {
			return c.closedError()
		}

		if response.Error != nil {
			return response.Error
		}

		if result == nil || len(response.Result) == 0 {
			return nil
		}

		if err := json.Unmarshal(response.Result, result); err != nil {
			return fmt.Errorf("decode %s result: %w", method, err)
		}

		return nil
	case <-ctx.Done():
		c.unregisterPending(string(id))

		return ctx.Err()
	}
}

// Notify sends one notification.
func (c *Client) Notify(method string, params any) error {
	encodedParams, err := marshalRaw(params)
	if err != nil {
		return fmt.Errorf("encode %s params: %w", method, err)
	}

	return c.write(message{Method: method, Params: encodedParams})
}

// Respond answers one server request. A nil rpcErr sends result; otherwise the
// error is sent and result is ignored.
func (c *Client) Respond(request ServerRequest, result any, rpcErr *RPCError) error {
	encodedResult, err := marshalRaw(result)
	if err != nil {
		return fmt.Errorf("encode %s response: %w", request.Method, err)
	}

	if rpcErr != nil {
		encodedResult = nil
	}

	return c.write(message{ID: request.ID, Result: encodedResult, Error: rpcErr})
}

func (c *Client) write(frame message) error {
	frame.JSONRPC = jsonRPCVersion

	encoded, err := json.Marshal(frame)
	if err != nil {
		return err
	}

	c.writeMu.Lock()
	defer c.writeMu.Unlock()

	record := make([]byte, len(encoded)+1)
	copy(record, encoded)
	record[len(encoded)] = '\n'

	n, err := c.stdin.Write(record)
	if err == nil && n != len(record) {
		err = io.ErrShortWrite
	}

	return err
}

func (c *Client) registerPending(id string, waiter chan message) error {
	c.pendingMu.Lock()
	defer c.pendingMu.Unlock()

	if c.failed {
		return c.closedErrorLocked()
	}

	c.pending[id] = waiter

	return nil
}

func (c *Client) unregisterPending(id string) {
	c.pendingMu.Lock()
	defer c.pendingMu.Unlock()

	delete(c.pending, id)
}

func (c *Client) closedError() error {
	c.pendingMu.Lock()
	defer c.pendingMu.Unlock()

	return c.closedErrorLocked()
}

func (c *Client) closedErrorLocked() error {
	if c.failure != nil {
		return fmt.Errorf("%w: %w", ErrTransportClosed, c.failure)
	}

	return ErrTransportClosed
}

func (c *Client) readLoop(ctx context.Context) {
	var failure error

	for c.scanner.Scan() {
		line := c.scanner.Bytes()
		if len(line) == 0 {
			continue
		}

		var frame message
		if err := json.Unmarshal(line, &frame); err != nil {
			// A line that is not JSON-RPC is app-server chatter on the wrong
			// stream; it is skipped rather than ending the transport.
			continue
		}

		if err := c.dispatch(ctx, frame); err != nil {
			failure = err

			break
		}
	}

	if failure == nil {
		if err := c.scanner.Err(); err != nil {
			failure = err
		}
	}

	c.finish(failure)
}

func (c *Client) dispatch(ctx context.Context, frame message) error {
	switch {
	case len(frame.ID) > 0 && frame.Method == "":
		c.pendingMu.Lock()
		waiter, ok := c.pending[string(frame.ID)]
		delete(c.pending, string(frame.ID))
		c.pendingMu.Unlock()

		if ok {
			waiter <- frame
		}

		return nil
	case len(frame.ID) > 0:
		select {
		case c.requests <- ServerRequest{ID: frame.ID, Method: frame.Method, Params: frame.Params}:
			return nil
		case <-ctx.Done():
			return context.Cause(ctx)
		}
	case frame.Method != "":
		select {
		case c.notifications <- Notification{Method: frame.Method, Params: frame.Params}:
			return nil
		case <-ctx.Done():
			return context.Cause(ctx)
		}
	default:
		return nil
	}
}

func (c *Client) finish(failure error) {
	c.pendingMu.Lock()
	c.failed = true
	c.failure = failure

	for id, waiter := range c.pending {
		close(waiter)
		delete(c.pending, id)
	}
	c.pendingMu.Unlock()

	close(c.notifications)
	close(c.requests)
	close(c.done)
}

func marshalRaw(value any) (json.RawMessage, error) {
	if value == nil {
		return nil, nil
	}

	if raw, ok := value.(json.RawMessage); ok {
		return raw, nil
	}

	return json.Marshal(value)
}

// IsMethodNotFound reports whether the app-server answered that it does not
// implement the method.
func IsMethodNotFound(err error) bool {
	var rpcErr *RPCError

	return errors.As(err, &rpcErr) && rpcErr.Code == -32601
}
