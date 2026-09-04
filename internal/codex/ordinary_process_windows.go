//go:build windows

package codex

import (
	"errors"
	"os"
	"os/exec"
	"syscall"
)

func configureProcess(*exec.Cmd) {}

func killProcess(cmd *exec.Cmd) error {
	if cmd == nil || cmd.Process == nil {
		return nil
	}

	err := cmd.Process.Kill()

	// A process that has already finished is contained, and asking Windows to
	// terminate it again is not a containment failure. Windows does not spell
	// that the way a unix host does: once the process has been waited on its
	// handle is released, and the terminate is refused with EINVAL rather than
	// answered with os.ErrProcessDone. Both spellings mean the same thing, and a
	// containment path that read either as a failure would refuse every close
	// over a turn whose native process had already exited.
	if errors.Is(err, os.ErrProcessDone) || errors.Is(err, syscall.EINVAL) {
		return nil
	}

	return err
}

// terminateProcess falls back to a hard kill: Windows has no SIGTERM
// equivalent for console child processes.
func terminateProcess(cmd *exec.Cmd) error {
	return killProcess(cmd)
}
