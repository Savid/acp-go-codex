//go:build linux

package codex

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/savid/acp-go-codex/internal/homelock"
	"github.com/stretchr/testify/require"
)

type supervisorErrorWriter struct{ err error }

func (writer supervisorErrorWriter) Write([]byte) (int, error) { return 0, writer.err }

type supervisorErrorReader struct{ err error }

func (reader supervisorErrorReader) Read([]byte) (int, error) { return 0, reader.err }

type supervisorCloseBuffer struct {
	bytes.Buffer
	closed bool
}

type supervisorWriteFailure struct {
	bytes.Buffer
	failAt int
	writes int
	closed bool
}

func (writer *supervisorWriteFailure) Write(value []byte) (int, error) {
	writer.writes++
	if writer.writes == writer.failAt {
		return 0, errors.New("write failed")
	}

	return writer.Buffer.Write(value)
}

func (writer *supervisorWriteFailure) Close() error {
	writer.closed = true

	return nil
}

func (buffer *supervisorCloseBuffer) Close() error {
	buffer.closed = true

	return nil
}

func preserveSupervisorGlobals(t *testing.T) {
	t.Helper()
	oldExecutable := supervisorExecutable
	oldCommand := supervisorExecCommand
	oldRandRead := supervisorRandRead
	oldChmod := supervisorChmod
	oldOpenFile := supervisorOpenFile
	oldEncode := supervisorEncodeConfig
	oldGuardianContainment := supervisorNewGuardianContainment
	oldLivenessContainment := supervisorOpenLivenessContainment
	oldGuardianQuiesce := supervisorGuardianQuiesce
	oldLivenessQuiesce := supervisorLivenessQuiesce
	oldQuarantineRetry := supervisorQuarantineRetry
	oldGuardianQuarantineRetry := supervisorGuardianQuarantineRetry
	oldInput := supervisorInput
	oldOutput := supervisorOutput
	oldError := supervisorError
	oldExit := supervisorExit
	oldAcquireIdentityAuthority := supervisorAcquireIdentityAuthority
	oldVerifyTrustedIdentity := supervisorVerifyTrustedIdentity
	oldAdoptIdentityLock := supervisorAdoptIdentityLock
	oldAdoptAuthorityDomain := supervisorAdoptAuthorityDomain
	oldValidateAdoptedAuthority := supervisorValidateAdoptedAuthority
	oldValidateGuardianPeer := supervisorValidateGuardianPeer
	t.Cleanup(func() {
		supervisorExecutable = oldExecutable
		supervisorExecCommand = oldCommand
		supervisorRandRead = oldRandRead
		supervisorChmod = oldChmod
		supervisorOpenFile = oldOpenFile
		supervisorEncodeConfig = oldEncode
		supervisorNewGuardianContainment = oldGuardianContainment
		supervisorOpenLivenessContainment = oldLivenessContainment
		supervisorGuardianQuiesce = oldGuardianQuiesce
		supervisorLivenessQuiesce = oldLivenessQuiesce
		supervisorQuarantineRetry = oldQuarantineRetry
		supervisorGuardianQuarantineRetry = oldGuardianQuarantineRetry
		supervisorInput = oldInput
		supervisorOutput = oldOutput
		supervisorError = oldError
		supervisorExit = oldExit
		supervisorAcquireIdentityAuthority = oldAcquireIdentityAuthority
		supervisorVerifyTrustedIdentity = oldVerifyTrustedIdentity
		supervisorAdoptIdentityLock = oldAdoptIdentityLock
		supervisorAdoptAuthorityDomain = oldAdoptAuthorityDomain
		supervisorValidateAdoptedAuthority = oldValidateAdoptedAuthority
		supervisorValidateGuardianPeer = oldValidateGuardianPeer
	})
}

// withTestSupervisorIdentity fills the identity a private supervisor config
// must carry. readSupervisorConfig rejects a config whose isolation IDs are
// zero, and on Linux the guardian binds the standalone owner and state root
// before it dispatches, so a config naming neither never reaches the branch the
// case is about. Every fixture claims the one package identity: the authority
// binds a UID to a single owner and state root permanently.
func withTestSupervisorIdentity(config supervisorConfig) supervisorConfig {
	isolation := testProcessIsolation()
	config.IsolationUID = isolation.UID
	config.IsolationGID = isolation.GID
	config.StandaloneOwnerID = isolation.StandaloneOwnerID
	config.StandaloneStateRoot = isolation.StandaloneStateRoot
	config.Isolation = isolation

	return config
}

func TestSupervisorConfigAndDispatchFailures(t *testing.T) {
	_, err := readSupervisorConfig(nil)
	require.ErrorContains(t, err, "missing private")

	root := t.TempDir()
	_, err = readSupervisorConfig(strings.NewReader("{"))
	require.ErrorContains(t, err, "decode private")

	_, err = readSupervisorConfig(strings.NewReader(`{"nativePath":"x"}`))
	require.ErrorContains(t, err, "incomplete")

	_, err = writeSupervisorConfig("", supervisorConfig{})
	require.ErrorContains(t, err, "scratch root is required")
	notDir := filepath.Join(root, "not-dir")
	require.NoError(t, os.WriteFile(notDir, []byte("x"), 0o600))
	_, err = writeSupervisorConfig(filepath.Join(notDir, "child"), supervisorConfig{})
	require.ErrorContains(t, err, "create private")

	config := supervisorConfig{NativePath: "x", Home: "h", Scratch: "s", IsolationUID: 1, IsolationGID: 2}
	path, err := writeSupervisorConfig(root, config)
	require.NoError(t, err)
	loaded, err := readSupervisorConfig(path)
	require.NoError(t, err)
	require.Equal(t, config.NativePath, loaded.NativePath)

	path, err = writeSupervisorConfig(root, config)
	require.NoError(t, err)
	err = runSupervisor("unknown", path)
	require.ErrorContains(t, err, "unknown internal mode")
}

func TestSupervisorCommandNonceEnvironmentAndProof(t *testing.T) {
	preserveSupervisorGlobals(t)
	root := t.TempDir()
	isolation := testProcessIsolation()
	supervisorExecutable = func() (string, error) { return "", errors.New("lookup failed") }
	_, _, err := supervisorCommand(context.Background(), supervisorConfig{Scratch: root, Isolation: isolation})
	require.ErrorContains(t, err, "resolve embedded")

	supervisorExecutable = os.Executable
	cmd, proof, err := supervisorCommand(context.Background(), supervisorConfig{
		NativePath: "/bin/true", Home: filepath.Join(root, "home"), Scratch: root, Isolation: isolation,
	})
	require.NoError(t, err)
	require.NotNil(t, cmd)
	require.NotNil(t, proof)
	require.Equal(t, []string{supervisorModeEnv + "=" + supervisorModeGuardian}, cmd.Env)
	require.Equal(t, "/", cmd.Dir)
	if cmd.SysProcAttr != nil {
		require.Nil(t, cmd.SysProcAttr.Credential)
	}

	nonce, err := supervisorNonce()
	require.NoError(t, err)
	require.Len(t, nonce, 32)

	require.NoError(t, (*supervisorProof)(nil).awaitCompletion())
	require.ErrorIs(t, (&supervisorProof{
		started: filepath.Join(root, "never-started"), completion: filepath.Join(root, "never-completed"),
		startupWait: 20 * time.Millisecond,
	}).awaitCompletion(), ErrProcessContainmentIncomplete)

	started := filepath.Join(root, "started")
	completed := filepath.Join(root, "completed")
	require.NoError(t, writeSupervisorMarker(started))
	go func() {
		time.Sleep(20 * time.Millisecond)
		_ = writeSupervisorMarker(completed)
	}()
	require.NoError(t, (&supervisorProof{started: started, completion: completed}).awaitCompletion())
}

func TestSupervisorMarkerPIDReadyAndCopyUtilities(t *testing.T) {
	root := t.TempDir()
	require.Error(t, writeSupervisorMarker(""))
	marker := filepath.Join(root, "marker")
	require.NoError(t, writeSupervisorMarker(marker))
	require.NoError(t, writeSupervisorMarker(marker))
	require.Error(t, writeSupervisorMarker(filepath.Join(marker, "child")))

	require.Error(t, writeNativePID("", 1))
	require.Error(t, writeNativePID(filepath.Join(root, "pid"), 0))
	pidPath := filepath.Join(root, "pid")
	require.NoError(t, writeNativePID(pidPath, 123))
	pid, err := readNativePID(pidPath)
	require.NoError(t, err)
	require.Equal(t, 123, pid)
	_, err = readNativePID(filepath.Join(root, "missing"))
	require.Error(t, err)
	require.NoError(t, os.WriteFile(pidPath, []byte("bad"), 0o600))
	_, err = readNativePID(pidPath)
	require.ErrorContains(t, err, "invalid")

	_, err = parseSupervisorReady("bad")
	require.ErrorContains(t, err, "invalid readiness")
	_, err = parseSupervisorReady(supervisorReadyPrefix + "{")
	require.ErrorContains(t, err, "decode readiness")
	_, err = parseSupervisorReady(supervisorReadyPrefix + `{"nativePid":0}`)
	require.ErrorContains(t, err, "omitted")
	ready, err := parseSupervisorReady(supervisorReadyPrefix + `{"nativePid":123}`)
	require.NoError(t, err)
	require.Equal(t, 123, ready.NativePID)

	buffer := new(supervisorCloseBuffer)
	done := make(chan struct{}, 1)
	copySupervisorStream(buffer, strings.NewReader("payload"), done)
	<-done
	require.Equal(t, "payload", buffer.String())
	require.True(t, buffer.closed)

	tries := 0
	require.NoError(t, awaitQuiescence(func() error {
		tries++

		return nil
	}))
	require.Equal(t, 1, tries)
}

func TestRunLivenessControlEOFAndNativeFailure(t *testing.T) {
	preserveSupervisorGlobals(t)
	root := t.TempDir()
	supervisorInput = strings.NewReader("hello\n")
	supervisorOutput = io.Discard
	supervisorError = io.Discard
	config := supervisorConfig{
		NativePath: "/bin/sh", NativeArgs: []string{"-c", "cat"}, NativeEnv: os.Environ(),
		Home: filepath.Join(root, "home"), Scratch: root,
		Started: filepath.Join(root, "started"), Completion: filepath.Join(root, "complete"),
		NativePIDFile: filepath.Join(root, "native.pid"),
	}
	require.NoError(t, runLiveness(config))
	_, err := os.Stat(config.Completion)
	require.NoError(t, err)

	root = t.TempDir()
	supervisorInput = strings.NewReader("")
	config.Home = filepath.Join(root, "home")
	config.Scratch = root
	config.Started = filepath.Join(root, "started")
	config.Completion = filepath.Join(root, "complete")
	config.NativePIDFile = filepath.Join(root, "native.pid")
	config.NativePath = filepath.Join(root, "missing")
	err = runLiveness(config)
	require.ErrorContains(t, err, "start contained native root")
}

func TestRunLivenessPublishAndPIDFailures(t *testing.T) {
	preserveSupervisorGlobals(t)
	root := t.TempDir()
	supervisorInput = strings.NewReader("")
	supervisorOutput = io.Discard
	supervisorError = supervisorErrorWriter{err: errors.New("write failed")}
	config := supervisorConfig{
		NativePath: "/bin/sh", NativeArgs: []string{"-c", "while :; do sleep 1; done"}, NativeEnv: os.Environ(),
		Home: filepath.Join(root, "home"), Scratch: root,
		Started: filepath.Join(root, "started"), Completion: filepath.Join(root, "complete"),
		NativePIDFile: filepath.Join(root, "native.pid"),
	}
	err := runLiveness(config)
	require.ErrorContains(t, err, "publish supervisor readiness")

	root = t.TempDir()
	supervisorError = io.Discard
	config.Home = filepath.Join(root, "home")
	config.Started = filepath.Join(root, "started")
	config.Completion = filepath.Join(root, "complete")
	config.NativePIDFile = filepath.Join(root, "missing", "native.pid")
	err = runLiveness(config)
	require.ErrorContains(t, err, "write private native PID proof")
}

func TestRunGuardianHappyPathAndPreReadinessFailure(t *testing.T) {
	preserveSupervisorGlobals(t)
	withNeutralSupervisorIdentityHooks(t)
	root := t.TempDir()
	supervisorInput = strings.NewReader("payload\n")
	supervisorOutput = io.Discard
	supervisorError = io.Discard
	config := withTestSupervisorIdentity(supervisorConfig{
		NativePath: "/bin/sh", NativeArgs: []string{"-c", "cat"}, NativeEnv: os.Environ(),
		Home: filepath.Join(root, "home"), Scratch: root,
		Started: filepath.Join(root, "started"), Completion: filepath.Join(root, "complete"),
		NativePIDFile: filepath.Join(root, "native.pid"),
	})
	require.NoError(t, runGuardian(config))

	root = t.TempDir()
	supervisorExecutable = func() (string, error) { return "/bin/false", nil }
	config.Home = filepath.Join(root, "home")
	config.Scratch = root
	config.Started = filepath.Join(root, "started")
	config.Completion = filepath.Join(root, "complete")
	config.NativePIDFile = filepath.Join(root, "native.pid")
	err := runGuardian(config)
	require.ErrorContains(t, err, "failed before readiness")
}

func TestSupervisorBootstrapAndUnixContainmentBranches(t *testing.T) {
	preserveSupervisorGlobals(t)
	supervisorExit = func(int) {}
	supervisorError = io.Discard
	t.Setenv(supervisorModeEnv, "")
	supervisorBootstrap()

	guardian, err := newGuardianContainment(supervisorConfig{})
	require.NoError(t, err)
	require.Equal(t, "linux-subreaper", guardian.Name())
	require.NoError(t, guardian.Close())
	liveness, err := openLivenessContainment(supervisorConfig{})
	require.NoError(t, err)
	require.NoError(t, liveness.Close())

	cmd := exec.Command("/bin/true")
	configureIndependentSupervisor(cmd)
	require.True(t, cmd.SysProcAttr.Setpgid)

	oldKill := killProcessID
	t.Cleanup(func() { killProcessID = oldKill })
	killProcessID = func(int, syscall.Signal) error { return syscall.ESRCH }
	require.NoError(t, signalProcessGroup(123, syscall.SIGTERM))

	killProcessID = func(int, syscall.Signal) error { return errors.New("probe failed") }
	require.Error(t, signalProcessGroup(123, syscall.SIGTERM))
}

func TestSupervisorEntropyAndProofStatFailures(t *testing.T) {
	preserveSupervisorGlobals(t)
	supervisorRandRead = func([]byte) (int, error) { return 0, errors.New("entropy failed") }
	_, err := supervisorNonce()
	require.ErrorContains(t, err, "marker nonce")

	root := t.TempDir()
	notDirectory := filepath.Join(root, "file")
	require.NoError(t, os.WriteFile(notDirectory, []byte("x"), 0o600))
	err = (&supervisorProof{completion: filepath.Join(notDirectory, "child")}).awaitCompletion()
	require.ErrorContains(t, err, "stat liveness completion")
	require.ErrorIs(t, err, ErrProcessContainmentIncomplete)
	err = (&supervisorProof{started: filepath.Join(notDirectory, "child"), completion: filepath.Join(root, "missing")}).awaitCompletion()
	require.ErrorContains(t, err, "stat liveness start")
	require.ErrorIs(t, err, ErrProcessContainmentIncomplete)
}

func TestRunGuardianPipeAndStartFailures(t *testing.T) {
	tests := map[string]func(*exec.Cmd){
		"stdin":  func(command *exec.Cmd) { command.Stdin = strings.NewReader("") },
		"stderr": func(command *exec.Cmd) { command.Stderr = io.Discard },
	}
	for name, configure := range tests {
		t.Run(name, func(t *testing.T) {
			preserveSupervisorGlobals(t)
			root := t.TempDir()
			supervisorExecutable = func() (string, error) { return "/bin/true", nil }
			supervisorExecCommand = func(name string, args ...string) *exec.Cmd {
				command := exec.Command(name, args...)
				configure(command)

				return command
			}
			err := runGuardian(supervisorConfig{Home: filepath.Join(root, "home"), Scratch: root})
			require.ErrorContains(t, err, "open liveness")
		})
	}

	t.Run("start", func(t *testing.T) {
		preserveSupervisorGlobals(t)
		root := t.TempDir()
		supervisorExecutable = func() (string, error) { return "/bin/true", nil }
		supervisorExecCommand = func(string, ...string) *exec.Cmd { return exec.Command(filepath.Join(root, "missing")) }
		err := runGuardian(supervisorConfig{Home: filepath.Join(root, "home"), Scratch: root})
		require.ErrorContains(t, err, "start liveness supervisor")
	})

	t.Run("executable", func(t *testing.T) {
		preserveSupervisorGlobals(t)
		root := t.TempDir()
		supervisorExecutable = func() (string, error) { return "", errors.New("lookup failed") }
		err := runGuardian(supervisorConfig{Home: filepath.Join(root, "home"), Scratch: root})
		require.ErrorContains(t, err, "resolve liveness")
	})
}

func TestRunLivenessPipeAndExitFailures(t *testing.T) {
	tests := map[string]func(*exec.Cmd){
		"stdin":  func(command *exec.Cmd) { command.Stdin = strings.NewReader("") },
		"stderr": func(command *exec.Cmd) { command.Stderr = io.Discard },
	}
	for name, configure := range tests {
		t.Run(name, func(t *testing.T) {
			preserveSupervisorGlobals(t)
			root := t.TempDir()
			supervisorExecCommand = func(path string, args ...string) *exec.Cmd {
				command := exec.Command(path, args...)
				configure(command)

				return command
			}
			err := runLiveness(supervisorConfig{
				NativePath: "/bin/true", NativeEnv: os.Environ(), Home: filepath.Join(root, "home"), Scratch: root,
				Started: filepath.Join(root, "started"), Completion: filepath.Join(root, "complete"), NativePIDFile: filepath.Join(root, "pid"),
			})
			require.ErrorContains(t, err, "open native")
		})
	}

	t.Run("native exit", func(t *testing.T) {
		preserveSupervisorGlobals(t)
		root := t.TempDir()
		input, inputWriter := io.Pipe()
		t.Cleanup(func() { _ = inputWriter.Close() })
		supervisorInput = input
		supervisorOutput = io.Discard
		supervisorError = io.Discard
		err := runLiveness(supervisorConfig{
			NativePath: "/bin/false", NativeEnv: os.Environ(), Home: filepath.Join(root, "home"), Scratch: root,
			Started: filepath.Join(root, "started"), Completion: filepath.Join(root, "complete"), NativePIDFile: filepath.Join(root, "pid"),
		})
		require.ErrorContains(t, err, "native root exited")
	})

	t.Run("native exit without quiescence proof", func(t *testing.T) {
		preserveSupervisorGlobals(t)
		oldTaskEntries := linuxTaskEntries
		t.Cleanup(func() { linuxTaskEntries = oldTaskEntries })
		linuxTaskEntries = func() ([]os.DirEntry, error) {
			return nil, errors.New("task scan failed")
		}

		root := t.TempDir()
		input, inputWriter := io.Pipe()
		t.Cleanup(func() { _ = inputWriter.Close() })
		supervisorInput = input
		supervisorOutput = io.Discard
		supervisorError = io.Discard
		err := runLiveness(supervisorConfig{
			NativePath: "/bin/true", NativeEnv: os.Environ(), Home: filepath.Join(root, "home"), Scratch: root,
			Started: filepath.Join(root, "started"), Completion: filepath.Join(root, "complete"), NativePIDFile: filepath.Join(root, "pid"),
		})
		require.ErrorIs(t, err, ErrProcessContainmentIncomplete)
	})
}

func TestUnixQuiescenceSignalEscalationAndTimeout(t *testing.T) {
	oldKill := killProcessID
	t.Cleanup(func() {
		killProcessID = oldKill
	})

	command := &exec.Cmd{Process: &os.Process{Pid: 123}}
	killProcessID = func(int, syscall.Signal) error { return syscall.ESRCH }
	require.NoError(t, terminateIndependentSupervisor(command))

	liveness, err := openLivenessContainment(supervisorConfig{})
	require.NoError(t, err)
	command = exec.Command("/bin/true")
	require.NoError(t, liveness.Start(command))
	require.NoError(t, <-liveness.Wait())
	require.NoError(t, liveness.Quiesce(0, time.Second))
	guardian, err := newGuardianContainment(supervisorConfig{})
	require.NoError(t, err)
	require.NotNil(t, guardian)
}

func TestSupervisorInjectedFilesystemAndContainmentFailures(t *testing.T) {
	t.Run("encode", func(t *testing.T) {
		preserveSupervisorGlobals(t)
		supervisorEncodeConfig = func(io.Writer, supervisorConfig) error { return errors.New("encode failed") }
		_, err := writeSupervisorConfig(t.TempDir(), supervisorConfig{})
		require.ErrorContains(t, err, "write private supervisor config")
	})

	t.Run("guardian containment", func(t *testing.T) {
		preserveSupervisorGlobals(t)
		supervisorNewGuardianContainment = func(supervisorConfig) (*guardianContainment, error) { return nil, errors.New("containment failed") }
		root := t.TempDir()
		err := runGuardian(supervisorConfig{Home: filepath.Join(root, "home"), Scratch: root})
		require.ErrorContains(t, err, "containment failed")
	})

	t.Run("liveness containment", func(t *testing.T) {
		preserveSupervisorGlobals(t)
		supervisorOpenLivenessContainment = func(supervisorConfig) (*livenessContainment, error) { return nil, errors.New("containment failed") }
		root := t.TempDir()
		err := runLiveness(supervisorConfig{
			Home: filepath.Join(root, "home"), Scratch: root, Started: filepath.Join(root, "started"), Completion: filepath.Join(root, "complete"),
		})
		require.ErrorContains(t, err, "containment failed")
	})
}

func TestSupervisorDispatchBootstrapAndEarlyFailures(t *testing.T) {
	preserveSupervisorGlobals(t)
	withNeutralSupervisorIdentityHooks(t)
	root := t.TempDir()
	config := withTestSupervisorIdentity(supervisorConfig{
		NativePath: "/bin/true", NativeEnv: os.Environ(), Home: filepath.Join(root, "home"), Scratch: root,
		Started: filepath.Join(root, "started"), Completion: filepath.Join(root, "complete"), NativePIDFile: filepath.Join(root, "pid"),
	})
	path, err := writeSupervisorConfig(root, config)
	require.NoError(t, err)
	supervisorInput = strings.NewReader("")
	supervisorOutput = io.Discard
	supervisorError = io.Discard
	require.NoError(t, runSupervisor(supervisorModeLiveness, path))

	supervisorRandRead = func([]byte) (int, error) { return 0, errors.New("entropy failed") }
	_, _, err = supervisorCommand(context.Background(), withTestSupervisorIdentity(supervisorConfig{Scratch: t.TempDir()}))
	require.ErrorContains(t, err, "marker nonce")
	supervisorRandRead = func(value []byte) (int, error) {
		for index := range value {
			value[index] = 1
		}

		return len(value), nil
	}
	requireSupervisorCommandWithoutScratchRoot(t)

	root = t.TempDir()
	claim, err := homelock.AcquireClaim(filepath.Join(root, "home"))
	require.NoError(t, err)
	err = runGuardian(supervisorConfig{Home: filepath.Join(root, "home"), Scratch: root})
	require.Error(t, err)
	require.NoError(t, claim.Release())

	root = t.TempDir()
	liveness, err := homelock.AcquireLiveness(filepath.Join(root, "home"))
	require.NoError(t, err)
	err = runLiveness(supervisorConfig{
		Home: filepath.Join(root, "home"), Scratch: root, Started: filepath.Join(root, "started"), Completion: filepath.Join(root, "complete"),
	})
	require.Error(t, err)
	require.NoError(t, liveness.Release())

	root = t.TempDir()
	notDirectory := filepath.Join(root, "file")
	require.NoError(t, os.WriteFile(notDirectory, []byte("x"), 0o600))
	err = runLiveness(supervisorConfig{Started: filepath.Join(notDirectory, "child"), Completion: filepath.Join(root, "complete")})
	require.Error(t, err)
}

func TestGuardianPreReadinessRecoveryProofBranches(t *testing.T) {
	t.Run("completion stat", func(t *testing.T) {
		preserveSupervisorGlobals(t)
		root := t.TempDir()
		notDirectory := filepath.Join(root, "file")
		require.NoError(t, os.WriteFile(notDirectory, []byte("x"), 0o600))
		supervisorExecutable = func() (string, error) { return "/bin/false", nil }
		err := runGuardian(supervisorConfig{
			Home: filepath.Join(root, "home"), Scratch: root, Completion: filepath.Join(notDirectory, "child"),
		})
		require.Error(t, err)
	})

	t.Run("started stat", func(t *testing.T) {
		preserveSupervisorGlobals(t)
		root := t.TempDir()
		notDirectory := filepath.Join(root, "file")
		require.NoError(t, os.WriteFile(notDirectory, []byte("x"), 0o600))
		supervisorExecutable = func() (string, error) { return "/bin/false", nil }
		err := runGuardian(supervisorConfig{
			Home: filepath.Join(root, "home"), Scratch: root,
			Started: filepath.Join(notDirectory, "child"), Completion: filepath.Join(root, "missing"),
		})
		require.Error(t, err)
	})

	t.Run("started pid proof", func(t *testing.T) {
		preserveSupervisorGlobals(t)
		root := t.TempDir()
		started := filepath.Join(root, "started")
		pid := filepath.Join(root, "pid")
		require.NoError(t, writeSupervisorMarker(started))
		require.NoError(t, writeNativePID(pid, 99999999))
		supervisorExecutable = func() (string, error) { return "/bin/false", nil }
		err := runGuardian(supervisorConfig{
			Home: filepath.Join(root, "home"), Scratch: root, Started: started,
			Completion: filepath.Join(root, "complete"), NativePIDFile: pid,
		})
		require.Error(t, err)
		_, statErr := os.Stat(filepath.Join(root, "complete"))
		require.NoError(t, statErr)
	})

	t.Run("completion appears while pid absent", func(t *testing.T) {
		preserveSupervisorGlobals(t)
		root := t.TempDir()
		started := filepath.Join(root, "started")
		completion := filepath.Join(root, "complete")
		require.NoError(t, writeSupervisorMarker(started))
		supervisorExecutable = func() (string, error) { return "/bin/false", nil }
		wroteCompletion := make(chan error, 1)
		go func() {
			time.Sleep(20 * time.Millisecond)
			wroteCompletion <- writeSupervisorMarker(completion)
		}()
		err := runGuardian(supervisorConfig{
			Home: filepath.Join(root, "home"), Scratch: root, Started: started,
			Completion: completion, NativePIDFile: filepath.Join(root, "missing-pid"),
		})
		require.NoError(t, <-wroteCompletion)
		require.Error(t, err)
	})
}

func TestSupervisorFinalRemainingBranches(t *testing.T) {
	t.Run("guardian dispatch", func(t *testing.T) {
		preserveSupervisorGlobals(t)
		withNeutralSupervisorIdentityHooks(t)
		root := t.TempDir()
		supervisorInput = strings.NewReader("")
		supervisorOutput = io.Discard
		supervisorError = io.Discard
		config := withTestSupervisorIdentity(supervisorConfig{
			NativePath: "/bin/true", NativeEnv: os.Environ(), Home: filepath.Join(root, "home"), Scratch: root,
			Started: filepath.Join(root, "started"), Completion: filepath.Join(root, "complete"), NativePIDFile: filepath.Join(root, "pid"),
			FramedInput: true,
		})
		path, err := writeSupervisorConfig(root, config)
		require.NoError(t, err)
		require.NoError(t, runSupervisor(supervisorModeGuardian, path))
	})

	t.Run("guardian config", func(t *testing.T) {
		preserveSupervisorGlobals(t)
		withNeutralSupervisorIdentityHooks(t)
		err := runGuardian(withTestSupervisorIdentity(supervisorConfig{Home: t.TempDir(), Scratch: ""}))
		require.ErrorContains(t, err, guardianWithoutScratchRootRefusal)
	})

	t.Run("guardian completion publish", func(t *testing.T) {
		preserveSupervisorGlobals(t)
		root := t.TempDir()
		notDirectory := filepath.Join(root, "file")
		require.NoError(t, os.WriteFile(notDirectory, []byte("x"), 0o600))
		supervisorInput = strings.NewReader("")
		supervisorOutput = io.Discard
		supervisorError = io.Discard
		err := runGuardian(supervisorConfig{
			NativePath: "/bin/true", NativeEnv: os.Environ(), Home: filepath.Join(root, "home"), Scratch: root,
			Started: filepath.Join(root, "started"), Completion: filepath.Join(notDirectory, "child"), NativePIDFile: filepath.Join(root, "pid"),
		})
		require.Error(t, err)
	})

	t.Run("guardian reports post-readiness liveness failure", func(t *testing.T) {
		preserveSupervisorGlobals(t)
		root := t.TempDir()
		liveness := filepath.Join(root, "liveness")
		require.NoError(t, os.WriteFile(liveness, []byte("#!/bin/sh\nprintf '%s\\n' '"+supervisorReadyPrefix+`{"nativePid":99999999}`+"' >&2\nexit 7\n"), 0o700))
		supervisorExecutable = func() (string, error) { return liveness, nil }
		supervisorInput = strings.NewReader("")
		supervisorOutput = io.Discard
		supervisorError = io.Discard
		err := runGuardian(supervisorConfig{
			Home: filepath.Join(root, "home"), Scratch: root,
			Started: filepath.Join(root, "started"), Completion: filepath.Join(root, "complete"), NativePIDFile: filepath.Join(root, "pid"),
		})
		require.ErrorContains(t, err, "liveness supervisor exited")
	})

	t.Run("liveness native success", func(t *testing.T) {
		preserveSupervisorGlobals(t)
		root := t.TempDir()
		input, writer := io.Pipe()
		t.Cleanup(func() { _ = writer.Close() })
		supervisorInput = input
		supervisorOutput = io.Discard
		supervisorError = io.Discard
		err := runLiveness(supervisorConfig{
			NativePath: "/bin/true", NativeEnv: os.Environ(), Home: filepath.Join(root, "home"), Scratch: root,
			Started: filepath.Join(root, "started"), Completion: filepath.Join(root, "complete"), NativePIDFile: filepath.Join(root, "pid"),
			FramedInput: true,
		})
		require.NoError(t, err)
	})

	t.Run("proof second stat", func(t *testing.T) {
		root := t.TempDir()
		started := filepath.Join(root, "started")
		completionParent := filepath.Join(root, "parent")
		completion := filepath.Join(completionParent, "child")
		require.NoError(t, writeSupervisorMarker(started))
		go func() {
			time.Sleep(20 * time.Millisecond)
			_ = os.WriteFile(completionParent, []byte("x"), 0o600)
		}()
		err := (&supervisorProof{started: started, completion: completion}).awaitCompletion()
		require.ErrorContains(t, err, "stat liveness completion")
	})
}

func TestSupervisorFramedInputBranches(t *testing.T) {
	t.Run("encode payload and eof", func(t *testing.T) {
		destination := &supervisorCloseBuffer{}
		done := make(chan struct{}, 1)
		copySupervisorFramedInput(destination, strings.NewReader("payload"), done)
		<-done
		require.False(t, destination.closed)

		decoded := &supervisorCloseBuffer{}
		controlDone := make(chan struct{})
		copyFramedSupervisorInput(decoded, bytes.NewReader(destination.Bytes()), controlDone)
		<-controlDone
		require.True(t, decoded.closed)
		require.Equal(t, "payload", decoded.String())
	})

	t.Run("encode read failure", func(t *testing.T) {
		destination := &supervisorCloseBuffer{}
		done := make(chan struct{}, 1)
		copySupervisorFramedInput(destination, supervisorErrorReader{err: errors.New("read")}, done)
		<-done
		require.True(t, destination.closed)
	})

	for _, failAt := range []int{1, 2} {
		t.Run("encode write failure", func(t *testing.T) {
			destination := &supervisorWriteFailure{failAt: failAt}
			done := make(chan struct{}, 1)
			copySupervisorFramedInput(destination, strings.NewReader("payload"), done)
			<-done
			require.True(t, destination.closed)
		})
	}

	t.Run("decode missing header", func(t *testing.T) {
		destination := &supervisorCloseBuffer{}
		done := make(chan struct{})
		copyFramedSupervisorInput(destination, strings.NewReader(""), done)
		<-done
		require.True(t, destination.closed)
	})

	t.Run("decode oversized frame", func(t *testing.T) {
		var frame [4]byte
		binary.BigEndian.PutUint32(frame[:], supervisorInputFrameLimit+1)
		destination := &supervisorCloseBuffer{}
		done := make(chan struct{})
		copyFramedSupervisorInput(destination, bytes.NewReader(frame[:]), done)
		<-done
		require.True(t, destination.closed)
	})

	t.Run("decode truncated payload", func(t *testing.T) {
		var frame [4]byte
		binary.BigEndian.PutUint32(frame[:], 2)
		destination := &supervisorCloseBuffer{}
		done := make(chan struct{})
		copyFramedSupervisorInput(destination, bytes.NewReader(append(frame[:], 'x')), done)
		<-done
		require.True(t, destination.closed)
	})
}

func TestUnixSignalProcessGroupBranches(t *testing.T) {
	oldKill := killProcessID
	t.Cleanup(func() { killProcessID = oldKill })

	killProcessID = func(int, syscall.Signal) error { return syscall.ESRCH }
	require.NoError(t, signalProcessGroup(123, syscall.SIGTERM))
	killProcessID = func(int, syscall.Signal) error { return errors.New("signal failed") }
	require.ErrorContains(t, signalProcessGroup(123, syscall.SIGKILL), "signal failed")
}
