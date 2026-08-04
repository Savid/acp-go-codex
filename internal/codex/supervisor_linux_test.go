//go:build linux

package codex

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/savid/acp-go-codex/internal/homelock"
	"github.com/stretchr/testify/require"
	"golang.org/x/sys/unix"
)

type supervisedNative struct {
	cmd         *exec.Cmd
	waiter      *supervisorWaiter
	stdin       io.WriteCloser
	home        string
	rootPID     int
	descPID     int
	livenessPID int
	stderr      *bytes.Buffer
	cancel      context.CancelFunc
	proof       *supervisorProof
	attackPath  string
	forgePath   string
}

type linuxSupervisorDirEntry struct {
	name string
	dir  bool
}

func (entry linuxSupervisorDirEntry) Name() string      { return entry.name }
func (entry linuxSupervisorDirEntry) IsDir() bool       { return entry.dir }
func (entry linuxSupervisorDirEntry) Type() os.FileMode { return 0 }
func (entry linuxSupervisorDirEntry) Info() (os.FileInfo, error) {
	return nil, errors.New("test directory entry has no file info")
}

func preserveLinuxSupervisorGlobals(t *testing.T) {
	t.Helper()
	oldSubreaper := linuxSetSubreaper
	oldNoNewPrivileges := linuxSetNoNewPrivileges
	oldCoreLimit := linuxSetCoreLimit
	oldTasks := linuxTaskEntries
	oldChildren := linuxTaskChildren
	oldWait4 := linuxWait4
	oldWaitid := linuxWaitid
	oldKill := killProcessID
	t.Cleanup(func() {
		linuxSetSubreaper = oldSubreaper
		linuxSetNoNewPrivileges = oldNoNewPrivileges
		linuxSetCoreLimit = oldCoreLimit
		linuxTaskEntries = oldTasks
		linuxTaskChildren = oldChildren
		linuxWait4 = oldWait4
		linuxWaitid = oldWaitid
		killProcessID = oldKill
	})
}

const linuxSecurityLimitsProofEnv = "ACP_GO_CODEX_TEST_SECURITY_LIMITS_PROOF"

func TestLinuxSupervisorChildInheritsSecurityLimits(t *testing.T) {
	proofPath := os.Getenv(linuxSecurityLimitsProofEnv)
	if proofPath != "" {
		native := exec.Command("/bin/sh", "-c", `nnp=$(awk '$1 == "NoNewPrivs:" { print $2 }' /proc/self/status); printf '%s %s\n' "$nnp" "$(ulimit -c)" > "$1"`, "sh", proofPath)
		require.NoError(t, startLinuxSecurityLimited(native.Start))
		require.NoError(t, native.Wait())

		return
	}

	proofPath = filepath.Join(t.TempDir(), "security-limits")
	helper := exec.Command(os.Args[0], "-test.run=^TestLinuxSupervisorChildInheritsSecurityLimits$")
	helper.Env = append(os.Environ(), linuxSecurityLimitsProofEnv+"="+proofPath)
	output, err := helper.CombinedOutput()
	require.NoErrorf(t, err, "run security-limits proof helper: %s", output)

	proof, err := os.ReadFile(proofPath)
	require.NoError(t, err)
	require.Equal(t, "1 0", strings.TrimSpace(string(proof)))
}

func TestLinuxSupervisorLaunchesFailClosedWhenSecurityLimitsCannotBeSet(t *testing.T) {
	preserveLinuxSupervisorGlobals(t)
	linuxSetCoreLimit = func() error { return errors.New("setrlimit failed") }

	require.ErrorContains(t, startIndependentSupervisor(exec.Command("/bin/true")), "disable core dumps")
	require.ErrorContains(t, (&livenessContainment{}).Start(exec.Command("/bin/true")), "disable core dumps")

	linuxSetCoreLimit = func() error { return nil }
	linuxSetNoNewPrivileges = func() error { return errors.New("prctl failed") }

	require.ErrorContains(t, startIndependentSupervisor(exec.Command("/bin/true")), "disable privilege elevation")
	require.ErrorContains(t, (&livenessContainment{}).Start(exec.Command("/bin/true")), "disable privilege elevation")
}

func TestLinuxLivenessContainmentUsesCreatorThreadWait(t *testing.T) {
	preserveLinuxSupervisorGlobals(t)

	command := exec.Command("/bin/sh", "-c", "exit 0")
	containment := &livenessContainment{}
	require.NoError(t, containment.Start(command))
	require.NoError(t, <-containment.Wait())
	require.NotNil(t, command.ProcessState)
}

func TestSupervisorControlEOFKillsTreeBeforeUnlock(t *testing.T) {
	runtime := startSupervisedNative(t)
	_, err := homelock.Acquire(runtime.home)
	require.Error(t, err, "second claimant must fail while the tree is live")

	require.NoError(t, runtime.stdin.Close())
	require.NoError(t, waitSupervisor(runtime, 10*time.Second))
	assertProcessGone(t, runtime.rootPID)
	assertProcessGone(t, runtime.descPID)
	assertHomeReacquires(t, runtime.home)
}

func TestSupervisorNativeSuccessKillsDetachedTreeBeforeUnlock(t *testing.T) {
	runtime := startSupervisedNative(t)
	_, err := io.WriteString(runtime.stdin, "exit\n")
	require.NoError(t, err)
	require.NoError(t, waitSupervisor(runtime, 10*time.Second))
	assertProcessGone(t, runtime.rootPID)
	assertProcessGone(t, runtime.descPID)
	assertHomeReacquires(t, runtime.home)
}

func TestTrustedSupervisorDeniesNativeAuthorityAttacks(t *testing.T) {
	runtime := startSupervisedNative(t)
	raw, err := os.ReadFile(runtime.attackPath)
	require.NoError(t, err)
	require.Equal(t, []string{"denied", "denied", "denied", "denied", "0", "65534", "65534", "0"}, strings.Fields(string(raw)))
	require.NoFileExists(t, runtime.forgePath)

	require.NoError(t, runtime.stdin.Close())
	require.NoError(t, waitSupervisor(runtime, 10*time.Second))
	assertProcessGone(t, runtime.rootPID)
	assertProcessGone(t, runtime.descPID)
}

func TestLinuxAgentIdentityLockSerializesAndCancels(t *testing.T) {
	if os.Geteuid() != 0 {
		t.Skip("requires the trusted root supervisor identity")
	}

	uid := uint32(62000 + os.Getpid()%1000)
	first, err := acquireLinuxAgentIdentityLock(uid, strings.NewReader(""))
	require.NoError(t, err)
	defer first.Close()

	controlRead, controlWrite, err := os.Pipe()
	require.NoError(t, err)
	defer controlRead.Close()
	result := make(chan error, 1)
	go func() {
		lock, lockErr := acquireLinuxAgentIdentityLock(uid, controlRead)
		if lock != nil {
			_ = lock.Close()
		}
		result <- lockErr
	}()

	select {
	case err := <-result:
		t.Fatalf("contending identity lock completed early: %v", err)
	case <-time.After(75 * time.Millisecond):
	}
	require.NoError(t, controlWrite.Close())
	select {
	case err := <-result:
		require.ErrorIs(t, err, ErrProcessContainmentIncomplete)
	case <-time.After(time.Second):
		t.Fatal("contending identity lock ignored closed control")
	}
}

func TestLinuxSupervisorConfigIsSealed(t *testing.T) {
	file, err := writeLinuxSupervisorConfig("", supervisorConfig{NativePath: "/bin/true"})
	require.NoError(t, err)
	defer file.Close()

	seals, err := unix.FcntlInt(file.Fd(), unix.F_GET_SEALS, 0)
	require.NoError(t, err)
	require.Equal(t, unix.F_SEAL_WRITE|unix.F_SEAL_GROW|unix.F_SEAL_SHRINK|unix.F_SEAL_SEAL, seals)
	_, err = file.WriteAt([]byte("x"), 0)
	require.Error(t, err)
}

func TestLinuxAgentIdentityLockRejectsWrongModeWithoutRepair(t *testing.T) {
	trusted := unix.Stat_t{Uid: 0, Gid: 0, Mode: unix.S_IFREG | 0o600, Nlink: 1}
	require.NoError(t, validateLinuxAgentIdentityLock(trusted))

	wrongMode := trusted
	wrongMode.Mode = unix.S_IFREG | 0o640
	require.ErrorContains(t, validateLinuxAgentIdentityLock(wrongMode), "mode-0600")
	require.Equal(t, uint32(unix.S_IFREG|0o640), wrongMode.Mode)
}

func TestPersistentProofFailureRetainsIdentityLockUntilRecovery(t *testing.T) {
	preserveSupervisorGlobals(t)
	supervisorInput = strings.NewReader("")
	supervisorOutput = io.Discard
	supervisorError = io.Discard

	lockPath := filepath.Join(t.TempDir(), "identity.lock")
	guardianFile, err := os.OpenFile(lockPath, os.O_RDWR|os.O_CREATE, 0o600)
	require.NoError(t, err)
	require.NoError(t, unix.Flock(int(guardianFile.Fd()), unix.LOCK_EX|unix.LOCK_NB))

	survivorFD, err := unix.Dup(int(guardianFile.Fd()))
	require.NoError(t, err)
	survivorFile := os.NewFile(uintptr(survivorFD), "surviving-identity-lock")
	require.NotNil(t, survivorFile)
	require.NoError(t, (&linuxAgentIdentityLock{file: guardianFile}).Close())

	contender, err := os.OpenFile(lockPath, os.O_RDWR, 0)
	require.NoError(t, err)
	defer contender.Close()
	require.ErrorIs(t, unix.Flock(int(contender.Fd()), unix.LOCK_EX|unix.LOCK_NB), unix.EWOULDBLOCK)

	recoverProof := make(chan struct{})
	var attempts atomic.Int32
	supervisorLivenessQuiesce = func(*livenessContainment, int, time.Duration) error {
		attempts.Add(1)
		select {
		case <-recoverProof:
			return nil
		default:
			return ErrProcessContainmentIncomplete
		}
	}
	supervisorQuarantineRetry = retryLinuxLivenessContainment

	root := t.TempDir()
	config := supervisorConfig{
		Started: filepath.Join(root, "started"), Completion: filepath.Join(root, "complete"),
		Quarantine: filepath.Join(root, "quarantine"), NativePIDFile: filepath.Join(root, "pid"),
		ProviderSnapshot: filepath.Join(root, "snapshot"),
	}
	require.NoError(t, writeSupervisorMarker(config.Started))

	livenessDone := make(chan error)
	go func() {
		livenessDone <- completeOrQuarantineLiveness(config, &livenessContainment{}, ErrProcessContainmentIncomplete)
		_ = survivorFile.Close()
	}()
	guardianDone := make(chan error, 1)
	go func() { guardianDone <- finishQuarantinedLiveness(livenessDone, config) }()
	require.Eventually(t, func() bool {
		_, err := os.Stat(config.Quarantine)

		return err == nil
	}, time.Second, 10*time.Millisecond)

	startedAt := time.Now()
	proofErr := (&supervisorProof{
		started: config.Started, completion: config.Completion, quarantine: config.Quarantine,
		nativePIDFile: config.NativePIDFile, providerSnapshot: config.ProviderSnapshot,
	}).awaitCompletion()
	require.ErrorIs(t, proofErr, ErrProcessContainmentIncomplete)
	require.Less(t, time.Since(startedAt), time.Second)
	require.FileExists(t, config.Quarantine)
	require.FileExists(t, config.Started)
	require.ErrorIs(t, unix.Flock(int(contender.Fd()), unix.LOCK_EX|unix.LOCK_NB), unix.EWOULDBLOCK)
	require.GreaterOrEqual(t, attempts.Load(), int32(1))

	close(recoverProof)
	require.ErrorIs(t, <-guardianDone, ErrProcessContainmentIncomplete)
	require.NoFileExists(t, config.Quarantine)
	require.NoFileExists(t, config.Started)
	require.Eventually(t, func() bool {
		return unix.Flock(int(contender.Fd()), unix.LOCK_EX|unix.LOCK_NB) == nil
	}, time.Second, 10*time.Millisecond)
}

func TestGuardianPersistentProofFailureQuarantinesUntilRecovery(t *testing.T) {
	preserveSupervisorGlobals(t)
	supervisorInput = strings.NewReader("")
	supervisorOutput = io.Discard
	supervisorError = io.Discard

	recoverProof := make(chan struct{})
	supervisorGuardianQuiesce = func(*guardianContainment, int, time.Duration) error {
		select {
		case <-recoverProof:
			return nil
		default:
			return ErrProcessContainmentIncomplete
		}
	}
	supervisorGuardianQuarantineRetry = retryLinuxGuardianContainment

	root := t.TempDir()
	config := supervisorConfig{
		Started: filepath.Join(root, "started"), Completion: filepath.Join(root, "complete"),
		Quarantine: filepath.Join(root, "quarantine"), NativePIDFile: filepath.Join(root, "pid"),
	}
	done := make(chan error, 1)
	go func() {
		done <- completeOrQuarantineGuardian(config, &guardianContainment{}, ErrProcessContainmentIncomplete)
	}()
	require.Eventually(t, func() bool {
		_, err := os.Stat(config.Quarantine)

		return err == nil
	}, time.Second, 10*time.Millisecond)
	select {
	case err := <-done:
		t.Fatalf("guardian quarantine returned before kernel recovery: %v", err)
	default:
	}

	close(recoverProof)
	require.ErrorIs(t, <-done, ErrProcessContainmentIncomplete)
}

func TestSupervisorGuardianSIGKILLLeavesLivenessLockedUntilTreeExit(t *testing.T) {
	runtime := startSupervisedNative(t)
	require.NoError(t, syscall.Kill(-runtime.rootPID, syscall.SIGSTOP))
	require.NoError(t, runtime.cmd.Process.Kill())

	var claim *homelock.Lock
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		claim, _ = homelock.AcquireClaim(runtime.home)
		if claim != nil {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	require.NotNil(t, claim, "guardian death must release only claim")
	defer func() { require.NoError(t, claim.Release()) }()
	_, err := homelock.AcquireLiveness(runtime.home)
	require.Error(t, err, "surviving liveness supervisor must retain its lock")
	runtime.waiter.start()
	<-runtime.waiter.result()

	liveness := acquireLivenessEventually(t, runtime.home)
	require.NoError(t, liveness.Release())
	assertProcessGone(t, runtime.rootPID)
	assertProcessGone(t, runtime.descPID)
}

func TestSupervisorLivenessSIGKILLLeavesClaimLockedUntilTreeExit(t *testing.T) {
	runtime := startSupervisedNative(t)
	require.NoError(t, syscall.Kill(-runtime.rootPID, syscall.SIGSTOP))
	require.NoError(t, syscall.Kill(runtime.livenessPID, syscall.SIGKILL))

	_, err := homelock.AcquireClaim(runtime.home)
	require.Error(t, err, "surviving guardian must retain claim while it kills the tree")
	waitErr := waitSupervisor(runtime, 10*time.Second)
	require.Error(t, waitErr, "guardian must report its killed liveness child")
	require.NotErrorIs(t, waitErr, ErrProcessContainmentIncomplete)

	assertProcessGone(t, runtime.rootPID)
	assertProcessGone(t, runtime.descPID)
	assertHomeReacquires(t, runtime.home)
}

func TestLinuxSubreaperProofFailureIsBoundedAndTyped(t *testing.T) {
	preserveLinuxSupervisorGlobals(t)

	killProcessID = func(int, syscall.Signal) error { return nil }
	linuxTaskEntries = func() ([]os.DirEntry, error) { return nil, nil }
	linuxWaitid = func(int, int, *unix.Siginfo, int, *unix.Rusage) error { return nil }

	startedAt := time.Now()
	err := quiesceSubreaper(123, 25*time.Millisecond, false)
	require.ErrorIs(t, err, ErrProcessContainmentIncomplete)
	require.Less(t, time.Since(startedAt), time.Second)

	root := t.TempDir()
	started := filepath.Join(root, "started")
	require.NoError(t, writeSupervisorMarker(started))
	startedAt = time.Now()
	err = (&supervisorProof{
		started:        started,
		completion:     filepath.Join(root, "missing-completion"),
		completionWait: 25 * time.Millisecond,
	}).awaitCompletion()
	require.ErrorIs(t, err, ErrProcessContainmentIncomplete)
	require.Less(t, time.Since(startedAt), time.Second)
}

func TestLinuxSubreaperPrimitiveFailureBranches(t *testing.T) {
	t.Run("subreaper setup", func(t *testing.T) {
		preserveLinuxSupervisorGlobals(t)
		linuxSetSubreaper = func() error { return errors.New("prctl failed") }

		_, err := newGuardianContainment(supervisorConfig{})
		require.ErrorContains(t, err, "guardian child subreaper")
		_, err = openLivenessContainment(supervisorConfig{})
		require.ErrorContains(t, err, "liveness child subreaper")
	})

	t.Run("direct child snapshots", func(t *testing.T) {
		preserveLinuxSupervisorGlobals(t)
		linuxTaskEntries = func() ([]os.DirEntry, error) { return nil, errors.New("tasks failed") }

		count, available := (&livenessContainment{}).DescendantCount()
		require.Zero(t, count)
		require.False(t, available)
		_, err := linuxDirectChildren()
		require.ErrorContains(t, err, "list subreaper tasks")

		linuxTaskEntries = func() ([]os.DirEntry, error) {
			return []os.DirEntry{
				linuxSupervisorDirEntry{name: "file"},
				linuxSupervisorDirEntry{name: "gone", dir: true},
				linuxSupervisorDirEntry{name: "live", dir: true},
			}, nil
		}
		linuxTaskChildren = func(path string) ([]byte, error) {
			if strings.Contains(path, "gone") {
				return nil, os.ErrNotExist
			}

			return []byte("123 123 456"), nil
		}

		children, err := linuxDirectChildren()
		require.NoError(t, err)
		require.ElementsMatch(t, []int{123, 456}, children)
		count, available = (&livenessContainment{}).DescendantCount()
		require.Equal(t, 2, count)
		require.True(t, available)

		linuxTaskChildren = func(string) ([]byte, error) { return nil, errors.New("read failed") }
		_, err = linuxDirectChildren()
		require.ErrorContains(t, err, "read subreaper task children")

		linuxTaskChildren = func(string) ([]byte, error) { return []byte("invalid"), nil }
		_, err = linuxDirectChildren()
		require.ErrorContains(t, err, "parse subreaper child PID")
	})

	t.Run("adopted child kill and reap", func(t *testing.T) {
		preserveLinuxSupervisorGlobals(t)
		linuxTaskEntries = func() ([]os.DirEntry, error) {
			return []os.DirEntry{linuxSupervisorDirEntry{name: "live", dir: true}}, nil
		}
		linuxTaskChildren = func(string) ([]byte, error) { return []byte("123 456"), nil }
		killProcessID = func(int, syscall.Signal) error { return syscall.ESRCH }
		linuxWait4 = func(int, *unix.WaitStatus, int, *unix.Rusage) (int, error) { return 0, syscall.ECHILD }
		require.NoError(t, killAndReapAdoptedChildren(123, true))

		killProcessID = func(int, syscall.Signal) error { return errors.New("kill failed") }
		require.ErrorContains(t, killAndReapAdoptedChildren(0, false), "kill adopted descendant")

		killProcessID = func(int, syscall.Signal) error { return nil }
		linuxWait4 = func(int, *unix.WaitStatus, int, *unix.Rusage) (int, error) {
			return 0, errors.New("wait failed")
		}
		require.ErrorContains(t, killAndReapAdoptedChildren(0, false), "reap adopted descendant")

		linuxWait4 = func(int, *unix.WaitStatus, int, *unix.Rusage) (int, error) { return 999, nil }
		require.ErrorContains(t, killAndReapAdoptedChildren(0, false), "returned pid")

		linuxTaskEntries = func() ([]os.DirEntry, error) { return nil, errors.New("snapshot failed") }
		require.ErrorContains(t, killAndReapAdoptedChildren(0, false), "snapshot failed")
	})

	t.Run("kernel proof and quiescence", func(t *testing.T) {
		preserveLinuxSupervisorGlobals(t)
		linuxTaskEntries = func() ([]os.DirEntry, error) { return nil, nil }
		killProcessID = func(int, syscall.Signal) error { return nil }

		linuxWaitid = func(int, int, *unix.Siginfo, int, *unix.Rusage) error { return syscall.ECHILD }
		empty, err := subreaperHasNoChildren()
		require.NoError(t, err)
		require.True(t, empty)
		require.NoError(t, quiesceSubreaper(0, time.Second, false))

		linuxWaitid = func(int, int, *unix.Siginfo, int, *unix.Rusage) error { return errors.New("waitid failed") }
		empty, err = subreaperHasNoChildren()
		require.ErrorContains(t, err, "waitid failed")
		require.False(t, empty)
		require.ErrorContains(t, quiesceSubreaper(123, time.Second, false), "prove subreaper")

		linuxTaskEntries = func() ([]os.DirEntry, error) { return nil, errors.New("children failed") }
		require.ErrorContains(t, quiesceSubreaper(123, time.Second, false), "drain adopted")
		require.ErrorIs(t, quiesceSubreaper(123, 0, false), ErrProcessContainmentIncomplete)

		linuxTaskEntries = func() ([]os.DirEntry, error) { return nil, nil }
		linuxWaitid = func(int, int, *unix.Siginfo, int, *unix.Rusage) error { return nil }
		var signals []syscall.Signal
		killProcessID = func(_ int, signal syscall.Signal) error {
			signals = append(signals, signal)

			return nil
		}
		err = quiesceSubreaper(123, 510*time.Millisecond, true)
		require.ErrorIs(t, err, ErrProcessContainmentIncomplete)
		require.Contains(t, signals, syscall.SIGTERM)
		require.Contains(t, signals, syscall.SIGKILL)

		signals = nil
		err = quiesceSubreaper(123, 25*time.Millisecond, true)
		require.ErrorIs(t, err, ErrProcessContainmentIncomplete)
		require.Contains(t, signals, syscall.SIGKILL)
	})
}

func TestSupervisorPropagatesKernelProofFailure(t *testing.T) {
	t.Run("liveness", func(t *testing.T) {
		preserveSupervisorGlobals(t)
		preserveLinuxSupervisorGlobals(t)
		root := t.TempDir()
		supervisorInput = strings.NewReader("")
		supervisorOutput = io.Discard
		supervisorError = io.Discard
		linuxWaitid = func(int, int, *unix.Siginfo, int, *unix.Rusage) error {
			return errors.New("kernel proof failed")
		}

		config := supervisorConfig{
			NativePath: "/bin/true", NativeEnv: os.Environ(), Home: filepath.Join(root, "home"), Scratch: root,
			Started: filepath.Join(root, "started"), Completion: filepath.Join(root, "complete"), NativePIDFile: filepath.Join(root, "pid"),
		}
		err := runLiveness(config)
		require.ErrorIs(t, err, ErrProcessContainmentIncomplete)
		require.NoFileExists(t, config.Completion)
	})

	t.Run("guardian", func(t *testing.T) {
		preserveSupervisorGlobals(t)
		preserveLinuxSupervisorGlobals(t)
		root := t.TempDir()
		liveness := filepath.Join(root, "liveness")
		require.NoError(t, os.WriteFile(liveness, []byte("#!/bin/sh\nprintf '%s\\n' '"+supervisorReadyPrefix+`{"nativePid":99999999}`+"' >&2\nexit 0\n"), 0o700))
		supervisorExecutable = func() (string, error) { return liveness, nil }
		supervisorInput = strings.NewReader("")
		supervisorOutput = io.Discard
		supervisorError = io.Discard
		linuxWaitid = func(int, int, *unix.Siginfo, int, *unix.Rusage) error {
			return errors.New("kernel proof failed")
		}

		config := supervisorConfig{
			Home: filepath.Join(root, "home"), Scratch: root, Completion: filepath.Join(root, "complete"),
		}
		err := runGuardian(config)
		require.ErrorIs(t, err, ErrProcessContainmentIncomplete)
		require.NoFileExists(t, config.Completion)
	})
}

func startSupervisedNative(t *testing.T) *supervisedNative {
	t.Helper()
	root, err := os.MkdirTemp("", "acp-go-codex-authority-")
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, os.RemoveAll(root)) })
	require.NoError(t, os.Chmod(root, 0o777))
	home := filepath.Join(root, "home")
	scratch := filepath.Join(root, "scratch")
	require.NoError(t, os.MkdirAll(scratch, 0o700))
	rootPIDPath := filepath.Join(root, "root.pid")
	descPIDPath := filepath.Join(root, "desc.pid")
	attackPath := filepath.Join(root, "attacks")
	forgePath := filepath.Join(linuxSupervisorProofNamespace, fmt.Sprintf("native-forge-%d", os.Getpid()))
	_ = os.Remove(forgePath)
	script := filepath.Join(root, "native.sh")
	require.NoError(t, os.WriteFile(script, []byte(`#!/bin/sh
if [ "$1" = "descendant" ]; then
  trap '' HUP INT TERM
  while :; do sleep 1; done
fi
setsid "$0" descendant &
echo "$!" > "$DESC_PID_FILE"
echo "$$" > "$ROOT_PID_FILE"
parent=$PPID
stop=denied; kill -STOP "$parent" 2>/dev/null && stop=allowed
kill_result=denied; kill -KILL "$parent" 2>/dev/null && kill_result=allowed
forge=denied; : > "$FORGE_PATH" 2>/dev/null && forge=allowed
config=denied; cat /proc/$parent/fd/3 >/dev/null 2>&1 && config=allowed
printf '%s %s %s %s %s %s %s %s\n' "$stop" "$kill_result" "$forge" "$config" "$(awk '$1 == "Uid:" {print $2}' /proc/$parent/status)" "$(id -u)" "$(id -g)" "$(awk '$1 == "Groups:" {print NF-1}' /proc/self/status)" > "$ATTACK_PATH"
trap '' TERM
while IFS= read -r line; do
  [ "$line" = "exit" ] && exit 0
  printf '%s\n' "$line"
done
`), 0o755))

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	nativeEnv := append(os.Environ(),
		"ROOT_PID_FILE="+rootPIDPath,
		"DESC_PID_FILE="+descPIDPath,
		"ATTACK_PATH="+attackPath,
		"FORGE_PATH="+forgePath,
	)
	cmd, proof, err := supervisorCommand(ctx, supervisorConfig{
		NativePath: script,
		NativeArgs: []string{"root"},
		NativeEnv:  nativeEnv,
		Home:       home,
		Scratch:    scratch,
		Isolation:  &ProcessIsolation{UID: 65534, GID: 65534, BaseEnvironment: environmentMap(nativeEnv)},
	})
	require.NoError(t, err)
	require.Nil(t, cmd.Cancel, "trusted supervisor must outlive caller-context cancellation")
	stdin, err := cmd.StdinPipe()
	require.NoError(t, err)
	stderr := new(bytes.Buffer)
	cmd.Stdout = io.Discard
	cmd.Stderr = stderr
	waiter, err := startProcess(cmd)
	require.NoError(t, err)
	require.NoError(t, proof.closeInherited())

	runtime := &supervisedNative{
		cmd:        cmd,
		waiter:     waiter,
		stdin:      stdin,
		home:       home,
		stderr:     stderr,
		cancel:     cancel,
		proof:      proof,
		attackPath: attackPath,
		forgePath:  forgePath,
	}
	t.Cleanup(func() {
		cancel()
		_ = stdin.Close()
		if runtime.rootPID > 0 {
			_ = syscall.Kill(-runtime.rootPID, syscall.SIGKILL)
		}
		// The descendant sets its own session, so the group kill above never
		// reaches it.
		if runtime.descPID > 0 && syscall.Kill(runtime.descPID, 0) == nil {
			_ = syscall.Kill(runtime.descPID, syscall.SIGKILL)
		}
		if cmd.Process != nil {
			_ = cmd.Process.Kill()
		}
	})

	runtime.rootPID = waitPIDFile(t, rootPIDPath)
	runtime.descPID = waitPIDFile(t, descPIDPath)
	runtime.livenessPID = parentPID(t, runtime.rootPID)
	require.Positive(t, runtime.livenessPID)

	return runtime
}

func waitPIDFile(t *testing.T, path string) int {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		raw, err := os.ReadFile(path)
		if err == nil {
			pid, parseErr := strconv.Atoi(strings.TrimSpace(string(raw)))
			if parseErr == nil && pid > 0 {
				return pid
			}
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("PID file %s was not published", path)

	return 0
}

func parentPID(t *testing.T, pid int) int {
	t.Helper()
	raw, err := os.ReadFile(fmt.Sprintf("/proc/%d/stat", pid))
	require.NoError(t, err)
	closeParen := strings.LastIndexByte(string(raw), ')')
	require.Greater(t, closeParen, 0)
	fields := strings.Fields(string(raw[closeParen+1:]))
	require.GreaterOrEqual(t, len(fields), 2)
	parent, err := strconv.Atoi(fields[1])
	require.NoError(t, err)

	return parent
}

func waitSupervisor(runtime *supervisedNative, timeout time.Duration) error {
	runtime.waiter.start()

	select {
	case err := <-runtime.waiter.result():
		runtime.cancel()

		return errors.Join(err, runtime.proof.awaitCompletion())
	case <-time.After(timeout):
		return fmt.Errorf("supervisor did not exit; stderr=%s", runtime.stderr.String())
	}
}

func assertProcessGone(t *testing.T, pid int) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		err := syscall.Kill(pid, 0)
		if errors.Is(err, syscall.ESRCH) {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("process %d survived supervisor containment", pid)
}

func assertHomeReacquires(t *testing.T, home string) {
	t.Helper()
	lock, err := homelock.Acquire(home)
	require.NoError(t, err)
	require.NoError(t, lock.Release())
}

func acquireLivenessEventually(t *testing.T, home string) *homelock.Lock {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		lock, err := homelock.AcquireLiveness(home)
		if err == nil {
			return lock
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("liveness lock was not released after tree quiescence")

	return nil
}
