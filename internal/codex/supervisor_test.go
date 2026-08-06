package codex

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/savid/acp-go-codex/internal/homelock"
	"github.com/stretchr/testify/require"
)

func TestSupervisorHelpersAndBootstrap(t *testing.T) {
	root := t.TempDir()
	config := testSupervisorConfig(t, root, "/bin/sh", []string{"-c", "exit 0"})
	path, err := writeSupervisorConfig(config.Scratch, config)
	require.NoError(t, err)
	info, err := path.Stat()
	require.NoError(t, err)
	require.Equal(t, os.FileMode(0o600), info.Mode().Perm())

	decoded, err := readSupervisorConfig(path)
	require.NoError(t, err)
	require.Equal(t, config.NativePath, decoded.NativePath)

	_, err = readSupervisorConfig(nil)
	require.Error(t, err)
	_, err = readSupervisorConfig(strings.NewReader("{"))
	require.Error(t, err)
	_, err = readSupervisorConfig(strings.NewReader("{}"))
	require.Error(t, err)

	_, err = parseSupervisorReady("wrong\n")
	require.Error(t, err)
	_, err = parseSupervisorReady(supervisorReadyPrefix + "{\n")
	require.Error(t, err)
	_, err = parseSupervisorReady(supervisorReadyPrefix + `{"nativePid":0}` + "\n")
	require.Error(t, err)
	ready, err := parseSupervisorReady(supervisorReadyPrefix + `{"nativePid":42}` + "\n")
	require.NoError(t, err)
	require.Equal(t, 42, ready.NativePID)

	pidPath := filepath.Join(root, "pid")
	require.Error(t, writeNativePID("", 0))
	require.NoError(t, writeNativePID(pidPath, 42))
	pid, err := readNativePID(pidPath)
	require.NoError(t, err)
	require.Equal(t, 42, pid)
	require.NoError(t, os.WriteFile(pidPath, []byte("bad"), 0o600))
	_, err = readNativePID(pidPath)
	require.Error(t, err)

	marker := filepath.Join(root, "marker")
	require.Error(t, writeSupervisorMarker(""))
	require.NoError(t, writeSupervisorMarker(marker))
	require.NoError(t, writeSupervisorMarker(marker))

	unknownPath, err := writeSupervisorConfig(config.Scratch, config)
	require.NoError(t, err)
	require.Error(t, runSupervisor("unknown", unknownPath))

	oldInput, oldOutput, oldError, oldExit := supervisorInput, supervisorOutput, supervisorError, supervisorExit
	t.Cleanup(func() {
		supervisorInput, supervisorOutput, supervisorError, supervisorExit = oldInput, oldOutput, oldError, oldExit
	})
	supervisorInput = strings.NewReader("")
	supervisorOutput = io.Discard
	errorOutput := new(bytes.Buffer)
	supervisorError = errorOutput
	exitCode := -1
	supervisorExit = func(code int) { exitCode = code }

	t.Setenv(supervisorModeEnv, "")
	supervisorBootstrap()
	require.Equal(t, -1, exitCode)
}

func TestRunGuardianAndLivenessBranches(t *testing.T) {
	skipUnprivilegedDarwinIsolation(t)
	oldInput, oldOutput, oldError, oldExecutable := supervisorInput, supervisorOutput, supervisorError, supervisorExecutable
	t.Cleanup(func() {
		supervisorInput, supervisorOutput, supervisorError, supervisorExecutable = oldInput, oldOutput, oldError, oldExecutable
	})
	supervisorInput = strings.NewReader("")
	supervisorOutput = io.Discard
	supervisorError = io.Discard

	// The guardian runs in this process, so it uses the platform-neutral
	// identity hooks: the real authority claims one identity exclusively down an
	// inherited descriptor, which a single test process cannot establish and
	// release repeatedly. supervisor_linux_test.go proves the real authority.
	withNeutralSupervisorIdentityHooks(t)

	root := t.TempDir()
	config := testSupervisorConfig(t, filepath.Join(root, "guardian"), "/bin/sh", []string{"-c", "cat"})
	require.NoError(t, runGuardian(config))

	claim, err := homelock.AcquireClaim(config.Home)
	require.NoError(t, err)
	require.Error(t, runGuardian(config))
	require.NoError(t, claim.Release())

	livenessLock, err := homelock.AcquireLiveness(config.Home)
	require.NoError(t, err)
	require.Error(t, runGuardian(config))
	require.NoError(t, livenessLock.Release())

	missingExecutable := oldExecutable
	supervisorExecutable = func() (string, error) { return filepath.Join(root, "missing"), nil }
	require.Error(t, runGuardian(testSupervisorConfig(t, filepath.Join(root, "missing-exec"), "/bin/sh", []string{"-c", "cat"})))
	supervisorExecutable = missingExecutable

	blockingInput, blockingWriter := io.Pipe()
	supervisorInput = blockingInput
	defer blockingWriter.Close()
	failing := testSupervisorConfig(t, filepath.Join(root, "failure"), "/bin/sh", []string{"-c", "exit 7"})
	require.Error(t, runLiveness(failing))
	missing := testSupervisorConfig(t, filepath.Join(root, "missing-native"), filepath.Join(root, "does-not-exist"), nil)
	require.Error(t, runLiveness(missing))
}

func TestSupervisorCommandAndProof(t *testing.T) {
	root := t.TempDir()
	_, _, err := supervisorCommand(context.Background(), supervisorConfig{})
	require.Error(t, err)

	oldExecutable := supervisorExecutable
	t.Cleanup(func() { supervisorExecutable = oldExecutable })
	supervisorExecutable = func() (string, error) { return "", errors.New("executable failed") }
	config := testSupervisorConfig(t, root, "/bin/sh", []string{"-c", "cat"})
	_, _, err = supervisorCommand(context.Background(), config)
	require.Error(t, err)

	started := filepath.Join(root, "started")
	complete := filepath.Join(root, "complete")
	proof := &supervisorProof{started: started, completion: complete}
	require.NoError(t, writeSupervisorMarker(complete))
	require.NoError(t, proof.awaitCompletion())
	require.NoFileExists(t, complete)
	require.NoError(t, (*supervisorProof)(nil).awaitCompletion())

	badProof := &supervisorProof{started: filepath.Join(root, "directory"), completion: filepath.Join(root, "completion-directory")}
	require.NoError(t, os.Mkdir(badProof.completion, 0o700))
	require.NoError(t, badProof.awaitCompletion())

	lateStarted := filepath.Join(root, "late-started")
	lateCompletion := filepath.Join(root, "late-completion")
	lateDone := make(chan struct{})
	go func() {
		time.Sleep(75 * time.Millisecond)
		_ = writeSupervisorMarker(lateStarted)
		_ = writeSupervisorMarker(lateCompletion)
		close(lateDone)
	}()
	err = (&supervisorProof{
		started: lateStarted, completion: lateCompletion, startupWait: 25 * time.Millisecond,
	}).awaitCompletion()
	require.ErrorIs(t, err, ErrProcessContainmentIncomplete)
	<-lateDone
}

func TestGuardianCleansQuarantineMarkersWithoutCaller(t *testing.T) {
	root := t.TempDir()
	config := supervisorConfig{
		Started: filepath.Join(root, "started"), Completion: filepath.Join(root, "complete"),
		Quarantine: filepath.Join(root, "quarantine"), NativePIDFile: filepath.Join(root, "pid"),
		ProviderSnapshot: filepath.Join(root, "snapshot"),
	}
	for _, path := range []string{config.Started, config.Quarantine, config.NativePIDFile, config.ProviderSnapshot} {
		require.NoError(t, writeSupervisorMarker(path))
	}

	livenessDone := make(chan error)
	cleanupDone := make(chan error, 1)
	go func() { cleanupDone <- finishQuarantinedLiveness(livenessDone, config) }()
	require.FileExists(t, config.Quarantine)
	livenessDone <- nil
	require.ErrorIs(t, <-cleanupDone, ErrProcessContainmentIncomplete)

	for _, path := range []string{config.Started, config.Completion, config.Quarantine, config.NativePIDFile, config.ProviderSnapshot} {
		require.NoFileExists(t, path)
	}
}

func TestSupervisorStreamAndQuiescenceHelpers(t *testing.T) {
	var output bytes.Buffer
	done := make(chan struct{}, 1)
	copySupervisorStream(&output, strings.NewReader("data"), done)
	<-done
	require.Equal(t, "data", output.String())

	attempts := 0
	require.NoError(t, awaitQuiescence(func() error {
		attempts++

		return nil
	}))
	require.Equal(t, 1, attempts)
	require.ErrorIs(t, awaitQuiescence(func() error { return errors.New("unproven") }), ErrProcessContainmentIncomplete)
}

func testSupervisorConfig(t *testing.T, root string, nativePath string, args []string) supervisorConfig {
	t.Helper()
	scratch := filepath.Join(root, "scratch")
	require.NoError(t, os.MkdirAll(scratch, 0o700))

	return supervisorConfig{
		NativePath:          nativePath,
		NativeArgs:          args,
		NativeEnv:           os.Environ(),
		Home:                filepath.Join(root, "home"),
		Scratch:             scratch,
		ScratchParent:       root,
		LifecycleKind:       "runtime",
		DarwinBestEffort:    true,
		Started:             filepath.Join(scratch, "started"),
		Completion:          filepath.Join(scratch, "complete"),
		NativePIDFile:       filepath.Join(scratch, "native.pid"),
		IsolationUID:        testProcessIsolation().UID,
		IsolationGID:        testProcessIsolation().GID,
		StandaloneOwnerID:   testProcessIsolation().StandaloneOwnerID,
		StandaloneStateRoot: testProcessIsolation().StandaloneStateRoot,
		Isolation:           testProcessIsolation(),
	}
}

func TestSupervisorConfigRootPermissions(t *testing.T) {
	root := filepath.Join(t.TempDir(), "private")
	config := testSupervisorConfig(t, root, "/bin/sh", []string{"-c", "exit 0"})
	path, err := writeSupervisorConfig(config.Scratch, config)
	require.NoError(t, err)
	defer path.Close()
	info, err := os.Stat(config.Scratch)
	require.NoError(t, err)
	require.Equal(t, os.FileMode(0o700), info.Mode().Perm())

	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	cmd, _, err := supervisorCommand(ctx, supervisorConfig{Scratch: filepath.Join(root, "other"), Home: config.Home, NativePath: config.NativePath, NativeEnv: os.Environ(), Isolation: testProcessIsolation()})
	require.NoError(t, err)
	require.Zero(t, cmd.WaitDelay)
	require.Nil(t, cmd.Cancel)
}
