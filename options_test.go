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
		WithDefaultModel("gpt-5.5"),
		WithLogger(logger),
		WithEnv(map[string]string{"A": "B"}),
		WithSessionStore(store),
		WithSessionStoreLoadTimeout(time.Second),
		WithConcurrencyLimits(ConcurrencyLimits{MaxActiveSessions: 1, MaxConcurrentPrompts: 2, MaxConcurrentClientCalls: 3}),
		WithTracerProvider(tracerProvider),
		WithMeterProvider(meterProvider),
		WithTextMapPropagator(propagator),
	})
	if options.ExecutablePath != "/bin/codex" ||
		options.Home != "/tmp/codex" ||
		options.DefaultModel != "gpt-5.5" ||
		options.Logger != logger ||
		options.Env["A"] != "B" ||
		options.SessionStore != store ||
		options.SessionStoreLoadTimeout != time.Second ||
		options.ConcurrencyLimits.MaxActiveSessions != 1 ||
		options.ConcurrencyLimits.MaxConcurrentPrompts != 2 ||
		options.ConcurrencyLimits.MaxConcurrentClientCalls != 3 ||
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
	if _, err := applyOptions(nil).clientFactory(context.Background(), codex.Options{CLIPath: "/definitely/missing/codex"}); err == nil {
		t.Fatal("default client factory accepted missing codex path")
	}
}
