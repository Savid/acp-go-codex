package codexacp

import (
	"context"
	"strings"
	"testing"

	"github.com/coder/acp-go-sdk"
	"github.com/savid/acp-go-codex/internal/codex"
)

func TestCodexOTELEffectiveEnvPrecedence(t *testing.T) {
	original := osEnviron
	t.Cleanup(func() { osEnviron = original })
	osEnviron = func() []string {
		return []string{
			"OTEL_TRACES_EXPORTER=none",
			"BASE=os",
			"MALFORMED",
			"=empty",
		}
	}

	env := codexOTELEffectiveEnv(
		map[string]string{"OTEL_TRACES_EXPORTER": "otlp", "BASE": "agent"},
		map[string]string{"BASE": "session", "REQUEST": "session"},
	)
	if env["OTEL_TRACES_EXPORTER"] != "otlp" || env["BASE"] != "session" || env["REQUEST"] != "session" {
		t.Fatalf("effective env = %#v", env)
	}
	if _, ok := env[""]; ok {
		t.Fatalf("effective env kept empty key: %#v", env)
	}
}

func TestAgentNewClientMergesCodexOTELWithMCPAndEnv(t *testing.T) {
	original := osEnviron
	t.Cleanup(func() { osEnviron = original })
	osEnviron = func() []string { return nil }

	var gotOptions codex.Options
	agent := NewAgent(
		WithEnv(map[string]string{
			"OTEL_TRACES_EXPORTER":        "none",
			"OTEL_EXPORTER_OTLP_ENDPOINT": "http://collector:4318",
			"BASE":                        "agent",
		}),
		withClientFactory(func(_ context.Context, options codex.Options) (codex.Client, error) {
			gotOptions = options

			return newSpyCodexClient(), nil
		}),
	)

	_, err := agent.newClient(context.Background(), []acp.McpServer{{
		Stdio: &acp.McpServerStdio{Name: "Echo", Command: "echo"},
	}}, map[string]string{
		"OTEL_TRACES_EXPORTER":        "otlp",
		"OTEL_EXPORTER_OTLP_PROTOCOL": "grpc",
		"OTEL_METRICS_EXPORTER":       "none",
		"BASE":                        "session",
	}, "")
	if err != nil {
		t.Fatalf("newClient returned error: %v", err)
	}
	if gotOptions.Env["BASE"] != "session" {
		t.Fatalf("Codex env precedence = %#v", gotOptions.Env)
	}
	args := strings.Join(gotOptions.ExtraArgs, " ")
	requireContainsAll(t, args,
		"--analytics-default-enabled",
		`otel.trace_exporter="otlp-grpc"`,
		`otel.trace_exporter.otlp-grpc.endpoint="http://collector:4318"`,
		`mcp_servers.Echo.command="echo"`,
	)
	requireNotContainsAny(t, args, `otel.metrics_exporter`)
}

func TestAgentNewClientReturnsCodexOTELError(t *testing.T) {
	agent := NewAgent(
		WithEnv(map[string]string{
			"OTEL_TRACES_EXPORTER":               "otlp",
			"OTEL_EXPORTER_OTLP_TRACES_PROTOCOL": "invalid",
		}),
		withClientFactory(func(context.Context, codex.Options) (codex.Client, error) {
			t.Fatal("client factory called after invalid Codex OTEL config")

			return newSpyCodexClient(), nil
		}),
	)
	if _, err := agent.newClient(context.Background(), nil, nil, ""); err == nil {
		t.Fatal("newClient accepted invalid Codex OTEL config")
	}
}

func requireContainsAll(t *testing.T, value string, wants ...string) {
	t.Helper()
	for _, want := range wants {
		if !strings.Contains(value, want) {
			t.Fatalf("%q did not contain %q", value, want)
		}
	}
}

func requireNotContainsAny(t *testing.T, value string, unwanted ...string) {
	t.Helper()
	for _, item := range unwanted {
		if strings.Contains(value, item) {
			t.Fatalf("%q unexpectedly contained %q", value, item)
		}
	}
}
