package codexacp

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/coder/acp-go-sdk"
	"github.com/savid/acp-go-codex/internal/codex"
	"github.com/savid/acp-go-codex/internal/lifecycle"
	"github.com/stretchr/testify/require"
)

func TestEstablishmentRegistryBoundAndRetirement(t *testing.T) {
	hooks := newEstablishmentHooks(slog.Default())
	obligations := make([]*establishmentObligation, 0, establishmentHookLimit)
	for i := range establishmentHookLimit {
		obligation, err := hooks.reserve(strconv.Itoa(i + 1))
		require.NoError(t, err)
		obligations = append(obligations, obligation)
	}

	_, err := hooks.reserve("overflow")
	require.ErrorContains(t, err, "registry is full")

	hooks.cancel(obligations[0], errEstablishmentCancelled)
	replacement, err := hooks.reserve("replacement")
	require.NoError(t, err, "retiring an obligation must release exact registry capacity")
	hooks.cancel(replacement, errEstablishmentCancelled)
	for _, obligation := range obligations[1:] {
		hooks.cancel(obligation, errEstablishmentCancelled)
	}
}

func TestEstablishmentRequiresAnExactResponseFrame(t *testing.T) {
	hooks := newEstablishmentHooks(slog.Default())
	obligation, err := hooks.reserve("1")
	require.NoError(t, err)

	hooks.complete([]byte(`{"jsonrpc":"2.0","id":1,"method":"session/request_permission","params":{}}`), nil)
	hooks.complete([]byte(`{"jsonrpc":"2.0","id":"1","result":{}}`), nil)
	hooks.complete([]byte(`{"jsonrpc":"2.0","id":2,"result":{}}`), nil)
	hooks.complete([]byte(`{malformed`), nil)

	hooks.mu.Lock()
	require.Same(t, obligation, hooks.all["1"])
	hooks.mu.Unlock()

	hooks.complete([]byte(`{"jsonrpc":"2.0","id":1,"result":{}}`), nil)
	require.NoError(t, obligation.wait(t.Context()))
	hooks.mu.Lock()
	require.NotContains(t, hooks.all, "1")
	hooks.mu.Unlock()
}

func TestEstablishmentRejectsAmbiguousMatchingResponses(t *testing.T) {
	for _, frame := range []string{
		`{"jsonrpc":"2.0","id":1}`,
		`{"jsonrpc":"2.0","id":1,"result":{},"error":null}`,
	} {
		hooks := newEstablishmentHooks(slog.Default())
		obligation, err := hooks.reserve("1")
		require.NoError(t, err)

		hooks.complete([]byte(frame), nil)
		require.NoError(t, obligation.wait(t.Context()))
		hooks.mu.Lock()
		require.NotContains(t, hooks.all, "1")
		hooks.mu.Unlock()
	}
}

func TestEstablishmentNeverOpensForNonExactJSONRPCResponses(t *testing.T) {
	for _, frame := range []string{
		`{"id":1,"result":{}}`,
		`{"jsonrpc":"1.0","id":1,"result":{}}`,
		`{"jsonrpc":"2.0","id":1,"result":{},"extra":true}`,
		`{"jsonrpc":"2.0","id":1,"error":null}`,
		`{"jsonrpc":"2.0","id":true,"result":{}}`,
	} {
		t.Run(frame, func(t *testing.T) {
			agent := NewAgent()
			agent.lifecycle = lifecycle.Negotiated{Version: 1}
			s := newSession(agent, "session", t.TempDir(), nil, codex.Thread{ID: "thread"}, newSpyCodexClient(), sessionMeta{}, nil)
			hooks := newEstablishmentHooks(agent.log)
			obligation := armTestEstablishment(t, hooks, s, "1")

			hooks.complete([]byte(frame), nil)
			if strings.Contains(frame, `"id":true`) {
				hooks.mu.Lock()
				required := hooks.all["1"]
				hooks.mu.Unlock()
				require.Same(t, obligation, required)
				hooks.cancel(obligation, errEstablishmentCancelled)
			} else {
				require.NoError(t, obligation.wait(t.Context()))
				require.ErrorIs(t, s.establishmentErr, errEstablishmentResponseMalformed)
			}

			s.lifecycleMu.Lock()
			require.Nil(t, s.lifecycleStream)
			s.lifecycleMu.Unlock()
		})
	}
}

func TestEstablishmentReaderEnforcesFixedSecretSafeFrameBound(t *testing.T) {
	exact := strings.Repeat("x", establishmentFrameLimit-1) + "\n"
	read, err := io.ReadAll(newEstablishmentTagReader(strings.NewReader(exact)))
	require.NoError(t, err)
	require.Len(t, read, establishmentFrameLimit)

	secret := "establishment-secret-sentinel"
	overflow := strings.Repeat("x", establishmentFrameLimit) + secret + "\n"
	read, err = io.ReadAll(newEstablishmentTagReader(strings.NewReader(overflow)))
	require.ErrorIs(t, err, errEstablishmentFrameTooLarge)
	require.Empty(t, read)
	require.NotContains(t, err.Error(), secret)
}

func TestEstablishmentReaderBoundsPrivateHookExpansion(t *testing.T) {
	base := []byte(`{"jsonrpc":"2.0","id":7,"method":"session/resume","params":{"padding":""}}` + "\n")
	taggedBase, err := tagEstablishingRequest(base)
	require.NoError(t, err)
	padding := strings.Repeat("x", establishmentFrameLimit-len(taggedBase))
	exact := []byte(`{"jsonrpc":"2.0","id":7,"method":"session/resume","params":{"padding":"` + padding + `"}}` + "\n")

	read, err := io.ReadAll(newEstablishmentTagReader(bytes.NewReader(exact)))
	require.NoError(t, err)
	require.Len(t, read, establishmentFrameLimit)
	require.Equal(t, "7", establishmentHookID(json.RawMessage(extractTaggedParams(t, read))))

	tooLargeAfterHook := append([]byte(nil), exact...)
	insert := len(tooLargeAfterHook) - len(`"}}`+"\n")
	tooLargeAfterHook = append(tooLargeAfterHook[:insert], append([]byte("x"), tooLargeAfterHook[insert:]...)...)
	require.LessOrEqual(t, len(tooLargeAfterHook), establishmentFrameLimit)
	read, err = io.ReadAll(newEstablishmentTagReader(bytes.NewReader(tooLargeAfterHook)))
	require.ErrorIs(t, err, errEstablishmentFrameTooLarge)
	require.Empty(t, read)
}

type establishmentPublicationClient struct {
	*spyCodexClient
	modelsStarted chan struct{}
	releaseModels chan struct{}
	once          sync.Once
}

func (c *establishmentPublicationClient) ModelList(ctx context.Context) ([]codex.Model, error) {
	c.once.Do(func() { close(c.modelsStarted) })
	select {
	case <-c.releaseModels:
		return c.spyCodexClient.ModelList(ctx)
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

type gatedEstablishmentWriter struct {
	started chan struct{}
	release chan struct{}
	once    sync.Once
	mu      sync.Mutex
	frames  [][]byte
	err     error
	short   bool
}

type lockedLogBuffer struct {
	mu     sync.Mutex
	buffer bytes.Buffer
}

func (b *lockedLogBuffer) Write(data []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()

	return b.buffer.Write(data)
}

func (b *lockedLogBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()

	return b.buffer.String()
}

func (w *gatedEstablishmentWriter) Write(data []byte) (int, error) {
	w.once.Do(func() { close(w.started) })
	<-w.release
	w.mu.Lock()
	w.frames = append(w.frames, append([]byte(nil), data...))
	w.mu.Unlock()
	if w.err != nil {
		return 0, w.err
	}
	if w.short {
		return len(data) - 1, nil
	}

	return len(data), nil
}

func armTestEstablishment(
	t *testing.T,
	hooks *establishmentHooks,
	s *session,
	responseID string,
) *establishmentObligation {
	t.Helper()

	obligation, err := hooks.reserve(responseID)
	require.NoError(t, err)
	require.NoError(t, s.armLifecycleEstablishment(obligation))

	return obligation
}

func TestLifecycleEstablishmentWaitsForExactResponseOnEveryPath(t *testing.T) {
	for _, method := range []string{
		acp.AgentMethodSessionNew,
		acp.AgentMethodSessionLoad,
		acp.AgentMethodSessionResume,
		ForkSessionMethod,
	} {
		t.Run(method, func(t *testing.T) {
			agent := NewAgent()
			agent.lifecycle = lifecycle.Negotiated{Version: 1, ActivityKinds: []lifecycle.ActivityKind{}}
			recorder := newRecordingAgentClient()
			agent.setAgentClient(recorder)
			client := newSpyCodexClient()
			s := newSession(agent, "session", t.TempDir(), nil, codex.Thread{ID: "thread"}, client, sessionMeta{}, nil)
			hooks := newEstablishmentHooks(agent.log)
			obligation := armTestEstablishment(t, hooks, s, "1")
			require.NoError(t, s.attachNativeEvents())
			agent.sessions[s.id] = s
			t.Cleanup(s.fenceSession)

			established := obligation.done

			require.NoError(t, client.publishTurn("thread", "native-before-prompt", []codex.Event{
				{Kind: codex.EventAgentMessageDelta, ItemID: "message", Text: "background"},
				{Kind: codex.EventCompleted, StopReason: codex.StopReasonEndTurn},
			}))
			require.NoError(t, s.drainNativeEvents(t.Context()))
			s.lifecycleMu.Lock()
			require.Len(t, s.preOpenEvents, 2)
			s.lifecycleMu.Unlock()
			require.Empty(t, recorder.updates)

			wire := &gatedEstablishmentWriter{started: make(chan struct{}), release: make(chan struct{})}
			writeDone := make(chan error, 1)
			go func() {
				_, err := hooks.wrap(wire).Write([]byte(`{"jsonrpc":"2.0","id":1,"result":{}}` + "\n"))
				writeDone <- err
			}()
			<-wire.started
			require.Empty(t, recorder.updates, "a blocked response cannot be overtaken")
			close(wire.release)
			require.NoError(t, <-writeDone)
			select {
			case <-established:
			case <-time.After(time.Second):
				t.Fatal("post-response lifecycle establishment did not finish")
			}

			require.Len(t, recorder.updates, 4)
			require.Equal(t, "lifecycle_snapshot", lifecycleEventType(recorder.updates[0]))
			require.Equal(t, "state_update", lifecycleEventType(recorder.updates[1]))
			require.Equal(t, "background", recorder.updates[2].Update.AgentMessageChunk.Content.Text.Text)
			require.Equal(t, "state_update", lifecycleEventType(recorder.updates[3]))
			require.Nil(t, s.agentIncarnation)
		})
	}
}

func TestLifecycleEstablishmentWriteFailureContainsTheExactSession(t *testing.T) {
	var logOutput lockedLogBuffer
	agent := NewAgent(WithLogger(slog.New(slog.NewTextHandler(&logOutput, nil))))
	agent.lifecycle = lifecycle.Negotiated{Version: 1, ActivityKinds: []lifecycle.ActivityKind{}}
	agent.setAgentClient(newRecordingAgentClient())
	client := newSpyCodexClient()
	s := newSession(agent, "session", t.TempDir(), nil, codex.Thread{ID: "thread"}, client, sessionMeta{}, nil)
	hooks := newEstablishmentHooks(agent.log)
	obligation := armTestEstablishment(t, hooks, s, "1")
	require.NoError(t, s.attachNativeEvents())
	agent.sessions[s.id] = s
	established := obligation.done

	wire := &gatedEstablishmentWriter{
		started: make(chan struct{}), release: make(chan struct{}), err: io.ErrClosedPipe,
	}
	close(wire.release)
	_, err := hooks.wrap(wire).Write([]byte(`{"jsonrpc":"2.0","id":1,"result":{}}` + "\n"))
	require.ErrorIs(t, err, io.ErrClosedPipe)
	select {
	case <-established:
	case <-time.After(time.Second):
		t.Fatal("failed response did not finish containment")
	}

	require.ErrorIs(t, s.establishmentErr, io.ErrClosedPipe)
	s.lifecycleMu.Lock()
	require.ErrorIs(t, s.lifecycleFailure, io.ErrClosedPipe)
	s.lifecycleMu.Unlock()
	client.mu.Lock()
	_, subscribed := client.feeds["thread"]
	client.mu.Unlock()
	require.False(t, subscribed, "failed establishment retained its native event source")
	require.NotContains(t, strings.ToLower(logOutput.String()), strings.ToLower(io.ErrClosedPipe.Error()))
}

func TestLifecycleEstablishmentSnapshotFailureContainsTheExactSession(t *testing.T) {
	deliveryErr := errors.New("snapshot delivery failed")
	agent := NewAgent()
	agent.lifecycle = lifecycle.Negotiated{Version: 1, ActivityKinds: []lifecycle.ActivityKind{}}
	agent.setAgentClient(&failingLifecycleClient{
		recordingAgentClient: newRecordingAgentClient(),
		err:                  deliveryErr,
	})
	client := newSpyCodexClient()
	s := newSession(agent, "session", t.TempDir(), nil, codex.Thread{ID: "thread"}, client, sessionMeta{}, nil)
	hooks := newEstablishmentHooks(agent.log)
	obligation := armTestEstablishment(t, hooks, s, "1")
	require.NoError(t, s.attachNativeEvents())
	agent.sessions[s.id] = s
	established := obligation.done

	wire := &gatedEstablishmentWriter{started: make(chan struct{}), release: make(chan struct{})}
	close(wire.release)
	_, err := hooks.wrap(wire).Write([]byte(`{"jsonrpc":"2.0","id":1,"result":{}}` + "\n"))
	require.NoError(t, err)
	promptDone := make(chan error, 1)
	go func() {
		request := TextPromptRequest(s.id, "nonce", "hello")
		request.Meta[lifecycle.MetaKey] = map[string]any{
			"version": 1,
			"submission": map[string]any{
				"submissionId": "submission",
				"clientNonce":  "nonce",
			},
		}
		_, promptErr := s.Prompt(t.Context(), request)
		promptDone <- promptErr
	}()
	select {
	case <-established:
	case <-time.After(time.Second):
		t.Fatal("snapshot failure did not finish containment")
	}
	select {
	case promptErr := <-promptDone:
		require.ErrorIs(t, promptErr, deliveryErr)
	case <-time.After(time.Second):
		t.Fatal("prompt deadlocked behind failed establishment containment")
	}

	require.ErrorIs(t, s.establishmentErr, deliveryErr)
	s.lifecycleMu.Lock()
	require.ErrorIs(t, s.lifecycleFailure, deliveryErr)
	require.True(t, s.lifecycleStream.Fenced())
	s.lifecycleMu.Unlock()
	client.mu.Lock()
	_, subscribed := client.feeds["thread"]
	client.mu.Unlock()
	require.False(t, subscribed, "snapshot failure retained its native event source")
}

func TestLifecycleEstablishmentBlocksPreResponseServerRequest(t *testing.T) {
	agent := NewAgent()
	agent.lifecycle = lifecycle.Negotiated{Version: 1, ActivityKinds: []lifecycle.ActivityKind{}}
	recorder := newRecordingAgentClient()
	agent.setAgentClient(recorder)
	client := newSpyCodexClient()
	s := newSession(agent, "session", t.TempDir(), nil, codex.Thread{ID: "thread"}, client, sessionMeta{}, nil)
	hooks := newEstablishmentHooks(agent.log)
	_ = armTestEstablishment(t, hooks, s, "1")
	require.NoError(t, s.attachNativeEvents())
	t.Cleanup(s.fenceSession)
	agent.sessions[s.id] = s

	type claimResult struct {
		incarnation *promptIncarnation
		claimed     bool
		err         error
	}
	probeCtx, cancelProbe := context.WithCancel(t.Context())
	probeEntered := make(chan struct{})
	claimed := make(chan claimResult, 1)
	go func() {
		close(probeEntered)
		incarnation, lifecycleClaimed, err := s.claimLifecycleTurn(probeCtx, "native-server-request")
		claimed <- claimResult{incarnation: incarnation, claimed: lifecycleClaimed, err: err}
	}()
	<-probeEntered
	cancelProbe()
	probe := <-claimed
	require.ErrorIs(t, probe.err, context.Canceled)
	require.True(t, probe.claimed)
	require.Nil(t, probe.incarnation)
	require.Empty(t, recorder.updates)

	wire := &gatedEstablishmentWriter{started: make(chan struct{}), release: make(chan struct{})}
	close(wire.release)
	_, err := hooks.wrap(wire).Write([]byte(`{"jsonrpc":"2.0","id":1,"result":{}}` + "\n"))
	require.NoError(t, err)
	go func() {
		incarnation, lifecycleClaimed, err := s.claimLifecycleTurn(t.Context(), "native-server-request")
		claimed <- claimResult{incarnation: incarnation, claimed: lifecycleClaimed, err: err}
	}()
	select {
	case result := <-claimed:
		require.NoError(t, result.err)
		require.True(t, result.claimed)
		require.NotNil(t, result.incarnation)
		require.Equal(t, "native-server-request", result.incarnation.nativeTurnID)
	case <-time.After(time.Second):
		t.Fatal("server request did not resume after establishment")
	}
	require.NotEmpty(t, recorder.updates)
	require.Equal(t, "lifecycle_snapshot", lifecycleEventType(recorder.updates[0]))
}

func TestLifecycleEstablishmentIsArmedBeforeSessionPublication(t *testing.T) {
	client := &establishmentPublicationClient{
		spyCodexClient: newSpyCodexClient(), modelsStarted: make(chan struct{}), releaseModels: make(chan struct{}),
	}
	agent := NewAgent(withClientFactory(func(context.Context, codex.Options) (codex.Client, error) {
		return client, nil
	}))
	agent.lifecycle = lifecycle.Negotiated{Version: 1, ActivityKinds: []lifecycle.ActivityKind{}}
	recorder := newRecordingAgentClient()
	agent.setAgentClient(recorder)
	hooks := newEstablishmentHooks(agent.log)
	conn := &localAgentConnection{agent: agent, establishment: hooks}
	conn.initialized.Store(true)

	type handlerResult struct {
		value any
		err   *acp.RequestError
	}
	handled := make(chan handlerResult, 1)
	params := json.RawMessage(`{"cwd":` + strconv.Quote(t.TempDir()) + `,"mcpServers":[],"` + establishmentHookParam + `":"77"}`)
	go func() {
		value, reqErr := conn.handle(t.Context(), acp.AgentMethodSessionNew, params)
		handled <- handlerResult{value: value, err: reqErr}
	}()
	select {
	case <-client.modelsStarted:
	case result := <-handled:
		t.Fatalf("session handler returned before publication gate: value=%#v error=%v", result.value, result.err)
	case <-time.After(time.Second):
		t.Fatal("session handler did not reach publication gate")
	}

	client.mu.Lock()
	threadID := client.thread.ID
	client.mu.Unlock()
	s := agent.sessionByCodexThread(threadID)
	require.NotNil(t, s)
	t.Cleanup(s.fenceSession)
	s.lifecycleMu.Lock()
	require.NotNil(t, s.establishment)
	require.Equal(t, "77", s.establishment.responseID)
	s.lifecycleMu.Unlock()

	type claimResult struct {
		incarnation *promptIncarnation
		claimed     bool
		err         error
	}
	probeClaim := func() {
		probeCtx, cancelProbe := context.WithCancel(t.Context())
		entered := make(chan struct{})
		claimed := make(chan claimResult, 1)
		go func() {
			close(entered)
			in, owned, err := s.claimLifecycleTurn(probeCtx, "native-publication-race")
			claimed <- claimResult{incarnation: in, claimed: owned, err: err}
		}()
		<-entered
		cancelProbe()
		result := <-claimed
		require.ErrorIs(t, result.err, context.Canceled)
		require.True(t, result.claimed)
		require.Nil(t, result.incarnation)
	}
	probeClaim()

	close(client.releaseModels)
	result := <-handled
	require.Nil(t, result.err)
	require.IsType(t, acp.NewSessionResponse{}, result.value)
	probeClaim()

	wire := &gatedEstablishmentWriter{started: make(chan struct{}), release: make(chan struct{})}
	close(wire.release)
	_, err := hooks.wrap(wire).Write([]byte(`{"jsonrpc":"2.0","id":77,"result":{}}` + "\n"))
	require.NoError(t, err)
	claimed := make(chan claimResult, 1)
	go func() {
		in, owned, err := s.claimLifecycleTurn(t.Context(), "native-publication-race")
		claimed <- claimResult{incarnation: in, claimed: owned, err: err}
	}()
	select {
	case result := <-claimed:
		require.NoError(t, result.err)
		require.True(t, result.claimed)
		require.NotNil(t, result.incarnation)
	case <-time.After(time.Second):
		t.Fatal("server request did not resume after exact response write")
	}
}

func TestEstablishmentTaggingAndShortWrite(t *testing.T) {
	for _, method := range []string{
		acp.AgentMethodSessionNew,
		acp.AgentMethodSessionLoad,
		acp.AgentMethodSessionResume,
		ForkSessionMethod,
	} {
		tagged, err := tagEstablishingRequest([]byte(`{"jsonrpc":"2.0","id":7,"method":"` + method + `","params":{"sessionId":"s"}}` + "\n"))
		require.NoError(t, err)
		require.Equal(t, "7", establishmentHookID(json.RawMessage(extractTaggedParams(t, tagged))))
	}
	untagged, err := tagEstablishingRequest([]byte("not-json\n"))
	require.NoError(t, err)
	require.Equal(t, []byte("not-json\n"), untagged)
	for _, invalid := range []string{
		`{"id":7,"method":"session/resume","params":{}}` + "\n",
		`{"jsonrpc":"1.0","id":7,"method":"session/resume","params":{}}` + "\n",
		`{"jsonrpc":"2.0","id":null,"method":"session/resume","params":{}}` + "\n",
		`{"jsonrpc":"2.0","id":7,"method":"session/resume","params":{},"result":{}}` + "\n",
		`{"jsonrpc":"2.0","id":7,"method":"session/resume","params":[],"extra":true}` + "\n",
	} {
		invalidTagged, invalidErr := tagEstablishingRequest([]byte(invalid))
		require.NoError(t, invalidErr)
		require.Equal(t, []byte(invalid), invalidTagged)
	}

	hooks := newEstablishmentHooks(NewAgent().log)
	agent := NewAgent()
	agent.lifecycle = lifecycle.Negotiated{Version: 1}
	s := newSession(agent, "session", t.TempDir(), nil, codex.Thread{ID: "thread"}, newSpyCodexClient(), sessionMeta{}, nil)
	obligation := armTestEstablishment(t, hooks, s, "1")
	wire := &gatedEstablishmentWriter{started: make(chan struct{}), release: make(chan struct{}), short: true}
	close(wire.release)
	_, err = hooks.wrap(wire).Write([]byte(`{"jsonrpc":"2.0","id":1,"result":{}}` + "\n"))
	require.ErrorIs(t, err, io.ErrShortWrite)
	require.NoError(t, obligation.wait(t.Context()))
	require.ErrorIs(t, s.establishmentErr, io.ErrShortWrite)
}

func TestEstablishmentObligationRejectsInvalidOwnershipAndWaitsBoundedly(t *testing.T) {
	ctx := t.Context()
	require.Same(t, ctx, withEstablishmentObligation(ctx, nil))
	hooks := newEstablishmentHooks(slog.Default())
	_, err := hooks.reserve("")
	require.ErrorContains(t, err, "response id is required")
	obligation, err := hooks.reserve("one")
	require.NoError(t, err)
	_, err = hooks.reserve("one")
	require.ErrorContains(t, err, "already outstanding")
	require.Same(t, obligation, establishmentFromContext(withEstablishmentObligation(ctx, obligation)))

	require.Error(t, (*establishmentObligation)(nil).bind(&session{}))
	require.Error(t, obligation.bind(nil))
	first := &session{}
	require.NoError(t, obligation.bind(first))
	require.ErrorContains(t, obligation.bind(&session{}), "changed owner")

	cancelled, cancel := context.WithCancel(ctx)
	cancel()
	require.ErrorIs(t, obligation.wait(cancelled), context.Canceled)
	hooks.cancel(nil, errors.New("ignored"))
	hooks.cancel(obligation, errEstablishmentCancelled)
	require.NoError(t, obligation.wait(ctx))
	require.ErrorIs(t, obligation.bind(first), errEstablishmentCancelled)
	require.NoError(t, (*establishmentObligation)(nil).wait(ctx))
	(*establishmentObligation)(nil).finish(nil)

	unbound, err := hooks.reserve("unbound")
	require.NoError(t, err)
	unbound.finish(nil)
	require.NoError(t, unbound.wait(ctx))
}

func TestEstablishmentTagReaderPreservesDeferredEOFAndMalformedFrames(t *testing.T) {
	reader := newEstablishmentTagReader(strings.NewReader(`{"jsonrpc":"2.0","id":1,"method":"session/new","params":{}}`))
	var one [1]byte
	var output bytes.Buffer
	for {
		n, err := reader.Read(one[:])
		output.Write(one[:n])
		if err != nil {
			require.ErrorIs(t, err, io.EOF)

			break
		}
	}
	require.Contains(t, output.String(), establishmentHookParam)

	empty := newEstablishmentTagReader(strings.NewReader(""))
	n, err := empty.Read(one[:])
	require.Zero(t, n)
	require.ErrorIs(t, err, io.EOF)

	for _, malformed := range []string{
		`{"a":`,
		`{"a":1`,
		`{"a":1,"a":2}`,
		`{"a":1} true`,
	} {
		_, err = decodeExactJSONObject([]byte(malformed))
		require.ErrorIs(t, err, errEstablishmentResponseMalformed, malformed)
	}
}

func TestEstablishmentJSONRPCValidatorsRejectNonCanonicalValues(t *testing.T) {
	for _, raw := range []string{``, `1 2`, `null`, `true`, `{}`} {
		require.False(t, validJSONRPCID(json.RawMessage(raw)), raw)
	}
	for _, raw := range []string{
		`null`,
		`{"code":1}`,
		`{"code":1,"message":"x","extra":true}`,
		`{"code":1,"message":"x","data":{},"extra":true}`,
		`{"extra":1,"message":"x"}`,
		`{"code":"one","message":"x"}`,
		`{"code":1.5,"message":"x"}`,
		`{"code":1,"message":7}`,
	} {
		require.False(t, validJSONRPCError(json.RawMessage(raw)), raw)
	}
	require.True(t, validJSONRPCError(json.RawMessage(`{"code":-32000,"message":"x","data":{}}`)))
}

func TestActiveRebindPublishesOnlyAfterExactSuccessfulResponse(t *testing.T) {
	for _, test := range []struct {
		name    string
		respond func(*testing.T, *establishmentHooks, *session)
		success bool
	}{
		{
			name: "success",
			respond: func(t *testing.T, hooks *establishmentHooks, _ *session) {
				t.Helper()
				_, err := hooks.wrap(io.Discard).Write([]byte(`{"jsonrpc":"2.0","id":1,"result":{}}` + "\n"))
				require.NoError(t, err)
			},
			success: true,
		},
		{
			name: "rpc failure",
			respond: func(t *testing.T, hooks *establishmentHooks, _ *session) {
				t.Helper()
				_, err := hooks.wrap(io.Discard).Write([]byte(`{"jsonrpc":"2.0","id":1,"error":{"code":-32000,"message":"secret"}}` + "\n"))
				require.NoError(t, err)
			},
		},
		{
			name: "malformed frame",
			respond: func(t *testing.T, hooks *establishmentHooks, s *session) {
				t.Helper()
				_, err := hooks.wrap(io.Discard).Write([]byte("{malformed\n"))
				require.NoError(t, err)
				require.NoError(t, s.Close(t.Context()))
			},
		},
		{
			name: "wrong response id",
			respond: func(t *testing.T, hooks *establishmentHooks, s *session) {
				t.Helper()
				_, err := hooks.wrap(io.Discard).Write([]byte(`{"jsonrpc":"2.0","id":2,"result":{}}` + "\n"))
				require.NoError(t, err)
				require.NoError(t, s.Close(t.Context()))
			},
		},
		{
			name: "cancelled",
			respond: func(t *testing.T, _ *establishmentHooks, s *session) {
				t.Helper()
				require.NoError(t, s.Close(t.Context()))
			},
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			agent := NewAgent()
			agent.lifecycle = lifecycle.Negotiated{Version: 1, ActivityKinds: []lifecycle.ActivityKind{}}
			recorder := newRecordingAgentClient()
			agent.setAgentClient(recorder)
			client := newSpyCodexClient()
			s := newSession(agent, "session", t.TempDir(), nil, codex.Thread{ID: "thread"}, client, sessionMeta{}, nil)
			agent.sessions[s.id] = s
			agent.runtimeClient = client
			require.NoError(t, s.openLifecycleStream(t.Context(), agent.lifecycle))
			require.NoError(t, s.attachNativeEvents())
			base := len(recorder.updates)

			hooks := newEstablishmentHooks(agent.log)
			obligation := armTestEstablishment(t, hooks, s, "1")
			require.NoError(t, s.routeNativeEvent(codex.Event{
				Kind: codex.EventAgentMessageDelta, Scope: codex.EventScopeThread,
				ThreadID: "thread", TurnID: "racing-turn", ItemID: "message", Text: "staged-secret-marker",
			}))
			require.NoError(t, s.routeNativeEvent(codex.Event{
				Kind: codex.EventCompleted, Scope: codex.EventScopeThread,
				ThreadID: "thread", TurnID: "racing-turn", StopReason: codex.StopReasonEndTurn,
			}))
			require.Len(t, recorder.updates, base)

			test.respond(t, hooks, s)
			require.NoError(t, obligation.wait(t.Context()))
			if test.success {
				require.Greater(t, len(recorder.updates), base)
				require.Equal(t, "lifecycle_snapshot", lifecycleEventType(recorder.updates[base]))
				require.Equal(t, 1, countAgentText(recorder.updates[base:], "staged-secret-marker"))
				s.fenceSession()

				return
			}

			require.Equal(t, 0, countAgentText(recorder.updates[base:], "staged-secret-marker"))
			s.lifecycleMu.Lock()
			require.True(t, s.lifecycleStream.Fenced())
			s.lifecycleMu.Unlock()
		})
	}
}

func TestRuntimeReadyCanaryPartitionsRacingAutonomousTurn(t *testing.T) {
	agent := NewAgent()
	agent.lifecycle = lifecycle.Negotiated{Version: 1, ActivityKinds: []lifecycle.ActivityKind{}}
	recorder := newRecordingAgentClient()
	agent.setAgentClient(recorder)
	client := newSpyCodexClient()
	s := newSession(agent, "session", t.TempDir(), nil, codex.Thread{ID: "thread"}, client, sessionMeta{}, nil)
	require.NoError(t, s.openLifecycleStream(t.Context(), agent.lifecycle))
	require.NoError(t, s.attachNativeEvents())
	base := len(recorder.updates)

	hooks := newEstablishmentHooks(agent.log)
	obligation := armTestEstablishment(t, hooks, s, "1")
	canary, err := s.beginNativeCanary()
	require.NoError(t, err)
	require.NoError(t, s.routeNativeEvent(codex.Event{
		Kind: codex.EventToolDelta, Scope: codex.EventScopeThread,
		ThreadID: "thread", TurnID: "canary-turn", ItemID: "canary", Text: "canary",
	}))
	require.NoError(t, s.routeNativeEvent(codex.Event{
		Kind: codex.EventAgentMessageDelta, Scope: codex.EventScopeThread,
		ThreadID: "thread", TurnID: "agent-turn", ItemID: "agent", Text: "autonomous",
	}))
	require.NoError(t, s.routeNativeEvent(codex.Event{
		Kind: codex.EventCompleted, Scope: codex.EventScopeThread,
		ThreadID: "thread", TurnID: "agent-turn", StopReason: codex.StopReasonEndTurn,
	}))
	require.NoError(t, s.bindNativeCanary(canary, "canary-turn"))
	canaryEvent, open := <-canary.events
	require.True(t, open)
	require.Equal(t, "canary-turn", canaryEvent.TurnID)
	s.endNativeCanary(canary)

	_, err = hooks.wrap(io.Discard).Write([]byte(`{"jsonrpc":"2.0","id":1,"result":{}}` + "\n"))
	require.NoError(t, err)
	require.NoError(t, obligation.wait(t.Context()))
	require.Equal(t, "lifecycle_snapshot", lifecycleEventType(recorder.updates[base]))
	require.Equal(t, 1, countAgentText(recorder.updates[base:], "autonomous"))
	require.Equal(t, 0, countAgentText(recorder.updates[base:], "canary"))
	s.fenceSession()
}

func TestSameSessionDualResumeResponseOrders(t *testing.T) {
	for _, secondBeforeFirst := range []bool{false, true} {
		name := "first response first"
		if secondBeforeFirst {
			name = "second response first"
		}
		t.Run(name, func(t *testing.T) {
			agent := NewAgent()
			agent.lifecycle = lifecycle.Negotiated{Version: 1, ActivityKinds: []lifecycle.ActivityKind{}}
			agent.setAgentClient(newRecordingAgentClient())
			client := newSpyCodexClient()
			cwd := t.TempDir()
			request := ResumeSessionRequest("session", cwd)
			s := newSession(agent, "session", cwd, nil, codex.Thread{ID: "thread"}, client, sessionMeta{}, nil)
			s.fingerprint = codexSessionStartFingerprint(codexSessionStart{
				Cwd: cwd, ResumeID: "session", Meta: sessionMeta{},
			})
			agent.sessions[s.id] = s
			require.NoError(t, s.openLifecycleStream(t.Context(), agent.lifecycle))

			hooks := newEstablishmentHooks(agent.log)
			conn := &localAgentConnection{agent: agent, establishment: hooks}
			conn.initialized.Store(true)
			params := func(id string) json.RawMessage {
				payload, err := json.Marshal(request)
				require.NoError(t, err)
				var members map[string]any
				require.NoError(t, json.Unmarshal(payload, &members))
				members[establishmentHookParam] = id
				payload, err = json.Marshal(members)
				require.NoError(t, err)

				return payload
			}

			first, firstErr := conn.handle(t.Context(), acp.AgentMethodSessionResume, params("1"))
			require.Nil(t, firstErr)
			require.IsType(t, acp.ResumeSessionResponse{}, first)
			s.lifecycleMu.Lock()
			firstObligation := s.establishment
			s.lifecycleMu.Unlock()
			require.NotNil(t, firstObligation)

			if secondBeforeFirst {
				_, secondErr := conn.handle(t.Context(), acp.AgentMethodSessionResume, params("2"))
				require.NotNil(t, secondErr)
				_, err := hooks.wrap(io.Discard).Write([]byte(`{"jsonrpc":"2.0","id":2,"error":{"code":-32600,"message":"busy"}}` + "\n"))
				require.NoError(t, err)
				s.lifecycleMu.Lock()
				require.Same(t, firstObligation, s.establishment)
				s.lifecycleMu.Unlock()
			}

			_, err := hooks.wrap(io.Discard).Write([]byte(`{"jsonrpc":"2.0","id":1,"result":{}}` + "\n"))
			require.NoError(t, err)
			require.NoError(t, firstObligation.wait(t.Context()))

			if !secondBeforeFirst {
				second, secondErr := conn.handle(t.Context(), acp.AgentMethodSessionResume, params("2"))
				require.Nil(t, secondErr)
				require.IsType(t, acp.ResumeSessionResponse{}, second)
				s.lifecycleMu.Lock()
				secondObligation := s.establishment
				s.lifecycleMu.Unlock()
				require.NotNil(t, secondObligation)
				_, err = hooks.wrap(io.Discard).Write([]byte(`{"jsonrpc":"2.0","id":2,"result":{}}` + "\n"))
				require.NoError(t, err)
				require.NoError(t, secondObligation.wait(t.Context()))
			}

			s.fenceSession()
		})
	}
}

func countAgentText(updates []acp.SessionNotification, text string) int {
	count := 0
	for _, update := range updates {
		if chunk := update.Update.AgentMessageChunk; chunk != nil && chunk.Content.Text != nil && chunk.Content.Text.Text == text {
			count++
		}
	}

	return count
}

func extractTaggedParams(t *testing.T, frame []byte) []byte {
	t.Helper()
	var message struct {
		Params json.RawMessage `json:"params"`
	}
	require.NoError(t, json.Unmarshal(frame, &message))

	return message.Params
}

func lifecycleEventType(update acp.SessionNotification) string {
	envelope, _ := update.Meta[lifecycle.MetaKey].(map[string]any)
	event, _ := envelope["event"].(map[string]any)
	typeName, _ := event["type"].(string)

	return typeName
}
