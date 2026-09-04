//go:build windows

package main

import (
	"bytes"
	"io"
	"os"
	"os/exec"
	"syscall"
	"testing"

	"github.com/stretchr/testify/require"
)

// requireNativeExitCodeMapping drives the only exit form a real child can take
// on Windows. There is no death by signal here: a terminated process still
// carries an ordinary exit status, which is why signalExitCode reports nothing
// for every ExitError on this platform and commandExitCode answers from the
// status alone.
func requireNativeExitCodeMapping(t *testing.T) {
	t.Helper()

	normalExit := exec.Command("cmd", "/c", "exit 2") // #nosec G204 -- fixed test command.
	normalErr := normalExit.Run()

	var normal *exec.ExitError

	require.ErrorAs(t, normalErr, &normal)
	require.Equal(t, 2, commandExitCode(normal))
	require.Zero(t, signalExitCode(normal))

	terminated := exec.Command("cmd", "/c", "pause") // #nosec G204 -- fixed test command.
	require.NoError(t, terminated.Start())
	require.NoError(t, terminated.Process.Kill())

	var killed *exec.ExitError

	require.ErrorAs(t, terminated.Wait(), &killed)
	require.Zero(t, signalExitCode(killed), "a terminated Windows process is not a signalled one")
	require.Equal(t, killed.ExitCode(), commandExitCode(killed))
}

// TestRunReturnsSubcommandFlagErrorAndRefusesInProcessSignals pins the half of
// the posix delivered-signal proof that Windows can answer. os.Process.Signal
// refuses everything but Kill here, so this process cannot be sent a SIGTERM to
// observe being forwarded, and the mapping the posix proof reaches through that
// delivery is pinned directly on signalCode above. The flag-error leg is the
// same on both platforms.
func TestRunReturnsSubcommandFlagErrorAndRefusesInProcessSignals(t *testing.T) {
	process, err := os.FindProcess(os.Getpid())
	require.NoError(t, err)
	require.Error(t, process.Signal(syscall.SIGTERM), "Windows delivered a signal it does not support")

	require.Equal(t, 2, run(t.Context(), []string{loginCommand, "-bad"}, bytes.NewReader(nil), io.Discard, io.Discard))
}
