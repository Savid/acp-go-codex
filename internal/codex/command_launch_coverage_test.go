package codex

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// writeFakeAppServer materialises a codex stand-in that answers the launch
// handshake and then holds its stdio open like the real app-server does.
func writeFakeAppServer(t *testing.T) string {
	t.Helper()

	path := filepath.Join(testTraversableTempDir(t), "codex-app-server")
	require.NoError(t, os.WriteFile(path, []byte(`#!/bin/sh
read line || exit 0
echo '{"jsonrpc":"2.0","id":1,"result":{}}'
read line || true
while read line; do :; done
`), 0o700))

	return path
}

func preserveLaunchHooks(t *testing.T) {
	t.Helper()

	command := execCommandContext
	writeConfig := supervisorWriteConfig
	t.Cleanup(func() {
		execCommandContext = command
		supervisorWriteConfig = writeConfig
	})
}

func TestNewAppServerClientCompletesLaunchHandshake(t *testing.T) {
	preserveLaunchHooks(t)

	appServer := writeFakeAppServer(t)
	// The guardian is replaced by the app-server stand-in so the launch keeps
	// its real supervisor config, inherited descriptor, and proof plumbing
	// while the handshake runs on a host that cannot drop supplementary groups.
	execCommandContext = func(context.Context, string, ...string) *exec.Cmd { return exec.Command(appServer) }

	observed := make(chan int, 1)
	unproven := make(chan struct{}, 1)

	client, err := NewAppServerClient(context.Background(), Options{
		CLIPath: appServer, CodexHome: testTraversableTempDir(t),
		SupervisorRoot: testTraversableTempDir(t), SupervisorParent: os.TempDir(),
		NativeVersion: minCodexVersion, LaunchTimeout: 5 * time.Second,
		ProcessIsolation: testProcessIsolation(),
		NewProcessSnapshotObserver: func(context.Context) ProcessSnapshotObserver {
			return ProcessSnapshotObserver{
				Observe:  func(_ context.Context, count int) { observed <- count },
				Unproven: func() { unproven <- struct{}{} },
			}
		},
	})
	require.NoError(t, err)
	require.Equal(t, minCodexVersion, client.nativeVersion)

	// No liveness supervisor published a completion proof for the stand-in, so
	// the close must fail closed rather than report a clean containment.
	require.ErrorIs(t, client.Close(context.Background()), ErrProcessContainmentIncomplete)
	require.Len(t, unproven, 1)
	require.Empty(t, observed)
}

func TestLaunchAppServerReportsLostInheritedDescriptor(t *testing.T) {
	preserveLaunchHooks(t)

	sleeper := sleepCommand(t, "10")
	execCommandContext = func(context.Context, string, ...string) *exec.Cmd {
		return exec.Command(sleeper.Path, "10")
	}
	supervisorWriteConfig = func(string, supervisorConfig) (*os.File, error) {
		file, err := os.CreateTemp(t.TempDir(), "supervisor-config")
		require.NoError(t, err)
		require.NoError(t, file.Close())

		return file, nil
	}

	transport, _, _, err := launchAppServer(context.Background(), context.Background(), Options{
		CLIPath: sleeper.Path, SupervisorRoot: testTraversableTempDir(t), SupervisorParent: os.TempDir(),
		WritableHome: testTraversableTempDir(t), NativeVersion: minCodexVersion,
		ProcessIsolation: testProcessIsolation(),
	})
	require.Nil(t, transport)
	require.ErrorContains(t, err, "close inherited supervisor config")
}

func TestLaunchAppServerRevalidatesCredentialPolicy(t *testing.T) {
	preserveLaunchHooks(t)

	isolation := testProcessIsolation()
	sleeper := sleepCommand(t, "10")
	// A caller-owned policy that loses its group identity after the child
	// environment was built must fail the launch instead of spawning.
	execCommandContext = func(context.Context, string, ...string) *exec.Cmd {
		isolation.GID = 0

		return exec.Command(sleeper.Path, "10")
	}

	transport, _, _, err := launchAppServer(context.Background(), context.Background(), Options{
		CLIPath: sleeper.Path, NativeVersion: minCodexVersion, skipSupervisor: true, ProcessIsolation: isolation,
	})
	require.Nil(t, transport)
	require.ErrorContains(t, err, "uid and gid must be nonzero")
}

func TestProcessBeginWaitRequiresAWaiter(t *testing.T) {
	cmd := sleepCommand(t, "10")
	waiter, err := startProcess(cmd)
	require.NoError(t, err)
	t.Cleanup(func() {
		_ = killProcess(cmd)
		waiter.start()
		<-waiter.result()
	})

	proc := &process{cmd: cmd}
	proc.beginWait()
	<-proc.waitDone
	require.ErrorContains(t, proc.waitErr, "codex process waiter is unavailable")
}
