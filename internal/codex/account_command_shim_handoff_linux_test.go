//go:build linux

package codex

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"testing"

	"github.com/stretchr/testify/require"
)

// TestLoginReportsAFailedShimHandoffAndRemovesTheShim proves the login leg
// treats a browser shim it cannot hand off to the native identity as fatal:
// the command refuses with the handoff's own reason before any native process
// is launched, and the same return removes the shim it just created, so a
// failed login leaves no launcher directory behind for a later leg to trust.
// The handoff is made to fail through its real path contract — a relative
// scratch parent yields a relative shim directory, which the Linux handoff
// refuses outright.
func TestLoginReportsAFailedShimHandoffAndRemovesTheShim(t *testing.T) {
	restoreAccountCommandHooks(t)
	t.Chdir(t.TempDir())
	require.NoError(t, os.Mkdir("scratch", 0o700))
	SetScratchParentResolver(func(string) (string, error) { return "scratch", nil })

	launched := false
	accountSupervisorCommand = func(context.Context, supervisorConfig) (*exec.Cmd, *supervisorProof, error) {
		launched = true

		return nil, nil, errors.New("must not launch")
	}

	err := RunAccountCommand(context.Background(), AccountCommandOptions{
		CLIPath: "/usr/bin/true", CodexHome: testNativeOwnedTempDir(t),
		Mode: accountCommandLogin, ProcessIsolation: testProcessIsolation(),
	})
	require.ErrorContains(t, err, "generated native path must be absolute")
	require.False(t, launched, "a login whose shim cannot be handed off must never launch")

	entries, readErr := os.ReadDir("scratch")
	require.NoError(t, readErr)
	require.Empty(t, entries, "the failed handoff must remove the shim it created")
}
