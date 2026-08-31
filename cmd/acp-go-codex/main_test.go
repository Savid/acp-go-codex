package main

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"

	codexacp "github.com/savid/acp-go-codex"
	"github.com/stretchr/testify/require"
)

func TestRunPassesCurrentOptions(t *testing.T) {
	originalServe := serve
	originalVersion := agentVersion
	originalShutdown := shutdownOpenTelemetry
	t.Cleanup(func() {
		serve = originalServe
		agentVersion = originalVersion
		shutdownOpenTelemetry = originalShutdown
	})

	hostConfig := filepath.Join(t.TempDir(), "config.toml")
	require.NoError(t, os.WriteFile(hostConfig, []byte("model = \"test\"\n"), 0o600))

	var got codexacp.Options
	serve = func(_ context.Context, _ io.Reader, _ io.Writer, opts ...codexacp.Option) error {
		for _, option := range opts {
			option(&got)
		}

		return nil
	}
	agentVersion = func() string { return "v9.8.7" }
	shutdownOpenTelemetry = func(context.Context, func(context.Context) error) error { return nil }

	code := run(context.Background(), []string{
		"-path", "/bin/codex",
		"-home", "/tmp/codex",
		"-scratch-dir", "/tmp/codex-scratch",
		"-provider-auth-root", "/tmp/provider-auth",
		"-provider-auth-direct-home", "/tmp/codex",
		"-model", "gpt-5.5",
		"-seed-file", "config.toml=" + hostConfig,
		"-codex-config", "model_provider=test",
		"-codex-allow-account-logout",
		"-debug",
	}, bytes.NewReader(nil), io.Discard, io.Discard)

	require.Zero(t, code)
	require.Equal(t, "v9.8.7", got.AgentVersion)
	require.Equal(t, "/bin/codex", got.ExecutablePath)
	require.Equal(t, "/tmp/codex", got.Home)
	require.Equal(t, "/tmp/codex-scratch", got.ScratchDir)
	require.Equal(t, "/tmp/provider-auth", got.ProviderAuthRoot)
	require.Equal(t, "/tmp/codex", got.ProviderAuthDirectHome)
	require.Equal(t, "gpt-5.5", got.DefaultModel)
	require.Equal(t, "model = \"test\"\n", got.SeedFiles["config.toml"])
	require.Equal(t, "test", got.Config["model_provider"])
	require.True(t, got.AllowAccountLogout)
	require.NotNil(t, got.Logger)
}

func TestRunErrorsVersionAndShutdown(t *testing.T) {
	originalServe := serve
	originalVersion := agentVersion
	originalShutdown := shutdownOpenTelemetry
	t.Cleanup(func() {
		serve = originalServe
		agentVersion = originalVersion
		shutdownOpenTelemetry = originalShutdown
	})

	require.Equal(t, 2, run(context.Background(), []string{"-bad"}, bytes.NewReader(nil), io.Discard, io.Discard))

	var stdout bytes.Buffer
	agentVersion = func() string { return "v1.2.3" }
	require.Zero(t, run(context.Background(), []string{"-version"}, bytes.NewReader(nil), &stdout, io.Discard))
	require.Equal(t, "v1.2.3", strings.TrimSpace(stdout.String()))

	serve = func(context.Context, io.Reader, io.Writer, ...codexacp.Option) error {
		return errors.New("serve failed")
	}
	shutdownOpenTelemetry = func(context.Context, func(context.Context) error) error { return nil }
	var stderr bytes.Buffer
	require.Equal(t, 1, run(context.Background(), nil, bytes.NewReader(nil), io.Discard, &stderr))
	require.Contains(t, stderr.String(), "serve failed")

	serve = func(context.Context, io.Reader, io.Writer, ...codexacp.Option) error { return nil }
	shutdownOpenTelemetry = func(context.Context, func(context.Context) error) error { return errors.New("shutdown failed") }
	stderr.Reset()
	require.Equal(t, 1, run(context.Background(), nil, bytes.NewReader(nil), io.Discard, &stderr))
	require.Contains(t, stderr.String(), "shutdown failed")
}

func TestRunCodexCLISubcommand(t *testing.T) {
	original := runCodexCLICommand
	t.Cleanup(func() { runCodexCLICommand = original })

	type invocation struct {
		path, home, scratch, mode string
		device                    bool
	}
	var got invocation
	runCodexCLICommand = func(
		_ context.Context,
		path string,
		home string,
		scratch string,
		mode string,
		device bool,
		_ io.Reader,
		_ io.Writer,
		_ io.Writer,
	) error {
		got = invocation{path: path, home: home, scratch: scratch, mode: mode, device: device}

		return nil
	}

	home := filepath.Join(t.TempDir(), "home")
	code := run(context.Background(), []string{
		loginCommand, "-path", "/bin/codex", "-home", home, "-scratch-dir", "/tmp/scratch", "-codex-device-auth",
	}, bytes.NewReader(nil), io.Discard, io.Discard)
	require.Zero(t, code)
	require.Equal(t, invocation{path: "/bin/codex", home: home, scratch: "/tmp/scratch", mode: loginCommand, device: true}, got)

	var stderr bytes.Buffer
	require.Equal(t, 1, run(context.Background(), []string{logoutCommand}, bytes.NewReader(nil), io.Discard, &stderr))
	require.Contains(t, stderr.String(), "-home is required")

	runCodexCLICommand = func(context.Context, string, string, string, string, bool, io.Reader, io.Writer, io.Writer) error {
		return &exec.ExitError{ProcessState: exitedProcessState(t, 7)}
	}
	require.Equal(t, 7, run(context.Background(), []string{logoutCommand, "-home", home}, bytes.NewReader(nil), io.Discard, io.Discard))
}

func TestCurrentFlagParsers(t *testing.T) {
	host := filepath.Join(t.TempDir(), "seed")
	require.NoError(t, os.WriteFile(host, []byte("contents"), 0o600))

	seed := &seedFileFlag{}
	require.NoError(t, seed.Set("config.toml="+host))
	require.Equal(t, "contents", seed.files["config.toml"])
	for _, value := range []string{"bad", "=" + host, "config.toml="} {
		require.Error(t, seed.Set(value))
	}
	require.Error(t, seed.Set("missing="+filepath.Join(t.TempDir(), "missing")))

	config := &configOverrideFlag{}
	require.NoError(t, config.Set("model_provider=test"))
	require.Equal(t, "test", config.overrides["model_provider"])
	require.Error(t, config.Set("bad"))
	require.Error(t, config.Set("=bad"))
}

func TestPendingSignalAndCommandExitCode(t *testing.T) {
	signals := make(chan os.Signal, 1)
	require.Nil(t, pendingSignal(signals))
	signals <- syscall.SIGTERM
	require.Equal(t, os.Signal(syscall.SIGTERM), pendingSignal(signals))
	require.Equal(t, 1, commandExitCode(errors.New("failure")))
}

func TestResolvedCodexCLIHome(t *testing.T) {
	home := filepath.Join(t.TempDir(), "home")
	resolved, err := resolvedCodexCLIHome(home)
	require.NoError(t, err)
	require.Equal(t, home, resolved)
	for _, invalid := range []string{"", "relative", home + string(filepath.Separator) + ".." + string(filepath.Separator) + "other"} {
		_, err := resolvedCodexCLIHome(invalid)
		require.Error(t, err)
	}
}

func TestVersionDefaultBranch(t *testing.T) {
	original := buildVersion
	t.Cleanup(func() { buildVersion = original })
	buildVersion = ""
	require.Equal(t, "dev", version())
}

func exitedProcessState(t *testing.T, code int) *os.ProcessState {
	t.Helper()

	cmd := exec.Command("sh", "-c", "exit "+string(rune('0'+code))) // #nosec G204 -- fixed single-digit test status.
	_ = cmd.Run()
	require.NotNil(t, cmd.ProcessState)

	return cmd.ProcessState
}
