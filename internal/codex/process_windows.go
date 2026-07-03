//go:build windows

package codex

import "os/exec"

func startProcess(cmd *exec.Cmd) error {
	return cmd.Start()
}

func killProcess(cmd *exec.Cmd) error {
	if cmd == nil || cmd.Process == nil {
		return nil
	}

	return cmd.Process.Kill()
}

// terminateProcess falls back to a hard kill: Windows has no SIGTERM
// equivalent for console child processes.
func terminateProcess(cmd *exec.Cmd) error {
	return killProcess(cmd)
}

func processCloseError(err error) error {
	return err
}
