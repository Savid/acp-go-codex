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

	// A live session gives the fresh account/rateLimits/read path an app-server
	// to query; without one the agent falls back to any cached snapshot.
	session, err := conn.NewSession(ctx, acp.NewSessionRequest{Cwd: cwd, McpServers: []acp.McpServer{}})
	if err != nil {
		t.Fatalf("new session: %v", err)
	}
	if session.SessionId == "" {
		t.Fatal("session id is empty")
	}

	raw, err := conn.CallExtension(ctx, codexacp.RateLimitsMethod, map[string]any{})
	if err != nil {
		t.Fatalf("call %s: %v", codexacp.RateLimitsMethod, err)
	}

	var resp codexacp.RateLimitsResponse
	if err := json.Unmarshal(raw, &resp); err != nil {
		t.Fatalf("decode rate limits response %s: %v", raw, err)
	}

	// windows is always present in the wire payload, even when empty.
	if resp.Windows == nil {
		t.Fatalf("windows missing from response %s", raw)
	}

	for _, window := range resp.Windows {
		if window.ID == "" {
			t.Fatalf("window missing id: %#v (raw %s)", window, raw)
		}
		if window.ID != "primary" && window.ID != "secondary" {
			t.Fatalf("unexpected window id %q; codex reports primary/secondary", window.ID)
		}
		if window.UsedPercent < 0 || window.UsedPercent > 100 {
			t.Fatalf("window %q usedPercent %v outside [0,100]; codex protocol changed", window.ID, window.UsedPercent)
		}
		if window.ResetsAt != "" {
			if _, err := time.Parse(time.RFC3339, window.ResetsAt); err != nil {
				t.Fatalf("window %q resetsAt %q is not RFC3339: %v", window.ID, window.ResetsAt, err)
			}
		}
	}
}
