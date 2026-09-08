//go:build integration

package codex

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"testing"
	"time"
)

func TestCodexOTELStrictConfigIntegration(t *testing.T) {
	codexPath := integrationCodexCLI(t)
	ca, clientCert, clientKey := integrationOTELCerts(t)

	cases := map[string]map[string]string{
		"http_binary_all_signals": {
			codexOTELEnvEnvironment:                 "strict-http",
			"OTEL_LOGS_EXPORTER":                    "otlp",
			"OTEL_TRACES_EXPORTER":                  "otlp",
			"OTEL_METRICS_EXPORTER":                 "otlp",
			otelEnvExporterOTLPEndpoint:             "http://127.0.0.1:14318",
			otelEnvExporterOTLPProtocol:             "http/protobuf",
			"OTEL_EXPORTER_OTLP_CERTIFICATE":        ca,
			"OTEL_EXPORTER_OTLP_CLIENT_CERTIFICATE": clientCert,
			"OTEL_EXPORTER_OTLP_CLIENT_KEY":         clientKey,
		},
		"http_json_all_signals": {
			codexOTELEnvEnvironment:     "strict-json",
			"OTEL_LOGS_EXPORTER":        "otlp",
			"OTEL_TRACES_EXPORTER":      "otlp",
			"OTEL_METRICS_EXPORTER":     "otlp",
			otelEnvExporterOTLPEndpoint: "http://127.0.0.1:14318",
			otelEnvExporterOTLPProtocol: "http/json",
		},
		"grpc_all_signals": {
			codexOTELEnvEnvironment:                 "strict-grpc",
			"OTEL_LOGS_EXPORTER":                    "otlp",
			"OTEL_TRACES_EXPORTER":                  "otlp",
			"OTEL_METRICS_EXPORTER":                 "otlp",
			otelEnvExporterOTLPEndpoint:             "http://127.0.0.1:14317",
			otelEnvExporterOTLPProtocol:             "grpc",
			"OTEL_EXPORTER_OTLP_CERTIFICATE":        ca,
			"OTEL_EXPORTER_OTLP_CLIENT_CERTIFICATE": clientCert,
			"OTEL_EXPORTER_OTLP_CLIENT_KEY":         clientKey,
		},
	}

	for name, env := range cases {
		t.Run(name, func(t *testing.T) {
			config, err := OTELConfigFromEnv(env)
			if err != nil {
				t.Fatalf("build OTEL config: %v", err)
			}
			if !config.Enabled {
				t.Fatal("generated OTEL config was disabled")
			}
			runCodexStrictConfig(t, codexPath, config.ExtraArgs)
		})
	}
}

func integrationCodexCLI(t *testing.T) string {
	t.Helper()

	if os.Getenv(envRunIntegration) != "1" {
		t.Skipf("set %s=1 to run live Codex integration tests", envRunIntegration)
	}

	path := os.Getenv(envIntegrationCodexPath)
	if path == "" {
		path = "codex"
	}
	resolved, err := exec.LookPath(path)
	if err != nil {
		if os.Getenv(envLiveTurn) == "1" || os.Getenv("ACP_GO_CODEX_RUN_ATTENDED") == "1" || os.Getenv("ACP_GO_CODEX_RUN_KEYSTORE") == "1" {
			t.Fatalf("find codex CLI: %v", err)
		}
		t.Skipf("%s=1 smoke requires a Codex CLI: %v", envRunIntegration, err)
	}

	return resolved
}

func runCodexStrictConfig(t *testing.T, codexPath string, extraArgs []string) {
	t.Helper()

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	args := []string{"app-server", "--listen", "stdio://", "--disable", "plugins", "--strict-config"}
	args = append(args, extraArgs...)
	cmd := exec.CommandContext(ctx, codexPath, args...) // #nosec G204 -- integration test uses caller-selected Codex CLI.
	home := t.TempDir()
	cmd.Dir = home
	for key, value := range integrationNativeEnv(home) {
		cmd.Env = append(cmd.Env, key+"="+value)
	}
	sort.Strings(cmd.Env)
	cmd.WaitDelay = time.Second
	cmd.Stdin = strings.NewReader("")
	output, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("codex strict config failed: %v\nargs: %s\noutput:\n%s", err, strings.Join(args, " "), string(output))
	}
}

func integrationOTELCerts(t *testing.T) (string, string, string) {
	t.Helper()

	dir := t.TempDir()
	caKey := filepath.Join(dir, "ca.key")
	caCert := filepath.Join(dir, "ca.pem")
	clientKey := filepath.Join(dir, "client.key")
	clientCSR := filepath.Join(dir, "client.csr")
	clientCert := filepath.Join(dir, "client.pem")
	clientExt := filepath.Join(dir, "client.ext")

	runOpenSSL(t, "genrsa", "-out", caKey, "2048")
	runOpenSSL(t, "req", "-x509", "-new", "-nodes", "-key", caKey, "-sha256", "-days", "1", "-subj", "/CN=acp-go-codex-test-ca", "-out", caCert)
	runOpenSSL(t, "genrsa", "-out", clientKey, "2048")
	runOpenSSL(t, "req", "-new", "-key", clientKey, "-subj", "/CN=acp-go-codex-test-client", "-out", clientCSR)
	if err := os.WriteFile(clientExt, []byte("basicConstraints=CA:FALSE\nkeyUsage=digitalSignature,keyEncipherment\nextendedKeyUsage=clientAuth\n"), 0o600); err != nil {
		t.Fatalf("write client cert extension: %v", err)
	}
	runOpenSSL(t, "x509", "-req", "-in", clientCSR, "-CA", caCert, "-CAkey", caKey, "-CAcreateserial", "-out", clientCert, "-days", "1", "-sha256", "-extfile", clientExt)

	return caCert, clientCert, clientKey
}

func runOpenSSL(t *testing.T, args ...string) {
	t.Helper()

	path, err := exec.LookPath("openssl")
	if err != nil {
		t.Fatalf("find openssl: %v", err)
	}
	cmd := exec.Command(path, args...) // #nosec G204 -- test controls openssl arguments.
	if output, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("openssl %s failed: %v\n%s", strings.Join(args, " "), err, string(output))
	}
}

// integrationNativeEnv carries only process lookup and test-owned residences.
// No provider credential, native config, or ambient telemetry setting is inherited.
func integrationNativeEnv(home string) map[string]string {
	env := map[string]string{
		"HOME": home, "USERPROFILE": home, "CODEX_HOME": home,
		"XDG_CONFIG_HOME": filepath.Join(home, "config"),
		"XDG_CACHE_HOME":  filepath.Join(home, "cache"),
		"XDG_DATA_HOME":   filepath.Join(home, "data"),
		"TMPDIR":          home, "TMP": home, "TEMP": home,
	}
	for _, key := range []string{"PATH", "SystemRoot", "SYSTEMROOT", "WINDIR", "PATHEXT"} {
		if value, ok := os.LookupEnv(key); ok {
			env[key] = value
		}
	}
	return env
}

func TestStrictConfigProbeEnvironment(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("shell recorder requires Unix")
	}
	for _, key := range []string{"OPENAI_API_KEY", "CODEX_HOME", "HOME", "XDG_CONFIG_HOME", "OTEL_EXPORTER_OTLP_HEADERS"} {
		t.Setenv(key, "ambient-must-not-reach-probe")
	}
	dir := t.TempDir()
	recorder := filepath.Join(dir, "record-probe")
	record := filepath.Join(dir, "received")
	script := "#!/bin/sh\nfor arg do last=$arg; done\n{ pwd; env; printf '%s\\n' \"$@\"; } > \"$last\"\n"
	if err := os.WriteFile(recorder, []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	runCodexStrictConfig(t, recorder, []string{"-c", "otel.environment=\"probe-only\"", record})
	data, err := os.ReadFile(record)
	if err != nil {
		t.Fatal(err)
	}
	output := string(data)
	if strings.Contains(output, "ambient-must-not-reach-probe") {
		t.Fatal("probe inherited ambient configuration or credentials")
	}
	lines := strings.Split(output, "\n")
	if !strings.Contains(output, "CODEX_HOME="+lines[0]+"\n") || !strings.Contains(output, "HOME="+lines[0]+"\n") {
		t.Fatalf("probe home and cwd differ: %s", output)
	}
	if !strings.Contains(output, "otel.environment=\"probe-only\"") {
		t.Fatal("probe lost intended OTEL argument")
	}
}
