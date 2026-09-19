package codexacp

import (
	"context"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/coder/acp-go-sdk"
	"github.com/savid/acp-go-codex/internal/codex"
	acpcore "github.com/savid/acp-go-core"
	"github.com/savid/acp-go-core/lifecycle"
	"github.com/savid/acp-go-core/wire"
	"github.com/stretchr/testify/require"
)

func TestPromptStreamsTextAndUsage(t *testing.T) {
	t.Parallel()

	h := newHarness(t)
	h.initialize()
	session := h.newSession()

	resp, err := h.prompt(session.SessionId, "HELLO", nil)
	require.NoError(t, err)
	require.Equal(t, acp.StopReasonEndTurn, resp.StopReason)
	require.NotNil(t, resp.Usage)
	require.Equal(t, 15, resp.Usage.TotalTokens)

	updates := h.rec.snapshot()
	require.Equal(t, "Hello world", agentText(updates))

	var usage *acp.SessionUsageUpdate

	for _, update := range updates {
		if update.Update.UsageUpdate != nil {
			usage = update.Update.UsageUpdate
		}
	}

	require.NotNil(t, usage)
	require.Equal(t, 1000, usage.Size)
	require.Equal(t, 15, usage.Used)
}

func TestTerminalFrameContributesOnlySuffix(t *testing.T) {
	t.Parallel()

	h := newHarness(t)
	h.initialize()
	session := h.newSession()

	_, err := h.prompt(session.SessionId, "SUFFIX", nil)
	require.NoError(t, err)
	require.Equal(t, "Hello", agentText(h.rec.snapshot()))

	_, err = h.prompt(session.SessionId, "THINK", nil)
	require.NoError(t, err)

	var thoughts strings.Builder

	for _, update := range h.rec.snapshot() {
		if chunk := update.Update.AgentThoughtChunk; chunk != nil && chunk.Content.Text != nil {
			thoughts.WriteString(chunk.Content.Text.Text)
		}
	}

	require.Equal(t, "hmm", thoughts.String())
	require.Equal(t, "Hellook", agentText(h.rec.snapshot()))
}

func TestToolPermissionAllowAndDeny(t *testing.T) {
	t.Parallel()

	h := newHarness(t, WithSessionStore(nil))
	h.initialize(withLifecycle())
	session := h.newSession()

	_, err := h.prompt(session.SessionId, "TOOL", promptMeta(1))
	require.NoError(t, err)

	h.rec.mu.Lock()
	require.Len(t, h.rec.permissions, 1)
	permission := h.rec.permissions[0]
	h.rec.mu.Unlock()

	require.Equal(t, acp.ToolCallId("call-1"), permission.ToolCall.ToolCallId)
	require.Equal(t, "ls", *permission.ToolCall.Title)
	require.Len(t, permission.Options, 4)
	require.Contains(t, permission.Meta, wire.LifecycleKey)

	var statuses []acp.ToolCallStatus

	for _, update := range h.rec.snapshot() {
		switch {
		case update.Update.ToolCall != nil:
			statuses = append(statuses, update.Update.ToolCall.Status)
		case update.Update.ToolCallUpdate != nil && update.Update.ToolCallUpdate.Status != nil:
			statuses = append(statuses, *update.Update.ToolCallUpdate.Status)
		}
	}

	require.Equal(t, []acp.ToolCallStatus{acp.ToolCallStatusPending, acp.ToolCallStatusInProgress, acp.ToolCallStatusCompleted}, statuses)

	types := eventTypes(lifecycleEvents(h.rec.snapshot()))
	require.Contains(t, types, "action_update:pending")
	require.Contains(t, types, "state_update:requires_action")
	require.Contains(t, types, "action_update:accepted")

	h.rec.mu.Lock()
	h.rec.answer = func(acp.RequestPermissionRequest) acp.RequestPermissionResponse {
		return acp.RequestPermissionResponse{Outcome: acp.NewRequestPermissionOutcomeSelected("decline")}
	}
	h.rec.mu.Unlock()

	_, err = h.prompt(session.SessionId, "TOOL", promptMeta(2))
	require.NoError(t, err)

	failed := false

	for _, update := range h.rec.snapshot() {
		if update.Update.ToolCallUpdate != nil && update.Update.ToolCallUpdate.Status != nil && *update.Update.ToolCallUpdate.Status == acp.ToolCallStatusFailed {
			failed = true
		}
	}

	require.True(t, failed)
	require.Contains(t, eventTypes(lifecycleEvents(h.rec.snapshot())), "action_update:declined")
}

func TestToolImageOutput(t *testing.T) {
	t.Parallel()

	h := newHarness(t)
	h.initialize()
	session := h.newSession()

	_, err := h.prompt(session.SessionId, "TOOLIMAGE", nil)
	require.NoError(t, err)

	images := 0

	for _, update := range h.rec.snapshot() {
		if update.Update.ToolCallUpdate == nil {
			continue
		}

		for _, content := range update.Update.ToolCallUpdate.Content {
			if content.Content != nil && content.Content.Content.Image != nil {
				images++

				require.Equal(t, "image/png", content.Content.Content.Image.MimeType)
			}
		}
	}

	require.Equal(t, 1, images)
}

func TestElicitationFormAndFallback(t *testing.T) {
	t.Parallel()

	t.Run("form supported", func(t *testing.T) {
		t.Parallel()

		h := newHarness(t)
		h.initialize(withLifecycle(), withFormElicitation())
		session := h.newSession()

		h.rec.mu.Lock()
		h.rec.elicit = func(request acp.UnstableCreateElicitationRequest) (acp.UnstableCreateElicitationResponse, error) {
			require.NotNil(t, request.Form)
			require.Contains(t, request.Form.Meta, wire.LifecycleKey)
			require.Contains(t, request.Form.RequestedSchema.Properties, "name")

			return acp.UnstableCreateElicitationResponse{Accept: &acp.UnstableCreateElicitationAccept{Action: "accept", Content: map[string]any{"name": "Ada"}}}, nil
		}
		h.rec.mu.Unlock()

		_, err := h.prompt(session.SessionId, "ASK", promptMeta(1))
		require.NoError(t, err)
		require.Equal(t, "hi Ada", agentText(h.rec.snapshot()))
		require.Contains(t, eventTypes(lifecycleEvents(h.rec.snapshot())), "action_update:accepted")
	})

	t.Run("form unsupported", func(t *testing.T) {
		t.Parallel()

		h := newHarness(t)
		h.initialize()
		session := h.newSession()

		_, err := h.prompt(session.SessionId, "ASK", nil)
		require.NoError(t, err)
		require.Equal(t, "declined", agentText(h.rec.snapshot()))
	})
}

func TestProviderFailureShape(t *testing.T) {
	t.Parallel()

	h := newHarness(t, WithSessionStore(nil))
	h.initialize(withLifecycle())
	session := h.newSession()

	_, err := h.prompt(session.SessionId, "ERROR", promptMeta(1))
	require.Equal(t, -32603, requestErrorCode(t, err))

	data := requestErrorData(t, err)
	require.Equal(t, "codex_turn_failed", data["error"])
	require.Equal(t, "provider", data["cause"])
	require.Equal(t, "boom", data["message"])
	require.EqualValues(t, 429, data["statusCode"])
	require.Equal(t, "rate_limited", data["providerCode"])

	events := lifecycleEvents(h.rec.snapshot())
	last := events[len(events)-1]
	require.Equal(t, "failed", last["outcome"])
	require.NotContains(t, last, "stopReason")

	_, err = h.prompt(session.SessionId, "REJECT", promptMeta(2))
	require.Equal(t, "provider", requestErrorData(t, err)["cause"])
	require.Equal(t, "no provider key", requestErrorData(t, err)["message"])

	// The session stays addressable after a failure.
	resp, err := h.prompt(session.SessionId, "HELLO", promptMeta(3))
	require.NoError(t, err)
	require.Equal(t, acp.StopReasonEndTurn, resp.StopReason)
}

func TestCancelAndTimeout(t *testing.T) {
	t.Parallel()

	t.Run("cancel", func(t *testing.T) {
		t.Parallel()

		h := newHarness(t)
		h.initialize(withLifecycle())
		session := h.newSession()

		type result struct {
			resp acp.PromptResponse
			err  error
		}

		done := make(chan result, 1)

		go func() {
			resp, err := h.prompt(session.SessionId, "SLOW", promptMeta(1))
			done <- result{resp: resp, err: err}
		}()

		h.rec.waitFor(t, func(updates []acp.SessionNotification) bool { return len(lifecycleEvents(updates)) >= 3 })
		require.NoError(t, h.conn.Cancel(h.ctx(), wire.CancelRequest(session.SessionId)))

		outcome := <-done
		require.NoError(t, outcome.err)
		require.Equal(t, acp.StopReasonCancelled, outcome.resp.StopReason)

		events := lifecycleEvents(h.rec.snapshot())
		require.Equal(t, "cancelled", events[len(events)-1]["outcome"])
	})
}

func TestProcessExitFailsTurnAndReplacesRuntime(t *testing.T) {
	t.Parallel()

	h := newHarness(t)
	h.initialize(withLifecycle())
	session := h.newSession()
	peer := h.newSession()

	_, err := h.prompt(session.SessionId, "DIE", promptMeta(1))
	require.Equal(t, -32603, requestErrorCode(t, err))

	data := requestErrorData(t, err)
	require.Equal(t, "codex_turn_failed", data["error"])
	require.Equal(t, "process_exit", data["cause"])
	require.Contains(t, data["message"], "status 3")
	require.Contains(t, data["message"], "fatal: dead")

	// The next explicit operation starts one replacement and rebinds both
	// threads through it, each on a fresh incarnation.
	resp, err := h.prompt(session.SessionId, "HELLO", promptMeta(2))
	require.NoError(t, err)
	require.Equal(t, acp.StopReasonEndTurn, resp.StopReason)

	resp, err = h.prompt(peer.SessionId, "ECHO peer", promptMeta(3))
	require.NoError(t, err)
	require.Equal(t, acp.StopReasonEndTurn, resp.StopReason)

	streams := map[string]struct{}{}

	for _, update := range h.rec.snapshot() {
		if envelope, ok := update.Meta[wire.LifecycleKey].(map[string]any); ok {
			if id, isString := envelope["streamId"].(string); isString {
				streams[id] = struct{}{}
			}
		}
	}

	require.Len(t, streams, 4)
}

func TestStructuredOutput(t *testing.T) {
	t.Parallel()

	h := newHarness(t)
	h.initialize()
	session := h.newSession(WithSessionOutputSchema(map[string]any{"type": "object"}))

	resp, err := h.prompt(session.SessionId, "JSON", nil)
	require.NoError(t, err)

	codexMeta, _ := resp.Meta["codex"].(map[string]any)
	require.Equal(t, map[string]any{"answer": float64(42)}, codexMeta["structuredOutput"])
}

func TestPlanUpdates(t *testing.T) {
	t.Parallel()

	h := newHarness(t)
	h.initialize()
	session := h.newSession()

	_, err := h.prompt(session.SessionId, "PLAN", nil)
	require.NoError(t, err)

	var plan *acp.SessionUpdatePlan

	for _, update := range h.rec.snapshot() {
		if update.Update.Plan != nil {
			plan = update.Update.Plan
		}
	}

	require.NotNil(t, plan)
	require.Len(t, plan.Entries, 2)
	require.Equal(t, acp.PlanEntryStatusCompleted, plan.Entries[0].Status)
	require.Equal(t, acp.PlanEntryStatusInProgress, plan.Entries[1].Status)
}

func TestImageInputGates(t *testing.T) {
	t.Parallel()

	h := newHarness(t, WithDefaultModel("vision"))
	h.initialize()
	session := h.newSession()

	resp, err := h.conn.Prompt(h.ctx(), wire.PromptRequest(session.SessionId, acp.TextBlock("ECHO"), acp.ImageBlock(tinyPNG, "image/png")))
	require.NoError(t, err)
	require.Equal(t, acp.StopReasonEndTurn, resp.StopReason)
	require.Contains(t, agentText(h.rec.snapshot()), "images:1")

	_, err = h.conn.Prompt(h.ctx(), wire.PromptRequest(session.SessionId, acp.ImageBlock("not base64!", "image/png")))
	require.Equal(t, "invalid_base64", requestErrorData(t, err)["error"])

	_, err = h.conn.SetSessionConfigOption(h.ctx(), SetModelRequest(session.SessionId, "text-only"))
	require.NoError(t, err)

	_, err = h.conn.Prompt(h.ctx(), wire.PromptRequest(session.SessionId, acp.ImageBlock(tinyPNG, "image/png")))
	require.Equal(t, "unsupported_by_model", requestErrorData(t, err)["error"])
}

func TestConfigOptions(t *testing.T) {
	t.Parallel()

	h := newHarness(t, WithDefaultModel("vision"), WithConfiguredModels([]string{"gpt-x"}))
	h.initialize()
	session := h.newSession(WithSessionCodexOptions(NewCodexOptions(WithCodexServiceTier("flex"))))

	ids := map[acp.SessionConfigId]*acp.SessionConfigOptionSelect{}
	for _, option := range session.ConfigOptions {
		ids[option.Select.Id] = option.Select
	}

	require.Contains(t, ids, configModel)
	require.Contains(t, ids, configMode)
	require.Contains(t, ids, configEffort)
	require.Contains(t, ids, configServiceTier)
	require.NotContains(t, ids, configPersonality)
	require.Equal(t, acp.SessionConfigValueId("vision"), ids[configModel].CurrentValue)
	require.Equal(t, acp.SessionConfigValueId("medium"), ids[configEffort].CurrentValue)

	values := *ids[configModel].Options.Ungrouped
	require.Equal(t, acp.SessionConfigValueId("vision"), values[0].Value)

	modelMeta, ok := values[0].Meta["codex"].(map[string]any)
	require.True(t, ok)
	require.EqualValues(t, 1000, modelMeta["contextWindow"])
	require.Equal(t, acp.SessionConfigValueId("gpt-x"), values[len(values)-1].Value)

	resp, err := h.conn.SetSessionConfigOption(h.ctx(), wire.SetConfigOptionRequest(session.SessionId, configMode, "plan"))
	require.NoError(t, err)

	for _, option := range resp.ConfigOptions {
		if option.Select.Id == configMode {
			require.Equal(t, acp.SessionConfigValueId("plan"), option.Select.CurrentValue)
		}
	}

	_, err = h.conn.SetSessionConfigOption(h.ctx(), wire.SetConfigOptionRequest(session.SessionId, configEffort, ""))
	require.Equal(t, "value", requestErrorData(t, err)["field"])

	// mode is the adapter's own menu, so a value outside it is refused rather
	// than advertised back as a current value the menu does not carry.
	for _, value := range []acp.SessionConfigValueId{"", "banana"} {
		_, err = h.conn.SetSessionConfigOption(h.ctx(), wire.SetConfigOptionRequest(session.SessionId, configMode, value))
		require.Equal(t, "value", requestErrorData(t, err)["field"])
	}

	_, err = h.conn.SetSessionConfigOption(h.ctx(), wire.SetConfigOptionRequest(session.SessionId, "bogus", "x"))
	require.Equal(t, "configId", requestErrorData(t, err)["field"])

	_, err = h.conn.SetSessionConfigOption(h.ctx(), acp.SetSessionConfigOptionRequest{Boolean: &acp.SetSessionConfigOptionBoolean{SessionId: session.SessionId, ConfigId: "x", Value: true}})
	require.Equal(t, "type", requestErrorData(t, err)["field"])
}

func TestRawEventsOptIn(t *testing.T) {
	t.Parallel()

	h := newHarness(t)
	h.initialize()
	session := h.newSession(WithSessionRawEvents(true))

	_, err := h.prompt(session.SessionId, "HELLO", nil)
	require.NoError(t, err)

	h.rec.waitFor(t, func([]acp.SessionNotification) bool {
		h.rec.mu.Lock()
		defer h.rec.mu.Unlock()

		return len(h.rec.raw) >= 3
	})

	h.rec.mu.Lock()
	raw := h.rec.raw
	h.rec.mu.Unlock()

	var first map[string]any
	require.NoError(t, json.Unmarshal(raw[0], &first))
	require.EqualValues(t, 1, first["sequence"])
	require.Equal(t, string(session.SessionId), first["sessionId"])

	event, _ := first["event"].(map[string]any)
	require.Equal(t, "turn/started", event["method"])
}

func TestSessionEnvironmentReachesThread(t *testing.T) {
	t.Parallel()

	dump := filepath.Join(t.TempDir(), "env")
	binDir := filepath.Join(t.TempDir(), "bin")

	h := newHarness(t, WithEnv(map[string]string{
		fakeCodexEnv: "1", fakeCodexEnvDump: dump, "ACP_GO_CODEX_TEST_AGENT": "agent", "ACP_GO_CODEX_INTERNAL_LEAK": "x",
	}))
	h.initialize()
	h.newSession(WithSessionCodexOptions(NewCodexOptions(
		WithCodexEnv(map[string]string{"ACP_GO_CODEX_TEST_SESSION": "session", "EMPTY": ""}),
		WithCodexExtraPathDirs(binDir),
	)))

	processEnv, err := os.ReadFile(dump)
	require.NoError(t, err)
	require.Contains(t, string(processEnv), "ACP_GO_CODEX_TEST_AGENT=agent")
	require.NotContains(t, string(processEnv), "ACP_GO_CODEX_INTERNAL_LEAK")
	require.Contains(t, string(processEnv), codexHomeEnv+"="+h.home)

	threadEnv, err := os.ReadFile(dump + ".thread")
	require.NoError(t, err)

	var set map[string]any
	require.NoError(t, json.Unmarshal(threadEnv, &set))
	require.Equal(t, "session", set["ACP_GO_CODEX_TEST_SESSION"])
	require.Equal(t, "", set["EMPTY"])

	path, _ := set["PATH"].(string)
	require.True(t, strings.HasPrefix(path, binDir+string(os.PathListSeparator)), path)
}

func TestAgentCloseStopsRuntime(t *testing.T) {
	t.Parallel()

	h := newHarness(t)
	h.initialize()
	session := h.newSession()

	_, err := h.conn.CloseSession(h.ctx(), acp.CloseSessionRequest{SessionId: session.SessionId})
	require.NoError(t, err)

	_, err = h.prompt(session.SessionId, "HELLO", nil)
	require.Equal(t, "unknown session", requestErrorData(t, err)["error"])
}

func TestLateDialogAfterCancellationIsRefused(t *testing.T) {
	t.Parallel()
	for _, state := range []string{"cancelled turn", "closed", "disconnected"} {
		t.Run(state, func(t *testing.T) {
			t.Parallel()
			s := &session{rt: &runtime{}, turn: &turn{}}
			switch state {
			case "cancelled turn":
				s.turn.cancelled = true
			case "closed":
				s.closing = true
			case "disconnected":
				s.rt = nil
			}
			ctx, cancel := context.WithCancelCause(t.Context())
			defer cancel(nil)
			release := s.registerDialog("late-native-request", cancel)
			require.ErrorIs(t, context.Cause(ctx), errDialogCancelled)
			release()
			s.callbacks.Wait()
		})
	}
}

func TestCloseJoinsFirstMirrorAndFencesOpening(t *testing.T) {
	t.Parallel()
	for _, agentClose := range []bool{false, true} {
		name := "close_session"
		if agentClose {
			name = "close_agent"
		}
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			store := &commitBarrier{SessionStore: acpcore.NewInMemorySessionStore(), entered: make(chan acpcore.SessionKey, 1), release: make(chan struct{})}
			release := sync.OnceFunc(func() { close(store.release) })
			t.Cleanup(release)
			store.block.Store(true)
			a := NewAgent(testOptions(t, WithSessionStore(store))...)
			t.Cleanup(func() { _ = a.Close() })
			rec := newRecorder()
			a.attach(rec, nil)
			request := acp.InitializeRequest{ProtocolVersion: acp.ProtocolVersionNumber}
			withLifecycle()(&request)
			_, err := a.Initialize(t.Context(), request)
			require.NoError(t, err)
			cwd := t.TempDir()
			created := make(chan error, 1)
			go func() {
				_, createErr := a.NewSession(t.Context(), wire.NewSessionRequest(cwd))
				created <- createErr
			}()
			var key acpcore.SessionKey
			select {
			case key = <-store.entered:
			case <-time.After(testTimeout):
				t.Fatal("creation did not reach its first mirror")
			}
			s, err := a.session(t.Context(), acp.SessionId(key.SessionID))
			require.NoError(t, err)
			s.mu.Lock()
			rt := s.rt
			s.mu.Unlock()
			closed := make(chan error, 1)
			go func() {
				if agentClose {
					closed <- a.Close()

					return
				}
				_, err := a.CloseSession(t.Context(), acp.CloseSessionRequest{SessionId: acp.SessionId(key.SessionID)})
				closed <- err
			}()
			require.Eventually(t, func() bool {
				s.mu.Lock()
				defer s.mu.Unlock()

				return s.closing
			}, testTimeout, time.Millisecond)
			select {
			case err := <-closed:
				t.Fatalf("close returned while the first mirror was blocked: %v", err)
			case <-time.After(100 * time.Millisecond):
			}
			release()
			select {
			case err := <-closed:
				require.NoError(t, err)
			case <-time.After(testTimeout):
				t.Fatal("close did not join creation")
			}
			select {
			case err := <-created:
				require.Error(t, err, "a closing session must refuse its opening publication")
			case <-time.After(testTimeout):
				t.Fatal("creation did not release its gate before cleanup")
			}
			require.False(t, s.lc.Active())
			before := len(rec.snapshot())
			require.Error(t, s.openStream(t.Context(), rt))
			require.Len(t, rec.snapshot(), before, "closed session published commands or a lifecycle snapshot")
			require.False(t, s.lc.Active())
		})
	}
}

func TestOpeningRejectsReplacedNativeGeneration(t *testing.T) {
	t.Parallel()
	a := NewAgent(testOptions(t)...)
	t.Cleanup(func() { _ = a.Close() })
	rec := newRecorder()
	a.attach(rec, nil)
	request := acp.InitializeRequest{ProtocolVersion: acp.ProtocolVersionNumber}
	withLifecycle()(&request)
	_, err := a.Initialize(t.Context(), request)
	require.NoError(t, err)
	created, err := a.NewSession(t.Context(), wire.NewSessionRequest(t.TempDir()))
	require.NoError(t, err)
	s, err := a.session(t.Context(), created.SessionId)
	require.NoError(t, err)
	s.mu.Lock()
	stale := s.rt
	s.mu.Unlock()
	transport, meta := prepareOpeningResponse(t)
	a.attach(rec, transport)
	require.NoError(t, a.scheduleOpen(transport.RequestContext(t.Context(), meta), s))
	a.stopGeneration(t.Context(), stale)
	require.False(t, s.lc.Active())
	fresh, err := s.ensureBound(t.Context())
	require.NoError(t, err)
	require.NotSame(t, stale, fresh)
	require.True(t, s.lc.Active())
	before := len(rec.snapshot())
	require.Error(t, s.openStream(t.Context(), stale))
	require.Len(t, rec.snapshot(), before, "stale deferred opening published on the replacement generation")
	require.True(t, s.lc.Active(), "stale opening fenced the replacement stream")
	finishOpeningResponse(t, transport, s.id)
	require.Len(t, rec.snapshot(), before, "stale hook published on the replacement generation")
	current, err := a.session(t.Context(), s.id)
	require.NoError(t, err)
	require.Same(t, s, current)
	require.True(t, s.lc.Active(), "stale hook closed the replacement stream")
}

// openingCallbackClient exercises a synchronous embedded callback into admission.
type openingCallbackClient struct {
	*recorder
	agent *Agent
}

func (c *openingCallbackClient) SessionUpdate(ctx context.Context, notification acp.SessionNotification) error {
	if err := c.agent.Cancel(ctx, acp.CancelNotification{SessionId: notification.SessionId}); err != nil {
		return err
	}

	return c.recorder.SessionUpdate(ctx, notification)
}

func TestOpeningAllowsSynchronousSessionCallback(t *testing.T) {
	t.Parallel()
	a := NewAgent(testOptions(t)...)
	t.Cleanup(func() { _ = a.Close() })
	rec := newRecorder()
	a.attach(&openingCallbackClient{recorder: rec, agent: a}, nil)
	request := acp.InitializeRequest{ProtocolVersion: acp.ProtocolVersionNumber}
	withLifecycle()(&request)
	_, err := a.Initialize(t.Context(), request)
	require.NoError(t, err)
	_, err = a.NewSession(t.Context(), wire.NewSessionRequest(t.TempDir()))
	require.NoError(t, err)
	require.NotEmpty(t, rec.snapshot())
}

func prepareOpeningResponse(t *testing.T) (*wire.Transport, map[string]any) {
	t.Helper()
	transport := wire.NewTransport(strings.NewReader("{\"jsonrpc\":\"2.0\",\"id\":1,\"method\":\"session/new\",\"params\":{}}\n"), io.Discard)
	t.Cleanup(transport.Close)
	transport.Start()
	inbound, err := io.ReadAll(transport.Reader())
	require.NoError(t, err)

	var frame struct {
		Params acp.NewSessionRequest `json:"params"`
	}

	require.NoError(t, json.Unmarshal(inbound, &frame))

	return transport, frame.Params.Meta
}

func finishOpeningResponse(t *testing.T, transport *wire.Transport, id acp.SessionId) {
	t.Helper()
	_, err := transport.Writer().Write([]byte("{\"jsonrpc\":\"2.0\",\"id\":1,\"result\":{}}\n"))
	require.NoError(t, err)
	ctx, cancel := context.WithTimeout(t.Context(), testTimeout)
	defer cancel()
	require.NoError(t, transport.AwaitSession(ctx, id))
}

func TestDeferredOpeningFailureDetachesSession(t *testing.T) {
	t.Parallel()
	a := NewAgent(testOptions(t, WithConcurrencyLimits(ConcurrencyLimits{MaxActiveSessions: 1}))...)
	t.Cleanup(func() { _ = a.Close() })
	request := acp.InitializeRequest{ProtocolVersion: acp.ProtocolVersionNumber}
	withLifecycle()(&request)
	_, err := a.Initialize(t.Context(), request)
	require.NoError(t, err)
	transport, meta := prepareOpeningResponse(t)
	a.attach(&firstOpenFailureClient{recorder: newRecorder()}, transport)
	newRequest := wire.NewSessionRequest(t.TempDir())
	newRequest.Meta = meta
	created, err := a.NewSession(t.Context(), newRequest)
	require.NoError(t, err)
	a.mu.Lock()
	s := a.sessions[created.SessionId]
	a.mu.Unlock()
	require.NotNil(t, s)
	finishOpeningResponse(t, transport, created.SessionId)
	require.False(t, s.lc.Active())
	a.mu.Lock()
	_, installed := a.sessions[created.SessionId]
	a.mu.Unlock()
	require.False(t, installed, "failed deferred publication retained the active slot")
	s.mu.Lock()
	closed := s.closing
	s.mu.Unlock()
	require.True(t, closed)
}

type cancellingBackgroundClient struct {
	*recorder
	agent          *Agent
	terminalOnly   bool
	terminalCalled bool
}

func (c *cancellingBackgroundClient) SessionUpdate(ctx context.Context, notification acp.SessionNotification) error {
	if c.terminalOnly {
		envelope, _ := notification.Meta[wire.LifecycleKey].(map[string]any)
		event, _ := envelope["event"].(map[string]any)
		if event["state"] != "idle" {
			return c.recorder.SessionUpdate(ctx, notification)
		}
		c.terminalCalled = true
	}
	done := make(chan error, 1)
	go func() { done <- c.agent.Cancel(ctx, wire.CancelRequest(notification.SessionId)) }()
	select {
	case err := <-done:
		return err
	case <-time.After(time.Second):
		return context.DeadlineExceeded
	}
}

func TestBackgroundPublicationAllowsCancelCallback(t *testing.T) {
	a := NewAgent(testOptions(t)...)
	t.Cleanup(func() { _ = a.Close() })
	rec := newRecorder()
	a.attach(rec, nil)
	request := acp.InitializeRequest{ProtocolVersion: acp.ProtocolVersionNumber}
	withLifecycle()(&request)
	_, err := a.Initialize(t.Context(), request)
	require.NoError(t, err)
	created, err := a.NewSession(t.Context(), wire.NewSessionRequest(t.TempDir()))
	require.NoError(t, err)
	s, err := a.session(t.Context(), created.SessionId)
	require.NoError(t, err)
	s.mu.Lock()
	rt := s.rt
	s.mu.Unlock()
	a.attach(&cancellingBackgroundClient{recorder: rec, agent: a}, nil)
	s.openAgentCycle(t.Context(), rt, codex.Event{Kind: codex.EventTurnStarted, ThreadID: s.nativeID, TurnID: "background"})
	a.attach(rec, nil)
	s.mu.Lock()
	c := s.cycle
	s.mu.Unlock()
	require.NotNil(t, c)
	require.NoError(t, s.cycleFailure(c))
	require.True(t, s.cycleCancelled(c))
	s.settleAgentCycle(t.Context(), c)
}

func TestCancelAgentOriginResolvesDialogsAndSettlesCancelled(t *testing.T) {
	a := NewAgent(testOptions(t)...)
	t.Cleanup(func() { _ = a.Close() })
	rec := newRecorder()
	a.attach(rec, nil)
	request := acp.InitializeRequest{ProtocolVersion: acp.ProtocolVersionNumber}
	withLifecycle()(&request)
	_, err := a.Initialize(t.Context(), request)
	require.NoError(t, err)
	created, err := a.NewSession(t.Context(), wire.NewSessionRequest(t.TempDir()))
	require.NoError(t, err)
	s, err := a.session(t.Context(), created.SessionId)
	require.NoError(t, err)
	s.mu.Lock()
	rt := s.rt
	s.mu.Unlock()
	s.openAgentCycle(t.Context(), rt, codex.Event{Kind: codex.EventTurnStarted, ThreadID: s.nativeID, TurnID: "background"})
	s.mu.Lock()
	c := s.cycle
	s.mu.Unlock()
	require.NotNil(t, c)
	dialogCtx, cancelDialog := context.WithCancelCause(t.Context())
	defer cancelDialog(nil)
	unregister := s.registerDialog("permission", cancelDialog)
	require.NoError(t, s.lc.ActionPending(t.Context(), c.Cycle, "permission", lifecycle.ActionPermission))
	require.NoError(t, a.Cancel(t.Context(), wire.CancelRequest(created.SessionId)))
	require.ErrorIs(t, context.Cause(dialogCtx), errDialogCancelled)
	unregister()
	lateCtx, cancelLate := context.WithCancelCause(t.Context())
	defer cancelLate(nil)
	release := s.registerDialog("late", cancelLate)
	release()
	require.ErrorIs(t, context.Cause(lateCtx), errDialogCancelled)
	require.NoError(t, a.Cancel(t.Context(), wire.CancelRequest(created.SessionId)))
	prompt := wire.TextPromptRequest(created.SessionId, "HELLO")
	prompt.Meta = promptMeta(1)
	_, err = a.Prompt(t.Context(), prompt)
	require.Equal(t, "backpressure", requestErrorData(t, err)["error"])
	s.settleAgentCycle(t.Context(), c)
	s.mu.Lock()
	active := s.cycle
	s.mu.Unlock()
	require.Nil(t, active)
	cancelled := false
	for _, notification := range rec.snapshot() {
		envelope, _ := notification.Meta[wire.LifecycleKey].(map[string]any)
		event, _ := envelope["event"].(map[string]any)
		if event["type"] == "state_update" && event["state"] == "idle" && event["outcome"] == "cancelled" {
			cancelled = true
		}
	}
	require.True(t, cancelled)
}

func TestTerminalPublicationCannotInterruptNextCycle(t *testing.T) {
	a := NewAgent(testOptions(t)...)
	t.Cleanup(func() { _ = a.Close() })
	rec := newRecorder()
	a.attach(rec, nil)
	request := acp.InitializeRequest{ProtocolVersion: acp.ProtocolVersionNumber}
	withLifecycle()(&request)
	_, err := a.Initialize(t.Context(), request)
	require.NoError(t, err)
	created, err := a.NewSession(t.Context(), wire.NewSessionRequest(t.TempDir()))
	require.NoError(t, err)
	s, err := a.session(t.Context(), created.SessionId)
	require.NoError(t, err)
	s.mu.Lock()
	rt := s.rt
	s.mu.Unlock()
	terminalClient := &cancellingBackgroundClient{recorder: rec, agent: a, terminalOnly: true}
	a.attach(terminalClient, nil)
	s.openAgentCycle(t.Context(), rt, codex.Event{Kind: codex.EventTurnStarted, ThreadID: s.nativeID, TurnID: "background"})
	s.mu.Lock()
	c := s.cycle
	s.mu.Unlock()
	require.NotNil(t, c)
	require.NoError(t, s.cycleFailure(c))
	require.False(t, s.cycleCancelled(c))
	s.settleAgentCycle(t.Context(), c)
	require.False(t, s.cycleCancelled(c))
	require.True(t, terminalClient.terminalCalled)
	s.callbacks.Wait()
	a.attach(rec, nil)
}
