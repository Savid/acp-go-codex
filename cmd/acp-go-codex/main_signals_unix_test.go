//go:build unix

package main

import (
	"bytes"
	"context"
	"io"
	"os"
	"os/exec"
	"syscall"
	"testing"

	codexacp "github.com/savid/acp-go-codex"
	"github.com/stretchr/testify/require"
)

// requireNativeExitCodeMapping drives the two exit forms a real child can take
// on a unix host: an ordinary status, and a death by signal that has no exit
// status of its own and is reported as 128 plus the signal instead.
func requireNativeExitCodeMapping(t *testing.T) {
	t.Helper()

	normalExit := exec.Command("sh", "-c", "exit 2") // #nosec G204 -- fixed test command.
	normalErr := normalExit.Run()

	var normal *exec.ExitError

	require.ErrorAs(t, normalErr, &normal)
	require.Equal(t, 2, commandExitCode(normal))
	require.Zero(t, signalExitCode(normal))

	signaledExit := exec.Command("sh", "-c", "kill -TERM $$") // #nosec G204 -- fixed test command.
	signaledErr := signaledExit.Run()

	var signaled *exec.ExitError

	require.ErrorAs(t, signaledErr, &signaled)
	require.Equal(t, 128+int(syscall.SIGTERM), commandExitCode(signaled))
	require.Equal(t, 128+int(syscall.SIGTERM), signalExitCode(signaled))
}

func TestRunReturnsDeliveredSignalAndSubcommandFlagError(t *testing.T) {
	originalServe := serve
	originalShutdown := shutdownOpenTelemetry
	t.Cleanup(func() {
		serve = originalServe
		shutdownOpenTelemetry = originalShutdown
	})
	shutdownOpenTelemetry = func(context.Context, func(context.Context) error) error { return nil }
	serve = func(ctx context.Context, _ io.Reader, _ io.Writer, _ ...codexacp.Option) error {
		process, err := os.FindProcess(os.Getpid())
		require.NoError(t, err)
		require.NoError(t, process.Signal(syscall.SIGTERM))
		<-ctx.Done()

		return ctx.Err()
	}

	require.Equal(t, 128+int(syscall.SIGTERM), run(t.Context(), nil, bytes.NewReader(nil), io.Discard, io.Discard))
	require.Equal(t, 2, run(t.Context(), []string{loginCommand, "-bad"}, bytes.NewReader(nil), io.Discard, io.Discard))
}
