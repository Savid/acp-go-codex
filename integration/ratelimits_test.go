//go:build integration

package integration

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/coder/acp-go-sdk"

	codexacp "github.com/savid/acp-go-codex"
)

// TestCodexCLIRateLimits drives the real codex harness end-to-end and exercises
// the _codex/rateLimits extension. It is a smoke test: account/rateLimits/read
// spends no model tokens. Assertions are robust to account state — the window
// set may be empty — but any window that is present must carry the codex-native
// shape. The test's purpose is to catch upstream codex protocol changes to the
// rate-limit payload or account read.
func TestCodexCLIRateLimits(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()

	cwd := t.TempDir()
	client := &recordingClient{}
	conn, _ := initializeLiveAgentForTest(t, ctx, client, acp.InitializeRequest{})

	// A session selects the effective native provider configuration.
	session, err := conn.NewSession(ctx, acp.NewSessionRequest{Cwd: cwd, McpServers: []acp.McpServer{}})
	if err != nil {
		t.Fatalf("new session: %v", err)
	}
	if session.SessionId == "" {
		t.Fatal("session id is empty")
	}

	raw, err := conn.CallExtension(ctx, codexacp.RateLimitsMethod, map[string]any{"sessionId": session.SessionId})
	if err != nil {
		t.Fatalf("call %s: %v", codexacp.RateLimitsMethod, err)
	}

	var resp codexacp.RateLimitsResponse
	if err := json.Unmarshal(raw, &resp); err != nil {
		t.Fatalf("decode rate limits response %s: %v", raw, err)
	}

	if resp.Pools == nil {
		t.Fatalf("pools missing from response")
	}
	if resp.ProviderID == "" || resp.Availability == "" {
		t.Fatal("quota response missing provider or availability")
	}
	for _, pool := range resp.Pools {
		if pool.ID == "" || pool.Windows == nil {
			t.Fatal("quota pool missing id or windows")
		}
		for _, window := range pool.Windows {
			if window.ID != "primary" && window.ID != "secondary" {
				t.Fatalf("unexpected window id %q", window.ID)
			}
			if window.UsedPercent == nil || *window.UsedPercent < 0 {
				t.Fatal("window has no measured utilization")
			}
			if _, err := time.Parse(time.RFC3339Nano, window.ObservedAt); err != nil {
				t.Fatal("window observation is not RFC3339")
			}
		}
	}
}
