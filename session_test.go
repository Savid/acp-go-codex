package codexacp

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"

	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/coder/acp-go-sdk"

	"github.com/savid/acp-go-core/wire"
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

	t.Run("timeout", func(t *testing.T) {
		t.Parallel()

		h := newHarness(t, WithTurnTimeout(200*time.Millisecond))
		h.initialize()
		session := h.newSession()

		_, err := h.prompt(session.SessionId, "SLOW", nil)
		require.Equal(t, "timeout", requestErrorData(t, err)["cause"])
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
	for _, state := range []string{"cancelled turn", "timed out", "closed", "disconnected"} {
		t.Run(state, func(t *testing.T) {
			t.Parallel()
			s := &session{rt: &runtime{}, turn: &turn{}}
			switch state {
			case "cancelled turn":
				s.turn.cancelled = true
			case "timed out":
				s.turn.timedOut = true
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
