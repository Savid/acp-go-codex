//go:build !windows

package codex

import (
	"errors"
	"os"
	"os/exec"
	"syscall"
)

var (
	getProcessGroupID = syscall.Getpgid
	killProcessID     = syscall.Kill
)

func terminateProcess(cmd *exec.Cmd) error {
	return signalProcess(cmd, syscall.SIGTERM)
}

func killProcess(cmd *exec.Cmd) error {
	return signalProcess(cmd, syscall.SIGKILL)
}

func signalProcess(cmd *exec.Cmd, signal syscall.Signal) error {
	if cmd == nil || cmd.Process == nil {
		return nil
	}

	if pgid, err := getProcessGroupID(cmd.Process.Pid); err == nil {
		if err := killProcessID(-pgid, signal); err != nil && !errors.Is(err, syscall.ESRCH) {
			return err
		}

		return nil
	}

	err := cmd.Process.Signal(signal)
	if errors.Is(err, os.ErrProcessDone) {
		return nil
	}

	return err
}

func processCloseError(err error) error {
	if err == nil {
		return nil
	}

	if errors.Is(err, ErrProcessContainmentIncomplete) {
		return err
	}

	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) {
		if status, ok := exitErr.Sys().(syscall.WaitStatus); ok && status.Signaled() {
			return nil
		}
	}

	return err
}
