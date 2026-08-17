//go:build darwin

package codex

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"sync"
	"syscall"
	"time"
)

const darwinGroupTermGrace = 500 * time.Millisecond

var (
	darwinSupervisorGetpgid = syscall.Getpgid
	darwinSupervisorKill    = syscall.Kill
	darwinSupervisorNow     = time.Now
	darwinSupervisorSleep   = time.Sleep
	darwinFastExitWait      = supervisorQuiesceWindow
	darwinAbortWait         = supervisorQuiesceWindow
)

type guardianContainment struct{}

type livenessContainment struct {
	generation  *DarwinGeneration
	pgid        int
	process     *os.Process
	waiter      *supervisorWaiter
	beforeStart func() error

	cleanupOnce sync.Once
	cleanupErr  error
}

func (*livenessContainment) DescendantCount() (int, bool) { return 0, false }

func newGuardianContainment(config supervisorConfig) (*guardianContainment, error) {
	if !config.DarwinBestEffort {
		return nil, errors.New("codex runtime containment is unsupported on darwin without explicit best-effort opt-in")
	}

	return &guardianContainment{}, nil
}

func (*guardianContainment) Name() string { return "darwin-best-effort" }

func (*guardianContainment) Close() error { return nil }

func (*guardianContainment) Quiesce(nativePID int, _ time.Duration) error {
	if nativePID <= 0 {
		return fmt.Errorf("%w: liveness supervisor exited after native start without direct-child reap proof", ErrProcessContainmentIncomplete)
	}

	return fmt.Errorf(
		"%w: liveness supervisor exited after native start (reported pid %d); guardian has no stateful native-child handle",
		ErrProcessContainmentIncomplete,
		nativePID,
	)
}

func openLivenessContainment(config supervisorConfig) (*livenessContainment, error) {
	if !config.DarwinBestEffort {
		return nil, errors.New("codex runtime containment is unsupported on darwin without explicit best-effort opt-in")
	}

	if config.ScratchParent == "" || config.Scratch == "" {
		return nil, errors.New("darwin best-effort containment requires a scratch parent and generation root")
	}

	generation, err := NewDarwinGenerationRecord(config.ScratchParent, config.Scratch, config.LifecycleKind)
	if err != nil {
		return nil, err
	}

	return &livenessContainment{generation: generation}, nil
}

func (c *livenessContainment) Start(cmd *exec.Cmd) error {
	if c == nil || c.generation == nil {
		return errors.New("darwin best-effort containment generation is unavailable")
	}

	if err := c.generation.prepareCommand(cmd); err != nil {
		return errors.Join(err, c.generation.finish(true))
	}

	configureProcess(cmd)
	cmd.WaitDelay = supervisorQuiesceWindow

	if c.beforeStart != nil {
		if err := c.beforeStart(); err != nil {
			return errors.Join(err, c.generation.finish(true))
		}
	}

	if err := cmd.Start(); err != nil {
		return errors.Join(err, c.generation.finish(true))
	}

	c.pgid = cmd.Process.Pid
	c.process = cmd.Process
	c.waiter = newSupervisorWaiter(cmd, true)

	pgid, err := darwinSupervisorGetpgid(cmd.Process.Pid)
	if errors.Is(err, syscall.ESRCH) {
		return c.handleFastExit(cmd.Process.Pid)
	}

	if err != nil {
		return c.abortUnvalidated(cmd, fmt.Errorf("inspect Darwin native root process group: %w", err))
	}

	if pgid != cmd.Process.Pid {
		return c.abortUnvalidated(cmd, fmt.Errorf("darwin native root joined unexpected process group %d", pgid))
	}

	if err := c.generation.started(cmd.Process.Pid, pgid); err != nil {
		cleanupErr := c.Quiesce(cmd.Process.Pid, supervisorQuiesceWindow)

		return errors.Join(err, cleanupErr)
	}

	c.waiter.start()

	return nil
}

func (c *livenessContainment) handleFastExit(pid int) error {
	probeErr := darwinSupervisorKill(-pid, 0)
	switch {
	case errors.Is(probeErr, syscall.ESRCH):
		c.waiter.start()

		ctx, cancel := context.WithTimeout(context.Background(), darwinFastExitWait)
		defer cancel()

		waitErr, completed := c.waiter.await(ctx)
		if !completed {
			return errors.Join(
				fmt.Errorf("%w: reap fast-exit Darwin native root: %v", ErrProcessContainmentIncomplete, waitErr),
				c.generation.finish(false),
			)
		}

		return errors.Join(
			errors.New("darwin native root exited before containment identity capture"),
			waitErr,
			c.generation.finish(true),
		)
	case probeErr == nil || errors.Is(probeErr, syscall.EPERM):
		cleanupErr := c.Quiesce(pid, supervisorQuiesceWindow)

		return errors.Join(errors.New("darwin native root exited before containment identity capture"), cleanupErr)
	default:
		return c.abortUnvalidated(nil, fmt.Errorf("probe expected Darwin process group %d: %w", pid, probeErr))
	}
}

func (c *livenessContainment) abortUnvalidated(cmd *exec.Cmd, cause error) error {
	deadline := darwinSupervisorNow().Add(darwinAbortWait)

	process := c.process
	if cmd != nil && cmd.Process != nil {
		process = cmd.Process
	}

	if process != nil {
		_ = process.Signal(syscall.SIGTERM)
	}

	c.waiter.start()

	termWait := darwinGroupTermGrace
	if remaining := deadline.Sub(darwinSupervisorNow()); remaining < termWait {
		termWait = remaining
	}

	waitErr, completed := awaitSupervisorWaiter(c.waiter, termWait)
	if !completed && process != nil {
		_ = process.Kill()
		waitErr, completed = awaitSupervisorWaiter(c.waiter, deadline.Sub(darwinSupervisorNow()))
	}

	if !completed {
		waitErr = fmt.Errorf("direct child remained unreaped: %w", waitErr)
	}

	return errors.Join(
		fmt.Errorf("%w: %v", ErrProcessContainmentIncomplete, cause),
		waitErr,
		c.generation.finish(false),
	)
}

func awaitSupervisorWaiter(waiter *supervisorWaiter, timeout time.Duration) (error, bool) {
	if timeout <= 0 {
		return context.DeadlineExceeded, false
	}

	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()

	return waiter.await(ctx)
}

func (c *livenessContainment) Wait() <-chan error {
	return c.waiter.result()
}

func (c *livenessContainment) Close() error {
	if c == nil || c.generation == nil || c.waiter != nil {
		return nil
	}

	return c.generation.finish(true)
}

func (c *livenessContainment) Quiesce(_ int, timeout time.Duration) error {
	if c == nil || c.pgid <= 0 {
		return nil
	}

	c.cleanupOnce.Do(func() {
		c.cleanupErr = quiesceDarwinOriginalGroupWithProcess(c.pgid, c.process, c.waiter, c.generation, timeout)
	})

	return c.cleanupErr
}

func quiesceDarwinOriginalGroupWithProcess(
	pgid int,
	process *os.Process,
	waiter *supervisorWaiter,
	generation *DarwinGeneration,
	timeout time.Duration,
) error {
	if pgid <= 0 || timeout <= 0 {
		return finishDarwinGeneration(generation, fmt.Errorf("%w: positive Darwin quiescence boundary is required", ErrProcessContainmentIncomplete))
	}

	deadline := darwinSupervisorNow().Add(timeout)

	termDeadline := darwinSupervisorNow().Add(darwinGroupTermGrace)
	if termDeadline.After(deadline) {
		termDeadline = deadline
	}

	groupAbsent := false
	killed := false

	termErr := darwinSupervisorKill(-pgid, syscall.SIGTERM)

	if waiter != nil {
		waiter.start()
	}

	switch {
	case errors.Is(termErr, syscall.ESRCH):
		groupAbsent = true
	case termErr != nil && !errors.Is(termErr, syscall.EPERM):
		return abortDarwinCleanup(process, waiter, generation, deadline, fmt.Errorf("terminate original process group %d: %w", pgid, termErr))
	}

	for darwinSupervisorNow().Before(deadline) {
		if !groupAbsent {
			probeErr := darwinSupervisorKill(-pgid, 0)
			switch {
			case errors.Is(probeErr, syscall.ESRCH):
				groupAbsent = true
			case probeErr != nil && !errors.Is(probeErr, syscall.EPERM):
				return abortDarwinCleanup(process, waiter, generation, deadline, fmt.Errorf("inspect original process group %d: %w", pgid, probeErr))
			}
		}

		reaped := waiter == nil
		if waiter != nil {
			select {
			case <-waiter.done:
				reaped = true
			default:
			}
		}

		if groupAbsent && reaped {
			return finishDarwinGeneration(generation, nil)
		}

		if !groupAbsent && !killed && !darwinSupervisorNow().Before(termDeadline) {
			killErr := darwinSupervisorKill(-pgid, syscall.SIGKILL)
			switch {
			case errors.Is(killErr, syscall.ESRCH):
				groupAbsent = true
			case killErr != nil && !errors.Is(killErr, syscall.EPERM):
				return abortDarwinCleanup(process, waiter, generation, deadline, fmt.Errorf("kill original process group %d: %w", pgid, killErr))
			}

			killed = true
		}

		darwinSupervisorSleep(10 * time.Millisecond)
	}

	return finishDarwinGeneration(generation, fmt.Errorf("%w: direct child or original process group %d remained observable", ErrProcessContainmentIncomplete, pgid))
}

func abortDarwinCleanup(
	process *os.Process,
	waiter *supervisorWaiter,
	generation *DarwinGeneration,
	deadline time.Time,
	cause error,
) error {
	var directKillErr error
	if process != nil {
		directKillErr = process.Kill()
		if errors.Is(directKillErr, syscall.ESRCH) || errors.Is(directKillErr, os.ErrProcessDone) {
			directKillErr = nil
		}
	}

	var reapErr error

	if waiter != nil {
		waiter.start()

		remaining := deadline.Sub(darwinSupervisorNow())
		if remaining <= 0 {
			reapErr = errors.New("direct child remained unreaped at the containment deadline")
		} else {
			_, completed := awaitSupervisorWaiter(waiter, remaining)

			if !completed {
				reapErr = errors.New("direct child remained unreaped at the containment deadline")
			}
		}
	}

	return finishDarwinGeneration(generation, errors.Join(
		fmt.Errorf("%w: %v", ErrProcessContainmentIncomplete, cause),
		directKillErr,
		reapErr,
	))
}

func finishDarwinGeneration(generation *DarwinGeneration, cleanupErr error) error {
	return errors.Join(cleanupErr, generation.finish(cleanupErr == nil))
}

func configureIndependentSupervisor(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
}

func startIndependentSupervisor(cmd *exec.Cmd) error {
	return cmd.Start()
}

func terminateIndependentSupervisor(cmd *exec.Cmd) error {
	if cmd == nil || cmd.Process == nil {
		return nil
	}

	err := darwinSupervisorKill(-cmd.Process.Pid, syscall.SIGKILL)
	if errors.Is(err, syscall.ESRCH) {
		err = cmd.Process.Kill()
	}

	if errors.Is(err, os.ErrProcessDone) || errors.Is(err, syscall.ESRCH) {
		return nil
	}

	return err
}
