package codexacp

import (
	"context"
	"strings"
	"testing"

	"github.com/coder/acp-go-sdk"
	"github.com/savid/acp-go-codex/internal/codex"
)

func TestCodexOTELConfigDisabledByDefault(t *testing.T) {
	config, err := codexOTELConfigFromEnv(map[string]string{
		"OTEL_SERVICE_NAME": "ignored-for-codex-native",
	})
	if err != nil {
		t.Fatalf("codexOTELConfigFromEnv returned error: %v", err)
	}
	if config.Enabled || len(config.ExtraArgs) != 0 {
		t.Fatalf("empty OTEL config = %#v", config)
	}
}

func TestCodexOTELHTTPMapping(t *testing.T) {
	config, err := codexOTELConfigFromEnv(map[string]string{
		codexOTELEnvEnvironment:          "production",
		codexOTELEnvLogUserPrompt:        "yes",
		"OTEL_LOGS_EXPORTER":             "otlp",
		"OTEL_TRACES_EXPORTER":           "otlp",
		"OTEL_METRICS_EXPORTER":          "otlp",
		otelEnvExporterOTLPEndpoint:      "http://collector:4318/otlp",
		otelEnvExporterOTLPProtocol:      "http/json",
		otelEnvExporterOTLPHeaders:       "authorization=secret",
		"OTEL_SERVICE_NAME":              "not-codex-app-server",
		"OTEL_RESOURCE_ATTRIBUTES":       "service.name=also-ignored,deployment.environment=ignored",
		"OTEL_EXPORTER_OTLP_CERTIFICATE": "/ca.pem",
	})
	if err != nil {
		t.Fatalf("codexOTELConfigFromEnv returned error: %v", err)
	}

	args := strings.Join(config.ExtraArgs, " ")
	requireContainsAll(t, args,
		"--analytics-default-enabled",
		`otel.environment="production"`,
		`otel.exporter="otlp-http"`,
		`otel.exporter.otlp-http.endpoint="http://collector:4318/otlp/v1/logs"`,
		`otel.exporter.otlp-http.protocol="json"`,
		`otel.exporter.otlp-http.tls.ca-certificate="/ca.pem"`,
		`otel.trace_exporter="otlp-http"`,
		`otel.trace_exporter.otlp-http.endpoint="http://collector:4318/otlp/v1/traces"`,
		`otel.trace_exporter.otlp-http.protocol="json"`,
		`otel.trace_exporter.otlp-http.tls.ca-certificate="/ca.pem"`,
		`otel.metrics_exporter={ "otlp-http" = { endpoint = "http://collector:4318/otlp/v1/metrics", protocol = "json", tls = { ca-certificate = "/ca.pem" } } }`,
		`otel.log_user_prompt=true`,
	)
	requireNotContainsAny(t, args, "authorization=secret", "OTEL_SERVICE_NAME", "not-codex-app-server", "service.name")
}

func TestCodexOTELGRPCMappingAndSignalOverrides(t *testing.T) {
	config, err := codexOTELConfigFromEnv(map[string]string{
		"OTEL_LOGS_EXPORTER":                         "otlp",
		"OTEL_EXPORTER_OTLP_LOGS_PROTOCOL":           "grpc",
		"OTEL_EXPORTER_OTLP_LOGS_ENDPOINT":           "http://logs:4317",
		"OTEL_EXPORTER_OTLP_LOGS_CERTIFICATE":        "/logs-ca.pem",
		"OTEL_EXPORTER_OTLP_LOGS_CLIENT_CERTIFICATE": "/logs-client.pem",
		"OTEL_EXPORTER_OTLP_LOGS_CLIENT_KEY":         "/logs-client.key",
		"OTEL_TRACES_EXPORTER":                       "otlp",
		otelEnvExporterOTLPProtocol:                  "http/protobuf",
		"OTEL_EXPORTER_OTLP_TRACES_PROTOCOL":         "grpc",
		"OTEL_EXPORTER_OTLP_TRACES_ENDPOINT":         "http://traces:4317",
		"OTEL_EXPORTER_OTLP_CERTIFICATE":             "/generic-ca.pem",
		"OTEL_EXPORTER_OTLP_CLIENT_CERTIFICATE":      "/generic-client.pem",
		"OTEL_EXPORTER_OTLP_CLIENT_KEY":              "/generic-client.key",
		"OTEL_METRICS_EXPORTER":                      "otlp",
		"OTEL_EXPORTER_OTLP_METRICS_PROTOCOL":        "grpc",
	})
	if err != nil {
		t.Fatalf("codexOTELConfigFromEnv returned error: %v", err)
	}

	args := strings.Join(config.ExtraArgs, " ")
	requireContainsAll(t, args,
		`otel.exporter="otlp-grpc"`,
		`otel.exporter.otlp-grpc.endpoint="http://logs:4317"`,
		`otel.exporter.otlp-grpc.tls.ca-certificate="/logs-ca.pem"`,
		`otel.exporter.otlp-grpc.tls.client-certificate="/logs-client.pem"`,
		`otel.exporter.otlp-grpc.tls.client-private-key="/logs-client.key"`,
		`otel.trace_exporter="otlp-grpc"`,
		`otel.trace_exporter.otlp-grpc.endpoint="http://traces:4317"`,
		`otel.trace_exporter.otlp-grpc.tls.ca-certificate="/generic-ca.pem"`,
		`otel.trace_exporter.otlp-grpc.tls.client-certificate="/generic-client.pem"`,
		`otel.trace_exporter.otlp-grpc.tls.client-private-key="/generic-client.key"`,
		`otel.metrics_exporter={ "otlp-grpc" = { endpoint = "http://localhost:4317", tls = { ca-certificate = "/generic-ca.pem", client-certificate = "/generic-client.pem", client-private-key = "/generic-client.key" } } }`,
		`otel.log_user_prompt=false`,
	)
}

func TestCodexOTELSignalEnablementAndDefaults(t *testing.T) {
	config, err := codexOTELConfigFromEnv(map[string]string{
		"OTEL_TRACES_EXPORTER": "otlp",
	})
	if err != nil {
		t.Fatalf("trace default config returned error: %v", err)
	}
	args := strings.Join(config.ExtraArgs, " ")
	requireContainsAll(t, args,
		`otel.trace_exporter="otlp-http"`,
		`otel.trace_exporter.otlp-http.endpoint="http://localhost:4318/v1/traces"`,
		`otel.trace_exporter.otlp-http.protocol="binary"`,
	)

	config, err = codexOTELConfigFromEnv(map[string]string{
		otelEnvExporterOTLPProtocol: "grpc",
	})
	if err != nil {
		t.Fatalf("generic grpc config returned error: %v", err)
	}
	args = strings.Join(config.ExtraArgs, " ")
	requireContainsAll(t, args,
		`otel.trace_exporter="otlp-grpc"`,
		`otel.trace_exporter.otlp-grpc.endpoint="http://localhost:4317"`,
		`otel.metrics_exporter={ "otlp-grpc" = { endpoint = "http://localhost:4317" } }`,
	)
	requireNotContainsAny(t, args, `otel.exporter=`)
}

func TestCodexOTELExplicitDisableAndUnsupportedExporters(t *testing.T) {
	config, err := codexOTELConfigFromEnv(map[string]string{
		"OTEL_TRACES_EXPORTER":      "none",
		"OTEL_METRICS_EXPORTER":     "console",
		"OTEL_LOGS_EXPORTER":        "stdout",
		otelEnvExporterOTLPEndpoint: "http://collector:4318",
	})
	if err != nil {
		t.Fatalf("disabled config returned error: %v", err)
	}
	if config.Enabled {
		t.Fatalf("unsupported/disabled exporters enabled native Codex OTEL: %#v", config)
	}
}

func TestCodexOTELHeadersAloneDoNotEnableNativeTelemetry(t *testing.T) {
	config, err := codexOTELConfigFromEnv(map[string]string{
		otelEnvExporterOTLPHeaders: "authorization=secret",
	})
	if err != nil {
		t.Fatalf("headers-only config returned error: %v", err)
	}
	if config.Enabled {
		t.Fatalf("headers-only env enabled native Codex OTEL: %#v", config)
	}
}

func TestCodexOTELInvalidProtocol(t *testing.T) {
	_, err := codexOTELConfigFromEnv(map[string]string{
		"OTEL_TRACES_EXPORTER":               "otlp",
		"OTEL_EXPORTER_OTLP_TRACES_PROTOCOL": "invalid",
	})
	if err == nil || !strings.Contains(err.Error(), "unsupported OTLP protocol") {
		t.Fatalf("invalid protocol error = %v", err)
	}

	_, err = codexOTELConfigFromEnv(map[string]string{
		"OTEL_LOGS_EXPORTER":               "otlp",
		"OTEL_EXPORTER_OTLP_LOGS_PROTOCOL": "invalid",
	})
	if err == nil || !strings.Contains(err.Error(), "unsupported OTLP protocol") {
		t.Fatalf("invalid logs protocol error = %v", err)
	}

	_, err = codexOTELConfigFromEnv(map[string]string{
		"OTEL_METRICS_EXPORTER":               "otlp",
		"OTEL_EXPORTER_OTLP_METRICS_PROTOCOL": "invalid",
	})
	if err == nil || !strings.Contains(err.Error(), "unsupported OTLP protocol") {
		t.Fatalf("invalid metrics protocol error = %v", err)
	}
}

func TestCodexOTELSignalEndpointEnablesLogs(t *testing.T) {
	config, err := codexOTELConfigFromEnv(map[string]string{
		"OTEL_EXPORTER_OTLP_LOGS_ENDPOINT": "http://logs.example/v1/logs",
	})
	if err != nil {
		t.Fatalf("logs endpoint config returned error: %v", err)
	}
	args := strings.Join(config.ExtraArgs, " ")
	requireContainsAll(t, args,
		`otel.exporter="otlp-http"`,
		`otel.exporter.otlp-http.endpoint="http://logs.example/v1/logs"`,
	)
}

func TestCodexOTELEnvironmentFromResourceAttributes(t *testing.T) {
	config, err := codexOTELConfigFromEnv(map[string]string{
		"OTEL_TRACES_EXPORTER":    "otlp",
		otelEnvResourceAttributes: "service.namespace=agent,deployment.environment=prod,deployment.environment.name=blue",
	})
	if err != nil {
		t.Fatalf("environment config returned error: %v", err)
	}
	args := strings.Join(config.ExtraArgs, " ")
	requireContainsAll(t, args, `otel.environment="blue"`)

	config, err = codexOTELConfigFromEnv(map[string]string{
		"OTEL_TRACES_EXPORTER":    "otlp",
		otelEnvResourceAttributes: "bad,deployment.environment=prod",
	})
	if err != nil {
		t.Fatalf("fallback environment config returned error: %v", err)
	}
	args = strings.Join(config.ExtraArgs, " ")
	requireContainsAll(t, args, `otel.environment="prod"`)
}

func TestCodexOTELHTTPClientCertificatesAreNotMappedForLogsOrTraces(t *testing.T) {
	config, err := codexOTELConfigFromEnv(map[string]string{
		"OTEL_LOGS_EXPORTER":                    "otlp",
		"OTEL_TRACES_EXPORTER":                  "otlp",
		"OTEL_EXPORTER_OTLP_CLIENT_CERTIFICATE": "/client.pem",
		"OTEL_EXPORTER_OTLP_CLIENT_KEY":         "/client.key",
	})
	if err != nil {
		t.Fatalf("HTTP client certificate config returned error: %v", err)
	}
	args := strings.Join(config.ExtraArgs, " ")
	requireContainsAll(t, args,
		`otel.exporter.otlp-http.endpoint="http://localhost:4318/v1/logs"`,
		`otel.trace_exporter.otlp-http.endpoint="http://localhost:4318/v1/traces"`,
	)
	requireNotContainsAny(t, args, "client-certificate", "client-private-key")
}

func TestCodexOTELMetricsHTTPClientCertificatesAreNotMapped(t *testing.T) {
	config, err := codexOTELConfigFromEnv(map[string]string{
		"OTEL_METRICS_EXPORTER":                         "otlp",
		"OTEL_EXPORTER_OTLP_METRICS_CLIENT_CERTIFICATE": "/metrics-client.pem",
		"OTEL_EXPORTER_OTLP_METRICS_CLIENT_KEY":         "/metrics-client.key",
	})
	if err != nil {
		t.Fatalf("metrics client certificate config returned error: %v", err)
	}
	args := strings.Join(config.ExtraArgs, " ")
	requireContainsAll(t, args,
		`otel.metrics_exporter={ "otlp-http" = { endpoint = "http://localhost:4318/v1/metrics", protocol = "binary" } }`,
	)
	requireNotContainsAny(t, args, "client-certificate", "client-private-key")
}

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

func TestAppendOTLPSignalPath(t *testing.T) {
	cases := map[string]string{
		"http://collector:4318":             "http://collector:4318/v1/traces",
		"http://collector:4318/root":        "http://collector:4318/root/v1/traces",
		"http://collector:4318/v1/traces":   "http://collector:4318/v1/traces",
		"collector:4318/root":               "collector:4318/root/v1/traces",
		"collector/v1/traces":               "collector/v1/traces",
		"http://collector:4318/root?x=true": "http://collector:4318/root/v1/traces?x=true",
	}
	for input, want := range cases {
		if got := appendOTLPSignalPath(input, "/v1/traces"); got != want {
			t.Fatalf("appendOTLPSignalPath(%q) = %q, want %q", input, got, want)
		}
	}
}

func TestParseOTELResourceAttributesSkipsEmptyKeys(t *testing.T) {
	attrs := parseOTELResourceAttributes("=empty,key=value,missing")
	if attrs["key"] != "value" {
		t.Fatalf("parsed attrs = %#v", attrs)
	}
	if _, ok := attrs[""]; ok {
		t.Fatalf("parsed attrs kept empty key: %#v", attrs)
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
