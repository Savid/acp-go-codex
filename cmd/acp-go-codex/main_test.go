package main

import (
	"bytes"
	"context"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"go.uber.org/goleak"

	codexacp "github.com/savid/acp-go-codex"
	internalcodex "github.com/savid/acp-go-codex/internal/codex"
	"github.com/savid/acp-go-codex/internal/homelock"
)

func TestRunPassesOptions(t *testing.T) {
	stubProcessIsolationConfig(t)
	originalServe := serve
	originalAgentVersion := agentVersion
	originalShutdown := shutdownOpenTelemetry
	t.Cleanup(func() {
		serve = originalServe
		agentVersion = originalAgentVersion
		shutdownOpenTelemetry = originalShutdown
	})

	var got codexacp.Options
	serve = func(_ context.Context, _ io.Reader, _ io.Writer, opts ...codexacp.Option) error {
		for _, opt := range opts {
			opt(&got)
		}

		return nil
	}
	agentVersion = func() string { return "v9.8.7" }
	shutdownOpenTelemetry = func(context.Context, func(context.Context) error) error { return nil }

	code := run(
		context.Background(),
		[]string{
			"-process-isolation-config", testProcessIsolationConfigPath,
			"-path", "/bin/codex",
			"-home", "/tmp/codex",
			"-scratch-dir", "/tmp/codex-scratch",
			"-model", "gpt-5.5",
			"-debug",
			"-codex-allow-account-logout",
		},
		bytes.NewBuffer(nil),
		bytes.NewBuffer(nil),
		bytes.NewBuffer(nil),
	)

	if code != 0 {
		t.Fatalf("run code = %d, want 0", code)
	}
	if got.AgentVersion != "v9.8.7" {
		t.Fatalf("agent version = %q, want v9.8.7", got.AgentVersion)
	}
	if got.ExecutablePath != "/bin/codex" {
		t.Fatalf("executable path = %q", got.ExecutablePath)
	}
	if got.Home != "/tmp/codex" {
		t.Fatalf("home = %q", got.Home)
	}
	if got.ScratchDir != "/tmp/codex-scratch" {
		t.Fatalf("scratch dir = %q", got.ScratchDir)
	}
	if got.DefaultModel != "gpt-5.5" {
		t.Fatalf("default model = %q", got.DefaultModel)
	}
	if !got.AllowAccountLogout {
		t.Fatal("account logout opt-in was not passed")
	}
	if _, ok := any(got.Logger).(*slog.Logger); !ok {
		t.Fatalf("logger type = %T", got.Logger)
	}
	if got.ProcessIsolation == nil || got.ProcessIsolation.UID != 20001 {
		t.Fatalf("process isolation = %#v", got.ProcessIsolation)
	}
}

func TestRunErrorPathsAndVersion(t *testing.T) {
	stubProcessIsolationConfig(t)
	originalServe := serve
	originalAgentVersion := agentVersion
	originalShutdown := shutdownOpenTelemetry
	t.Cleanup(func() {
		serve = originalServe
		agentVersion = originalAgentVersion
		shutdownOpenTelemetry = originalShutdown
	})

	serve = func(context.Context, io.Reader, io.Writer, ...codexacp.Option) error {
		return assertError("serve failed")
	}
	shutdownOpenTelemetry = func(context.Context, func(context.Context) error) error { return nil }

	if code := run(context.Background(), []string{"-bad"}, bytes.NewBuffer(nil), bytes.NewBuffer(nil), bytes.NewBuffer(nil)); code != 2 {
		t.Fatalf("bad flags code = %d", code)
	}
	if code := run(context.Background(), []string{"-config", "model=gpt-5.5"}, bytes.NewBuffer(nil), bytes.NewBuffer(nil), bytes.NewBuffer(nil)); code != 2 {
		t.Fatalf("removed config flag code = %d", code)
	}

	var stdout bytes.Buffer
	agentVersion = func() string { return "v1.2.3" }
	if code := run(context.Background(), []string{"-version"}, bytes.NewBuffer(nil), &stdout, bytes.NewBuffer(nil)); code != 0 || strings.TrimSpace(stdout.String()) != "v1.2.3" {
		t.Fatalf("version code/stdout = %d %q", code, stdout.String())
	}

	var stderr bytes.Buffer
	if code := run(context.Background(), isolatedArgs(), bytes.NewBuffer(nil), bytes.NewBuffer(nil), &stderr); code != 1 || !strings.Contains(stderr.String(), "serve failed") {
		t.Fatalf("serve error code/stderr = %d %q", code, stderr.String())
	}
}

func TestRunPassesSeedFiles(t *testing.T) {
	stubProcessIsolationConfig(t)
	originalServe := serve
	originalAgentVersion := agentVersion
	originalShutdown := shutdownOpenTelemetry
	t.Cleanup(func() {
		serve = originalServe
		agentVersion = originalAgentVersion
		shutdownOpenTelemetry = originalShutdown
	})

	hostConfig := filepath.Join(t.TempDir(), "config.toml")
	contents := "[model_providers.litellm]\nbase_url = \"https://litellm.example/v1\"\n"
	if err := os.WriteFile(hostConfig, []byte(contents), 0o600); err != nil {
		t.Fatalf("write host seed file: %v", err)
	}

	var got codexacp.Options
	serve = func(_ context.Context, _ io.Reader, _ io.Writer, opts ...codexacp.Option) error {
		for _, opt := range opts {
			opt(&got)
		}

		return nil
	}
	agentVersion = func() string { return "v0.0.0" }
	shutdownOpenTelemetry = func(context.Context, func(context.Context) error) error { return nil }

	code := run(
		context.Background(),
		isolatedArgs("-home", "/tmp/codex", "-seed-file", "config.toml="+hostConfig),
		bytes.NewBuffer(nil),
		bytes.NewBuffer(nil),
		bytes.NewBuffer(nil),
	)

	if code != 0 {
		t.Fatalf("run code = %d, want 0", code)
	}
	if got.SeedFiles["config.toml"] != contents {
		t.Fatalf("seed files = %#v", got.SeedFiles)
	}
}

func TestRunPassesConfigOverrides(t *testing.T) {
	stubProcessIsolationConfig(t)
	originalServe := serve
	originalAgentVersion := agentVersion
	originalShutdown := shutdownOpenTelemetry
	t.Cleanup(func() {
		serve = originalServe
		agentVersion = originalAgentVersion
		shutdownOpenTelemetry = originalShutdown
	})

	var got codexacp.Options
	serve = func(_ context.Context, _ io.Reader, _ io.Writer, opts ...codexacp.Option) error {
		for _, opt := range opts {
			opt(&got)
		}

		return nil
	}
	agentVersion = func() string { return "v0.0.0" }
	shutdownOpenTelemetry = func(context.Context, func(context.Context) error) error { return nil }

	code := run(
		context.Background(),
		isolatedArgs("-codex-config", "model_provider=litellm", "-codex-config", "model_providers.litellm.base_url=https://litellm.example/v1"),
		bytes.NewBuffer(nil),
		bytes.NewBuffer(nil),
		bytes.NewBuffer(nil),
	)

	if code != 0 {
		t.Fatalf("run code = %d, want 0", code)
	}
	if got.Config["model_provider"] != "litellm" {
		t.Fatalf("config model_provider = %v, want litellm", got.Config["model_provider"])
	}
	if got.Config["model_providers.litellm.base_url"] != "https://litellm.example/v1" {
		t.Fatalf("config base_url = %v", got.Config["model_providers.litellm.base_url"])
	}
}

func TestConfigOverrideFlagSet(t *testing.T) {
	tests := []struct {
		name    string
		value   string
		wantErr bool
	}{
		{name: "missing separator", value: "model_provider", wantErr: true},
		{name: "empty key", value: "=litellm", wantErr: true},
		{name: "valid", value: "model_provider=litellm", wantErr: false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			flag := &configOverrideFlag{}
			err := flag.Set(tt.value)

			if tt.wantErr {
				if err == nil {
					t.Fatalf("Set(%q) = nil, want error", tt.value)
				}

				return
			}

			if err != nil {
				t.Fatalf("Set(%q) = %v, want nil", tt.value, err)
			}
			if flag.String() != "" {
				t.Fatalf("String() = %q, want empty", flag.String())
			}
			if flag.overrides["model_provider"] != "litellm" {
				t.Fatalf("overrides = %#v", flag.overrides)
			}
		})
	}
}

func TestSeedFileFlagSet(t *testing.T) {
	hostConfig := filepath.Join(t.TempDir(), "config.toml")
	if err := os.WriteFile(hostConfig, []byte("model = \"gpt-5.5\"\n"), 0o600); err != nil {
		t.Fatalf("write host seed file: %v", err)
	}

	tests := []struct {
		name    string
		value   string
		wantErr bool
	}{
		{name: "missing separator", value: "config.toml", wantErr: true},
		{name: "empty relative path", value: "=" + hostConfig, wantErr: true},
		{name: "empty host path", value: "config.toml=", wantErr: true},
		{name: "missing host file", value: "config.toml=/definitely/missing.toml", wantErr: true},
		{name: "valid", value: "config.toml=" + hostConfig, wantErr: false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			flag := &seedFileFlag{}
			err := flag.Set(tt.value)

			if tt.wantErr {
				if err == nil {
					t.Fatalf("Set(%q) = nil, want error", tt.value)
				}

				return
			}

			if err != nil {
				t.Fatalf("Set(%q) = %v, want nil", tt.value, err)
			}
			if flag.String() != "" {
				t.Fatalf("String() = %q, want empty", flag.String())
			}
			if flag.files["config.toml"] != "model = \"gpt-5.5\"\n" {
				t.Fatalf("files = %#v", flag.files)
			}
		})
	}
}

func TestRunRejectsBadSeedFileFlag(t *testing.T) {
	originalShutdown := shutdownOpenTelemetry
	t.Cleanup(func() { shutdownOpenTelemetry = originalShutdown })
	shutdownOpenTelemetry = func(context.Context, func(context.Context) error) error { return nil }

	var stderr bytes.Buffer
	code := run(
		context.Background(),
		[]string{"-seed-file", "config.toml=/definitely/missing/seed.toml"},
		bytes.NewBuffer(nil),
		bytes.NewBuffer(nil),
		&stderr,
	)

	if code != 2 {
		t.Fatalf("run code = %d, want 2", code)
	}
}

func TestRunReturnsPendingSignalCode(t *testing.T) {
	stubProcessIsolationConfig(t)
	originalServe := serve
	originalShutdown := shutdownOpenTelemetry
	t.Cleanup(func() {
		serve = originalServe
		shutdownOpenTelemetry = originalShutdown
	})

	serve = func(context.Context, io.Reader, io.Writer, ...codexacp.Option) error {
		proc, err := os.FindProcess(os.Getpid())
		if err != nil {
			t.Fatalf("FindProcess returned error: %v", err)
		}
		if err := proc.Signal(syscall.SIGHUP); err != nil {
			t.Fatalf("signal self: %v", err)
		}
		time.Sleep(50 * time.Millisecond)

		return nil
	}
	shutdownOpenTelemetry = func(context.Context, func(context.Context) error) error { return nil }

	if code := run(context.Background(), isolatedArgs(), bytes.NewBuffer(nil), bytes.NewBuffer(nil), bytes.NewBuffer(nil)); code != signalCode(syscall.SIGHUP) {
		t.Fatalf("run signal code = %d, want %d", code, signalCode(syscall.SIGHUP))
	}
}

func TestRunCodexCLISubcommand(t *testing.T) {
	stubProcessIsolationConfig(t)
	originalCLI := runCodexCLICommand
	t.Cleanup(func() { runCodexCLICommand = originalCLI })

	var gotPath, gotHome, gotScratch, gotMode string
	var gotDeviceAuth bool
	runCodexCLICommand = func(_ context.Context, path string, home string, scratch string, mode string, deviceAuth bool, isolation processIsolationConfig, _ io.Reader, _ io.Writer, _ io.Writer) error {
		gotPath, gotHome, gotScratch, gotMode, gotDeviceAuth = path, home, scratch, mode, deviceAuth
		if isolation.UID != 20001 {
			t.Fatalf("isolation = %#v", isolation)
		}

		return nil
	}
	if code := run(context.Background(), append([]string{"login"}, isolatedArgs("-path", "/bin/codex", "-home", "/tmp/codex", "-scratch-dir", "/tmp/codex-scratch", "-codex-device-auth")...), bytes.NewBuffer(nil), bytes.NewBuffer(nil), bytes.NewBuffer(nil)); code != 0 {
		t.Fatalf("successful login code = %d", code)
	}
	if gotPath != "/bin/codex" || gotHome != "/tmp/codex" || gotScratch != "/tmp/codex-scratch" || gotMode != "login" || !gotDeviceAuth {
		t.Fatalf("cli args path=%q home=%q scratch=%q mode=%q deviceAuth=%v", gotPath, gotHome, gotScratch, gotMode, gotDeviceAuth)
	}

	runCodexCLICommand = func(context.Context, string, string, string, string, bool, processIsolationConfig, io.Reader, io.Writer, io.Writer) error {
		return assertError("cli failed")
	}
	var stderr bytes.Buffer
	if code := run(context.Background(), append([]string{"logout"}, isolatedArgs("-home", "/tmp/codex")...), bytes.NewBuffer(nil), bytes.NewBuffer(nil), &stderr); code != 1 || !strings.Contains(stderr.String(), "cli failed") {
		t.Fatalf("cli error code/stderr = %d %q", code, stderr.String())
	}
	if code := run(context.Background(), []string{"login", "-bad"}, bytes.NewBuffer(nil), bytes.NewBuffer(nil), bytes.NewBuffer(nil)); code != 2 {
		t.Fatalf("bad cli flags code = %d", code)
	}
}

func TestMainUsesRunAndExitOnlyOnFailure(t *testing.T) {
	stubProcessIsolationConfig(t)
	originalServe := serve
	originalExit := exit
	originalArgs := os.Args
	originalShutdown := shutdownOpenTelemetry
	t.Cleanup(func() {
		serve = originalServe
		exit = originalExit
		os.Args = originalArgs
		shutdownOpenTelemetry = originalShutdown
	})

	shutdownOpenTelemetry = func(context.Context, func(context.Context) error) error { return nil }
	serve = func(context.Context, io.Reader, io.Writer, ...codexacp.Option) error {
		return nil
	}
	exit = func(code int) {
		t.Fatalf("exit called with code %d", code)
	}
	os.Args = []string{"acp-go-codex", "-process-isolation-config", testProcessIsolationConfigPath}
	main()

	serve = func(context.Context, io.Reader, io.Writer, ...codexacp.Option) error {
		return assertError("serve failed")
	}
	gotCode := 0
	exit = func(code int) { gotCode = code }
	main()
	if gotCode != 1 {
		t.Fatalf("exit code = %d, want 1", gotCode)
	}
}

func TestRunCodexCLI(t *testing.T) {
	skipUnprivilegedDarwinCLIIsolation(t)
	if os.Geteuid() == 0 {
		t.Skip("root distinct-identity coverage is owned by the terminal containment test")
	}
	dir := cliTempDir(t)
	if err := os.Chmod(dir, 0o711); err != nil {
		t.Fatal(err)
	}
	home := filepath.Join(dir, "home")
	if err := os.Mkdir(home, 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("CODEX_HOME", home)
	script := filepath.Join(dir, "codex")
	logPath := filepath.Join(home, "log")
	if err := os.WriteFile(script, []byte("#!/bin/sh\nif [ \"$1\" = \"--version\" ]; then echo codex-cli 0.144.1; exit 0; fi\necho \"$@:$CODEX_HOME\" > \"$TEST_LOG\"\n"), 0o755); err != nil {
		t.Fatalf("write script: %v", err)
	}
	t.Setenv("TEST_LOG", logPath)
	isolation := testCLIProcessIsolation()
	if err := os.Chown(home, int(isolation.UID), int(isolation.GID)); err != nil {
		t.Fatal(err)
	}
	var commandStderr bytes.Buffer

	scratch := cliTempDir(t)
	if err := os.Chmod(scratch, 0o711); err != nil {
		t.Fatal(err)
	}
	if err := runCodexCLI(context.Background(), script, home, scratch, "login", true, isolation, bytes.NewBuffer(nil), bytes.NewBuffer(nil), &commandStderr); err != nil {
		t.Fatalf("runCodexCLI login returned error: %v", err)
	}
	raw, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatalf("read log: %v; command stderr: %s", err, commandStderr.String())
	}
	if !strings.Contains(string(raw), "login --device-auth:"+home) {
		t.Fatalf("log = %q", string(raw))
	}
	if logoutErr := runCodexCLI(context.Background(), script, "", "", "logout", false, isolation, bytes.NewBuffer(nil), bytes.NewBuffer(nil), bytes.NewBuffer(nil)); logoutErr == nil {
		t.Fatal("runCodexCLI accepted an implicit root home")
	}
	if badErr := runCodexCLI(context.Background(), script, home, scratch, "bad", false, isolation, bytes.NewBuffer(nil), bytes.NewBuffer(nil), bytes.NewBuffer(nil)); badErr == nil {
		t.Fatal("unsupported CLI command succeeded")
	}

	fail := filepath.Join(dir, "codex-fail")
	if writeErr := os.WriteFile(fail, []byte("#!/bin/sh\nif [ \"$1\" = \"--version\" ]; then echo codex-cli 0.144.1; exit 0; fi\nexit 7\n"), 0o755); writeErr != nil {
		t.Fatalf("write failing script: %v", writeErr)
	}
	if failErr := runCodexCLI(context.Background(), fail, home, scratch, "login", false, isolation, bytes.NewBuffer(nil), bytes.NewBuffer(nil), bytes.NewBuffer(nil)); failErr == nil || commandExitCode(failErr) == 0 {
		t.Fatalf("failing cli err=%v code=%d", failErr, commandExitCode(failErr))
	}
	pathDir := cliTempDir(t)
	if err := os.Chmod(pathDir, 0o711); err != nil {
		t.Fatal(err)
	}
	pathScript := filepath.Join(pathDir, "codex")
	if writePathErr := os.WriteFile(pathScript, []byte("#!/bin/sh\nif [ \"$1\" = \"--version\" ]; then echo codex-cli 0.144.1; fi\nexit 0\n"), 0o755); writePathErr != nil {
		t.Fatalf("write PATH codex: %v", writePathErr)
	}
	t.Setenv("PATH", pathDir)
	isolation = testCLIProcessIsolation()
	var defaultStderr bytes.Buffer
	if defaultPathErr := runCodexCLI(context.Background(), "", home, scratch, "logout", false, isolation, bytes.NewBuffer(nil), bytes.NewBuffer(nil), &defaultStderr); defaultPathErr != nil {
		t.Fatalf("runCodexCLI default path returned error: %v: %s", defaultPathErr, defaultStderr.String())
	}
	if missingErr := runCodexCLI(context.Background(), filepath.Join(pathDir, "missing"), home, scratch, "logout", false, isolation, bytes.NewBuffer(nil), bytes.NewBuffer(nil), bytes.NewBuffer(nil)); missingErr == nil {
		t.Fatal("runCodexCLI accepted missing executable")
	}

	signalScript := filepath.Join(dir, "codex-signal")
	readyPath := filepath.Join(home, "ready")
	harnessPIDPath := filepath.Join(home, "signal-pid")
	if writeSignalErr := os.WriteFile(signalScript, []byte(`#!/bin/sh
if [ "$1" = "--version" ]; then
  echo codex-cli 0.144.1
  exit 0
fi
if [ "$1" = "logout" ]; then
  echo $$ > "$TEST_PID_LOG"
  echo ready > "$TEST_READY_LOG"
  read release
  exit 0
fi
exit 2
`), 0o755); writeSignalErr != nil {
		t.Fatalf("write signal script: %v", writeSignalErr)
	}
	t.Setenv("TEST_READY_LOG", readyPath)
	t.Setenv("TEST_PID_LOG", harnessPIDPath)
	isolation = testCLIProcessIsolation()

	// The fake harness stays terminal-logout-live by blocking on its own stdin,
	// so it burns no CPU and cannot outlive the pipe this process holds.
	holdRead, holdWrite, pipeErr := os.Pipe()
	if pipeErr != nil {
		t.Fatalf("open harness stdin hold: %v", pipeErr)
	}
	t.Cleanup(func() { _ = holdRead.Close() })

	accountSignals := make(chan os.Signal, 1)
	errCh := make(chan error, 1)
	runDone := make(chan struct{})
	go func() {
		defer close(runDone)

		errCh <- runCodexCLIWithSignals(context.Background(), signalScript, home, scratch, "logout", false, isolation, holdRead, bytes.NewBuffer(nil), bytes.NewBuffer(nil), accountSignals)
	}()
	t.Cleanup(func() { reapSignalHarness(t, holdWrite, harnessPIDPath, runDone) })
	waitUntilFor(t, 7*time.Second, func() bool {
		_, statErr := os.Stat(readyPath)

		return statErr == nil
	})
	lockRoot, rootErr := internalcodex.HomeLockRoot(scratch, home)
	if rootErr != nil {
		t.Fatal(rootErr)
	}
	_, lockErr := homelock.Acquire(lockRoot)
	if lockErr == nil {
		t.Fatal("terminal logout did not hold both writable-home locks")
	}
	accountSignals <- syscall.SIGHUP
	if runErr := <-errCh; runErr == nil || commandExitCode(runErr) <= 0 {
		t.Fatalf("runCodexCLI signal containment returned err=%v code=%d", runErr, commandExitCode(runErr))
	}
	lock, lockErr := homelock.Acquire(home)
	if lockErr != nil {
		t.Fatalf("writable home remained locked after auth tree quiesced: %v", lockErr)
	}
	if releaseErr := lock.Release(); releaseErr != nil {
		t.Fatalf("release writable-home lock: %v", releaseErr)
	}
	if commandExitCode(assertError("plain")) != 1 {
		t.Fatal("plain command error did not map to exit code 1")
	}
	signals := make(chan os.Signal, 1)
	if pendingSignal(signals) != nil {
		t.Fatal("empty signal channel returned a signal")
	}
	signals <- os.Interrupt
	if pendingSignal(signals) != os.Interrupt {
		t.Fatal("pendingSignal missed interrupt")
	}
	if signalCode(os.Interrupt) <= 0 || signalCode(testSignal("custom")) != 1 {
		t.Fatal("signalCode returned unexpected value")
	}
	cmd := exec.Command("/bin/sh", "-c", "kill -TERM $$")
	err = cmd.Run()
	if err != nil && commandExitCode(err) <= 0 {
		t.Fatalf("signal exit did not map to exit code: %v", err)
	}
	exitCmd := exec.Command("/bin/sh", "-c", "exit 3")
	if err := exitCmd.Run(); err != nil {
		if exitErr, ok := err.(*exec.ExitError); ok && signalExitCode(exitErr) != 0 {
			t.Fatalf("non-signal exit mapped to signal code")
		}
	}
}

func skipUnprivilegedDarwinCLIIsolation(t *testing.T) {
	t.Helper()
	if runtime.GOOS == "darwin" {
		t.Skip("requires a privileged two-principal fixture to clear supplementary groups")
	}
}

func TestResolvedCodexCLIHome(t *testing.T) {
	configured := filepath.Join(t.TempDir(), "home")
	got, err := resolvedCodexCLIHome(configured)
	if err != nil || got != configured {
		t.Fatalf("configured home = %q, %v", got, err)
	}

	for _, invalid := range []string{"", "relative", configured + string(filepath.Separator) + ".." + string(filepath.Separator) + "other"} {
		if _, err := resolvedCodexCLIHome(invalid); err == nil {
			t.Fatalf("invalid home %q was accepted", invalid)
		}
	}

	isolation := processIsolationConfig{BaseEnvironment: map[string]string{"HOME": configured}}
	if got, err = resolvedCodexIsolationHome("", isolation, false); err != nil || got != configured {
		t.Fatalf("default isolated home = %q, %v", got, err)
	}
	if _, err = resolvedCodexIsolationHome("", isolation, true); err == nil {
		t.Fatal("native account mode accepted an implicit home")
	}
	if _, err = resolvedCodexIsolationHome(filepath.Join(filepath.Dir(configured), "other"), isolation, true); err == nil {
		t.Fatal("alternate native account home was accepted")
	}
}

func testCLIProcessIsolation() processIsolationConfig {
	uid, gid := os.Getuid(), os.Getgid()
	if uid == 0 {
		uid = 65534
	}
	if gid == 0 {
		gid = 65534
	}
	environment := make(map[string]string)
	for _, entry := range os.Environ() {
		name, value, ok := strings.Cut(entry, "=")
		if ok {
			environment[name] = value
		}
	}

	return processIsolationConfig{UID: uint32(uid), GID: uint32(gid), BaseEnvironment: environment}
}

func cliTempDir(t *testing.T) string {
	t.Helper()
	if os.Geteuid() != 0 {
		return t.TempDir()
	}
	dir, err := os.MkdirTemp("/tmp", "acp-go-codex-cli-test-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })

	return dir
}

func TestVersionDefaultBranch(t *testing.T) {
	original := buildVersion
	t.Cleanup(func() { buildVersion = original })
	buildVersion = ""
	if version() != "dev" {
		t.Fatalf("empty build version returned %q", version())
	}
}

type assertError string

func (e assertError) Error() string { return string(e) }

type testSignal string

func (s testSignal) String() string { return string(s) }
func (s testSignal) Signal()        {}

// reapSignalHarness releases the fake harness's stdin so its blocked read
// returns, then kills the harness by the PID it recorded when the supervised
// tree does not collapse on its own. It runs on every exit path, including
// t.Fatal, so no harness process survives the test.
func reapSignalHarness(t *testing.T, hold *os.File, pidPath string, done <-chan struct{}) {
	t.Helper()

	_ = hold.Close()
	if awaitClose(done, 15*time.Second) {
		return
	}

	if raw, readErr := os.ReadFile(pidPath); readErr == nil {
		if pid, parseErr := strconv.Atoi(strings.TrimSpace(string(raw))); parseErr == nil && pid > 0 {
			harness, findErr := os.FindProcess(pid)
			if findErr == nil {
				_ = harness.Kill()
			}
		}
	}

	if !awaitClose(done, 15*time.Second) {
		t.Error("fake harness outlived the test")
	}
}

func awaitClose(done <-chan struct{}, timeout time.Duration) bool {
	timer := time.NewTimer(timeout)
	defer timer.Stop()

	select {
	case <-done:
		return true
	case <-timer.C:
		return false
	}
}

func waitUntilFor(t *testing.T, timeout time.Duration, ready func() bool) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if ready() {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatal("condition was not met")
}

func TestMain(m *testing.M) {
	goleak.VerifyTestMain(m)
}
