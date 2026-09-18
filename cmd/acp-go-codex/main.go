package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/signal"

	codexacp "github.com/savid/acp-go-codex"
	"github.com/savid/acp-go-core/process"
)

func main() {
	if code := run(context.Background(), os.Args[1:], os.Stdin, os.Stdout, os.Stderr); code != 0 {
		os.Exit(code)
	}
}

func run(ctx context.Context, args []string, stdin io.Reader, stdout io.Writer, stderr io.Writer) int {
	flags := flag.NewFlagSet("acp-go-codex", flag.ContinueOnError)
	flags.SetOutput(stderr)

	executablePath := flags.String("path", "", "codex executable; a bare name is searched on PATH")
	home := flags.String("home", "", "Codex home passed as CODEX_HOME; empty inherits Codex's own resolution")
	scratchDir := flags.String("scratch-dir", "", "additional read root for image output written outside the workspace")
	model := flags.String("model", "", "default model for new sessions")
	seedFiles := &process.SeedFileFlag{}
	flags.Var(seedFiles, "seed-file", "file seeded into Codex's home as <relpath>=<hostpath>; repeatable")
	debug := flags.Bool("debug", false, "write debug logs to stderr")
	printVersion := flags.Bool("version", false, "print adapter version and exit")

	if err := flags.Parse(args); err != nil {
		return 2
	}

	if *printVersion {
		_, _ = fmt.Fprintln(stdout, version())

		return 0
	}

	level := slog.LevelWarn
	if *debug {
		level = slog.LevelDebug
	}

	logger := slog.New(slog.NewTextHandler(stderr, &slog.HandlerOptions{Level: level}))

	telemetry, telemetryOptions, err := configureTelemetry(ctx, logger, version())
	if err != nil {
		_, _ = fmt.Fprintf(stderr, "acp-go-codex: configure OpenTelemetry: %v\n", err)

		return 1
	}

	logger = telemetry.Logger

	ctx, stop := signal.NotifyContext(ctx, forwardedSignals()...)
	defer stop()

	options := []codexacp.Option{
		codexacp.WithAgentVersion(version()),
		codexacp.WithExecutablePath(*executablePath),
		codexacp.WithHome(*home),
		codexacp.WithScratchDir(*scratchDir),
		codexacp.WithDefaultModel(*model),
		codexacp.WithLogger(logger),
	}
	if len(seedFiles.Files) > 0 {
		options = append(options, codexacp.WithSeedFiles(seedFiles.Files))
	}

	options = append(options, telemetryOptions...)

	serveErr := codexacp.Serve(ctx, stdin, stdout, options...)
	shutdownErr := telemetry.Shutdown(context.Background())

	if serveErr != nil && ctx.Err() == nil {
		_, _ = fmt.Fprintf(stderr, "acp-go-codex: %v\n", serveErr)

		return 1
	}

	if shutdownErr != nil {
		_, _ = fmt.Fprintf(stderr, "acp-go-codex: shutdown OpenTelemetry: %v\n", shutdownErr)

		return 1
	}

	return 0
}
