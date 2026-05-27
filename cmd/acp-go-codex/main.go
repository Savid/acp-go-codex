package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"os/signal"
	"strings"

	codexacp "github.com/savid/acp-go-codex"
)

var serve = codexacp.Serve
var agentVersion = version
var exit = os.Exit
var runCodexCLICommand = runCodexCLI
var shutdownOpenTelemetry = shutdownTelemetry

func main() {
	if code := run(context.Background(), os.Args[1:], os.Stdin, os.Stdout, os.Stderr); code != 0 {
		exit(code)
	}
}

func run(ctx context.Context, args []string, stdin io.Reader, stdout io.Writer, stderr io.Writer) int {
	if len(args) > 0 && args[0] == "mcp-proxy" {
		if err := runMCPProxy(ctx, args[1:], stdin, stdout, stderr); err != nil {
			_, _ = fmt.Fprintf(stderr, "acp-go-codex mcp-proxy: %v\n", err)
			return 1
		}

		return 0
	}

	flags := flag.NewFlagSet("acp-go-codex", flag.ContinueOnError)
	flags.SetOutput(stderr)

	codexPath := flags.String("codex", "", "path to codex CLI")
	codexHome := flags.String("codex-home", "", "Codex home directory")
	model := flags.String("model", "", "default Codex model")
	debug := flags.Bool("debug", false, "write debug logs to stderr")
	cli := flags.String("cli", "", "run a local Codex CLI auth command")
	deviceAuth := flags.Bool("device-auth", false, "use Codex device auth for --cli login")

	if err := flags.Parse(args); err != nil {
		return 2
	}
	if *cli != "" {
		if err := runCodexCLICommand(ctx, *codexPath, *codexHome, *cli, *deviceAuth, stdin, stdout, stderr); err != nil {
			_, _ = fmt.Fprintf(stderr, "acp-go-codex: %v\n", err)
			return commandExitCode(err)
		}

		return 0
	}

	logger := slog.New(slog.DiscardHandler)
	if *debug {
		logger = slog.New(slog.NewTextHandler(stderr, &slog.HandlerOptions{Level: slog.LevelDebug}))
	}

	signals := forwardedSignals()
	receivedSignals := make(chan os.Signal, 1)
	signal.Notify(receivedSignals, signals...)
	defer signal.Stop(receivedSignals)

	ctx, stop := signal.NotifyContext(ctx, signals...)
	defer stop()

	version := agentVersion()
	telemetry, err := configureTelemetry(ctx, logger, version)
	if err != nil {
		_, _ = fmt.Fprintf(stderr, "acp-go-codex: configure OpenTelemetry: %v\n", err)

		return 1
	}
	logger = telemetry.logger

	serveOptions := make([]codexacp.Option, 0, 5+len(telemetry.options))
	serveOptions = append(serveOptions,
		codexacp.WithAgentVersion(version),
		codexacp.WithCodexPath(*codexPath),
		codexacp.WithCodexHome(*codexHome),
		codexacp.WithDefaultModel(*model),
		codexacp.WithLogger(logger),
	)
	serveOptions = append(serveOptions, telemetry.options...)

	err = serve(ctx, stdin, stdout, serveOptions...)
	shutdownErr := shutdownOpenTelemetry(context.Background(), telemetry.shutdown)
	if shutdownErr != nil {
		_, _ = fmt.Fprintf(stderr, "acp-go-codex: shutdown OpenTelemetry: %v\n", shutdownErr)

		return 1
	}
	if err != nil && ctx.Err() == nil {
		_, _ = fmt.Fprintf(stderr, "acp-go-codex: %v\n", err)

		return 1
	}
	if sig := pendingSignal(receivedSignals); sig != nil {
		return signalCode(sig)
	}

	return 0
}

func runMCPProxy(ctx context.Context, args []string, stdin io.Reader, stdout io.Writer, stderr io.Writer) error {
	flags := flag.NewFlagSet("mcp-proxy", flag.ContinueOnError)
	flags.SetOutput(stderr)
	network := flags.String("network", "tcp", "bridge listener network")
	address := flags.String("address", "", "bridge listener address")
	acpID := flags.String("acp-id", "", "ACP MCP server id")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if *address == "" {
		return fmt.Errorf("-address is required")
	}
	if *acpID == "" {
		return fmt.Errorf("-acp-id is required")
	}

	tokenFile := os.Getenv(codexacp.MCPProxyTokenFileEnv)
	if tokenFile == "" {
		return fmt.Errorf("%s is required", codexacp.MCPProxyTokenFileEnv)
	}
	tokenBytes, err := os.ReadFile(tokenFile) // #nosec G304,G703 -- adapter-owned proxy passes the token file path through env.
	if err != nil {
		return fmt.Errorf("read MCP proxy token: %w", err)
	}

	return codexacp.RunMCPProxy(ctx, stdin, stdout, codexacp.MCPProxyOptions{
		Network: *network,
		Address: *address,
		Token:   strings.TrimSpace(string(tokenBytes)),
		ACPID:   *acpID,
	})
}

func runCodexCLI(ctx context.Context, codexPath string, codexHome string, mode string, deviceAuth bool, stdin io.Reader, stdout io.Writer, stderr io.Writer) error {
	if codexPath == "" {
		codexPath = "codex"
	}
	var args []string
	switch mode {
	case "login":
		args = []string{"login"}
		if deviceAuth {
			args = append(args, "--device-auth")
		}
	case "logout":
		args = []string{"logout"}
	default:
		return fmt.Errorf("unsupported --cli command %q", mode)
	}

	cmd := exec.CommandContext(ctx, codexPath, args...)
	cmd.Stdin = stdin
	cmd.Stdout = stdout
	cmd.Stderr = stderr
	if codexHome != "" {
		cmd.Env = append(os.Environ(), "CODEX_HOME="+codexHome)
	}

	if err := cmd.Start(); err != nil {
		return err
	}

	signals := make(chan os.Signal, 1)
	signal.Notify(signals, forwardedSignals()...)
	done := make(chan struct{})
	go func() {
		defer recoverMainGoroutine(ctx, "Codex CLI signal forwarder")
		defer signal.Stop(signals)
		for {
			select {
			case sig := <-signals:
				if cmd.Process != nil {
					_ = cmd.Process.Signal(sig)
				}
			case <-done:
				return
			}
		}
	}()

	err := cmd.Wait()
	close(done)

	return err
}

func pendingSignal(signals <-chan os.Signal) os.Signal {
	select {
	case sig := <-signals:
		return sig
	default:
		return nil
	}
}

func commandExitCode(err error) int {
	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) {
		code := exitErr.ExitCode()
		if code >= 0 {
			return code
		}
		if code := signalExitCode(exitErr); code > 0 {
			return code
		}
	}

	return 1
}
