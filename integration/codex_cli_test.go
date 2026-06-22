//go:build integration

package integration

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/coder/acp-go-sdk"
)

func TestCodexCLIACPStartupAndSessionSurface(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()

	cwd := t.TempDir()
	client := &recordingClient{}
	conn, initialize := initializeLiveAgentForTest(t, ctx, client, acp.InitializeRequest{
		ClientCapabilities: acp.ClientCapabilities{
			Auth: acp.AuthCapabilities{Terminal: true},
			Meta: map[string]any{"terminal-auth": true},
		},
	})

	methods := authAgentMethodIDs(initialize.AuthMethods)
	if !containsString(methods, "codex-login") {
		t.Fatalf("auth methods %v missing codex-login", methods)
	}
	if !containsString(methods, "codex-chatgpt-auth-tokens") {
		t.Fatalf("auth methods %v missing token auth", methods)
	}
	if initialize.AgentCapabilities.LoadSession != true {
		t.Fatalf("load session capability not advertised: %#v", initialize.AgentCapabilities)
	}

	session, err := conn.NewSession(ctx, acp.NewSessionRequest{Cwd: cwd, McpServers: []acp.McpServer{}})
	if err != nil {
		t.Fatalf("new session: %v", err)
	}
	if session.SessionId == "" {
		t.Fatal("session id is empty")
	}
	modelConfig := findSessionSelectConfig(session.ConfigOptions, "model")
	if modelConfig == nil || modelConfig.Category == nil || *modelConfig.Category != acp.SessionConfigOptionCategoryModel || len(*modelConfig.Options.Ungrouped) == 0 {
		t.Fatalf("model config not populated: %#v", session.ConfigOptions)
	}
	effortConfig := findSessionSelectConfig(session.ConfigOptions, "effort")
	if effortConfig == nil || effortConfig.Category == nil || *effortConfig.Category != acp.SessionConfigOptionCategoryThoughtLevel || len(*effortConfig.Options.Ungrouped) == 0 {
		t.Fatalf("effort config not populated: %#v", session.ConfigOptions)
	}
	threadID := codexThreadID(session.Meta)
	if threadID == "" {
		t.Fatalf("codex thread id missing from meta: %#v", session.Meta)
	}

	raw, err := conn.CallExtension(ctx, "_codex/collaborationMode/list", map[string]any{"sessionId": session.SessionId})
	if err != nil {
		t.Fatalf("collaboration mode list: %v", err)
	}
	if len(raw) == 0 || !json.Valid(raw) {
		t.Fatalf("collaboration mode list returned invalid JSON: %s", string(raw))
	}

	raw, err = conn.CallExtension(ctx, "_codex/mcpServerStatus/list", map[string]any{"sessionId": session.SessionId})
	if err != nil {
		t.Fatalf("MCP server status list: %v", err)
	}
	if len(raw) == 0 || !json.Valid(raw) {
		t.Fatalf("MCP status list returned invalid JSON: %s", string(raw))
	}

	raw, err = conn.CallExtension(ctx, "_codex/thread/read", map[string]any{"sessionId": session.SessionId})
	if err != nil {
		t.Fatalf("thread read: %v", err)
	}
	if !strings.Contains(string(raw), threadID) {
		t.Fatalf("thread read response %s did not mention thread id %q", string(raw), threadID)
	}

	if _, err := conn.SetSessionMode(ctx, acp.SetSessionModeRequest{
		SessionId: session.SessionId,
		ModeId:    "plan",
	}); err != nil {
		t.Fatalf("set session mode: %v", err)
	}
	if _, err := conn.SetSessionMode(ctx, acp.SetSessionModeRequest{
		SessionId: session.SessionId,
		ModeId:    "default",
	}); err != nil {
		t.Fatalf("reset session mode: %v", err)
	}
	if _, err := conn.SetSessionConfigOption(ctx, acp.SetSessionConfigOptionRequest{
		ValueId: &acp.SetSessionConfigOptionValueId{
			SessionId: session.SessionId,
			ConfigId:  "model",
			Value:     modelConfig.CurrentValue,
		},
	}); err != nil {
		t.Fatalf("set session model: %v", err)
	}

	eventually(t, 30*time.Second, 500*time.Millisecond, func() bool {
		list, listErr := conn.ListSessions(ctx, acp.ListSessionsRequest{Cwd: &cwd})
		if listErr != nil {
			return false
		}
		for _, listed := range list.Sessions {
			if listed.SessionId == session.SessionId {
				return true
			}
		}

		return false
	})

	if _, err := conn.Logout(ctx, acp.LogoutRequest{}); err == nil {
		t.Fatal("logout succeeded without explicit guarded logout opt-in")
	}
	if _, err := conn.CloseSession(ctx, acp.CloseSessionRequest{SessionId: session.SessionId}); err != nil {
		t.Fatalf("close session: %v", err)
	}
}

func TestCodexCLIACPConversation(t *testing.T) {
	requireLiveTurn(t)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	cwd := t.TempDir()
	client := &recordingClient{}
	conn := connectLiveAgent(t, ctx, client, acp.InitializeRequest{})

	session, err := conn.NewSession(ctx, acp.NewSessionRequest{Cwd: cwd, McpServers: []acp.McpServer{}})
	if err != nil {
		t.Fatalf("new session: %v", err)
	}
	messageID := "22222222-2222-4222-8222-222222222222"
	resp := promptWithRefusalRetry(t, func() (acp.PromptResponse, error) {
		return conn.Prompt(ctx, acp.PromptRequest{
			SessionId: session.SessionId,
			MessageId: &messageID,
			Prompt:    []acp.ContentBlock{acp.TextBlock("Reply with exactly ACP_OK and no punctuation.")},
		})
	})
	if resp.StopReason != acp.StopReasonEndTurn {
		t.Fatalf("stop reason = %s", resp.StopReason)
	}
	if resp.UserMessageId == nil || *resp.UserMessageId != messageID {
		t.Fatalf("user message id = %#v", resp.UserMessageId)
	}
	if resp.Usage == nil || resp.Usage.TotalTokens <= 0 {
		t.Fatalf("usage not populated with valid totals: %#v", resp.Usage)
	}
	streamedUsage := client.latestUsage()
	if streamedUsage == nil || streamedUsage.Used <= 0 {
		t.Fatalf("streamed usage not populated: %#v", streamedUsage)
	}
	if !strings.Contains(client.text(), "ACP_OK") {
		t.Fatalf("agent text %q does not contain sentinel", client.text())
	}

	raw, err := conn.CallExtension(ctx, "_codex/thread/turns/list", map[string]any{
		"sessionId": session.SessionId,
		"limit":     10,
	})
	if err != nil {
		t.Fatalf("thread turns list: %v", err)
	}
	if len(raw) == 0 || !json.Valid(raw) {
		t.Fatalf("thread turns list returned invalid JSON: %s", string(raw))
	}

	if _, err := conn.CloseSession(ctx, acp.CloseSessionRequest{SessionId: session.SessionId}); err != nil {
		t.Fatalf("close session: %v", err)
	}
}
