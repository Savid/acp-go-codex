package codex

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
)

// closedInheritedProof returns a proof whose inherited descriptor has already
// been released, so closing it again fails the way a lost descriptor does.
func closedInheritedProof(t *testing.T, proof *supervisorProof) *supervisorProof {
	t.Helper()

	file, err := os.CreateTemp(t.TempDir(), "inherited")
	require.NoError(t, err)
	require.NoError(t, file.Close())

	proof.inherited = []*os.File{file}

	return proof
}

// killedAfterTest releases a supervised command that the quarantine paths
// deliberately return from without reaping, so its waiter can finish.
func killedAfterTest(t *testing.T, cmd *exec.Cmd) *exec.Cmd {
	t.Helper()
	t.Cleanup(func() { _ = killProcess(cmd) })

	return cmd
}

func TestRunAccountCommandRefusesUnownedHomeAndMissingIsolation(t *testing.T) {
	restoreAccountCommandHooks(t)

	foreign := testProcessIsolation()
	foreign.UID++
	foreign.GID++
	require.ErrorContains(t, RunAccountCommand(context.Background(), AccountCommandOptions{
		CLIPath: "/usr/bin/true", CodexHome: testNativeOwnedTempDir(t), Mode: accountCommandLogout,
		ProcessIsolation: foreign,
	}), "validate codex writable home")

	require.ErrorContains(t, RunAccountCommand(context.Background(), AccountCommandOptions{
		CLIPath: "/usr/bin/true", CodexHome: testNativeOwnedTempDir(t), Mode: accountCommandLogout,
	}), "process isolation is required")
}

func TestRunAccountCommandRefusesWithoutAHomeLockRoot(t *testing.T) {
	restoreAccountCommandHooks(t)
	SetScratchParentResolver(func(string) (string, error) { return "", nil })
	accountProbeVersion = func(context.Context, VersionProbeOptions) (string, error) { return minCodexVersion, nil }
	accountMkdirTemp = func(_ string, pattern string) (string, error) { return os.MkdirTemp(t.TempDir(), pattern) }

	require.ErrorContains(t, RunAccountCommand(context.Background(), AccountCommandOptions{
		CLIPath: "/usr/bin/true", CodexHome: testNativeOwnedTempDir(t), Mode: accountCommandLogout,
		ProcessIsolation: testProcessIsolation(),
	}), "home-lock scratch parent is required")
}

func TestRunAccountCommandProofBranches(t *testing.T) {
	t.Run("close inherited", func(t *testing.T) {
		restoreAccountCommandHooks(t)
		accountProbeVersion = func(context.Context, VersionProbeOptions) (string, error) { return minCodexVersion, nil }
		accountSupervisorCommand = func(context.Context, supervisorConfig) (*exec.Cmd, *supervisorProof, error) {
			return exec.Command("/bin/sh", "-c", "sleep 10"), closedInheritedProof(t, &supervisorProof{}), nil
		}

		require.ErrorContains(t, RunAccountCommand(context.Background(), AccountCommandOptions{
			CLIPath: "/usr/bin/true", CodexHome: testNativeOwnedTempDir(t), Mode: accountCommandLogout,
			ProcessIsolation: testProcessIsolation(),
		}), "close inherited supervisor config")
	})

	t.Run("quarantine stat", func(t *testing.T) {
		restoreAccountCommandHooks(t)
		accountProbeVersion = func(context.Context, VersionProbeOptions) (string, error) { return minCodexVersion, nil }

		blocker := filepath.Join(t.TempDir(), "blocker")
		require.NoError(t, os.WriteFile(blocker, []byte("x"), 0o600))
		accountSupervisorCommand = func(context.Context, supervisorConfig) (*exec.Cmd, *supervisorProof, error) {
			return killedAfterTest(t, exec.Command("/bin/sh", "-c", "sleep 10")),
				&supervisorProof{quarantine: filepath.Join(blocker, "child")}, nil
		}

		err := RunAccountCommand(context.Background(), AccountCommandOptions{
			CLIPath: "/usr/bin/true", CodexHome: testNativeOwnedTempDir(t), Mode: accountCommandLogout,
			ProcessIsolation: testProcessIsolation(),
		})
		require.ErrorIs(t, err, ErrProcessContainmentIncomplete)
		require.ErrorContains(t, err, "not a directory")
	})

	t.Run("quarantined", func(t *testing.T) {
		restoreAccountCommandHooks(t)
		accountProbeVersion = func(context.Context, VersionProbeOptions) (string, error) { return minCodexVersion, nil }

		root := t.TempDir()
		quarantine := filepath.Join(root, "quarantine")
		require.NoError(t, writeSupervisorMarker(quarantine))
		accountSupervisorCommand = func(context.Context, supervisorConfig) (*exec.Cmd, *supervisorProof, error) {
			return killedAfterTest(t, exec.Command("/bin/sh", "-c", "sleep 10")), &supervisorProof{
				started:    filepath.Join(root, "started"),
				completion: filepath.Join(root, "complete"),
				quarantine: quarantine,
			}, nil
		}

		err := RunAccountCommand(context.Background(), AccountCommandOptions{
			CLIPath: "/usr/bin/true", CodexHome: testNativeOwnedTempDir(t), Mode: accountCommandLogout,
			ProcessIsolation: testProcessIsolation(),
		})
		require.ErrorIs(t, err, ErrProcessContainmentIncomplete)
		require.ErrorContains(t, err, "retained the identity lock")
	})
}

func TestProbeVersionRejectsPolicyAndLockFailures(t *testing.T) {
	_, err := ProbeVersion(context.Background(), VersionProbeOptions{CLIPath: "/usr/bin/true"})
	require.ErrorContains(t, err, "process isolation is required")

	options := withTestVersionIsolation(VersionProbeOptions{CLIPath: "/usr/bin/true"})
	options.ScratchParent = ""
	_, err = ProbeVersion(context.Background(), options)
	require.ErrorContains(t, err, "home-lock scratch parent is required")
}

func TestProbeVersionReportsLostInheritedDescriptor(t *testing.T) {
	original := versionSupervisorCommand
	t.Cleanup(func() { versionSupervisorCommand = original })

	versionSupervisorCommand = func(context.Context, supervisorConfig) (*exec.Cmd, *supervisorProof, error) {
		return exec.Command("/bin/sh", "-c", "sleep 10"), closedInheritedProof(t, &supervisorProof{}), nil
	}

	_, err := ProbeVersion(context.Background(), withTestVersionIsolation(VersionProbeOptions{CLIPath: "/usr/bin/true"}))
	require.ErrorContains(t, err, "close inherited supervisor config")
}

func TestSupervisorWaiterResultStartsWhenNotPaused(t *testing.T) {
	source := make(chan error, 1)
	released := make(chan struct{})
	waiter := newSupervisorWaiterResult(source, func() { close(released) }, false)

	source <- errors.New("wait result")
	<-released
	require.ErrorContains(t, <-waiter.result(), "wait result")
}

func TestAwaitLivenessTerminalReportsQuarantineStatFailure(t *testing.T) {
	blocker := filepath.Join(t.TempDir(), "blocker")
	require.NoError(t, os.WriteFile(blocker, []byte("x"), 0o600))

	waitErr, quarantined, terminalErr := awaitLivenessTerminal(make(chan error), filepath.Join(blocker, "child"))
	require.NoError(t, waitErr)
	require.False(t, quarantined)
	require.ErrorContains(t, terminalErr, "stat liveness quarantine proof")
}
