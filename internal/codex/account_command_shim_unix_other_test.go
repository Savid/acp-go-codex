//go:build unix && !linux

package codex

import (
	"context"
	"os"
	"os/exec"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestRunAccountCommandHandsTheBrowserShimToTheNativePrincipal(t *testing.T) {
	restoreAccountCommandHooks(t)
	restoreBrowserShimHooks(t)

	parent := testTraversableTempDir(t)
	accountProbeVersion = func(context.Context, VersionProbeOptions) (string, error) { return minCodexVersion, nil }

	var shimDir string

	accountSupervisorCommand = func(_ context.Context, config supervisorConfig) (*exec.Cmd, *supervisorProof, error) {
		shimDir = environmentMap(config.NativeEnv)[browserShimBrowserEnv]

		return exec.Command("/usr/bin/true"), &supervisorProof{}, nil
	}

	require.NoError(t, RunAccountCommand(context.Background(), AccountCommandOptions{
		CLIPath: "/usr/bin/true", CodexHome: testNativeOwnedTempDir(t), ScratchDir: parent,
		Mode: accountCommandLogin, ProcessIsolation: testProcessIsolation(),
	}))
	require.NotEmpty(t, shimDir)
}

func TestRunAccountCommandRefusesAShimHandoffToAChangedPrincipal(t *testing.T) {
	restoreAccountCommandHooks(t)
	restoreBrowserShimHooks(t)

	accountProbeVersion = func(context.Context, VersionProbeOptions) (string, error) { return minCodexVersion, nil }

	isolation := testProcessIsolation()
	// The writable home was validated against the launch principal; a policy
	// that changes identity before the generated shim is handed off must fail
	// closed rather than leave the shim owned by the wrong principal.
	browserShimMkdirTemp = func(parent string, pattern string) (string, error) {
		isolation.UID++
		isolation.GID++

		return os.MkdirTemp(parent, pattern)
	}

	require.ErrorContains(t, RunAccountCommand(context.Background(), AccountCommandOptions{
		CLIPath: "/usr/bin/true", CodexHome: testNativeOwnedTempDir(t), ScratchDir: testTraversableTempDir(t),
		Mode: accountCommandLogin, ProcessIsolation: isolation,
	}), "ownership handoff is unsupported")
}
