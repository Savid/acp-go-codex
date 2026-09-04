package codexacp

import (
	"context"
	"log/slog"
	"testing"
	"time"

	"github.com/savid/acp-go-codex/internal/codex"
	metricnoop "go.opentelemetry.io/otel/metric/noop"
	"go.opentelemetry.io/otel/propagation"
	tracenoop "go.opentelemetry.io/otel/trace/noop"
)

func TestOptionsSetters(t *testing.T) {
	logger := slog.Default()
	store := NewInMemorySessionStore()
	tracerProvider := tracenoop.NewTracerProvider()
	meterProvider := metricnoop.NewMeterProvider()
	propagator := propagation.TraceContext{}
	options := applyOptions([]Option{
		WithExecutablePath("/bin/codex"),
		WithHome("/tmp/codex"),
		WithScratchDir("/tmp/codex-scratch"),
		WithDefaultModel("gpt-5.5"),
		WithLogger(logger),
		WithEnv(map[string]string{"A": "B"}),
		WithSessionStore(store),
		WithSessionStoreLoadTimeout(time.Second),
		WithConcurrencyLimits(ConcurrencyLimits{MaxActiveSessions: 1, MaxConcurrentClientCalls: 3}),
		WithSeedFiles(map[string]string{"config.toml": "model = \"gpt-5.5\"\n"}),
		WithCodexConfigOverrides(map[string]any{"model_provider": "litellm"}),
		WithTracerProvider(tracerProvider),
		WithMeterProvider(meterProvider),
		WithTextMapPropagator(propagator),
	})
	if options.ExecutablePath != "/bin/codex" ||
		options.Home != "/tmp/codex" ||
		options.ScratchDir != "/tmp/codex-scratch" ||
		options.DefaultModel != "gpt-5.5" ||
		options.Logger != logger ||
		options.Env["A"] != "B" ||
		options.SessionStore != store ||
		options.SessionStoreLoadTimeout != time.Second ||
		options.ConcurrencyLimits.MaxActiveSessions != 1 ||
		options.ConcurrencyLimits.MaxConcurrentClientCalls != 3 ||
		options.SeedFiles["config.toml"] != "model = \"gpt-5.5\"\n" ||
		options.Config["model_provider"] != "litellm" ||
		options.TracerProvider != tracerProvider ||
		options.MeterProvider != meterProvider ||
		options.TextMapPropagator == nil {
		t.Fatalf("options = %#v", options)
	}
	options.Env["A"] = "changed"
	original := map[string]string{"A": "B"}
	WithEnv(original)(&options)
	original["A"] = "mutated"
	if options.Env["A"] != "B" {
		t.Fatalf("WithEnv did not clone input: %#v", options.Env)
	}
	options.SeedFiles["config.toml"] = "changed"
	originalSeed := map[string]string{"config.toml": "seed"}
	WithSeedFiles(originalSeed)(&options)
	originalSeed["config.toml"] = "mutated"
	if options.SeedFiles["config.toml"] != "seed" {
		t.Fatalf("WithSeedFiles did not clone input: %#v", options.SeedFiles)
	}
	options.Config["model_provider"] = "changed"
	originalConfig := map[string]any{"model_provider": "litellm"}
	WithCodexConfigOverrides(originalConfig)(&options)
	originalConfig["model_provider"] = "mutated"
	if options.Config["model_provider"] != "litellm" {
		t.Fatalf("WithCodexConfigOverrides did not clone input: %#v", options.Config)
	}
	if _, err := applyOptions(nil).clientFactory(context.Background(), codex.Options{CLIPath: "/definitely/missing/codex"}); err == nil {
		t.Fatal("default client factory accepted missing codex path")
	}
}

func TestCaptureAmbientEnvironmentFallsBackToUserHome(t *testing.T) {
	t.Setenv(managedHomeEnv, "")
	original := runtimeUserHomeDir
	runtimeUserHomeDir = func() (string, error) { return "/fallback/home", nil }
	t.Cleanup(func() { runtimeUserHomeDir = original })

	environment := captureAmbientEnvironment()
	if environment[managedHomeEnv] != "/fallback/home" {
		t.Fatalf("HOME = %q", environment[managedHomeEnv])
	}
}
