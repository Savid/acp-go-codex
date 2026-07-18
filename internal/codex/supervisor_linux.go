//go:build linux

package codex

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"golang.org/x/sys/unix"
)

type guardianContainment struct{}

type livenessContainment struct {
	waiter *supervisorWaiter
}

const linuxTaskRoot = "/proc/self/task"

var (
	linuxSetSubreaper = func() error {
		return unix.Prctl(unix.PR_SET_CHILD_SUBREAPER, 1, 0, 0, 0)
	}
	linuxTaskEntries = func() ([]os.DirEntry, error) {
		return os.ReadDir(linuxTaskRoot)
	}
	linuxTaskChildren = os.ReadFile
	linuxWait4        = unix.Wait4
	linuxWaitid       = unix.Waitid
)

func (*livenessContainment) DescendantCount() (int, bool) {
	children, err := linuxDirectChildren()
	if err != nil {
		return 0, false
	}

	return len(children), true
}

func newGuardianContainment(supervisorConfig) (*guardianContainment, error) {
	if err := linuxSetSubreaper(); err != nil {
		return nil, fmt.Errorf("enable guardian child subreaper: %w", err)
	}

	return &guardianContainment{}, nil
}

func (*guardianContainment) Name() string { return "linux-subreaper" }

func (*guardianContainment) Close() error { return nil }

func (*guardianContainment) Quiesce(nativePID int, timeout time.Duration) error {
	return quiesceSubreaper(nativePID, timeout, false)
}

func openLivenessContainment(supervisorConfig) (*livenessContainment, error) {
	if err := linuxSetSubreaper(); err != nil {
		return nil, fmt.Errorf("enable liveness child subreaper: %w", err)
	}

	return &livenessContainment{}, nil
}

func (c *livenessContainment) Start(cmd *exec.Cmd) error {
	if err := startProcess(cmd); err != nil {
		return err
	}

	c.waiter = newSupervisorWaiter(cmd, false)

	return nil
}

func (c *livenessContainment) Wait() <-chan error {
	return c.waiter.result()
}

func (*livenessContainment) Close() error { return nil }

func (*livenessContainment) Quiesce(nativePID int, timeout time.Duration) error {
	return quiesceSubreaper(nativePID, timeout, true)
}

func configureIndependentSupervisor(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
}

func terminateIndependentSupervisor(cmd *exec.Cmd) error {
	return signalProcess(cmd, syscall.SIGKILL)
}

// quiesceSubreaper first terminates the native root's original process group,
// then kills and reaps every descendant adopted by this dedicated subreaper.
// The final waitid(WNOWAIT) ECHILD result is a non-reaping kernel proof that no
// child remains; it cannot be fabricated by kill(pid, 0) or a /proc snapshot.
func quiesceSubreaper(nativePID int, timeout time.Duration, externalRootWaiter bool) error {
	if timeout <= 0 {
		return fmt.Errorf("%w: positive quiescence timeout is required", ErrProcessContainmentIncomplete)
	}

	deadline := time.Now().Add(timeout)

	termDeadline := time.Now().Add(500 * time.Millisecond)
	if termDeadline.After(deadline) {
		termDeadline = deadline
	}

	// Only liveness has an external cmd.Wait for a still-live native root. The
	// guardian runs after liveness has exited, so it drains adopted children by
	// PID and never signals a stale numeric process-group ID.
	if externalRootWaiter && nativePID > 0 {
		_ = signalProcessGroup(nativePID, syscall.SIGTERM)
	}

	killedGroup := !externalRootWaiter || nativePID <= 0
	for time.Now().Before(deadline) {
		if !killedGroup && !time.Now().Before(termDeadline) {
			_ = signalProcessGroup(nativePID, syscall.SIGKILL)
			killedGroup = true
		}

		if err := killAndReapAdoptedChildren(nativePID, externalRootWaiter); err != nil {
			return fmt.Errorf("%w: drain adopted native descendants: %v", ErrProcessContainmentIncomplete, err)
		}

		empty, err := subreaperHasNoChildren()
		if err != nil {
			return fmt.Errorf("%w: prove subreaper has no children: %v", ErrProcessContainmentIncomplete, err)
		}

		if empty {
			return nil
		}

		time.Sleep(10 * time.Millisecond)
	}

	if externalRootWaiter && nativePID > 0 && !killedGroup {
		_ = signalProcessGroup(nativePID, syscall.SIGKILL)
	}

	return fmt.Errorf("%w: native descendants remained after %s", ErrProcessContainmentIncomplete, timeout)
}

func killAndReapAdoptedChildren(nativePID int, externalRootWaiter bool) error {
	children, err := linuxDirectChildren()
	if err != nil {
		return err
	}

	for _, pid := range children {
		// cmd.Wait owns the native root. Reaping it here would race that waiter;
		// its completion makes the root disappear from the no-child proof.
		if externalRootWaiter && pid == nativePID {
			continue
		}

		if err := killProcessID(pid, syscall.SIGKILL); err != nil && !errors.Is(err, syscall.ESRCH) {
			return fmt.Errorf("kill adopted descendant %d: %w", pid, err)
		}

		var status unix.WaitStatus

		waited, err := linuxWait4(pid, &status, unix.WNOHANG, nil)
		if err != nil && !errors.Is(err, syscall.ECHILD) {
			return fmt.Errorf("reap adopted descendant %d: %w", pid, err)
		}

		if waited != 0 && waited != pid {
			return fmt.Errorf("reap adopted descendant %d returned pid %d", pid, waited)
		}
	}

	return nil
}

func linuxDirectChildren() ([]int, error) {
	tasks, err := linuxTaskEntries()
	if err != nil {
		return nil, fmt.Errorf("list subreaper tasks: %w", err)
	}

	seen := make(map[int]struct{})

	for _, task := range tasks {
		if !task.IsDir() {
			continue
		}

		path := filepath.Join(linuxTaskRoot, task.Name(), "children")

		raw, err := linuxTaskChildren(path)
		if errors.Is(err, os.ErrNotExist) {
			continue
		}

		if err != nil {
			return nil, fmt.Errorf("read subreaper task children: %w", err)
		}

		for _, field := range strings.Fields(string(raw)) {
			pid, err := strconv.Atoi(field)
			if err != nil || pid <= 0 {
				return nil, fmt.Errorf("parse subreaper child PID %q", field)
			}

			seen[pid] = struct{}{}
		}
	}

	children := make([]int, 0, len(seen))
	for pid := range seen {
		children = append(children, pid)
	}

	return children, nil
}

func subreaperHasNoChildren() (bool, error) {
	var info unix.Siginfo

	err := linuxWaitid(unix.P_ALL, 0, &info, unix.WEXITED|unix.WNOHANG|unix.WNOWAIT, nil)
	if errors.Is(err, syscall.ECHILD) {
		return true, nil
	}

	if err != nil {
		return false, err
	}

	return false, nil
}

func signalProcessGroup(nativePID int, signal syscall.Signal) error {
	err := killProcessID(-nativePID, signal)
	if errors.Is(err, syscall.ESRCH) {
		return nil
	}

	return err
}
