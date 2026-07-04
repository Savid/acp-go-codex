//go:build integration

package integration

import (
	"bufio"
	"context"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/coder/acp-go-sdk"
	codexacp "github.com/savid/acp-go-codex"
)

const (
	helperMCPStdioEnv = "ACP_GO_CODEX_MCP_STDIO_HELPER"
	helperMCPModeEnv  = "ACP_GO_CODEX_MCP_STDIO_MODE"
)

func TestCodexCLIRawExtensionNotifications(t *testing.T) {
	requireLiveTurn(t)
	t.Parallel()

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()

	client := &recordingClient{}
	conn := connectLiveAgent(t, ctx, client, acp.InitializeRequest{})

	session, err := conn.NewSession(ctx, acp.NewSessionRequest{
		Cwd:        t.TempDir(),
		McpServers: []acp.McpServer{},
		Meta: map[string]any{"codex": map[string]any{
			"rawEvent": map[string]any{"enabled": true},
		}},
	})
	if err != nil {
		t.Fatalf("new session: %v", err)
	}

	resp := promptWithRefusalRetry(t, func() (acp.PromptResponse, error) {
		return conn.Prompt(ctx, acp.PromptRequest{
			SessionId: session.SessionId,
			Prompt:    []acp.ContentBlock{acp.TextBlock("Reply with exactly ACP_RAW_OK and no punctuation.")},
		})
	})
	if resp.StopReason != acp.StopReasonEndTurn {
		t.Fatalf("stop reason = %s", resp.StopReason)
	}

	eventually(t, 30*time.Second, 250*time.Millisecond, func() bool {
		for _, notification := range client.extensionSnapshot() {
			event, _ := notification.Params["event"].(map[string]any)
			payload, _ := event["payload"].(map[string]any)
			rawJSON, _ := notification.Params["rawJSON"].(string)
			if notification.Method == codexacp.RawEventMethod &&
				notification.Params["sessionId"] == string(session.SessionId) &&
				event["type"] == "event_msg" &&
				payload["type"] == "agent_message" &&
				strings.Contains(rawJSON, `"agent_message"`) {
				return true
			}
		}

		return false
	})

	if _, err := conn.CloseSession(ctx, acp.CloseSessionRequest{SessionId: session.SessionId}); err != nil {
		t.Fatalf("close session: %v", err)
	}
}

func TestCodexCLIStructuredOutput(t *testing.T) {
	requireLiveTurn(t)
	t.Parallel()

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()

	client := &recordingClient{}
	conn := connectLiveAgent(t, ctx, client, acp.InitializeRequest{})

	session, err := conn.NewSession(ctx, codexacp.NewSessionRequest(
		t.TempDir(),
		codexacp.WithSessionOutputSchema(map[string]any{
			"type": "object",
			"properties": map[string]any{
				"ok":    map[string]any{"type": "boolean"},
				"label": map[string]any{"type": "string"},
			},
			"required":             []any{"ok", "label"},
			"additionalProperties": false,
		}),
	))
	if err != nil {
		t.Fatalf("new session: %v", err)
	}

	resp := promptWithRefusalRetry(t, func() (acp.PromptResponse, error) {
		return conn.Prompt(ctx, acp.PromptRequest{
			SessionId: session.SessionId,
			Prompt: []acp.ContentBlock{acp.TextBlock(
				`Reply with exactly {"ok":true,"label":"acp-structured"} and no markdown.`,
			)},
		})
	})
	if resp.StopReason != acp.StopReasonEndTurn {
		t.Fatalf("stop reason = %s", resp.StopReason)
	}

	structured, _ := codexMeta(resp.Meta)["structuredOutput"].(map[string]any)
	if structured["ok"] != true || structured["label"] != "acp-structured" {
		t.Fatalf("structured output metadata = %#v; full meta = %#v; text = %q", structured, resp.Meta, client.text())
	}
	if visibleStructuredOutputTool(client.updateSnapshot()) {
		t.Fatal("structured output was exposed as a visible tool update")
	}

	if _, err := conn.CloseSession(ctx, acp.CloseSessionRequest{SessionId: session.SessionId}); err != nil {
		t.Fatalf("close session: %v", err)
	}
}

func visibleStructuredOutputTool(updates []acp.SessionUpdate) bool {
	for _, update := range updates {
		switch {
		case update.ToolCall != nil:
			if updateToolName(update.ToolCall.Meta) == "StructuredOutput" ||
				strings.Contains(update.ToolCall.Title, "StructuredOutput") {
				return true
			}
		case update.ToolCallUpdate != nil:
			if updateToolName(update.ToolCallUpdate.Meta) == "StructuredOutput" {
				return true
			}
		}
	}

	return false
}

func updateToolName(meta map[string]any) string {
	codexMeta, _ := meta["codex"].(map[string]any)
	name, _ := codexMeta["toolName"].(string)

	return name
}

func TestCodexCLIResumeForkAndConcurrentSessions(t *testing.T) {
	requireLiveTurn(t)

	ctx, cancel := context.WithTimeout(context.Background(), 6*time.Minute)
	defer cancel()

	cwd := t.TempDir()
	client := &recordingClient{}
	conn := connectLiveAgent(t, ctx, client, acp.InitializeRequest{})

	first, err := conn.NewSession(ctx, acp.NewSessionRequest{Cwd: cwd, McpServers: []acp.McpServer{}})
	if err != nil {
		t.Fatalf("new first session: %v", err)
	}

	resp := promptWithRefusalRetry(t, func() (acp.PromptResponse, error) {
		return conn.Prompt(ctx, acp.PromptRequest{
			SessionId: first.SessionId,
			Prompt:    []acp.ContentBlock{acp.TextBlock("Reply exactly ACP_RESUME_SEED.")},
		})
	})
	if resp.StopReason != acp.StopReasonEndTurn {
		t.Fatalf("seed stop reason = %s", resp.StopReason)
	}
	threadID := codexThreadID(first.Meta)
	if threadID == "" {
		t.Fatalf("missing thread id: %#v", first.Meta)
	}
	if _, err := conn.CloseSession(ctx, acp.CloseSessionRequest{SessionId: first.SessionId}); err != nil {
		t.Fatalf("close first session: %v", err)
	}

	resumed, err := conn.ResumeSession(ctx, acp.ResumeSessionRequest{
		SessionId:  acp.SessionId(threadID),
		Cwd:        cwd,
		McpServers: []acp.McpServer{},
	})
	if err != nil {
		t.Fatalf("resume session: %v", err)
	}
	if resumed.ConfigOptions == nil {
		t.Fatalf("resume config options missing: %#v", resumed)
	}

	client.resetRecordedOutput()
	resp = promptWithRefusalRetry(t, func() (acp.PromptResponse, error) {
		return conn.Prompt(ctx, acp.PromptRequest{
			SessionId: acp.SessionId(threadID),
			Prompt:    []acp.ContentBlock{acp.TextBlock("Reply exactly ACP_RESUME_OK.")},
		})
	})
	if resp.StopReason != acp.StopReasonEndTurn {
		t.Fatalf("resume stop reason = %s", resp.StopReason)
	}
	if !strings.Contains(client.text(), "ACP_RESUME_OK") {
		t.Fatalf("resume text = %q", client.text())
	}

	fork, err := codexacp.CallForkSession(ctx, conn, acp.UnstableForkSessionRequest{
		SessionId:  acp.SessionId(threadID),
		Cwd:        cwd,
		McpServers: []acp.UnstableMcpServer{},
	})
	if err != nil {
		t.Fatalf("fork session: %v", err)
	}
	if fork.SessionId == "" || fork.SessionId == acp.SessionId(threadID) {
		t.Fatalf("bad fork response: %#v", fork)
	}

	second, err := conn.NewSession(ctx, acp.NewSessionRequest{Cwd: cwd, McpServers: []acp.McpServer{}})
	if err != nil {
		t.Fatalf("new second session: %v", err)
	}

	type promptResult struct {
		text string
		err  error
	}
	results := make(chan promptResult, 2)
	for _, item := range []struct {
		session acp.SessionId
		text    string
	}{
		{acp.SessionId(threadID), "ACP_CONCURRENT_ONE"},
		{second.SessionId, "ACP_CONCURRENT_TWO"},
	} {
		go func() {
			_, promptErr := conn.Prompt(ctx, acp.PromptRequest{
				SessionId: item.session,
				Prompt:    []acp.ContentBlock{acp.TextBlock("Reply exactly " + item.text + ".")},
			})
			results <- promptResult{text: item.text, err: promptErr}
		}()
	}

	for range 2 {
		result := <-results
		if result.err != nil {
			t.Fatalf("concurrent prompt %s: %v", result.text, result.err)
		}
		eventually(t, 30*time.Second, 500*time.Millisecond, func() bool {
			return strings.Contains(client.text(), result.text)
		})
	}

	for _, id := range []acp.SessionId{acp.SessionId(threadID), fork.SessionId, second.SessionId} {
		if _, err := conn.CloseSession(ctx, acp.CloseSessionRequest{SessionId: id}); err != nil {
			t.Fatalf("close session %s: %v", id, err)
		}
	}
}

func TestCodexCLIFailurePaths(t *testing.T) {
	t.Parallel()

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()

	client := &recordingClient{}
	conn := connectLiveAgent(t, ctx, client, acp.InitializeRequest{})

	if _, err := conn.Prompt(ctx, acp.PromptRequest{
		SessionId: "missing-session",
		Prompt:    []acp.ContentBlock{acp.TextBlock("hello")},
	}); err == nil {
		t.Fatal("prompt for missing session succeeded")
	}
	if _, err := conn.UnstableListProviders(ctx, acp.UnstableListProvidersRequest{}); err == nil {
		t.Fatal("provider list succeeded even though Codex provider management is unavailable")
	}
	if _, err := conn.UnstableSetProvider(ctx, acp.UnstableSetProviderRequest{Id: "test", BaseUrl: "https://example.com"}); err == nil {
		t.Fatal("provider set succeeded even though Codex provider management is unavailable")
	}
	if _, err := conn.UnstableDisableProvider(ctx, acp.UnstableDisableProviderRequest{Id: "test"}); err == nil {
		t.Fatal("provider disable succeeded even though Codex provider management is unavailable")
	}

	session, err := conn.NewSession(ctx, acp.NewSessionRequest{Cwd: t.TempDir(), McpServers: []acp.McpServer{}})
	if err != nil {
		t.Fatalf("new session: %v", err)
	}

	if _, err := conn.SetSessionConfigOption(ctx, acp.SetSessionConfigOptionRequest{
		ValueId: &acp.SetSessionConfigOptionValueId{
			SessionId: session.SessionId,
			ConfigId:  "missing_config",
			Value:     "x",
		},
	}); err == nil {
		t.Fatal("missing config option succeeded")
	}

	if _, err := conn.CloseSession(ctx, acp.CloseSessionRequest{SessionId: session.SessionId}); err != nil {
		t.Fatalf("close session: %v", err)
	}
}

func TestCodexCLIPermissionAllowForSessionAndReplay(t *testing.T) {
	requireLiveTurn(t)

	ctx, cancel := context.WithTimeout(context.Background(), 4*time.Minute)
	defer cancel()

	cwd := t.TempDir()
	client := &recordingClient{permission: acp.PermissionOptionId("acceptForSession")}
	conn := connectLiveAgent(t, ctx, client, acp.InitializeRequest{})

	session, err := conn.NewSession(ctx, acp.NewSessionRequest{Cwd: cwd, McpServers: []acp.McpServer{}})
	if err != nil {
		t.Fatalf("new session: %v", err)
	}

	resp := promptWithRefusalRetry(t, func() (acp.PromptResponse, error) {
		return conn.Prompt(ctx, acp.PromptRequest{
			SessionId: session.SessionId,
			Prompt: []acp.ContentBlock{acp.TextBlock(
				"Create a file named permission_one.txt containing ACP_PERMISSION_ONE. " +
					"After the file is written, reply exactly ACP_PERMISSION_ONE_DONE.",
			)},
		})
	})
	if resp.StopReason != acp.StopReasonEndTurn {
		t.Fatalf("stop reason = %s", resp.StopReason)
	}
	eventually(t, 30*time.Second, 250*time.Millisecond, func() bool { return client.permissionCount() >= 1 })
	if !strings.Contains(client.text(), "ACP_PERMISSION_ONE_DONE") {
		t.Fatalf("permission text = %q", client.text())
	}
	requireFileContains(t, filepath.Join(cwd, "permission_one.txt"), "ACP_PERMISSION_ONE")

	firstPermissionCount := client.permissionCount()
	client.resetRecordedOutput()

	resp = promptWithRefusalRetry(t, func() (acp.PromptResponse, error) {
		return conn.Prompt(ctx, acp.PromptRequest{
			SessionId: session.SessionId,
			Prompt: []acp.ContentBlock{acp.TextBlock(
				"Create a file named permission_two.txt containing ACP_PERMISSION_TWO. " +
					"After the file is written, reply exactly ACP_PERMISSION_TWO_DONE.",
			)},
		})
	})
	if resp.StopReason != acp.StopReasonEndTurn {
		t.Fatalf("second stop reason = %s", resp.StopReason)
	}
	if client.permissionCount() < firstPermissionCount {
		t.Fatalf("permission count regressed from %d to %d", firstPermissionCount, client.permissionCount())
	}
	if !strings.Contains(client.text(), "ACP_PERMISSION_TWO_DONE") {
		t.Fatalf("second permission text = %q", client.text())
	}
	requireFileContains(t, filepath.Join(cwd, "permission_two.txt"), "ACP_PERMISSION_TWO")

	if _, err := conn.CloseSession(ctx, acp.CloseSessionRequest{SessionId: session.SessionId}); err != nil {
		t.Fatalf("close session: %v", err)
	}
}

func TestCodexCLICancelPendingPermissionRequest(t *testing.T) {
	requireLiveTurn(t)

	ctx, cancel := context.WithTimeout(context.Background(), 4*time.Minute)
	defer cancel()

	client := newBlockingPermissionClient()
	conn := connectLiveAgent(t, ctx, client, acp.InitializeRequest{})

	session, err := conn.NewSession(ctx, acp.NewSessionRequest{Cwd: t.TempDir(), McpServers: []acp.McpServer{}})
	if err != nil {
		t.Fatalf("new session: %v", err)
	}

	respCh := make(chan acp.PromptResponse, 1)
	errCh := make(chan error, 1)
	go func() {
		resp, promptErr := conn.Prompt(ctx, acp.PromptRequest{
			SessionId: session.SessionId,
			Prompt: []acp.ContentBlock{acp.TextBlock(
				"Create a file named permission_cancel.txt containing ACP_PERMISSION_CANCEL. Do not use any other tool.",
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

func TestCodexCLIMultimodalPrompt(t *testing.T) {
	requireLiveTurn(t)
	t.Parallel()

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()

	client := &recordingClient{}
	conn := connectLiveAgent(t, ctx, client, acp.InitializeRequest{})

	session, err := conn.NewSession(ctx, acp.NewSessionRequest{Cwd: t.TempDir(), McpServers: []acp.McpServer{}})
	if err != nil {
		t.Fatalf("new session: %v", err)
	}

	resp := promptWithRefusalRetry(t, func() (acp.PromptResponse, error) {
		client.resetRecordedOutput()

		return conn.Prompt(ctx, acp.PromptRequest{
			SessionId: session.SessionId,
			Prompt: []acp.ContentBlock{
				acp.TextBlock("The attached image is intentionally tiny. Reply exactly ACP_MULTIMODAL_OK."),
				acp.ImageBlock("iVBORw0KGgoAAAANSUhEUgAAACAAAAAgAQMAAABJtOi3AAAAA1BMVEX/AAAZ4gk3AAAADElEQVQI12NgGNwAAACgAAFhJX1HAAAAAElFTkSuQmCC", "image/png"),
				acp.ResourceBlock(acp.EmbeddedResourceResource{
					TextResourceContents: &acp.TextResourceContents{
						Uri:  "memory://integration-note.txt",
						Text: "integration text resource",
					},
				}),
			},
		})
	})
	if resp.StopReason != acp.StopReasonEndTurn {
		t.Fatalf("stop reason = %s", resp.StopReason)
	}
	if !strings.Contains(client.text(), "ACP_MULTIMODAL_OK") {
		t.Fatalf("multimodal text = %q", client.text())
	}

	if _, err := conn.CloseSession(ctx, acp.CloseSessionRequest{SessionId: session.SessionId}); err != nil {
		t.Fatalf("close session: %v", err)
	}
}

func TestCodexCLIMCPStdioTool(t *testing.T) {
	requireLiveTurn(t)

	ctx, cancel := context.WithTimeout(context.Background(), 4*time.Minute)
	defer cancel()

	client := &recordingClient{permission: acp.PermissionOptionId("accept")}
	conn := connectLiveAgent(t, ctx, client, acp.InitializeRequest{})

	session, err := conn.NewSession(ctx, codexacp.NewSessionRequest(
		t.TempDir(),
		codexacp.WithSessionMCPServers(
			acp.McpServer{
				Stdio: &acp.McpServerStdio{
					Name:    "acp_stdio",
					Command: os.Args[0],
					Args:    []string{"-test.run=TestIntegrationMCPStdioHelper"},
					Env:     []acp.EnvVariable{{Name: helperMCPStdioEnv, Value: "1"}},
				},
			},
		),
		codexacp.WithSessionCodexOptions(codexacp.NewCodexOptions(
			codexacp.WithCodexMCPToolApprovalMode("approve"),
		)),
	))
	if err != nil {
		t.Fatalf("new session: %v", err)
	}

	resp := promptWithRefusalRetry(t, func() (acp.PromptResponse, error) {
		return conn.Prompt(ctx, acp.PromptRequest{
			SessionId: session.SessionId,
			Prompt: []acp.ContentBlock{acp.TextBlock(
				"Use the acp_stdio MCP server's echo tool with message ACP_MCP_STDIO_OK. " +
					"After the tool returns, reply exactly ACP_MCP_STDIO_DONE.",
			)},
		})
	})
	if resp.StopReason != acp.StopReasonEndTurn {
		t.Fatalf("stop reason = %s", resp.StopReason)
	}
	if !strings.Contains(client.text(), "ACP_MCP_STDIO_DONE") {
		t.Fatalf("MCP text = %q", client.text())
	}
	resultUpdate := completedToolResultUpdate(client.updateSnapshot(), "ACP_MCP_STDIO_OK")
	if resultUpdate == nil {
		if !toolCallSeen(client.updateSnapshot(), "acp_stdio", "echo") {
			t.Fatal("live MCP tool was not emitted as an ACP tool update")
		}
	} else {
		if resultUpdate.ToolCallId == "" {
			t.Fatalf("tool result missing id: %#v", resultUpdate)
		}
		if resultUpdate.RawOutput == nil || !rawValueContains(resultUpdate.RawOutput, "ACP_MCP_STDIO_OK") {
			t.Fatalf("tool result raw output = %#v", resultUpdate.RawOutput)
		}
	}

	if _, err := conn.CloseSession(ctx, acp.CloseSessionRequest{SessionId: session.SessionId}); err != nil {
		t.Fatalf("close session: %v", err)
	}
}

func toolLocationContains(updates []acp.SessionUpdate, path string) bool {
	for _, update := range updates {
		if update.ToolCall != nil {
			for _, location := range update.ToolCall.Locations {
				if strings.Contains(location.Path, path) {
					return true
				}
			}
		}

		if update.ToolCallUpdate != nil {
			for _, location := range update.ToolCallUpdate.Locations {
				if strings.Contains(location.Path, path) {
					return true
				}
			}
		}
	}

	return false
}

func requireFileContains(t *testing.T, path string, text string) {
	t.Helper()

	raw, err := os.ReadFile(path) // #nosec G304 -- integration test path is inside t.TempDir.
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	if !strings.Contains(string(raw), text) {
		t.Fatalf("%s contents %q do not contain %q", path, string(raw), text)
	}
}

func toolCallSeen(updates []acp.SessionUpdate, needles ...string) bool {
	for _, update := range updates {
		title := ""
		switch {
		case update.ToolCall != nil:
			title = update.ToolCall.Title
		case update.ToolCallUpdate != nil && update.ToolCallUpdate.Title != nil:
			title = *update.ToolCallUpdate.Title
		}
		if title == "" {
			continue
		}
		matched := true
		for _, needle := range needles {
			if !strings.Contains(title, needle) {
				matched = false
				break
			}
		}
		if matched {
			return true
		}
	}

	return false
}

func completedToolResultUpdate(updates []acp.SessionUpdate, text string) *acp.SessionToolCallUpdate {
	for i := range updates {
		update := updates[i].ToolCallUpdate
		if update == nil || update.Status == nil || *update.Status != acp.ToolCallStatusCompleted {
			continue
		}

		if !toolUpdateContentContains(update, text) {
			continue
		}

		return update
	}

	return nil
}

func toolUpdateContentContains(update *acp.SessionToolCallUpdate, text string) bool {
	for _, content := range update.Content {
		if content.Content == nil || content.Content.Content.Text == nil {
			continue
		}

		if strings.Contains(content.Content.Content.Text.Text, text) {
			return true
		}
	}

	return false
}

func TestIntegrationMCPStdioHelper(t *testing.T) {
	if os.Getenv(helperMCPStdioEnv) != "1" {
		return
	}

	if err := runMCPStdioServer(os.Stdin, os.Stdout); err != nil {
		os.Exit(1)
	}

	os.Exit(0)
}

func runMCPStdioServer(stdin io.Reader, stdout io.Writer) error {
	scanner := bufio.NewScanner(stdin)
	enc := json.NewEncoder(stdout)

	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" {
			continue
		}

		var msg map[string]any
		if err := json.Unmarshal([]byte(line), &msg); err != nil {
			return err
		}

		id, hasID := msg["id"]
		method, _ := msg["method"].(string)
		if !hasID {
			continue
		}

		result := mcpStdioResult(method, os.Getenv(helperMCPModeEnv))
		if err := enc.Encode(map[string]any{
			"jsonrpc": "2.0",
			"id":      id,
			"result":  result,
		}); err != nil {
			return err
		}
	}

	return scanner.Err()
}

func mcpStdioResult(method string, mode string) map[string]any {
	switch method {
	case "initialize":
		return map[string]any{
			"protocolVersion": "2024-11-05",
			"capabilities":    map[string]any{"tools": map[string]any{}},
			"serverInfo":      map[string]any{"name": "acp-stdio", "version": "1.0.0"},
		}
	case "tools/list":
		if mode == "slow" {
			return map[string]any{
				"tools": []map[string]any{
					{
						"name":        "wait",
						"description": "Wait long enough for ACP cancellation coverage.",
						"inputSchema": map[string]any{
							"type":       "object",
							"properties": map[string]any{},
						},
					},
				},
			}
		}

		return map[string]any{
			"tools": []map[string]any{
				{
					"name":        "echo",
					"description": "Return the provided message.",
					"inputSchema": map[string]any{
						"type":       "object",
						"properties": map[string]any{"message": map[string]any{"type": "string"}},
						"required":   []string{"message"},
					},
				},
			},
		}
	case "tools/call":
		if mode == "slow" {
			time.Sleep(30 * time.Second)

			return map[string]any{
				"content": []map[string]any{{"type": "text", "text": "ACP_MCP_WAIT_DONE"}},
				"isError": false,
			}
		}

		return map[string]any{
			"content": []map[string]any{{"type": "text", "text": "ACP_MCP_STDIO_OK"}},
			"isError": false,
		}
	default:
		return map[string]any{}
	}
}
