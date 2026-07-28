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
	"syscall"
	"testing"
	"time"

	"github.com/savid/acp-go-codex/internal/homelock"
	"github.com/stretchr/testify/require"
	"golang.org/x/sys/unix"
)

type supervisedNative struct {
	cmd         *exec.Cmd
	stdin       io.WriteCloser
	home        string
	rootPID     int
	descPID     int
	livenessPID int
	stderr      *bytes.Buffer
	cancel      context.CancelFunc
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
	oldTasks := linuxTaskEntries
	oldChildren := linuxTaskChildren
	oldWait4 := linuxWait4
	oldWaitid := linuxWaitid
	oldKill := killProcessID
	t.Cleanup(func() {
		linuxSetSubreaper = oldSubreaper
		linuxTaskEntries = oldTasks
		linuxTaskChildren = oldChildren
		linuxWait4 = oldWait4
		linuxWaitid = oldWaitid
		killProcessID = oldKill
	})
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
	_ = runtime.cmd.Wait()

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
	root := t.TempDir()
	home := filepath.Join(root, "home")
	scratch := filepath.Join(root, "scratch")
	require.NoError(t, os.MkdirAll(scratch, 0o700))
	rootPIDPath := filepath.Join(root, "root.pid")
	descPIDPath := filepath.Join(root, "desc.pid")
	script := filepath.Join(root, "native.sh")
	require.NoError(t, os.WriteFile(script, []byte(`#!/bin/sh
if [ "$1" = "descendant" ]; then
  trap '' HUP INT TERM
  while :; do sleep 1; done
fi
setsid "$0" descendant &
echo "$!" > "$DESC_PID_FILE"
echo "$$" > "$ROOT_PID_FILE"
trap '' TERM
while IFS= read -r line; do
  [ "$line" = "exit" ] && exit 0
  printf '%s\n' "$line"
done
`), 0o700))

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	cmd, _, err := supervisorCommand(ctx, supervisorConfig{
		NativePath: script,
		NativeArgs: []string{"root"},
		NativeEnv: append(os.Environ(),
			"ROOT_PID_FILE="+rootPIDPath,
			"DESC_PID_FILE="+descPIDPath,
		),
		Home:    home,
		Scratch: scratch,
	})
	require.NoError(t, err)
	stdin, err := cmd.StdinPipe()
	require.NoError(t, err)
	stderr := new(bytes.Buffer)
	cmd.Stdout = io.Discard
	cmd.Stderr = stderr
	require.NoError(t, startProcess(cmd))

	runtime := &supervisedNative{
		cmd:    cmd,
		stdin:  stdin,
		home:   home,
		stderr: stderr,
		cancel: cancel,
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
	done := make(chan error, 1)
	go func() { done <- runtime.cmd.Wait() }()
	select {
	case err := <-done:
		runtime.cancel()

		return err
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
