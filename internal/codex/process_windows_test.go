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
// equivalents so Windows compilation exercises ordinary native launch and
// explicit-policy refusal without excluding the rest of the package tests.
var (
	getProcessGroupID = func(pid int) (int, error) { return pid, nil }
	killProcessID     = func(int, syscall.Signal) error { return nil }
)

func TestOrdinaryWindowsNativeRuntimeAndLogout(t *testing.T) {
	originalGOOS := processIsolationGOOS
	originalScratchParent := accountScratchParent
	t.Cleanup(func() {
		processIsolationGOOS = originalGOOS
		accountScratchParent = originalScratchParent
	})
	processIsolationGOOS = processIsolationWindows

	executable, err := os.Executable()
	require.NoError(t, err)
	executable, err = filepath.Abs(executable)
	require.NoError(t, err)

	implicitEnvironment := environmentMap(os.Environ())
	implicitEnvironment["ACP_GO_CODEX_WINDOWS_EXECUTABLE_CHILD"] = "1"

	parent := t.TempDir()
	writableHome := t.TempDir()
	transport, _, _, _, err := launchAppServer(context.Background(), context.Background(), Options{
		CLIPath:             executable,
		CodexHome:           writableHome,
		WritableHome:        writableHome,
		SupervisorParent:    parent,
		SupervisorRoot:      t.TempDir(),
		NativeVersion:       minCodexVersion,
		ImplicitEnvironment: implicitEnvironment,
	})
	require.NoError(t, err)
	require.NoError(t, transport.Close())

	SetScratchParentResolver(func(string) (string, error) { return parent, nil })
	require.NoError(t, RunAccountCommand(context.Background(), AccountCommandOptions{
		CLIPath:             executable,
		CodexHome:           writableHome,
		ScratchDir:          parent,
		Mode:                accountCommandLogout,
		ImplicitEnvironment: implicitEnvironment,
	}))
}
