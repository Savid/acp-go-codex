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
	"path/filepath"
	"runtime"
	"strings"

	codexacp "github.com/savid/acp-go-codex"
	"github.com/savid/acp-go-codex/internal/codex"
)

const (
	loginCommand       = "login"
	logoutCommand      = "logout"
	containmentCommand = "containment"
	platformDarwin     = "darwin"
)

// seedFileFlag collects repeatable -seed-file <relpath>=<hostpath> flags,
// reading each host file's contents keyed by its relative destination path.
type seedFileFlag struct {
	files map[string]string
}

func (f *seedFileFlag) String() string {
	return ""
}

func (f *seedFileFlag) Set(value string) error {
	rel, host, ok := strings.Cut(value, "=")
	if !ok {
		return fmt.Errorf("expected <relpath>=<hostpath>, got %q", value)
	}

	if rel == "" {
		return fmt.Errorf("seed-file relative path must not be empty")
	}

	if host == "" {
		return fmt.Errorf("seed-file host path must not be empty")
	}

	contents, err := os.ReadFile(host) // #nosec G304 -- host path is an explicit operator-provided seed source.
	if err != nil {
		return fmt.Errorf("read seed file %q: %w", host, err)
	}

	if f.files == nil {
		f.files = make(map[string]string, 1)
	}

	f.files[rel] = string(contents)

	return nil
}

// configOverrideFlag collects repeatable -codex-config <key>=<value> flags into a map
// of string-valued TOML config overrides passed to codex app-server as
// `-c key=value`.
type configOverrideFlag struct {
	overrides map[string]any
}

func (f *configOverrideFlag) String() string {
	return ""
}

func (f *configOverrideFlag) Set(value string) error {
	key, val, ok := strings.Cut(value, "=")
	if !ok {
		return fmt.Errorf("expected <key>=<value>, got %q", value)
	}

	if key == "" {
		return fmt.Errorf("config key must not be empty")
	}

	if f.overrides == nil {
		f.overrides = make(map[string]any, 1)
	}

	f.overrides[key] = val

	return nil
}

var serve = codexacp.Serve
var agentVersion = version
var exit = os.Exit
var runCodexCLICommand = runCodexCLI
var shutdownOpenTelemetry = shutdownTelemetry
var codexCLIUserHomeDir = os.UserHomeDir
var runtimeGOOS = runtime.GOOS

func main() {
	if code := run(context.Background(), os.Args[1:], os.Stdin, os.Stdout, os.Stderr); code != 0 {
		exit(code)
	}
}

func run(ctx context.Context, args []string, stdin io.Reader, stdout io.Writer, stderr io.Writer) int {
	if len(args) > 0 && args[0] == containmentCommand {
		return runContainment(args[1:], stdout, stderr)
	}

	if len(args) > 0 && (args[0] == loginCommand || args[0] == logoutCommand) {
		return runCodexCLISubcommand(ctx, args, stdin, stdout, stderr)
	}

	flags := flag.NewFlagSet("acp-go-codex", flag.ContinueOnError)
	flags.SetOutput(stderr)

	codexPath := flags.String("path", "", "path to codex CLI")
	codexHome := flags.String("home", "", "Codex home directory")
	scratchDir := flags.String("scratch-dir", "", "parent directory for ephemeral session scratch; empty means the system temp directory")
	model := flags.String("model", "", "default Codex model")
	debug := flags.Bool("debug", false, "write debug logs to stderr")
	printVersion := flags.Bool("version", false, "print adapter version and exit")
	darwinBestEffort := flags.Bool("darwin-best-effort-containment", false, "accept Darwin process-group containment and its escaped-descendant and PGID-reuse risks")
	allowAccountLogout := flags.Bool("codex-allow-account-logout", false, "permit ACP logout to mutate adapter-owned Codex auth")

	seedFiles := &seedFileFlag{}
	flags.Var(seedFiles, "seed-file", "seed file as <relpath>=<hostpath>, repeatable; contents are written under CODEX_HOME before codex launches")

	configOverrides := &configOverrideFlag{}
	flags.Var(configOverrides, "codex-config", "Codex config override as <key>=<value>, repeatable; passed to codex app-server as -c key=value (dotted keys set nested config, nothing is written to disk)")

	if err := flags.Parse(args); err != nil {
		return 2
	}

	if *darwinBestEffort && runtimeGOOS != platformDarwin {
		_, _ = fmt.Fprintln(stderr, "acp-go-codex: -darwin-best-effort-containment is valid only on darwin")

		return 2
	}

	if *printVersion {
		_, _ = fmt.Fprintln(stdout, agentVersion())

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

	if *darwinBestEffort {
		_, _ = fmt.Fprintln(stderr, "WARNING containment=best_effort: escaped descendants may survive; numeric PGID reuse can cause collateral signalling; marker correlation is not ownership; markers can be scrubbed; native-root permits do not bound escaped provider work")
	}

	serveOptions := make([]codexacp.Option, 0, 8+len(telemetry.options))

	serveOptions = append(serveOptions,
		codexacp.WithAgentVersion(version),
		codexacp.WithExecutablePath(*codexPath),
		codexacp.WithHome(*codexHome),
		codexacp.WithScratchDir(*scratchDir),
		codexacp.WithDefaultModel(*model),
		codexacp.WithCodexAllowAccountLogout(*allowAccountLogout),
		codexacp.WithLogger(logger),
	)
	if *darwinBestEffort {
		serveOptions = append(serveOptions, codexacp.WithDarwinBestEffortContainment())
	}

	if len(seedFiles.files) > 0 {
		serveOptions = append(serveOptions, codexacp.WithSeedFiles(seedFiles.files))
	}

	if len(configOverrides.overrides) > 0 {
		serveOptions = append(serveOptions, codexacp.WithCodexConfigOverrides(configOverrides.overrides))
	}

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

func runCodexCLISubcommand(ctx context.Context, args []string, stdin io.Reader, stdout io.Writer, stderr io.Writer) int {
	mode := args[0]
	flags := flag.NewFlagSet("acp-go-codex "+mode, flag.ContinueOnError)
	flags.SetOutput(stderr)
	codexPath := flags.String("path", "", "path to codex CLI")
	codexHome := flags.String("home", "", "Codex home directory")
	scratchDir := flags.String("scratch-dir", "", "parent directory for ephemeral account-command scratch; empty means the system temp directory")
	darwinBestEffort := flags.Bool("darwin-best-effort-containment", false, "accept Darwin process-group containment and its escaped-descendant and PGID-reuse risks")

	deviceAuth := flags.Bool("codex-device-auth", false, "use Codex device auth for login")
	if err := flags.Parse(args[1:]); err != nil {
		return 2
	}

	if *darwinBestEffort && runtimeGOOS != platformDarwin {
		_, _ = fmt.Fprintf(stderr, "acp-go-codex %s: -darwin-best-effort-containment is valid only on darwin\n", mode)

		return 2
	}

	if *darwinBestEffort {
		_, _ = fmt.Fprintln(stderr, "WARNING containment=best_effort: escaped descendants may survive; numeric PGID reuse can cause collateral signalling; marker correlation is not ownership; markers can be scrubbed; native-root permits do not bound escaped provider work")
	}

	if err := runCodexCLICommand(ctx, *codexPath, *codexHome, *scratchDir, mode, *deviceAuth, *darwinBestEffort, stdin, stdout, stderr); err != nil {
		_, _ = fmt.Fprintf(stderr, "acp-go-codex %s: %v\n", mode, err)

		return commandExitCode(err)
	}

	return 0
}

func runCodexCLI(ctx context.Context, codexPath string, codexHome string, scratchDir string, mode string, deviceAuth bool, darwinBestEffort bool, stdin io.Reader, stdout io.Writer, stderr io.Writer) error {
	home, err := resolvedCodexCLIHome(codexHome)
	if err != nil {
		return err
	}

	signals := make(chan os.Signal, 1)

	signal.Notify(signals, forwardedSignals()...)
	defer signal.Stop(signals)

	return runCodexCLIWithSignals(ctx, codexPath, home, scratchDir, mode, deviceAuth, darwinBestEffort, stdin, stdout, stderr, signals)
}

func runCodexCLIWithSignals(
	ctx context.Context,
	codexPath string,
	home string,
	scratchDir string,
	mode string,
	deviceAuth bool,
	darwinBestEffort bool,
	stdin io.Reader,
	stdout io.Writer,
	stderr io.Writer,
	signals <-chan os.Signal,
) error {
	return codex.RunAccountCommand(ctx, codex.AccountCommandOptions{
		CLIPath:          codexPath,
		CodexHome:        home,
		ScratchDir:       scratchDir,
		Mode:             mode,
		DeviceAuth:       deviceAuth,
		DarwinBestEffort: darwinBestEffort,
		Stdin:            stdin,
		Stdout:           stdout,
		Stderr:           stderr,
		Signals:          signals,
	})
}

func resolvedCodexCLIHome(configured string) (string, error) {
	if configured != "" {
		return filepath.Clean(configured), nil
	}

	if value := os.Getenv("CODEX_HOME"); value != "" {
		return filepath.Clean(value), nil
	}

	home, err := codexCLIUserHomeDir()
	if err != nil {
		return "", fmt.Errorf("resolve Codex writable home: %w", err)
	}

	if home == "" {
		return "", errors.New("resolve Codex writable home: user home is empty")
	}

	return filepath.Join(home, ".codex"), nil
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
