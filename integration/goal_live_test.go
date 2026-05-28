//go:build integration

package integration

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/coder/acp-go-sdk"
	codexacp "github.com/savid/acp-go-codex"
)

const (
	codexSessionSetGoalMethod = "_codex/session/setGoal"
	goalSidecarSubpath        = "goal.json"
)

func TestCodexGoalExtensionLive(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	store := newRecordingSessionStore()
	client := &recordingClient{}
	conn, init := initializeLiveAgentForTest(
		t,
		ctx,
		client,
		acp.InitializeRequest{},
		codexacp.WithCodexGoals(true),
		codexacp.WithSessionStore(store),
	)

	goals := codexGoalsCapability(t, init.AgentCapabilities.Meta)
	if goals["scope"] != "session" ||
		goals["setMethod"] != codexSessionSetGoalMethod ||
		goals["state"] != "session_info_update._meta.codex.goal" {
		t.Fatalf("goals capability = %#v", goals)
	}

	budget := int64(50000)
	session, err := conn.NewSession(ctx, codexacp.NewSessionRequest(
		t.TempDir(),
		codexacp.WithSessionGoal(codexacp.CodexGoal{
			Objective:   "integration initial goal",
			Status:      codexacp.CodexGoalStatusActive,
			TokenBudget: &budget,
		}),
	))
	skipIfCodexGoalUnavailable(t, err)
	if err != nil {
		t.Fatalf("new session: %v", err)
	}
	if goal := codexGoalMap(t, session.Meta); goal["objective"] != "integration initial goal" {
		t.Fatalf("initial goal = %#v", goal)
	}
	threadID := codexThreadID(session.Meta)
	if threadID == "" {
		t.Fatalf("missing thread id: %#v", session.Meta)
	}
	eventually(t, 30*time.Second, 250*time.Millisecond, func() bool {
		return store.hasGoalSidecar(string(session.SessionId), "integration initial goal")
	})

	raw, err := conn.CallExtension(ctx, codexSessionSetGoalMethod, codexacp.SetGoalRequest(
		session.SessionId,
		codexacp.CodexGoal{
			Objective: "integration updated goal",
			Status:    codexacp.CodexGoalStatusPaused,
		},
	))
	skipIfCodexGoalUnavailable(t, err)
	if err != nil {
		t.Fatalf("set goal: %v", err)
	}
	if goal := extensionCodexGoalMap(t, raw); goal["objective"] != "integration updated goal" {
		t.Fatalf("extension goal = %#v", goal)
	}
	eventually(t, 30*time.Second, 250*time.Millisecond, func() bool {
		goal, ok := latestCodexGoalUpdate(client)

		return ok && goal != nil &&
			goal["objective"] == "integration updated goal" &&
			goal["status"] == codexacp.CodexGoalStatusPaused
	})

	raw, err = conn.CallExtension(ctx, codexSessionSetGoalMethod, codexacp.ClearGoalRequest(session.SessionId))
	skipIfCodexGoalUnavailable(t, err)
	if err != nil {
		t.Fatalf("clear goal: %v", err)
	}
	if value := extensionCodexGoalValue(t, raw); value != nil {
		t.Fatalf("clear response goal = %#v", value)
	}
	eventually(t, 30*time.Second, 250*time.Millisecond, func() bool {
		goal, ok := latestCodexGoalUpdate(client)

		return ok && goal == nil
	})

	if _, err := conn.CloseSession(ctx, acp.CloseSessionRequest{SessionId: session.SessionId}); err != nil {
		t.Fatalf("close session: %v", err)
	}
}

func TestCodexGoalDisabledExtensionLive(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()

	client := &recordingClient{}
	conn, init := initializeLiveAgentForTest(t, ctx, client, acp.InitializeRequest{})
	if codexMeta(init.AgentCapabilities.Meta)["goals"] != nil {
		t.Fatalf("goals advertised without opt-in: %#v", init.AgentCapabilities.Meta)
	}

	session, err := conn.NewSession(ctx, codexacp.NewSessionRequest(t.TempDir()))
	if err != nil {
		t.Fatalf("new session: %v", err)
	}
	if _, err := conn.CallExtension(ctx, codexSessionSetGoalMethod, codexacp.SetGoalRequest(
		session.SessionId,
		codexacp.CodexGoal{Objective: "disabled integration goal"},
	)); err == nil {
		t.Fatal("set goal succeeded without goals enabled")
	} else if !codexGoalUnavailableError(err) {
		t.Fatalf("disabled goal error = %v", err)
	}

	if _, err := conn.CloseSession(ctx, acp.CloseSessionRequest{SessionId: session.SessionId}); err != nil {
		t.Fatalf("close session: %v", err)
	}
}

func TestCodexGoalStoreLoadRestoreLive(t *testing.T) {
	requireLiveTurn(t)

	ctx, cancel := context.WithTimeout(context.Background(), 6*time.Minute)
	defer cancel()

	cwd := t.TempDir()
	store := newRecordingSessionStore()
	client := &recordingClient{}
	conn := connectLiveAgent(
		t,
		ctx,
		client,
		acp.InitializeRequest{},
		codexacp.WithCodexGoals(true),
		codexacp.WithSessionStore(store),
	)

	session, err := conn.NewSession(ctx, codexacp.NewSessionRequest(
		cwd,
		codexacp.WithSessionGoal(codexacp.CodexGoal{
			Objective: "integration stored goal",
			Status:    codexacp.CodexGoalStatusPaused,
		}),
	))
	skipIfCodexGoalUnavailable(t, err)
	if err != nil {
		t.Fatalf("new session: %v", err)
	}
	threadID := codexThreadID(session.Meta)
	if threadID == "" {
		t.Fatalf("missing thread id: %#v", session.Meta)
	}

	resp := promptWithRefusalRetry(t, func() (acp.PromptResponse, error) {
		return conn.Prompt(ctx, acp.PromptRequest{
			SessionId: session.SessionId,
			Prompt:    []acp.ContentBlock{acp.TextBlock("Reply with exactly ACP_GOAL_STORE_OK and no punctuation.")},
		})
	})
	if resp.StopReason != acp.StopReasonEndTurn {
		t.Fatalf("stop reason = %s", resp.StopReason)
	}
	eventually(t, 30*time.Second, 250*time.Millisecond, func() bool {
		return store.hasMainSession(string(session.SessionId)) && store.hasGoalSidecar(string(session.SessionId), "integration stored goal")
	})
	if _, err := conn.CloseSession(ctx, acp.CloseSessionRequest{SessionId: session.SessionId}); err != nil {
		t.Fatalf("close source session: %v", err)
	}

	client = &recordingClient{}
	conn = connectLiveAgent(
		t,
		ctx,
		client,
		acp.InitializeRequest{},
		codexacp.WithCodexGoals(true),
		codexacp.WithSessionStore(store),
	)
	loaded, err := conn.LoadSession(ctx, acp.LoadSessionRequest{
		SessionId:  session.SessionId,
		Cwd:        cwd,
		McpServers: []acp.McpServer{},
	})
	skipIfCodexGoalUnavailable(t, err)
	if err != nil {
		t.Fatalf("load stored session: %v", err)
	}
	if goal := codexGoalMap(t, loaded.Meta); goal["objective"] != "integration stored goal" {
		t.Fatalf("loaded goal = %#v", goal)
	}

	if _, err := conn.CloseSession(ctx, acp.CloseSessionRequest{SessionId: session.SessionId}); err != nil {
		t.Fatalf("close loaded session: %v", err)
	}
}

func TestCodexGoalNativeTurnUpdateLive(t *testing.T) {
	requireLiveTurn(t)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	client := &recordingClient{}
	conn := connectLiveAgent(
		t,
		ctx,
		client,
		acp.InitializeRequest{},
		codexacp.WithCodexGoals(true),
	)

	session, err := conn.NewSession(ctx, codexacp.NewSessionRequest(
		t.TempDir(),
		codexacp.WithSessionGoal(codexacp.CodexGoal{
			Objective: "Prove 1 + 1 = 2 in one sentence. Include the exact text 1 + 1 = 2, then mark this goal complete.",
		}),
	))
	skipIfCodexGoalUnavailable(t, err)
	if err != nil {
		t.Fatalf("new session: %v", err)
	}

	resp := promptWithRefusalRetry(t, func() (acp.PromptResponse, error) {
		return conn.Prompt(ctx, acp.PromptRequest{
			SessionId: session.SessionId,
			Prompt:    []acp.ContentBlock{acp.TextBlock("Follow the current session goal now.")},
		})
	})
	if resp.StopReason != acp.StopReasonEndTurn {
		t.Fatalf("stop reason = %s", resp.StopReason)
	}
	if !strings.Contains(client.text(), "1 + 1 = 2") {
		t.Fatalf("goal-guided text = %q", client.text())
	}
	eventually(t, 30*time.Second, 250*time.Millisecond, func() bool {
		goal, ok := latestCodexGoalUpdate(client)

		return ok && goal != nil && goal["status"] == codexacp.CodexGoalStatusComplete
	})

	if _, err := conn.CloseSession(ctx, acp.CloseSessionRequest{SessionId: session.SessionId}); err != nil {
		t.Fatalf("close session: %v", err)
	}
}

func TestCodexGoalSetDuringPendingPermissionLive(t *testing.T) {
	requireLiveTurn(t)

	ctx, cancel := context.WithTimeout(context.Background(), 4*time.Minute)
	defer cancel()

	client := newBlockingPermissionClient()
	conn := connectLiveAgent(
		t,
		ctx,
		client,
		acp.InitializeRequest{},
		codexacp.WithCodexGoals(true),
	)

	session, err := conn.NewSession(ctx, codexacp.NewSessionRequest(t.TempDir()))
	skipIfCodexGoalUnavailable(t, err)
	if err != nil {
		t.Fatalf("new session: %v", err)
	}

	respCh := make(chan acp.PromptResponse, 1)
	errCh := make(chan error, 1)
	go func() {
		resp, promptErr := conn.Prompt(ctx, acp.PromptRequest{
			SessionId: session.SessionId,
			Prompt: []acp.ContentBlock{acp.TextBlock(
				"Create a file named goal_midturn.txt containing ACP_GOAL_MIDTURN. Do not use any other tool.",
			)},
		})
		if promptErr != nil {
			errCh <- promptErr

			return
		}
		respCh <- resp
	}()

	select {
	case <-client.permissionRequested:
	case err := <-errCh:
		t.Fatalf("prompt errored before requesting permission: %v", err)
	case resp := <-respCh:
		t.Fatalf("prompt returned before requesting permission: %#v", resp)
	case <-ctx.Done():
		t.Fatalf("context ended before permission request: %v", ctx.Err())
	}

	raw, err := conn.CallExtension(ctx, codexSessionSetGoalMethod, codexacp.SetGoalRequest(
		session.SessionId,
		codexacp.CodexGoal{Objective: "integration midturn goal"},
	))
	skipIfCodexGoalUnavailable(t, err)
	if err != nil {
		t.Fatalf("set midturn goal: %v", err)
	}
	if goal := extensionCodexGoalMap(t, raw); goal["objective"] != "integration midturn goal" {
		t.Fatalf("midturn goal response = %#v", goal)
	}
	eventually(t, 30*time.Second, 250*time.Millisecond, func() bool {
		return codexGoalUpdateCount(&client.recordingClient, "integration midturn goal") == 1
	})
	never(t, 2*time.Second, 250*time.Millisecond, func() bool {
		return codexGoalUpdateCount(&client.recordingClient, "integration midturn goal") > 1
	})

	if err := conn.Cancel(ctx, acp.CancelNotification{SessionId: session.SessionId}); err != nil {
		t.Fatalf("cancel: %v", err)
	}
	select {
	case returned := <-client.permissionReturned:
		if returned.Outcome.Cancelled == nil {
			t.Fatalf("permission outcome = %#v", returned.Outcome)
		}
	case <-ctx.Done():
		t.Fatalf("context ended before permission returned: %v", ctx.Err())
	}
	select {
	case err := <-errCh:
		if err != nil {
			t.Fatalf("prompt after cancel: %v", err)
		}
	case resp := <-respCh:
		if resp.StopReason != acp.StopReasonCancelled {
			t.Fatalf("cancelled stop reason = %s", resp.StopReason)
		}
	case <-ctx.Done():
		t.Fatalf("context ended before prompt returned: %v", ctx.Err())
	}

	if _, err := conn.CloseSession(ctx, acp.CloseSessionRequest{SessionId: session.SessionId}); err != nil {
		t.Fatalf("close session: %v", err)
	}
}

func skipIfCodexGoalUnavailable(t *testing.T, err error) {
	t.Helper()
	if err == nil {
		return
	}
	if codexGoalUnavailableError(err) {
		t.Skipf("Codex goal APIs unavailable for this Codex app-server/home: %v", err)
	}
}

func codexGoalUnavailableError(err error) bool {
	if err == nil {
		return false
	}
	text := strings.ToLower(err.Error())

	return strings.Contains(text, "codex goal support is not available") ||
		strings.Contains(text, "goals feature is disabled") ||
		strings.Contains(text, "no such table: thread_goals")
}

func codexGoalsCapability(t *testing.T, meta map[string]any) map[string]any {
	t.Helper()

	goals, _ := codexMeta(meta)["goals"].(map[string]any)
	if goals == nil {
		t.Fatalf("goals capability missing: %#v", meta)
	}

	return goals
}

func codexGoalMap(t *testing.T, meta map[string]any) map[string]any {
	t.Helper()

	goal, _ := codexMeta(meta)["goal"].(map[string]any)
	if goal == nil {
		t.Fatalf("goal missing: %#v", meta)
	}

	return goal
}

func extensionCodexGoalMap(t *testing.T, raw json.RawMessage) map[string]any {
	t.Helper()

	goal, _ := extensionCodexGoalValue(t, raw).(map[string]any)
	if goal == nil {
		t.Fatalf("extension goal missing: %s", string(raw))
	}

	return goal
}

func extensionCodexGoalValue(t *testing.T, raw json.RawMessage) any {
	t.Helper()

	var response map[string]any
	if err := json.Unmarshal(raw, &response); err != nil {
		t.Fatalf("decode extension response: %v", err)
	}

	return response["goal"]
}

func latestCodexGoalUpdate(client *recordingClient) (map[string]any, bool) {
	updates := client.updateSnapshot()
	for index := len(updates) - 1; index >= 0; index-- {
		info := updates[index].SessionInfoUpdate
		if info == nil || info.Meta == nil {
			continue
		}
		value, ok := codexMeta(info.Meta)["goal"]
		if !ok {
			continue
		}
		if value == nil {
			return nil, true
		}
		goal, _ := value.(map[string]any)

		return goal, goal != nil
	}

	return nil, false
}

func codexGoalUpdateCount(client *recordingClient, objective string) int {
	count := 0
	for _, update := range client.updateSnapshot() {
		info := update.SessionInfoUpdate
		if info == nil || info.Meta == nil {
			continue
		}
		goal, _ := codexMeta(info.Meta)["goal"].(map[string]any)
		if goal != nil && goal["objective"] == objective {
			count++
		}
	}

	return count
}

func (s *recordingSessionStore) hasGoalSidecar(sessionID string, objective string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()

	for key, entries := range s.entries {
		if key.SessionID != sessionID || key.Subpath != goalSidecarSubpath || len(entries) == 0 {
			continue
		}
		if strings.Contains(string(entries[len(entries)-1]), objective) {
			return true
		}
	}

	return false
}
