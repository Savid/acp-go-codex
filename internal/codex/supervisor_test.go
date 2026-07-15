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
	info, err := os.Stat(path)
	require.NoError(t, err)
	require.Equal(t, os.FileMode(0o600), info.Mode().Perm())

	decoded, err := readSupervisorConfig(path)
	require.NoError(t, err)
	require.Equal(t, config.NativePath, decoded.NativePath)
	_, err = os.Stat(path)
	require.ErrorIs(t, err, os.ErrNotExist)

	_, err = readSupervisorConfig("")
	require.Error(t, err)
	_, err = readSupervisorConfig(filepath.Join(root, "missing"))
	require.Error(t, err)
	bad := filepath.Join(root, "bad.json")
	require.NoError(t, os.WriteFile(bad, []byte("{"), 0o600))
	_, err = readSupervisorConfig(bad)
	require.Error(t, err)
	incomplete := filepath.Join(root, "incomplete.json")
	require.NoError(t, os.WriteFile(incomplete, []byte("{}"), 0o600))
	_, err = readSupervisorConfig(incomplete)
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

	unknownPath, err = writeSupervisorConfig(config.Scratch, config)
	require.NoError(t, err)
	t.Setenv(supervisorModeEnv, "unknown")
	t.Setenv(supervisorConfigEnv, unknownPath)
	supervisorBootstrap()
	require.Equal(t, 1, exitCode)
	require.Contains(t, errorOutput.String(), "unknown internal mode")

	exitCode = -1
	config = testSupervisorConfig(t, filepath.Join(root, "success"), "/bin/sh", []string{"-c", "exit 0"})
	successPath, err := writeSupervisorConfig(config.Scratch, config)
	require.NoError(t, err)
	t.Setenv(supervisorModeEnv, supervisorModeLiveness)
	t.Setenv(supervisorConfigEnv, successPath)
	supervisorBootstrap()
	require.Equal(t, 0, exitCode)
}

func TestRunGuardianAndLivenessBranches(t *testing.T) {
	oldInput, oldOutput, oldError, oldExecutable := supervisorInput, supervisorOutput, supervisorError, supervisorExecutable
	t.Cleanup(func() {
		supervisorInput, supervisorOutput, supervisorError, supervisorExecutable = oldInput, oldOutput, oldError, oldExecutable
	})
	supervisorInput = strings.NewReader("")
	supervisorOutput = io.Discard
	supervisorError = io.Discard

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
	require.NoError(t, (*supervisorProof)(nil).awaitCompletion())

	badProof := &supervisorProof{started: filepath.Join(root, "directory"), completion: filepath.Join(root, "completion-directory")}
	require.NoError(t, os.Mkdir(badProof.completion, 0o700))
	require.NoError(t, badProof.awaitCompletion())
}

func TestSupervisorStreamAndQuiescenceHelpers(t *testing.T) {
	var output bytes.Buffer
	done := make(chan struct{}, 1)
	copySupervisorStream(&output, strings.NewReader("data"), done)
	<-done
	require.Equal(t, "data", output.String())

	attempts := 0
	awaitQuiescence(func() error {
		attempts++
		if attempts == 1 {
			return errors.New("not yet")
		}

		return nil
	})
	require.Equal(t, 2, attempts)
}

func testSupervisorConfig(t *testing.T, root string, nativePath string, args []string) supervisorConfig {
	t.Helper()
	scratch := filepath.Join(root, "scratch")
	require.NoError(t, os.MkdirAll(scratch, 0o700))

	return supervisorConfig{
		NativePath:    nativePath,
		NativeArgs:    args,
		NativeEnv:     os.Environ(),
		Home:          filepath.Join(root, "home"),
		Scratch:       scratch,
		Started:       filepath.Join(scratch, "started"),
		Completion:    filepath.Join(scratch, "complete"),
		NativePIDFile: filepath.Join(scratch, "native.pid"),
	}
}

func TestSupervisorConfigRootPermissions(t *testing.T) {
	root := filepath.Join(t.TempDir(), "private")
	config := testSupervisorConfig(t, root, "/bin/sh", []string{"-c", "exit 0"})
	path, err := writeSupervisorConfig(config.Scratch, config)
	require.NoError(t, err)
	defer os.Remove(path)
	info, err := os.Stat(config.Scratch)
	require.NoError(t, err)
	require.Equal(t, os.FileMode(0o700), info.Mode().Perm())

	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	_, _, err = supervisorCommand(ctx, supervisorConfig{Scratch: filepath.Join(root, "other"), Home: config.Home, NativePath: config.NativePath})
	require.NoError(t, err)
}
