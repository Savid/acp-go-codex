//go:build windows

package codex

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"syscall"
	"testing"

	"github.com/stretchr/testify/require"
)

// The cross-platform coverage tests replace these Unix seams. Define test-only
// equivalents so Windows compilation exercises ordinary native launch.
var (
	getProcessGroupID = func(pid int) (int, error) { return pid, nil }
	killProcessID     = func(int, syscall.Signal) error { return nil }
)

func TestOrdinaryWindowsNativeRuntimeAndLogout(t *testing.T) {
	originalScratchParent := accountScratchParent
	originalGOOS := processGOOS
	t.Cleanup(func() {
		accountScratchParent = originalScratchParent
		processGOOS = originalGOOS
	})
	processGOOS = platformWindows

	executable, err := os.Executable()
	require.NoError(t, err)
	executable, err = filepath.Abs(executable)
	require.NoError(t, err)

	implicitEnvironment := environmentMap(os.Environ())
	implicitEnvironment["ACP_GO_CODEX_WINDOWS_EXECUTABLE_CHILD"] = "1"

	parent := t.TempDir()
	writableHome := t.TempDir()
	transport, _, _, err := launchAppServer(context.Background(), Options{
		CLIPath:             executable,
		CodexHome:           writableHome,
		WritableHome:        writableHome,
		Scratch:             parent,
		ScratchParent:       parent,
		NativeVersion:       minCodexVersion,
		ImplicitEnvironment: implicitEnvironment,
	})
	require.NoError(t, err)
	require.NoError(t, transport.Close())

	SetScratchParentResolver(func(string) (string, error) { return parent, nil })
	require.NoError(t, RunAccountCommand(context.Background(), AccountCommandOptions{
		CLIPath:             executable,
		CodexHome:           writableHome,
		Scratch:             parent,
		Mode:                accountCommandLogout,
		ImplicitEnvironment: implicitEnvironment,
	}))
}

// TestKillProcessTreatsAFinishedProcessAsContained pins the Windows spelling of
// "already gone", and pins it as a platform fact rather than an assumption. A
// unix host answers os.ErrProcessDone; Windows releases the process handle at
// Wait and refuses the terminate with EINVAL. A containment path that read that
// as a failure would refuse every close over a turn whose native process had
// already exited.
func TestKillProcessTreatsAFinishedProcessAsContained(t *testing.T) {
	command := exec.Command("cmd", "/c", "exit 0") // #nosec G204 -- fixed test command.
	require.NoError(t, command.Run())

	require.ErrorIs(t, command.Process.Kill(), syscall.EINVAL,
		"Windows no longer refuses a terminate on a finished process with EINVAL")

	require.NoError(t, killProcess(command))
	require.NoError(t, terminateProcess(command))
	require.NoError(t, killProcess(nil))
	require.NoError(t, killProcess(&exec.Cmd{}))
}
