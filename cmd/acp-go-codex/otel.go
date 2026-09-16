package main

import (
	"context"
	"log/slog"

	codexacp "github.com/savid/acp-go-codex"
	"github.com/savid/acp-go-core/observer/exporters"
)

// configureTelemetry builds the OTEL_*-configured providers and maps the ones
// that are enabled onto the agent's options.
func configureTelemetry(ctx context.Context, baseLogger *slog.Logger, version string) (exporters.Bundle, []codexacp.Option, error) {
	bundle, err := exporters.Configure(ctx, exporters.Config{Vendor: "codex", Version: version, Logger: baseLogger})
	if err != nil {
		return exporters.Bundle{}, nil, err
	}

	options := []codexacp.Option{codexacp.WithTextMapPropagator(bundle.Propagator)}

	if bundle.TracerProvider != nil {
		options = append(options, codexacp.WithTracerProvider(bundle.TracerProvider))
	}

	if bundle.MeterProvider != nil {
		options = append(options, codexacp.WithMeterProvider(bundle.MeterProvider))
	}

	return bundle, options, nil
}
