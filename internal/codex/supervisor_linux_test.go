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
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/savid/acp-go-codex/internal/homelock"
	"github.com/stretchr/testify/require"
	"golang.org/x/sys/unix"
)

type supervisedNative struct {
	cmd          *exec.Cmd
	waiter       *supervisorWaiter
	stdin        io.WriteCloser
	home         string
	rootPID      int
	descPID      int
	livenessPID  int
	stderr       *supervisorStderr
	cancel       context.CancelFunc
	proof        *supervisorProof
	attackPath   string
	forgePath    string
	isolationUID uint32
}

// supervisorStderr collects the guardian's error stream while the test reads it
// for diagnostics. os/exec copies into cmd.Stderr from its own goroutine until
// Wait returns, so every read has to share the writer's lock.
type supervisorStderr struct {
	mutex   sync.Mutex
	content bytes.Buffer
}

func (s *supervisorStderr) Write(payload []byte) (int, error) {
	s.mutex.Lock()
	defer s.mutex.Unlock()

	return s.content.Write(payload)
}

func (s *supervisorStderr) String() string {
	s.mutex.Lock()
	defer s.mutex.Unlock()

	return s.content.String()
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

func TestSupervisedNativeIsolationDropsStandaloneFieldsAfterAuthorityAdoption(t *testing.T) {
	config := supervisorConfig{
		IsolationUID: 123, IsolationGID: 456,
		NativeEnv:           []string{"PATH=/usr/bin:/bin"},
		IdentityLock:        true,
		AuthorityDomain:     true,
		StandaloneOwnerID:   "standalone-owner",
		StandaloneStateRoot: "/var/lib/standalone-owner",
	}

	isolation := supervisedNativeIsolation(config)
	require.True(t, isolation.identityAuthorityAdopted)
	require.Empty(t, isolation.StandaloneOwnerID)
	require.Empty(t, isolation.StandaloneStateRoot)
	require.NoError(t, validateProcessIsolation(isolation))
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
	require.NoError(t, (&agentIdentityLock{file: guardianFile}).Close())

	contender, err := os.OpenFile(lockPath, os.O_RDWR, 0)
	require.NoError(t, err)
	defer contender.Close()
	require.ErrorIs(t, unix.Flock(int(contender.Fd()), unix.LOCK_EX|unix.LOCK_NB), unix.EWOULDBLOCK)

	recoverProof := make(chan struct{})
	var attempts atomic.Int32
	supervisorLivenessQuiesce = func(*livenessContainment, int, time.Duration) error {
		if attempts.Add(1) == 1 {
			return ErrProcessContainmentIncomplete
		}
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
	var guardianAttempts atomic.Int32
	supervisorGuardianQuiesce = func(*guardianContainment, int, time.Duration) error {
		if guardianAttempts.Add(1) == 1 {
			return ErrProcessContainmentIncomplete
		}
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

// TestSupervisorGuardianSIGKILLOrphanHangupLeavesLivenessLockedUntilTreeExit
// runs the real orphan case. Killing the guardian reparents the liveness
// supervisor away, which orphans its process group; because the group holds a
// stopped job the kernel sends the whole group SIGHUP and then SIGCONT. The
// witness inside that group reports the hangup, so the case cannot pass with
// the kernel's signal suppressed. The survivor must live through it, keep every
// lock it holds until the kernel proves the tree gone, and quiesce only because
// the guardian died rather than because a hangup arrived.
func TestSupervisorGuardianSIGKILLOrphanHangupLeavesLivenessLockedUntilTreeExit(t *testing.T) {
	runtime := startSupervisedNative(t)
	guardianPID := runtime.cmd.Process.Pid

	// Neither trusted role reaches for SIG_IGN, which execve would have carried
	// into the native child and left it deaf to a real hangup.
	requireHangupNotIgnored(t, runtime.livenessPID)
	requireHangupNotIgnored(t, guardianPID)
	requireHangupNotIgnored(t, runtime.rootPID)

	// A hangup is not an operator stop request. Quiescence answers only to the
	// guardian peer descriptor and the guardian control EOF.
	require.NoError(t, syscall.Kill(runtime.livenessPID, syscall.SIGHUP))
	require.NoError(t, syscall.Kill(guardianPID, syscall.SIGHUP))
	require.Never(t, func() bool {
		_, err := os.Stat(runtime.proof.completion)

		return err == nil
	}, 500*time.Millisecond, 25*time.Millisecond, "a bare hangup quiesced the supervised tree")
	requireLivenessLockHeld(t, runtime.home)
	require.NoError(t, syscall.Kill(runtime.rootPID, 0), "native root died on a bare hangup")
	require.NoError(t, syscall.Kill(runtime.descPID, 0), "setsid descendant died on a bare hangup")

	witness := startOrphanHangupWitness(t, runtime.livenessPID)

	// The native group stays frozen so quiescence cannot finish before the
	// survivor's locks have been observed, and the witness is the stopped job
	// the kernel requires before it signals a newly orphaned group.
	stopProcessGroup(t, runtime.rootPID)
	stopProcess(t, witness.pid)

	require.NoError(t, runtime.cmd.Process.Kill())

	// The witness ran its hangup trap, so the kernel really did send the
	// orphaned group SIGHUP, and really did send the SIGCONT that thawed the
	// witness enough to act on it.
	waitFile(t, witness.marker)
	require.NoError(t, syscall.Kill(runtime.livenessPID, 0), "liveness supervisor died on the orphan hangup")

	// Freeze the survivor mid-quiescence. The group is already orphaned, so no
	// second hangup follows, and every lock it still owes stays observable.
	stopProcess(t, runtime.livenessPID)

	claim := acquireClaimEventually(t, runtime.home)
	defer func() { require.NoError(t, claim.Release()) }()
	requireLivenessLockHeld(t, runtime.home)
	assertAgentIdentityAuthorityLocked(t, runtime.isolationUID)

	require.NoError(t, syscall.Kill(runtime.livenessPID, syscall.SIGCONT))

	// Completion is published only after the subreaper's waitid reported
	// ECHILD, so the whole tree is already gone the moment the marker appears.
	require.Eventually(t, func() bool {
		_, err := os.Stat(runtime.proof.completion)

		return err == nil
	}, 15*time.Second, 10*time.Millisecond, "liveness supervisor never published completion")
	require.ErrorIs(t, syscall.Kill(runtime.rootPID, 0), syscall.ESRCH)
	require.ErrorIs(t, syscall.Kill(runtime.descPID, 0), syscall.ESRCH)

	runtime.waiter.start()
	<-runtime.waiter.result()
	runtime.cancel()
	require.NoError(t, runtime.proof.awaitCompletion())

	liveness := acquireLivenessEventually(t, runtime.home)
	require.NoError(t, liveness.Release())
	assertAgentIdentityAuthorityReacquires(t, runtime.isolationUID)
}

func TestSupervisorGuardianSIGKILLBeforeNativeLaunchRefusesStartAndCompletesAfterECHILD(t *testing.T) {
	preserveSupervisorGlobals(t)
	preserveLinuxSupervisorGlobals(t)

	peerRead, peerWrite, err := os.Pipe()
	require.NoError(t, err)
	t.Cleanup(func() {
		_ = peerWrite.Close()
		_ = peerRead.Close()
	})

	oldValidator := supervisorValidateGuardianPeer
	oldPeer := supervisorGuardianPeer
	t.Cleanup(func() {
		supervisorValidateGuardianPeer = oldValidator
		supervisorGuardianPeer = oldPeer
	})
	supervisorGuardianPeer = peerRead
	peerChecks := 0
	supervisorValidateGuardianPeer = func(peer *os.File, done <-chan struct{}) error {
		peerChecks++
		peerErr := validateLinuxSupervisorGuardianPeer(peer, done)
		if peerChecks == 1 && peerErr == nil {
			if closeErr := peerWrite.Close(); closeErr != nil {
				return closeErr
			}
			select {
			case <-done:
			case <-time.After(time.Second):
				return errors.New("guardian peer did not close")
			}
		}

		return peerErr
	}

	root := t.TempDir()
	config := supervisorConfig{
		NativePath:    "/bin/true",
		NativeArgs:    []string{"true"},
		NativeEnv:     os.Environ(),
		Home:          filepath.Join(root, "home"),
		Started:       filepath.Join(root, "started"),
		Completion:    filepath.Join(root, "completion"),
		Quarantine:    filepath.Join(root, "quarantine"),
		NativePIDFile: filepath.Join(root, "native-pid"),
	}
	var native *exec.Cmd
	supervisorExecCommand = func(string, ...string) *exec.Cmd {
		native = exec.Command("/bin/true")

		return native
	}

	err = runLiveness(config)
	require.ErrorContains(t, err, "guardian exited before native launch")
	require.Equal(t, 2, peerChecks, "guardian must be fenced again at the native Start boundary")
	require.NotNil(t, native)
	require.Nil(t, native.Process, "native command must not start after the guardian peer fence fails")
	require.FileExists(t, config.Completion)
	require.NoFileExists(t, config.NativePIDFile)
}

func TestSupervisorLivenessSIGKILLLeavesClaimLockedUntilTreeExit(t *testing.T) {
	runtime := startSupervisedNative(t)
	stopProcessGroup(t, runtime.rootPID)
	stopProcess(t, runtime.cmd.Process.Pid)
	t.Cleanup(func() { _ = syscall.Kill(runtime.cmd.Process.Pid, syscall.SIGCONT) })
	require.NoError(t, syscall.Kill(runtime.livenessPID, syscall.SIGKILL))

	_, err := homelock.AcquireClaim(runtime.home)
	require.Error(t, err, "surviving guardian must retain claim while it kills the tree")
	require.NoError(t, syscall.Kill(runtime.descPID, 0), "setsid descendant must still be live during authority contention")
	assertAgentIdentityAuthorityLocked(t, runtime.isolationUID)
	require.NoError(t, syscall.Kill(runtime.cmd.Process.Pid, syscall.SIGCONT))
	waitErr := waitSupervisor(runtime, 10*time.Second)
	require.Error(t, waitErr, "guardian must report its killed liveness child")
	require.NotErrorIs(t, waitErr, ErrProcessContainmentIncomplete)

	assertProcessGone(t, runtime.rootPID)
	assertProcessGone(t, runtime.descPID)
	assertHomeReacquires(t, runtime.home)
	assertAgentIdentityAuthorityReacquires(t, runtime.isolationUID)
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
	// Production runs no subreaper above the supervisor pair, so a killed
	// guardian's liveness supervisor really does reparent away and really does
	// end up in an orphaned process group. Other cases in this package set the
	// attribute on the test process as a side effect of running a supervisor
	// role in-process; clearing it here keeps every fixture user on the
	// production topology instead of a suppressed one.
	require.NoError(t, unix.Prctl(unix.PR_SET_CHILD_SUBREAPER, 0, 0, 0, 0))

	const standaloneStateRoot = "/var/lib/acp-go-codex-test"
	require.NoError(t, os.MkdirAll(standaloneStateRoot, 0o700))
	require.NoError(t, os.Chown(standaloneStateRoot, 65534, 65534))
	require.NoError(t, os.Chmod(standaloneStateRoot, 0o700))

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
	attackReadyPath := filepath.Join(root, "attacks.ready")
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
forge=denied; touch "$FORGE_PATH" 2>/dev/null && forge=allowed
config=denied; cat /proc/$parent/fd/3 >/dev/null 2>&1 && config=allowed
printf '%s %s %s %s %s %s %s %s\n' "$stop" "$kill_result" "$forge" "$config" "$(awk '$1 == "Uid:" {print $2}' /proc/$parent/status)" "$(id -u)" "$(id -g)" "$(awk '$1 == "Groups:" {print NF-1}' /proc/self/status)" > "$ATTACK_PATH"
touch "$ATTACK_READY_FILE"
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
		"ATTACK_READY_FILE="+attackReadyPath,
		"FORGE_PATH="+forgePath,
	)
	const isolationUID = 65534
	cmd, proof, err := supervisorCommand(ctx, supervisorConfig{
		NativePath: script,
		NativeArgs: []string{"root"},
		NativeEnv:  nativeEnv,
		Home:       home,
		Scratch:    scratch,
		Isolation: &ProcessIsolation{
			UID: isolationUID, GID: 65534, BaseEnvironment: environmentMap(nativeEnv),
			StandaloneOwnerID: "test-owner", StandaloneStateRoot: standaloneStateRoot,
		},
	})
	require.NoError(t, err)
	require.Nil(t, cmd.Cancel, "trusted supervisor must outlive caller-context cancellation")
	stdin, err := cmd.StdinPipe()
	require.NoError(t, err)
	stderr := new(supervisorStderr)
	cmd.Stdout = io.Discard
	cmd.Stderr = stderr
	waiter, err := startProcess(cmd)
	require.NoError(t, err)
	require.NoError(t, proof.closeInherited())

	runtime := &supervisedNative{
		cmd:          cmd,
		waiter:       waiter,
		stdin:        stdin,
		home:         home,
		stderr:       stderr,
		cancel:       cancel,
		proof:        proof,
		attackPath:   attackPath,
		forgePath:    forgePath,
		isolationUID: isolationUID,
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

	runtime.rootPID = waitPIDFile(t, rootPIDPath, stderr)
	runtime.descPID = waitPIDFile(t, descPIDPath, stderr)
	waitFile(t, attackReadyPath)
	runtime.livenessPID = parentPID(t, runtime.rootPID)
	require.Positive(t, runtime.livenessPID)

	return runtime
}

func assertAgentIdentityAuthorityLocked(t *testing.T, uid uint32) {
	t.Helper()
	for _, name := range []string{strconv.FormatUint(uint64(uid), 10) + ".lock", "domain.lock"} {
		fd, err := unix.Open(filepath.Join(linuxAgentIdentityNamespace, name), unix.O_RDWR|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0)
		require.NoError(t, err)
		lockErr := unix.Flock(fd, unix.LOCK_EX|unix.LOCK_NB)
		if lockErr == nil {
			_ = unix.Close(fd)
			t.Fatalf("authority lock %s became available before survivor containment", name)
		}
		require.True(t, errors.Is(lockErr, unix.EWOULDBLOCK) || errors.Is(lockErr, unix.EAGAIN), "contend %s: %v", name, lockErr)
		require.NoError(t, unix.Close(fd))
	}
}

func assertAgentIdentityAuthorityReacquires(t *testing.T, uid uint32) {
	t.Helper()
	for _, name := range []string{strconv.FormatUint(uint64(uid), 10) + ".lock", "domain.lock"} {
		require.Eventually(t, func() bool {
			fd, err := unix.Open(filepath.Join(linuxAgentIdentityNamespace, name), unix.O_RDWR|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0)
			if err != nil {
				return false
			}
			defer unix.Close(fd)

			return unix.Flock(fd, unix.LOCK_EX|unix.LOCK_NB) == nil
		}, 5*time.Second, 10*time.Millisecond, "authority lock %s did not release after ECHILD", name)
	}
}

func waitFile(t *testing.T, path string) {
	t.Helper()
	require.Eventually(t, func() bool {
		info, err := os.Stat(path)

		return err == nil && info.Mode().IsRegular()
	}, 5*time.Second, 10*time.Millisecond, "file %s was not published", path)
}

func waitPIDFile(t *testing.T, path string, stderr *supervisorStderr) int {
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
	t.Fatalf("PID file %s was not published; supervisor stderr: %s", path, stderr.String())

	return 0
}

func parentPID(t *testing.T, pid int) int {
	t.Helper()
	fields, err := procStatFields(fmt.Sprintf("/proc/%d/stat", pid))
	require.NoError(t, err)
	require.GreaterOrEqual(t, len(fields), 2)
	parent, err := strconv.Atoi(fields[1])
	require.NoError(t, err)

	return parent
}

// procStatFields returns the stat fields that follow the comm field, so a
// process name containing spaces or parentheses cannot shift the offsets.
func procStatFields(path string) ([]string, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	closeParen := strings.LastIndexByte(string(raw), ')')
	if closeParen <= 0 {
		return nil, fmt.Errorf("%s has no comm field", path)
	}

	return strings.Fields(string(raw[closeParen+1:])), nil
}

func stopProcessGroup(t *testing.T, pid int) {
	t.Helper()
	require.NoError(t, syscall.Kill(-pid, syscall.SIGSTOP))
	awaitProcessStopped(t, pid)
}

func stopProcess(t *testing.T, pid int) {
	t.Helper()
	require.NoError(t, syscall.Kill(pid, syscall.SIGSTOP))
	awaitProcessStopped(t, pid)
}

// awaitProcessStopped blocks until every task of the process has entered the
// group stop. kill returns as soon as the signal is queued, so a supervisor
// that is only nominally frozen can still observe its peer's death, release its
// runtime lock, and quiesce the tree before the stop latches.
func awaitProcessStopped(t *testing.T, pid int) {
	t.Helper()
	taskRoot := fmt.Sprintf("/proc/%d/task", pid)
	require.Eventually(t, func() bool {
		tasks, err := os.ReadDir(taskRoot)
		if err != nil || len(tasks) == 0 {
			return false
		}
		for _, task := range tasks {
			fields, statErr := procStatFields(filepath.Join(taskRoot, task.Name(), "stat"))
			if statErr != nil || len(fields) == 0 || fields[0] != "T" {
				return false
			}
		}

		return true
	}, 5*time.Second, 5*time.Millisecond, "process %d did not enter the stopped state", pid)
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

// hangupSignalMask selects SIGHUP inside the hexadecimal signal masks
// /proc/<pid>/status publishes, which number signal N at bit N-1.
const hangupSignalMask = uint64(1) << (uint(syscall.SIGHUP) - 1)

// orphanHangupWitness is a process planted inside the liveness supervisor's
// process group that reports the SIGHUP the kernel sends a newly orphaned
// group. Its parent lives outside the supervised session, so the group's
// orphan status turns on the guardian alone.
type orphanHangupWitness struct {
	pid    int
	marker string
}

func startOrphanHangupWitness(t *testing.T, groupPID int) orphanHangupWitness {
	t.Helper()

	root := t.TempDir()
	witness := orphanHangupWitness{marker: filepath.Join(root, "hangup")}
	pidPath := filepath.Join(root, "witness.pid")
	script := filepath.Join(root, "witness.sh")
	require.NoError(t, os.WriteFile(script, []byte(`#!/bin/sh
if [ "$1" = "witness" ]; then
  trap 'printf hangup > "$WITNESS_MARKER"; exit 0' HUP
  echo "$$" > "$WITNESS_PID_FILE"
  while :; do sleep 30; done
fi
"$0" witness &
`), 0o755))

	// The launcher joins the group and then exits, so the witness it leaves
	// behind is reparented away from this test process before the group's
	// orphan status is ever computed.
	launcher := exec.Command(script)
	launcher.Env = append(os.Environ(), "WITNESS_MARKER="+witness.marker, "WITNESS_PID_FILE="+pidPath)
	launcher.SysProcAttr = &syscall.SysProcAttr{Setpgid: true, Pgid: groupPID}
	require.NoError(t, launcher.Start())

	witness.pid = waitWitnessPID(t, pidPath)
	t.Cleanup(func() {
		_ = syscall.Kill(witness.pid, syscall.SIGCONT)
		_ = syscall.Kill(witness.pid, syscall.SIGKILL)
	})

	require.NoError(t, launcher.Wait())
	require.Equal(t, groupPID, procStatField(t, witness.pid, procStatProcessGroup),
		"orphan hangup witness must share the liveness supervisor's process group")

	// A process group stays connected while any member has a parent in another
	// group of the same session, and the witness reparents to whatever would
	// adopt the liveness supervisor. An adopter inside this session would
	// suppress the exact signal this proof is about, so it is a failure here
	// rather than a reason to stand the case down.
	parent := procStatField(t, witness.pid, procStatParent)
	if parent != 1 {
		require.NotEqual(t, procStatField(t, groupPID, procStatSession), procStatField(t, parent, procStatSession),
			"the orphan hangup proof needs the supervisor pair to reparent outside its own session, but process %d adopted it from within",
			parent)
	}

	return witness
}

func waitWitnessPID(t *testing.T, path string) int {
	t.Helper()

	pid := 0
	deadline := time.Now().Add(5 * time.Second)

	for time.Now().Before(deadline) {
		raw, err := os.ReadFile(path)
		if err == nil {
			parsed, parseErr := strconv.Atoi(strings.TrimSpace(string(raw)))
			if parseErr == nil && parsed > 0 {
				pid = parsed

				break
			}
		}

		time.Sleep(10 * time.Millisecond)
	}

	require.Positive(t, pid, "orphan hangup witness never published its PID")

	return pid
}

// The stat fields that follow comm, as procStatFields returns them.
const (
	procStatParent       = 1
	procStatProcessGroup = 2
	procStatSession      = 3
)

func procStatField(t *testing.T, pid int, index int) int {
	t.Helper()

	fields, err := procStatFields(fmt.Sprintf("/proc/%d/stat", pid))
	require.NoError(t, err)
	require.Greater(t, len(fields), index)

	value, err := strconv.Atoi(fields[index])
	require.NoError(t, err)

	return value
}

// requireHangupNotIgnored proves a process reached SIGHUP through a handler
// rather than SIG_IGN. Only SIG_IGN survives execve, so an ignored hangup in a
// trusted role would leave the native child it launches deaf to a real one.
func requireHangupNotIgnored(t *testing.T, pid int) {
	t.Helper()
	require.Zero(t, procSignalMask(t, pid, "SigIgn")&hangupSignalMask, "process %d ignores SIGHUP", pid)
}

func procSignalMask(t *testing.T, pid int, field string) uint64 {
	t.Helper()

	raw, err := os.ReadFile(fmt.Sprintf("/proc/%d/status", pid))
	require.NoError(t, err)

	for line := range strings.SplitSeq(string(raw), "\n") {
		value, found := strings.CutPrefix(line, field+":")
		if !found {
			continue
		}

		mask, parseErr := strconv.ParseUint(strings.TrimSpace(value), 16, 64)
		require.NoError(t, parseErr)

		return mask
	}

	require.FailNowf(t, "missing signal mask", "/proc/%d/status published no %s field", pid, field)

	return 0
}

func requireLivenessLockHeld(t *testing.T, home string) {
	t.Helper()

	_, err := homelock.AcquireLiveness(home)
	require.Error(t, err, "liveness supervisor must still hold its lock")
}

func acquireClaimEventually(t *testing.T, home string) *homelock.Lock {
	t.Helper()

	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		lock, err := homelock.AcquireClaim(home)
		if err == nil {
			return lock
		}

		time.Sleep(10 * time.Millisecond)
	}

	require.FailNow(t, "guardian death did not release the home claim")

	return nil
}
