//go:build integration

package codex

import (
	"context"
	"os"
	"strings"
	"testing"
	"time"
)

const (
	envRunIntegration       = "ACP_GO_CODEX_RUN_INTEGRATION"
	envIntegrationCodexPath = "ACP_GO_CODEX_HARNESS_PATH"
	envLiveTurn             = "ACP_GO_CODEX_RUN_LIVE_TOKENS"
)

func TestIntegrationAppServerSmoke(t *testing.T) {
	if os.Getenv(envRunIntegration) != "1" {
		t.Skipf("set %s=1 to run live Codex app-server integration tests", envRunIntegration)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
	defer cancel()

	codexPath := integrationCodexCLI(t)
	home := t.TempDir()
	client, err := NewAppServerClient(ctx, integrationAppServerOptions(codexPath, home, t.TempDir()))
	if err != nil {
		t.Fatalf("NewAppServerClient returned error: %v", err)
	}
	t.Cleanup(func() {
		if closeErr := client.Close(context.Background()); closeErr != nil {
			t.Fatalf("Close returned error: %v", closeErr)
		}
	})

	models, err := client.ModelList(ctx)
	if err != nil {
		t.Fatalf("ModelList returned error: %v", err)
	}
	if len(models) == 0 {
		t.Fatal("ModelList returned no models")
	}
	if _, collabErr := client.CollaborationModeList(ctx); collabErr != nil {
		t.Fatalf("CollaborationModeList returned error: %v", collabErr)
	}
	thread, err := client.StartThread(ctx, ThreadStartRequest{
		Cwd:   t.TempDir(),
		Model: models[0].ID,
	})
	if err != nil {
		t.Fatalf("StartThread returned error: %v", err)
	}
	if thread.ID == "" {
		t.Fatalf("thread missing ID: %#v", thread)
	}
	if _, mcpErr := client.MCPServerStatusList(ctx, thread.ID); mcpErr != nil {
		t.Fatalf("MCPServerStatusList returned error: %v", mcpErr)
	}
	terminals, terminalsErr := client.ListBackgroundTerminals(ctx, BackgroundTerminalListRequest{ThreadID: thread.ID})
	if terminalsErr != nil {
		t.Fatalf("ListBackgroundTerminals returned error: %v", terminalsErr)
	}
	if len(terminals.Terminals) != 0 {
		t.Fatalf("new thread unexpectedly has background terminals: %#v", terminals.Terminals)
	}
	t.Cleanup(func() {
		if unsubErr := client.UnsubscribeThread(context.Background(), thread.ID); unsubErr != nil {
			t.Fatalf("UnsubscribeThread returned error: %v", unsubErr)
		}
	})

	if _, readErr := client.ReadThread(ctx, ThreadReadRequest{ThreadID: thread.ID}); readErr != nil {
		t.Fatalf("ReadThread returned error: %v", readErr)
	}
	if os.Getenv(envLiveTurn) == "1" {
		runLiveTurn(ctx, t, client, thread.ID)
		if _, listErr := client.ListTurns(ctx, ThreadTurnsListRequest{ThreadID: thread.ID, Limit: 10}); listErr != nil {
			t.Fatalf("ListTurns after live turn returned error: %v", listErr)
		}
	} else if _, listErr := client.ListTurns(ctx, ThreadTurnsListRequest{ThreadID: thread.ID, Limit: 10}); listErr != nil && !strings.Contains(listErr.Error(), "not materialized yet") {
		t.Fatalf("ListTurns before materialization returned unexpected error: %v", listErr)
	}
}

func runLiveTurn(ctx context.Context, t *testing.T, client Client, threadID string) {
	t.Helper()
	feed, err := client.SubscribeThread(ctx, threadID)
	if err != nil {
		t.Fatalf("SubscribeThread returned error: %v", err)
	}
	defer feed.Release()

	_, err = client.RunTurn(ctx, TurnStartRequest{
		ThreadID: threadID,
		Prompt: []UserInput{{
			"type": "text",
			"text": "Reply with exactly CODEX_INTEGRATION_OK and do not use tools.",
		}},
	})
	if err != nil {
		t.Fatalf("RunTurn returned error: %v", err)
	}

	for {
		var event Event
		select {
		case <-ctx.Done():
			t.Fatalf("live turn did not settle: %v", ctx.Err())
		case next, ok := <-feed.Events:
			if !ok {
				t.Fatal("live event feed closed before completion")
			}
			event = next
		}
		if event.Kind == EventError {
			t.Fatalf("live turn event error: %v", event.Err)
		}
		if event.Kind == EventCompleted {
			return
		}
	}
}

func integrationAppServerOptions(path, home, scratch string) Options {
	options := Options{
		CLIPath: path, CodexHome: home, Scratch: scratch, NativeVersion: minCodexVersion,
		ImplicitEnvironment: integrationNativeEnv(home),
	}
	if os.Getenv(envRunIntegration) == "1" && os.Getenv(envLiveTurn) == "1" {
		if key := os.Getenv("OPENAI_API_KEY"); key != "" {
			options.Env = map[string]string{"OPENAI_API_KEY": key}
		}
	}
	return options
}

func TestIntegrationAppServerEnvironment(t *testing.T) {
	for _, key := range []string{"ANTHROPIC_API_KEY", "OPENAI_API_KEY", "CODEX_HOME", "HOME", "XDG_CONFIG_HOME", "OTEL_EXPORTER_OTLP_HEADERS"} {
		t.Setenv(key, "ambient-must-not-reach-appserver")
	}
	for _, tc := range []struct {
		name, integration, live string
		wantKey                 bool
	}{
		{"smoke", "1", "0", false},
		{"live_without_integration", "0", "1", false},
		{"live", "1", "1", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv(envRunIntegration, tc.integration)
			t.Setenv(envLiveTurn, tc.live)
			home := t.TempDir()
			env, err := buildMergedEnv(integrationAppServerOptions("unused-test-cli", home, t.TempDir()))
			if err != nil {
				t.Fatal(err)
			}
			values := environmentMap(env)
			for _, key := range []string{"ANTHROPIC_API_KEY", "OTEL_EXPORTER_OTLP_HEADERS"} {
				if _, ok := values[key]; ok {
					t.Fatalf("ambient %s reached launch", key)
				}
			}
			if got := values["OPENAI_API_KEY"]; (got == "ambient-must-not-reach-appserver") != tc.wantKey {
				t.Fatalf("live key present = %t, want %t", got != "", tc.wantKey)
			}
			for _, key := range []string{"HOME", "CODEX_HOME"} {
				if values[key] != home {
					t.Fatalf("%s is not test-owned", key)
				}
			}
			if values["XDG_CONFIG_HOME"] == "ambient-must-not-reach-appserver" {
				t.Fatal("ambient config reached launch")
			}
		})
	}
}
