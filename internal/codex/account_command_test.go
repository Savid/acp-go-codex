package codex

import (
	"bytes"
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestAccountCommandArgs(t *testing.T) {
	require.Equal(t, []string{"login"}, requireAccountArgs(t, "login", false))
	require.Equal(t, []string{"login", "--device-auth"}, requireAccountArgs(t, "login", true))
	require.Equal(t, []string{"logout"}, requireAccountArgs(t, "logout", true))
	_, err := accountCommandArgs("invalid", false)
	require.Error(t, err)
}

func requireAccountArgs(t *testing.T, mode string, deviceAuth bool) []string {
	t.Helper()
	args, err := accountCommandArgs(mode, deviceAuth)
	require.NoError(t, err)

	return args
}

func TestRunAccountCommandFailureBranches(t *testing.T) {
	restoreAccountCommandHooks(t)

	require.Error(t, RunAccountCommand(context.Background(), AccountCommandOptions{Mode: "invalid"}))
	require.Error(t, RunAccountCommand(context.Background(), AccountCommandOptions{Mode: "logout"}))
	t.Setenv("PATH", "")
	require.ErrorContains(t, RunAccountCommand(context.Background(), AccountCommandOptions{
		CodexHome: testNativeOwnedTempDir(t), Mode: "logout", ProcessIsolation: testProcessIsolation(),
	}), "find codex CLI")
	require.Error(t, RunAccountCommand(context.Background(), AccountCommandOptions{
		CLIPath: filepath.Join(testTraversableTempDir(t), "missing"), CodexHome: testNativeOwnedTempDir(t), Mode: "logout", ProcessIsolation: testProcessIsolation(),
	}))

	old := writeAccountCommandScript(t, "#!/bin/sh\necho codex-cli 0.144.0\n")
	require.ErrorContains(t, RunAccountCommand(context.Background(), AccountCommandOptions{
		CLIPath: old, CodexHome: testNativeOwnedTempDir(t), Mode: "logout", ProcessIsolation: testProcessIsolation(),
	}), "too old")

	valid := writeAccountCommandScript(t, "#!/bin/sh\necho codex-cli 0.144.1\n")
	SetScratchParentResolver(nil)
	require.ErrorContains(t, RunAccountCommand(context.Background(), AccountCommandOptions{
		CLIPath: valid, CodexHome: testNativeOwnedTempDir(t), Mode: "logout", ProcessIsolation: testProcessIsolation(),
	}), "scratch parent resolver")

	SetScratchParentResolver(func(string) (string, error) { return "", errors.New("scratch parent") })
	require.ErrorContains(t, RunAccountCommand(context.Background(), AccountCommandOptions{
		CLIPath: valid, CodexHome: testNativeOwnedTempDir(t), Mode: "logout", ProcessIsolation: testProcessIsolation(),
	}), "scratch parent")

	configuredScratch := testTraversableTempDir(t)
	SetScratchParentResolver(func(dir string) (string, error) { return dir, nil })
	accountMkdirTemp = func(parent string, _ string) (string, error) {
		require.Equal(t, configuredScratch, parent)

		return "", errors.New("mkdir")
	}
	require.ErrorContains(t, RunAccountCommand(context.Background(), AccountCommandOptions{
		CLIPath: valid, CodexHome: testNativeOwnedTempDir(t), ScratchDir: configuredScratch, Mode: "logout", ProcessIsolation: testProcessIsolation(),
	}), "mkdir")

	defaultScratch := testTraversableTempDir(t)
	SetScratchParentResolver(func(dir string) (string, error) {
		if dir != "" {
			return dir, nil
		}

		return defaultScratch, nil
	})
	accountMkdirTemp = os.MkdirTemp
	accountSupervisorCommand = func(context.Context, supervisorConfig) (*exec.Cmd, *supervisorProof, error) {
		return nil, nil, errors.New("supervisor")
	}
	require.ErrorContains(t, RunAccountCommand(context.Background(), AccountCommandOptions{
		CLIPath: valid, CodexHome: testNativeOwnedTempDir(t), Mode: "logout", ProcessIsolation: testProcessIsolation(),
	}), "supervisor")

	accountSupervisorCommand = func(context.Context, supervisorConfig) (*exec.Cmd, *supervisorProof, error) {
		return exec.Command(valid, "logout"), &supervisorProof{}, nil
	}
	accountStartProcess = func(*exec.Cmd) (*supervisorWaiter, error) {
		return nil, errors.New("start")
	}
	require.ErrorContains(t, RunAccountCommand(context.Background(), AccountCommandOptions{
		CLIPath: valid, CodexHome: testNativeOwnedTempDir(t), Mode: "logout", ProcessIsolation: testProcessIsolation(),
	}), "start")
}

func TestRunAccountCommandForwardsSignalToGuardian(t *testing.T) {
	restoreAccountCommandHooks(t)
	valid := writeAccountCommandScript(t, "#!/bin/sh\necho codex-cli 0.144.1\n")
	accountSupervisorCommand = func(context.Context, supervisorConfig) (*exec.Cmd, *supervisorProof, error) {
		return exec.Command("/bin/sh", "-c", "sleep 10"), &supervisorProof{}, nil
	}
	signals := make(chan os.Signal, 1)
	signals <- os.Kill
	err := RunAccountCommand(context.Background(), AccountCommandOptions{
		CLIPath: valid, CodexHome: testNativeOwnedTempDir(t), Mode: "logout", Signals: signals, ProcessIsolation: testProcessIsolation(),
	})
	require.Error(t, err)
}

func TestRunAccountCommandClosedSignalsAndCleanupError(t *testing.T) {
	restoreAccountCommandHooks(t)
	script := writeAccountCommandScript(t, `#!/bin/sh
if [ "$1" = "--version" ]; then
  echo codex-cli 0.144.1
  exit 0
fi
sleep 0.05
`)

	closedSignals := make(chan os.Signal)
	close(closedSignals)
	var scratch string
	accountRemoveAll = func(path string) error {
		scratch = path

		return errors.New("cleanup")
	}
	var output bytes.Buffer
	err := RunAccountCommand(context.Background(), AccountCommandOptions{
		CLIPath: script, CodexHome: testNativeOwnedTempDir(t), Mode: "logout", ProcessIsolation: testProcessIsolation(),
		Stdout: &output, Stderr: &output, Signals: closedSignals,
	})
	require.ErrorContains(t, err, "cleanup")
	require.NotEmpty(t, scratch)
	require.NoError(t, os.RemoveAll(scratch))
}

func TestRunAccountCommandUsesSeparateVersionGeneration(t *testing.T) {
	restoreAccountCommandHooks(t)
	parent := testTraversableTempDir(t)
	home := testNativeOwnedTempDir(t)
	SetScratchParentResolver(func(string) (string, error) { return parent, nil })

	var versionScratch string
	accountProbeVersion = func(_ context.Context, options VersionProbeOptions) (string, error) {
		versionScratch = options.Scratch

		return minCodexVersion, nil
	}

	var accountScratch string
	accountSupervisorCommand = func(_ context.Context, config supervisorConfig) (*exec.Cmd, *supervisorProof, error) {
		accountScratch = config.Scratch

		return exec.Command("/usr/bin/true"), &supervisorProof{}, nil
	}

	err := RunAccountCommand(context.Background(), AccountCommandOptions{
		CLIPath: "/usr/bin/true", CodexHome: home, ScratchDir: parent, Mode: accountCommandLogout, ProcessIsolation: testProcessIsolation(),
	})
	require.NoError(t, err)
	require.NotEmpty(t, versionScratch)
	require.NotEmpty(t, accountScratch)
	require.NotEqual(t, versionScratch, accountScratch)
	require.Equal(t, filepath.Dir(versionScratch), parent)
	require.Equal(t, filepath.Dir(accountScratch), parent)
	require.NoDirExists(t, versionScratch)
	require.NoDirExists(t, accountScratch)
}

func TestRunAccountCommandAccountGenerationAndClosedSignalBranches(t *testing.T) {
	t.Run("account generation", func(t *testing.T) {
		restoreAccountCommandHooks(t)
		parent := testTraversableTempDir(t)
		SetScratchParentResolver(func(string) (string, error) { return parent, nil })
		accountProbeVersion = func(context.Context, VersionProbeOptions) (string, error) {
			return minCodexVersion, nil
		}
		calls := 0
		accountMkdirTemp = func(parent, pattern string) (string, error) {
			calls++
			if calls == 2 {
				return "", errors.New("account generation")
			}

			return os.MkdirTemp(parent, pattern)
		}
		err := RunAccountCommand(context.Background(), AccountCommandOptions{
			CLIPath: "/usr/bin/true", CodexHome: testNativeOwnedTempDir(t), Mode: accountCommandLogout, ProcessIsolation: testProcessIsolation(),
		})
		require.ErrorContains(t, err, "account generation")
		require.Equal(t, 2, calls)
	})

	t.Run("closed signals", func(t *testing.T) {
		restoreAccountCommandHooks(t)
		accountProbeVersion = func(context.Context, VersionProbeOptions) (string, error) {
			return minCodexVersion, nil
		}
		accountSupervisorCommand = func(context.Context, supervisorConfig) (*exec.Cmd, *supervisorProof, error) {
			return exec.Command("/bin/sh", "-c", "sleep 0.05"), &supervisorProof{}, nil
		}
		signals := make(chan os.Signal)
		close(signals)
		require.NoError(t, RunAccountCommand(context.Background(), AccountCommandOptions{
			CLIPath: "/usr/bin/true", CodexHome: testNativeOwnedTempDir(t), Mode: accountCommandLogout, Signals: signals, ProcessIsolation: testProcessIsolation(),
		}))
	})
}

func writeAccountCommandScript(t *testing.T, contents string) string {
	t.Helper()
	path := filepath.Join(testTraversableTempDir(t), "codex")
	require.NoError(t, os.WriteFile(path, []byte(contents), 0o700))

	return path
}

func restoreAccountCommandHooks(t *testing.T) {
	t.Helper()
	scratchParent := accountScratchParent
	mkdirTemp := accountMkdirTemp
	removeAll := accountRemoveAll
	start := accountStartProcess
	supervisor := accountSupervisorCommand
	probeVersion := accountProbeVersion
	t.Cleanup(func() {
		accountScratchParent = scratchParent
		accountMkdirTemp = mkdirTemp
		accountRemoveAll = removeAll
		accountStartProcess = start
		accountSupervisorCommand = supervisor
		accountProbeVersion = probeVersion
	})
	accountProbeVersion = func(_ context.Context, options VersionProbeOptions) (string, error) {
		output, err := exec.Command(options.CLIPath, codexVersionArgument).Output()
		if err != nil {
			return "", err
		}

		return validateCodexVersionOutput(string(output))
	}
	defaultScratch := t.TempDir()
	SetScratchParentResolver(func(dir string) (string, error) {
		if dir != "" {
			return dir, nil
		}

		return defaultScratch, nil
	})
}
