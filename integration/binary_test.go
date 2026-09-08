//go:build integration

package integration

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
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
			Meta:      newTurnRouteMeta(),
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

// TestBinarySmokeInitializeClose exercises the compiled adapter's ACP startup
// and graceful EOF without launching a native harness or reading credentials.
// It proves wrapper wiring and counter production, not native compatibility.
func TestBinarySmokeInitializeClose(t *testing.T) {
	if os.Getenv(envRunIntegration) != "1" {
		t.Skipf("set %s=1 to run compiled adapter smoke", envRunIntegration)
	}
	t.Parallel()
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()
	home := t.TempDir()
	cmd := exec.CommandContext(ctx, integrationBinaryPath(t), "-path", filepath.Join(home, "no-native-harness"), "-home", home)
	cmd.Dir = home
	cmd.Env = []string{"HOME=" + home, "USERPROFILE=" + home, "XDG_CONFIG_HOME=" + home, "TMPDIR=" + home, "TEMP=" + home, "TMP=" + home}
	for _, key := range []string{"PATH", "SystemRoot", "SYSTEMROOT", "WINDIR", "GOCOVERDIR"} {
		if value, ok := os.LookupEnv(key); ok {
			cmd.Env = append(cmd.Env, key+"="+value)
		}
	}
	process := startIntegrationProcess(t, cmd)
	conn := acp.NewClientSideConnection(&recordingClient{}, process.stdin, process.stdout)
	response, err := conn.Initialize(ctx, acp.InitializeRequest{ProtocolVersion: acp.ProtocolVersionNumber})
	if err != nil {
		t.Fatalf("compiled initialize: %v; stderr: %s", err, process.stderr.String())
	}
	if response.ProtocolVersion != acp.ProtocolVersionNumber || response.AgentInfo == nil {
		t.Fatalf("unexpected initialize response: %#v", response)
	}
	if err := process.stdin.Close(); err != nil {
		t.Fatal(err)
	}
	if err := process.wait(ctx); err != nil {
		t.Fatalf("compiled adapter EOF: %v; stderr: %s", err, process.stderr.String())
	}
}
