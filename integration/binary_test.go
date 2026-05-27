//go:build integration

package integration

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/coder/acp-go-sdk"
)

func TestCodexACPAgentBinaryConversation(t *testing.T) {
	requireLiveTurn(t)
	t.Parallel()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	client := &recordingClient{}
	conn := connectLiveAgentBinary(t, ctx, client, acp.InitializeRequest{})

	session, err := conn.NewSession(ctx, acp.NewSessionRequest{Cwd: t.TempDir(), McpServers: []acp.McpServer{}})
	if err != nil {
		t.Fatalf("new session: %v", err)
	}

	resp := promptWithRefusalRetry(t, func() (acp.PromptResponse, error) {
		return conn.Prompt(ctx, acp.PromptRequest{
			SessionId: session.SessionId,
			Prompt:    []acp.ContentBlock{acp.TextBlock("Reply with exactly ACP_BINARY_OK and no punctuation.")},
		})
	})
	if resp.StopReason != acp.StopReasonEndTurn {
		t.Fatalf("stop reason = %s", resp.StopReason)
	}
	if !strings.Contains(client.text(), "ACP_BINARY_OK") {
		t.Fatalf("agent text %q does not contain sentinel", client.text())
	}

	if _, err := conn.CloseSession(ctx, acp.CloseSessionRequest{SessionId: session.SessionId}); err != nil {
		t.Fatalf("close session: %v", err)
	}
}
