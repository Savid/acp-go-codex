//go:build unix && !linux

package codex

import (
	"context"
	"errors"
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
		CLIPath: "/usr/bin/true", CodexHome: t.TempDir(), ScratchDir: parent,
		Mode: accountCommandLogin,
	}))
	require.NotEmpty(t, shimDir)
}

func TestRunAccountCommandRefusesFailedBrowserShimHandoff(t *testing.T) {
	restoreAccountCommandHooks(t)
	restoreBrowserShimHooks(t)

	accountProbeVersion = func(context.Context, VersionProbeOptions) (string, error) { return minCodexVersion, nil }
	browserShimHandoffGeneratedNativeTree = func(string, *ProcessIsolation) error {
		return errors.New("handoff failed")
	}

	require.ErrorContains(t, RunAccountCommand(context.Background(), AccountCommandOptions{
		CLIPath: "/usr/bin/true", CodexHome: t.TempDir(), ScratchDir: testTraversableTempDir(t),
		Mode: accountCommandLogin,
	}), "handoff failed")
}
