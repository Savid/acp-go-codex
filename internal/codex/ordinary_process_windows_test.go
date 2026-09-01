//go:build windows

package codex

import (
	"context"
	"os"
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
