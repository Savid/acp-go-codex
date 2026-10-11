package codex

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"sync"
	"sync/atomic"

	"github.com/savid/acp-go-core/wire"
)

// ErrTransportClosed reports that the app-server stdout stream ended before a
// call received its response.
var ErrTransportClosed = errors.New("codex app-server transport closed")

const jsonRPCVersion = "2.0"

// message is one JSON-RPC frame the adapter writes.
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

// inbound is one JSON-RPC frame the app-server writes, decoded in one pass.
// Params decode straight into the map every consumer reads; a result stays
// raw for the caller's typed decode.
type inbound struct {
	ID     json.RawMessage `json:"id"`
	Method string          `json:"method"`
	Params map[string]any  `json:"params"`
	Result json.RawMessage `json:"result"`
	Error  *RPCError       `json:"error"`
}

// Notification is one app-server notification. Params is nil when the
// notification carried no params object.
type Notification struct {
	Method string
	Params map[string]any
}

// ServerRequest is one request the app-server sent to the adapter, answered
// through Client.Respond. Params is nil when the request carried no params
// object.
type ServerRequest struct {
	ID     json.RawMessage
	Method string
	Params map[string]any
}

// Client speaks the app-server's JSON-RPC protocol over its stdin and stdout.
// Calls are correlated by id; notifications and server requests are delivered
// on channels in stream order. Delivery is synchronous, so the consumer keeps
// draining both channels while calls are in flight.
type Client struct {
	stdin  io.Writer
	reader *wire.LineReader

	writeMu sync.Mutex

	pendingMu sync.Mutex
	pending   map[string]chan inbound
	failed    bool
	failure   error

	nextID  atomic.Int64
	started atomic.Bool

	notifications chan Notification
	requests      chan ServerRequest
}

// NewClient constructs a client over the app-server's stdin writer and stdout
// reader.
func NewClient(stdin io.Writer, stdout io.Reader) *Client {
	return &Client{
		stdin:         stdin,
		reader:        wire.NewLineReader(stdout, wire.NativeLineMaxBytes),
		pending:       make(map[string]chan inbound, 4),
		notifications: make(chan Notification),
		requests:      make(chan ServerRequest),
	}
}

// Start launches the stdout read loop. It must be called exactly once before
// any call is issued; the loop stops when the stream ends or ctx is cancelled.
func (c *Client) Start(ctx context.Context) error {
	if !c.started.CompareAndSwap(false, true) {
		return errors.New("codex client already started")
	}

	go c.readLoop(ctx)

	return nil
}

// Notifications returns the notification stream. It is closed when the read
// loop exits.
func (c *Client) Notifications() <-chan Notification { return c.notifications }

// Requests returns the server request stream. It is closed when the read loop
// exits. Every request must be answered through Respond.
func (c *Client) Requests() <-chan ServerRequest { return c.requests }

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

	waiter := make(chan inbound, 1)

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

func (c *Client) registerPending(id string, waiter chan inbound) error {
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

	for {
		line, err := c.reader.Next()
		if err != nil {
			if !errors.Is(err, io.EOF) {
				failure = err
			}

			break
		}

		frame, ok := decodeInbound(line)
		if !ok {
			// A line that is not JSON-RPC is app-server chatter on the wrong
			// stream; it is skipped rather than ending the transport.
			continue
		}

		if err := c.dispatch(ctx, frame); err != nil {
			failure = err

			break
		}
	}

	c.finish(failure)
}

// decodeInbound decodes one frame. Params that are not an object decode as
// nil rather than refusing the frame.
func decodeInbound(line []byte) (inbound, bool) {
	var frame inbound

	err := json.Unmarshal(line, &frame)
	if _, typed := errors.AsType[*json.UnmarshalTypeError](err); err != nil && !typed {
		return inbound{}, false
	}

	return frame, true
}

func (c *Client) dispatch(ctx context.Context, frame inbound) error {
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
