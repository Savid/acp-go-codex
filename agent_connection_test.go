package codexacp

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/coder/acp-go-sdk"
	"github.com/savid/acp-go-codex/internal/lifecycle"
	"github.com/stretchr/testify/require"
)

type actionWireWriter struct {
	mu      sync.Mutex
	methods []string
	err     error
}

type shortActionWireWriter struct{}

func (shortActionWireWriter) Write(payload []byte) (int, error) { return len(payload) - 1, nil }

type deadlineOnlyWriter struct {
	err error
}

func (*deadlineOnlyWriter) Write(payload []byte) (int, error) { return len(payload), nil }

func (w *deadlineOnlyWriter) SetWriteDeadline(time.Time) error { return w.err }

func (w *actionWireWriter) InterruptWrite() error { return nil }

type gatedActionWireWriter struct {
	started chan []byte
	release chan struct{}
}

func (w *gatedActionWireWriter) InterruptWrite() error { return nil }

type interruptibleBlockingWriter struct {
	started chan struct{}
	exited  chan struct{}
	unblock chan struct{}
	start   sync.Once
	close   sync.Once
}

func newInterruptibleBlockingWriter() *interruptibleBlockingWriter {
	return &interruptibleBlockingWriter{
		started: make(chan struct{}),
		exited:  make(chan struct{}),
		unblock: make(chan struct{}),
	}
}

func (w *interruptibleBlockingWriter) Write([]byte) (int, error) {
	w.start.Do(func() { close(w.started) })
	<-w.unblock
	close(w.exited)

	return 0, io.ErrClosedPipe
}

func (w *interruptibleBlockingWriter) Close() error {
	w.close.Do(func() { close(w.unblock) })

	return nil
}

func (w *interruptibleBlockingWriter) InterruptWrite() error { return w.Close() }

func (w *gatedActionWireWriter) Write(payload []byte) (int, error) {
	w.started <- append([]byte(nil), payload...)
	<-w.release

	return len(payload), nil
}

func (w *actionWireWriter) Write(payload []byte) (int, error) {
	var message struct {
		Method string `json:"method"`
	}
	_ = json.Unmarshal(payload, &message)
	w.mu.Lock()
	w.methods = append(w.methods, message.Method)
	w.mu.Unlock()
	if w.err != nil {
		return 0, w.err
	}

	return len(payload), nil
}

func (w *actionWireWriter) snapshot() []string {
	w.mu.Lock()
	defer w.mu.Unlock()

	return append([]string(nil), w.methods...)
}

func TestLifecycleNotificationCancellationInterruptsAndJoinsStalledWriter(t *testing.T) {
	inputR, inputW := io.Pipe()
	t.Cleanup(func() {
		_ = inputR.Close()
		_ = inputW.Close()
	})
	wire := newInterruptibleBlockingWriter()
	agent := NewAgent()
	conn := newLocalAgentConnection(agent, wire, inputR)
	agent.setAgentClient(conn)

	ctx, cancel := context.WithCancel(t.Context())
	result := make(chan error, 1)
	go func() {
		result <- conn.SessionUpdateLifecycle(ctx, acp.SessionNotification{
			SessionId: "session",
			Update:    acp.SessionUpdate{SessionInfoUpdate: &acp.SessionSessionInfoUpdate{}},
		})
	}()

	<-wire.started
	cancel()
	err := <-result
	require.ErrorIs(t, err, context.Canceled)
	select {
	case <-wire.exited:
	default:
		t.Fatal("lifecycle notification returned before its stalled writer exited")
	}
}

func TestLifecycleNegotiationOmitsNonInterruptibleCarrier(t *testing.T) {
	inputR, inputW := io.Pipe()
	t.Cleanup(func() {
		_ = inputR.Close()
		_ = inputW.Close()
	})

	agent := NewAgent()
	conn := newLocalAgentConnection(agent, io.Discard, inputR)
	agent.setAgentClient(conn)

	params, err := json.Marshal(map[string]any{
		"protocolVersion":    1,
		"clientCapabilities": map[string]any{},
		"_meta":              map[string]any{lifecycle.MetaKey: map[string]any{"version": 1}},
	})
	require.NoError(t, err)
	result, reqErr := conn.handle(t.Context(), acp.AgentMethodInitialize, params)
	require.Nil(t, reqErr)
	response := asType[acp.InitializeResponse](t, result)
	require.NotContains(t, response.Meta, lifecycle.MetaKey)
	require.Error(t, conn.SessionUpdateLifecycle(t.Context(), acp.SessionNotification{}))

	closed := make(chan error, 1)
	go func() { closed <- agent.Close() }()
	select {
	case err := <-closed:
		require.NoError(t, err)
	case <-time.After(time.Second):
		t.Fatal("Agent.Close waited on a non-interruptible lifecycle carrier")
	}
}

func TestACPTransportLoggerRedactsEveryOpaqueSDKAttribute(t *testing.T) {
	var logs lockedLogBuffer
	agent := NewAgent(WithLogger(slog.New(slog.NewTextHandler(&logs, &slog.HandlerOptions{Level: slog.LevelDebug}))))
	inputR, inputW := io.Pipe()
	conn := newLocalAgentConnection(agent, io.Discard, inputR)
	agent.setAgentClient(conn)

	secret := "sdk-log-secret-sentinel"
	_, err := io.WriteString(inputW, `{"jsonrpc":"2.0","raw":"`+secret+`"`+"\n")
	require.NoError(t, err)
	_, err = io.WriteString(inputW, `{"jsonrpc":"2.0","method":"$/cancel_request","params":{"requestId":{"secret":"`+secret+`"}}}`+"\n")
	require.NoError(t, err)
	_, err = io.WriteString(inputW, `{"jsonrpc":"2.0","unknown":{"prompt":"`+secret+`"}}`+"\n")
	require.NoError(t, err)

	require.Eventually(t, func() bool {
		return strings.Count(logs.String(), valueInternalFailure) >= 5
	}, time.Second, time.Millisecond)
	require.NotContains(t, logs.String(), secret)

	require.NoError(t, inputW.Close())
	<-conn.Done()
	require.NoError(t, inputR.Close())
}

func TestACPTransportLoggerUsesOnlyClassifiedMessagesAndValues(t *testing.T) {
	var logs lockedLogBuffer
	base := slog.New(slog.NewTextHandler(&logs, nil))
	logger := secretSafeLogger(base).
		With(slog.String("opaque", "logger-secret-sentinel"), slog.Int("capacity", 3), slog.Uint64("queued", 4)).
		WithGroup("logger-secret-sentinel")
	logger.Info("logger-secret-sentinel", slog.Any("structured", map[string]any{"token": "logger-secret-sentinel"}))

	output := logs.String()
	require.NotContains(t, output, "logger-secret-sentinel")
	require.Contains(t, output, "ACP transport diagnostic")
	require.Contains(t, output, "capacity=3")
	require.Contains(t, output, "queued=4")
	require.True(t, secretSafeLogger(nil).Enabled(t.Context(), slog.LevelInfo))
}

func TestConcurrentAgentCloseInterruptsActiveLifecycleWriter(t *testing.T) {
	inputR, inputW := io.Pipe()
	t.Cleanup(func() {
		_ = inputR.Close()
		_ = inputW.Close()
	})
	wire := newInterruptibleBlockingWriter()
	agent := NewAgent()
	conn := newLocalAgentConnection(agent, wire, inputR)
	agent.setAgentClient(conn)

	s := &session{agent: agent, id: "session"}
	agent.mu.Lock()
	agent.sessions[s.id] = s
	agent.mu.Unlock()

	s.lifecycleMu.Lock()
	delivery, err := s.enqueueLifecycleDeliveryLocked(t.Context(), acp.SessionNotification{
		SessionId: s.id,
		Update:    acp.SessionUpdate{SessionInfoUpdate: &acp.SessionSessionInfoUpdate{}},
	})
	s.lifecycleMu.Unlock()
	require.NoError(t, err)
	<-wire.started

	first := make(chan error, 1)
	second := make(chan error, 1)
	go func() { first <- agent.Close() }()
	go func() { second <- agent.Close() }()

	require.Error(t, <-delivery)
	firstErr := <-first
	secondErr := <-second
	require.Equal(t, firstErr, secondErr)
	select {
	case <-wire.exited:
	default:
		t.Fatal("Agent.Close returned before the interrupted writer exited")
	}
}

func TestLifecycleRequestErrorDeliveryPreservesHostProvenance(t *testing.T) {
	deliveryErr := acp.NewInternalError(map[string]any{"error": "lifecycle-host-delivery"})
	agent := NewAgent()
	agent.setAgentClient(&errorAgentClient{recordingAgentClient: newRecordingAgentClient(), updateErr: deliveryErr})
	s := &session{agent: agent, id: "session"}

	s.lifecycleMu.Lock()
	result, err := s.enqueueLifecycleDeliveryLocked(t.Context(), acp.SessionNotification{
		SessionId: s.id,
		Update:    acp.SessionUpdate{SessionInfoUpdate: &acp.SessionSessionInfoUpdate{}},
	})
	s.lifecycleMu.Unlock()
	require.NoError(t, err)
	err = <-result
	var hostFailure *hostDeliveryError
	require.ErrorAs(t, err, &hostFailure)
	var got *acp.RequestError
	require.ErrorAs(t, err, &got)
	require.Same(t, deliveryErr, got)
}

func TestLocalActionRegistrationBarrierOrdersAndContainsFailures(t *testing.T) {
	request := testPermissionRequest()
	request.Meta = map[string]any{lifecycle.MetaKey: lifecycle.ActionCorrelation{
		StreamID: "stream", ActionID: "action", Owner: lifecycle.Owner{Type: lifecycle.OwnerTurn, ID: "turn"},
	}.Value()}

	t.Run("registered request precedes lifecycle callback", func(t *testing.T) {
		inputR, inputW := io.Pipe()
		defer inputR.Close()
		defer inputW.Close()
		wire := &actionWireWriter{}
		agent := NewAgent(WithConcurrencyLimits(ConcurrencyLimits{MaxConcurrentClientCalls: 1}))
		conn := newLocalAgentConnection(agent, wire, inputR)
		agent.setAgentClient(conn)
		callbackErr := errors.New("callback failed")
		called := false
		_, err := conn.RequestPermissionRegistered(context.Background(), request, "action", func() error {
			called = true
			if got := wire.snapshot(); len(got) != 1 || got[0] != acp.ClientMethodSessionRequestPermission {
				t.Fatalf("wire methods at registration = %#v", got)
			}
			if release, acquireErr := agent.acquireClientCall(context.Background()); acquireErr != nil {
				t.Fatal("registered permission retained its client-call lease after full write")
			} else {
				release()
			}
			if updateErr := conn.SessionUpdateLifecycle(context.Background(), acp.SessionNotification{
				SessionId: "session", Update: acp.SessionUpdate{SessionInfoUpdate: &acp.SessionSessionInfoUpdate{}},
			}); updateErr != nil {
				t.Fatalf("ordered lifecycle callback could not acquire its notification lane: %v", updateErr)
			}

			return callbackErr
		})
		if !errors.Is(err, callbackErr) || !called {
			t.Fatalf("registered request error=%v called=%v", err, called)
		}
	})

	t.Run("write failure never publishes lifecycle action", func(t *testing.T) {
		inputR, inputW := io.Pipe()
		defer inputR.Close()
		defer inputW.Close()
		wire := &actionWireWriter{err: errors.New("write failed")}
		agent := NewAgent()
		conn := newLocalAgentConnection(agent, wire, inputR)
		agent.setAgentClient(conn)
		called := false
		_, err := conn.RequestPermissionRegistered(context.Background(), request, "action", func() error {
			called = true

			return nil
		})
		if err == nil || called {
			t.Fatalf("write failure error=%v callback called=%v", err, called)
		}
	})

	elicitation := acp.NewUnstableCreateElicitationRequestForm(acp.UnstableElicitationSchema{Type: "object"})
	elicitation.Form.Message = "Need input"
	scope := elicitationScope{
		SessionID: "session", TurnNonce: "nonce", ToolCallID: "tool",
		ActionCorrelation: lifecycle.ActionCorrelation{
			StreamID: "stream", ActionID: "elicitation-action",
			Owner: lifecycle.Owner{Type: lifecycle.OwnerTurn, ID: "turn"},
		}.Value(),
	}

	t.Run("registered elicitation precedes lifecycle callback", func(t *testing.T) {
		inputR, inputW := io.Pipe()
		defer inputR.Close()
		defer inputW.Close()
		wire := &actionWireWriter{}
		agent := NewAgent(WithConcurrencyLimits(ConcurrencyLimits{MaxConcurrentClientCalls: 1}))
		conn := newLocalAgentConnection(agent, wire, inputR)
		agent.setAgentClient(conn)
		callbackErr := errors.New("elicitation callback failed")
		called := false
		_, err := conn.CreateElicitationRegistered(
			context.Background(), elicitation, scope, "elicitation-action", func() error {
				called = true
				if got := wire.snapshot(); len(got) != 1 || got[0] != acp.ClientMethodElicitationCreate {
					t.Fatalf("wire methods at registration = %#v", got)
				}
				if release, acquireErr := agent.acquireClientCall(context.Background()); acquireErr != nil {
					t.Fatal("registered elicitation retained its client-call lease after full write")
				} else {
					release()
				}
				if updateErr := conn.SessionUpdateLifecycle(context.Background(), acp.SessionNotification{
					SessionId: "session", Update: acp.SessionUpdate{SessionInfoUpdate: &acp.SessionSessionInfoUpdate{}},
				}); updateErr != nil {
					t.Fatalf("ordered lifecycle callback could not acquire its notification lane: %v", updateErr)
				}

				return callbackErr
			},
		)
		if !errors.Is(err, callbackErr) || !called {
			t.Fatalf("registered elicitation error=%v called=%v", err, called)
		}
	})

	t.Run("elicitation write failure never publishes lifecycle action", func(t *testing.T) {
		inputR, inputW := io.Pipe()
		defer inputR.Close()
		defer inputW.Close()
		wire := &actionWireWriter{err: errors.New("write failed")}
		agent := NewAgent()
		conn := newLocalAgentConnection(agent, wire, inputR)
		agent.setAgentClient(conn)
		called := false
		_, err := conn.CreateElicitationRegistered(
			context.Background(), elicitation, scope, "elicitation-action", func() error {
				called = true

				return nil
			},
		)
		if err == nil || called {
			t.Fatalf("elicitation write failure error=%v callback called=%v", err, called)
		}
	})
}

func TestLocalActionRegistrationBarrierWaitsForFullWriteAfterEarlyResponse(t *testing.T) {
	inputR, inputW := io.Pipe()
	t.Cleanup(func() {
		_ = inputR.Close()
		_ = inputW.Close()
	})
	wire := &gatedActionWireWriter{started: make(chan []byte, 1), release: make(chan struct{})}
	agent := NewAgent(WithConcurrencyLimits(ConcurrencyLimits{MaxConcurrentClientCalls: 1}))
	conn := newLocalAgentConnection(agent, wire, inputR)
	agent.setAgentClient(conn)
	request := testPermissionRequest()
	request.Meta = map[string]any{lifecycle.MetaKey: lifecycle.ActionCorrelation{
		StreamID: "stream", ActionID: "early-action", Owner: lifecycle.Owner{Type: lifecycle.OwnerTurn, ID: "turn"},
	}.Value()}
	registered := make(chan struct{}, 1)
	finished := make(chan error, 1)
	var orderMu sync.Mutex
	var order []string
	go func() {
		_, err := conn.RequestPermissionRegistered(t.Context(), request, "early-action", func() error {
			if updateErr := conn.SessionUpdateLifecycle(t.Context(), acp.SessionNotification{
				SessionId: "session", Update: acp.SessionUpdate{SessionInfoUpdate: &acp.SessionSessionInfoUpdate{}},
			}); updateErr != nil {
				return updateErr
			}
			if release, acquireErr := agent.acquireClientCall(t.Context()); acquireErr != nil {
				return errors.New("client-call lease remained held after the full request write")
			} else {
				release()
			}
			orderMu.Lock()
			order = append(order, "pending")
			orderMu.Unlock()
			registered <- struct{}{}

			return nil
		})
		orderMu.Lock()
		order = append(order, "terminal")
		orderMu.Unlock()
		finished <- err
	}()

	payload := <-wire.started
	var wireRequest struct {
		ID json.RawMessage `json:"id"`
	}
	require.NoError(t, json.Unmarshal(payload, &wireRequest))
	responseWritten := make(chan error, 1)
	go func() {
		_, err := inputW.Write([]byte(`{"jsonrpc":"2.0","id":` + string(wireRequest.ID) + `,"result":{"outcome":{"cancelled":{}}}}` + "\n"))
		responseWritten <- err
	}()
	require.NoError(t, <-responseWritten)
	conn.registrations.mu.Lock()
	_, stillPending := conn.registrations.pending["early-action"]
	conn.registrations.mu.Unlock()
	require.True(t, stillPending, "early host response bypassed the full-write registration barrier")

	close(wire.release)
	select {
	case <-registered:
	case err := <-finished:
		t.Fatalf("request finished before lifecycle registration: %v", err)
	}
	require.NoError(t, <-finished)
	release, acquireErr := agent.acquireClientCall(t.Context())
	require.NoError(t, acquireErr)
	release()
	orderMu.Lock()
	require.Equal(t, []string{"pending", "terminal"}, order)
	orderMu.Unlock()
}

func TestLocalAgentConnectionHandleBranches(t *testing.T) {
	agent := newPlaceholderAgent()
	conn := &localAgentConnection{agent: agent}
	ctx := context.Background()

	if _, reqErr := conn.handle(ctx, acp.AgentMethodSessionList, json.RawMessage(`{}`)); reqErr == nil {
		t.Fatal("uninitialized non-initialize request succeeded")
	}

	if _, reqErr := conn.handle(ctx, acp.AgentMethodInitialize, json.RawMessage(`{`)); reqErr == nil {
		t.Fatal("invalid JSON initialize request succeeded")
	}
	if _, reqErr := conn.handle(ctx, acp.AgentMethodInitialize, json.RawMessage(`{}`)); reqErr != nil {
		t.Fatalf("initialize returned request error: %v", reqErr)
	}
	if _, reqErr := conn.handle(ctx, "missing/method", json.RawMessage(`{}`)); reqErr == nil {
		t.Fatal("unknown method succeeded")
	}
	if _, reqErr := conn.handle(ctx, ForkSessionMethod, json.RawMessage(`{`)); reqErr == nil {
		t.Fatal("invalid extension payload succeeded")
	}
}

func TestConnectionCarrierAndRegistrationValidationBranches(t *testing.T) {
	require.False(t, interruptibleOutput(nil))
	deadlineErr := errors.New("deadline failed")
	deadline := &deadlineOnlyWriter{err: deadlineErr}
	require.True(t, interruptibleOutput(deadline))
	require.ErrorIs(t, (&localAgentConnection{output: deadline}).InterruptTransport(), deadlineErr)
	require.ErrorContains(t, (*localAgentConnection)(nil).InterruptTransport(), "unavailable")
	require.ErrorContains(t, (&localAgentConnection{output: io.Discard}).InterruptTransport(), "cannot interrupt")

	writer := newRequestRegistrationWriter(io.Discard)
	_, _, err := writer.expect("")
	require.ErrorContains(t, err, "requires an action id")
	ready, abandon, err := writer.expect("action")
	require.NoError(t, err)
	_, _, err = writer.expect("action")
	require.ErrorContains(t, err, "duplicate")
	abandon()
	select {
	case <-ready:
		t.Fatal("abandoned registration was signaled")
	default:
	}

	short := newRequestRegistrationWriter(shortActionWireWriter{})
	ready, _, err = short.expect("short")
	require.NoError(t, err)
	payload := []byte(`{"jsonrpc":"2.0","id":1,"method":"session/request_permission","params":{"_meta":{"acp-go.dev/lifecycle":{"action":{"actionId":"short"}}}}}`)
	_, err = short.Write(payload)
	require.ErrorIs(t, err, io.ErrShortWrite)
	require.ErrorIs(t, <-ready, io.ErrShortWrite)
}

func TestRegisteredActionBarrierHandlesEarlyAndCancelledRequests(t *testing.T) {
	_, err := registeredActionRequest[int](t.Context(), nil, "action", nil, nil, func(context.Context) (int, error) {
		return 0, nil
	})
	require.ErrorContains(t, err, "barrier is unavailable")

	wirePayload := func(actionID string) []byte {
		return []byte(`{"jsonrpc":"2.0","id":1,"method":"session/request_permission","params":{"_meta":{"acp-go.dev/lifecycle":{"action":{"actionId":"` + actionID + `"}}}}}`)
	}

	t.Run("early response waits until caller cancellation", func(t *testing.T) {
		writer := newRequestRegistrationWriter(io.Discard)
		ctx, cancel := context.WithCancel(t.Context())
		done := make(chan error, 1)
		go func() {
			_, callErr := registeredActionRequest[int](ctx, writer, "early", nil, nil, func(context.Context) (int, error) {
				return 7, errors.New("early response")
			})
			done <- callErr
		}()
		require.Eventually(t, func() bool {
			writer.mu.Lock()
			defer writer.mu.Unlock()

			return writer.pending["early"] != nil
		}, time.Second, time.Millisecond)
		cancel()
		err := <-done
		require.ErrorContains(t, err, "early response")
		require.ErrorIs(t, err, context.Canceled)
	})

	t.Run("cancellation interrupts and joins request", func(t *testing.T) {
		writer := newRequestRegistrationWriter(io.Discard)
		ctx, cancel := context.WithCancel(t.Context())
		started := make(chan struct{})
		interrupted := errors.New("interrupted")
		done := make(chan error, 1)
		go func() {
			_, callErr := registeredActionRequest[int](ctx, writer, "cancel", func() error { return interrupted }, nil, func(requestCtx context.Context) (int, error) {
				close(started)
				<-requestCtx.Done()

				return 9, context.Cause(requestCtx)
			})
			done <- callErr
		}()
		<-started
		cancel()
		err := <-done
		require.ErrorIs(t, err, context.Canceled)
		require.ErrorIs(t, err, interrupted)
	})

	t.Run("early request retains write failure", func(t *testing.T) {
		writeErr := errors.New("write failed")
		wire := &actionWireWriter{err: writeErr}
		writer := newRequestRegistrationWriter(wire)
		done := make(chan error, 1)
		go func() {
			_, callErr := registeredActionRequest[int](t.Context(), writer, "write", nil, nil, func(context.Context) (int, error) {
				return 11, errors.New("early")
			})
			done <- callErr
		}()
		require.Eventually(t, func() bool {
			writer.mu.Lock()
			defer writer.mu.Unlock()

			return writer.pending["write"] != nil
		}, time.Second, time.Millisecond)
		_, err := writer.Write(wirePayload("write"))
		require.ErrorIs(t, err, writeErr)
		err = <-done
		require.ErrorIs(t, err, writeErr)
		require.ErrorContains(t, err, "early")
	})

	t.Run("early request returns after successful registration write", func(t *testing.T) {
		writer := newRequestRegistrationWriter(io.Discard)
		observedCtx := &observedDoneContext{Context: t.Context(), observed: make(chan struct{})}
		done := make(chan registeredActionResult[int], 1)
		go func() {
			value, callErr := registeredActionRequest[int](observedCtx, writer, "success", nil, nil, func(context.Context) (int, error) {
				return 13, errors.New("early")
			})
			done <- registeredActionResult[int]{value: value, err: callErr}
		}()
		require.Eventually(t, func() bool {
			writer.mu.Lock()
			defer writer.mu.Unlock()

			return writer.pending["success"] != nil
		}, time.Second, time.Millisecond)
		<-observedCtx.observed
		_, err := writer.Write(wirePayload("success"))
		require.NoError(t, err)
		result := <-done
		require.Equal(t, 13, result.value)
		require.ErrorContains(t, result.err, "early")
	})
}

func TestRegisteredClientCallsRejectInvalidScopeAndBackpressure(t *testing.T) {
	agent := NewAgent(WithConcurrencyLimits(ConcurrencyLimits{MaxConcurrentClientCalls: 1}))
	agent.clientCalls <- struct{}{}
	conn := &localAgentConnection{agent: agent, registrations: newRequestRegistrationWriter(io.Discard)}
	_, err := conn.CreateElicitationRegistered(t.Context(), acp.UnstableCreateElicitationRequest{}, elicitationScope{}, "action", nil)
	require.ErrorContains(t, err, "must include form or url")

	form := acp.NewUnstableCreateElicitationRequestForm(acp.UnstableElicitationSchema{Type: "object"})
	form.Form.Message = "message"
	_, err = conn.CreateElicitationRegistered(t.Context(), form, elicitationScope{SessionID: "session", TurnNonce: "turn", ToolCallID: "tool"}, "action", nil)
	require.ErrorContains(t, err, valueBackpressure)
	_, err = conn.RequestPermissionRegistered(t.Context(), testPermissionRequest(), "action", nil)
	require.ErrorContains(t, err, valueBackpressure)
	<-agent.clientCalls

	_, err = registeredActionRequest[int](t.Context(), conn.registrations, "", nil, nil, func(context.Context) (int, error) { return 0, nil })
	require.ErrorContains(t, err, "requires an action id")

	agent.lifecycleCalls <- struct{}{}
	cancelled, cancel := context.WithCancel(t.Context())
	cancel()
	conn.lifecycleOK = true
	_, err = conn.agent.acquireLifecycleCall(cancelled)
	require.ErrorIs(t, err, context.Canceled)
	require.ErrorIs(t, conn.SessionUpdateLifecycle(cancelled, acp.SessionNotification{}), context.Canceled)
	<-agent.lifecycleCalls
	withoutLimit := NewAgent()
	withoutLimit.lifecycleCalls = nil
	release, err := withoutLimit.acquireLifecycleCall(t.Context())
	require.NoError(t, err)
	release()
}

func TestEstablishmentReservationCollisionRefusesBeforeSessionDispatch(t *testing.T) {
	agent := newPlaceholderAgent()
	hooks := newEstablishmentHooks(agent.log)
	_, err := hooks.reserve("1")
	require.NoError(t, err)
	conn := &localAgentConnection{agent: agent, establishment: hooks}
	conn.initialized.Store(true)
	params := json.RawMessage(`{"` + establishmentHookParam + `":"1"}`)
	_, reqErr := conn.handle(t.Context(), acp.AgentMethodSessionNew, params)
	require.NotNil(t, reqErr)
	require.Equal(t, -32600, reqErr.Code)
	hooks.failAll(errEstablishmentCancelled)
}

// TestRequestErrorCancelPrecedence pins which signal decides -32800. Only an
// honored $/cancel_request cancels a request context with cause
// context.Canceled, and it outranks whatever error the handler was carrying;
// a connection teardown or an adapter deadline carries a different cause and
// must not be reported as a cancellation even when the error itself wraps
// context.Canceled.
func TestRequestErrorCancelPrecedence(t *testing.T) {
	invalidParams := acp.NewInvalidParams(map[string]any{jsonFieldError: errValueUnsupported, jsonFieldField: jsonFieldPrompt})

	for name, test := range map[string]struct {
		cause    error
		err      error
		wantCode int
		wantSame bool
	}{
		"honored cancel outranks a request error": {
			cause:    context.Canceled,
			err:      invalidParams,
			wantCode: -32800,
		},
		"honored cancel with a plain error": {
			cause:    context.Canceled,
			err:      context.Canceled,
			wantCode: -32800,
		},
		"connection teardown is not a cancellation": {
			cause:    errors.New("connection closed"),
			err:      fmt.Errorf("write update: %w", context.Canceled),
			wantCode: -32603,
		},
		"adapter deadline is an internal failure": {
			cause:    context.DeadlineExceeded,
			err:      context.DeadlineExceeded,
			wantCode: -32603,
		},
		"live request keeps its request error": {
			err:      invalidParams,
			wantCode: -32602,
			wantSame: true,
		},
		"live request wraps an opaque failure": {
			err:      errors.New("boom"),
			wantCode: -32603,
		},
	} {
		t.Run(name, func(t *testing.T) {
			ctx, cancel := context.WithCancelCause(context.Background())
			defer cancel(errors.New("test cleanup"))

			if test.cause != nil {
				cancel(test.cause)
			}

			got := requestError(ctx, test.err)
			if got == nil || got.Code != test.wantCode {
				t.Fatalf("requestError = %#v, want code %d", got, test.wantCode)
			}
			if test.wantSame && got != invalidParams {
				t.Fatalf("requestError = %#v, want the original request error", got)
			}
		})
	}

	if got := requestError(context.Background(), nil); got != nil {
		t.Fatalf("requestError(nil) = %#v", got)
	}
}

func TestLocalAgentConnectionClosedWinsBeforeDispatchAndDecode(t *testing.T) {
	agent := NewAgent()
	if err := agent.Close(); err != nil {
		t.Fatalf("Close returned error: %v", err)
	}

	conn := &localAgentConnection{agent: agent}
	for name, test := range map[string]struct {
		method string
		params json.RawMessage
	}{
		"initialize malformed": {method: acp.AgentMethodInitialize, params: json.RawMessage(`{`)},
		"known malformed":      {method: acp.AgentMethodSessionList, params: json.RawMessage(`{`)},
		"unknown stable":       {method: "missing/method", params: json.RawMessage(`{}`)},
		"unknown extension":    {method: "_codex/missing", params: json.RawMessage(`{`)},
	} {
		t.Run(name, func(t *testing.T) {
			if _, reqErr := conn.handle(context.Background(), test.method, test.params); reqErr == nil || reqErr.Code != -32600 {
				t.Fatalf("closed request error = %#v, want -32600", reqErr)
			}
		})
	}
}

func TestLocalAgentConnectionOutboundClientCalls(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	c2aR, c2aW := io.Pipe()
	a2cR, a2cW := io.Pipe()
	t.Cleanup(func() {
		_ = c2aR.Close()
		_ = c2aW.Close()
		_ = a2cR.Close()
		_ = a2cW.Close()
	})

	client := &recordingClient{}
	clientConn := acp.NewClientSideConnection(client, c2aW, a2cR)
	t.Cleanup(func() {
		_ = clientConn
	})

	agent := NewAgent()
	agentConn := newLocalAgentConnection(agent, a2cW, c2aR)
	agent.setAgentClient(agentConn)

	if err := agentConn.SessionUpdate(ctx, acp.SessionNotification{
		SessionId: "session-1",
		Update:    acp.UpdateAgentMessageText("hello"),
	}); err != nil {
		t.Fatalf("SessionUpdate returned error: %v", err)
	}
	eventually(t, func() bool { return len(client.Updates()) == 1 })
	if len(client.Updates()) != 1 {
		t.Fatalf("client updates = %d, want 1", len(client.Updates()))
	}

	permission, err := agentConn.RequestPermission(ctx, testPermissionRequest())
	if err != nil {
		t.Fatalf("RequestPermission returned error: %v", err)
	}
	if permission.Outcome.Cancelled == nil {
		t.Fatalf("permission response = %#v", permission)
	}

	elicitation, err := agentConn.CreateElicitation(ctx, acp.UnstableCreateElicitationRequest{
		Form: &acp.UnstableCreateElicitationForm{
			Message: "Need a value",
			Mode:    "form",
			RequestedSchema: acp.UnstableElicitationSchema{
				Type: acp.UnstableElicitationSchemaTypeObject,
			},
			Meta: map[string]any{"source": "test"},
		},
	}, elicitationScope{SessionID: "session-1", TurnNonce: "turn-1", ToolCallID: "tool-1"})
	if err != nil {
		t.Fatalf("UnstableCreateElicitation returned error: %v", err)
	}
	if elicitation.Accept == nil || elicitation.Accept.Content["ok"] != true {
		t.Fatalf("elicitation response = %#v", elicitation)
	}
	requestIDValue := acp.RequestIdStr("request-2")
	requestID := acp.RequestId{Str: &requestIDValue}
	urlElicitation, err := agentConn.CreateElicitation(ctx, acp.NewUnstableCreateElicitationRequestUrl(
		"elicitation-2", "https://example.test/open",
	), elicitationScope{SessionID: "session-1", TurnNonce: "turn-1", RequestID: &requestID})
	if err != nil {
		t.Fatalf("URL elicitation returned error: %v", err)
	}
	if urlElicitation.Accept == nil {
		t.Fatalf("URL elicitation response = %#v", urlElicitation)
	}
	if got := client.Elicitations(); len(got) != 2 || got[0].Form == nil || got[0].Form.Meta["source"] != "test" || got[1].Url == nil {
		t.Fatalf("client elicitations = %#v", got)
	}
	gotElicitations := client.Elicitations()
	formRoute := asType[map[string]any](t, gotElicitations[0].Form.Meta[routeMetaKey])
	if !reflect.DeepEqual(formRoute, map[string]any{
		routeVersionKey:    float64(routeVersion),
		jsonFieldSessionID: "session-1",
		routeTurnNonceKey:  "turn-1",
		routeToolCallIDKey: "tool-1",
	}) {
		t.Fatalf("decoded form elicitation route = %#v", formRoute)
	}
	urlRoute := asType[map[string]any](t, gotElicitations[1].Url.Meta[routeMetaKey])
	if !reflect.DeepEqual(urlRoute, map[string]any{
		routeVersionKey:    float64(routeVersion),
		jsonFieldSessionID: "session-1",
		routeTurnNonceKey:  "turn-1",
		routeRequestIDKey:  "request-2",
	}) {
		t.Fatalf("decoded URL elicitation route = %#v", urlRoute)
	}

	if err := agentConn.NotifyExtension(ctx, "_codex.test/event", map[string]any{"ok": true}); err != nil {
		t.Fatalf("NotifyExtension returned error: %v", err)
	}
	eventually(t, func() bool { return len(client.Extensions()) == 1 })
	if got := client.Extensions(); len(got) != 1 || got[0].method != "_codex.test/event" {
		t.Fatalf("client extensions = %#v", got)
	}
}

func TestLocalAgentConnectionClientCallBackpressure(t *testing.T) {
	agent := NewAgent(WithConcurrencyLimits(ConcurrencyLimits{MaxConcurrentClientCalls: 1}))
	conn := &localAgentConnection{agent: agent}
	agent.clientCalls <- struct{}{}

	if _, err := conn.RequestPermission(context.Background(), testPermissionRequest()); err == nil {
		t.Fatal("RequestPermission ignored client-call backpressure")
	}
	if err := conn.SessionUpdate(context.Background(), acp.SessionNotification{
		SessionId: "session-1",
		Update:    acp.UpdateAgentMessageText("hello"),
	}); err == nil {
		t.Fatal("SessionUpdate ignored client-call backpressure")
	}
	if err := conn.NotifyExtension(context.Background(), "_codex.test/event", nil); err == nil {
		t.Fatal("NotifyExtension ignored client-call backpressure")
	}
	if _, err := conn.CreateElicitation(context.Background(), acp.UnstableCreateElicitationRequest{
		Form: &acp.UnstableCreateElicitationForm{Message: "m", Mode: "form"},
	}, elicitationScope{}); err == nil {
		t.Fatal("CreateElicitation ignored client-call backpressure")
	}
}

func TestLocalAgentConnectionHelpers(t *testing.T) {
	agent := NewAgent()
	if _, err := (&localAgentConnection{agent: agent}).CreateElicitation(context.Background(), acp.UnstableCreateElicitationRequest{}, elicitationScope{}); err == nil {
		t.Fatal("CreateElicitation accepted empty request")
	}

	if _, err := scopedElicitationParams(acp.UnstableCreateElicitationRequest{}, elicitationScope{}); err == nil {
		t.Fatal("empty elicitation params succeeded")
	}
	raw, err := scopedElicitationParams(acp.UnstableCreateElicitationRequest{
		Url: &acp.UnstableCreateElicitationUrl{
			ElicitationId: "elicitation-1",
			Message:       "Open",
			Mode:          "url",
			Url:           "https://example.test",
			Meta:          map[string]any{"m": true},
		},
	}, elicitationScope{SessionID: "session-1", TurnNonce: "turn-1", ToolCallID: "tool-1"})
	if err != nil {
		t.Fatalf("url elicitation params returned error: %v", err)
	}
	got := mapFromRaw(raw)
	route := asType[map[string]any](t, asType[map[string]any](t, got[jsonFieldMeta])[routeMetaKey])
	if route[jsonFieldSessionID] != "session-1" || route[routeToolCallIDKey] != "tool-1" || got["url"] != "https://example.test" {
		t.Fatalf("scoped elicitation payload = %#v", got)
	}

	if err := (&localAgentConnection{}).NotifyExtension(context.Background(), "bad", nil); err == nil {
		t.Fatal("NotifyExtension accepted non-extension method")
	}
}

func TestLocalResponseAndNotificationHelpers(t *testing.T) {
	ctx := context.Background()
	agent := NewAgent()

	response := localResponse(func(*Agent, context.Context, acp.ListSessionsRequest) (acp.ListSessionsResponse, error) {
		return acp.ListSessionsResponse{Sessions: []acp.SessionInfo{{SessionId: "session-1"}}}, nil
	})
	if _, reqErr := response(ctx, agent, json.RawMessage(`{"cursor":`)); reqErr == nil {
		t.Fatal("localResponse accepted invalid JSON")
	}
	newSessionResponse := localResponse(func(*Agent, context.Context, acp.NewSessionRequest) (acp.NewSessionResponse, error) {
		return acp.NewSessionResponse{}, nil
	})
	if _, reqErr := newSessionResponse(ctx, agent, json.RawMessage(`{}`)); reqErr == nil {
		t.Fatal("localResponse accepted invalid NewSession params")
	}
	result, reqErr := response(ctx, agent, json.RawMessage(`{}`))
	if reqErr != nil {
		t.Fatalf("localResponse returned request error: %v", reqErr)
	}
	listResult, ok := result.(acp.ListSessionsResponse)
	if !ok {
		t.Fatalf("localResponse result type = %T", result)
	}
	if len(listResult.Sessions) != 1 {
		t.Fatalf("localResponse result = %#v", result)
	}
	responseErr := localResponse(func(*Agent, context.Context, acp.ListSessionsRequest) (acp.ListSessionsResponse, error) {
		return acp.ListSessionsResponse{}, acp.NewInvalidParams(map[string]any{"bad": true})
	})
	if _, reqErr := responseErr(ctx, agent, json.RawMessage(`{}`)); reqErr == nil || reqErr.Code != -32602 {
		t.Fatalf("localResponse error branch = %#v", reqErr)
	}

	notification := localNotification(func(*Agent, context.Context, acp.CancelNotification) error { return nil })
	if _, reqErr := notification(ctx, agent, json.RawMessage(`{"sessionId":`)); reqErr == nil {
		t.Fatal("localNotification accepted invalid JSON")
	}
	if result, reqErr := notification(ctx, agent, json.RawMessage(`{"sessionId":"session-1"}`)); reqErr != nil || result != nil {
		t.Fatalf("localNotification success result=%#v reqErr=%v", result, reqErr)
	}
	notificationErr := localNotification(func(*Agent, context.Context, acp.CancelNotification) error {
		return errors.New("cancel failed")
	})
	if _, reqErr := notificationErr(ctx, agent, json.RawMessage(`{"sessionId":"session-1"}`)); reqErr == nil || reqErr.Code != -32603 {
		t.Fatalf("localNotification error branch = %#v", reqErr)
	}
}

func testPermissionRequest() acp.RequestPermissionRequest {
	return acp.RequestPermissionRequest{
		SessionId: "session-1",
		ToolCall: acp.ToolCallUpdate{
			ToolCallId: "tool-1",
			Title:      acp.Ptr("Run"),
			Kind:       acp.Ptr(acp.ToolKindExecute),
			Status:     acp.Ptr(acp.ToolCallStatusPending),
			Content:    []acp.ToolCallContent{acp.ToolContent(acp.TextBlock("cmd"))},
		},
		Options: []acp.PermissionOption{
			{Kind: acp.PermissionOptionKindAllowOnce, Name: "Allow", OptionId: "allow"},
			{Kind: acp.PermissionOptionKindRejectOnce, Name: "Reject", OptionId: "reject"},
		},
	}
}

func eventually(t *testing.T, done func() bool) {
	t.Helper()

	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if done() {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
}
