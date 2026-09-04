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
	signalOneProcess  = func(process *os.Process, signal os.Signal) error { return process.Signal(signal) }
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

	err := signalOneProcess(cmd.Process, signal)
	if errors.Is(err, os.ErrProcessDone) {
		return nil
	}

	return err
}
