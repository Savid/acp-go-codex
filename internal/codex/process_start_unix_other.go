//go:build unix && !linux

package codex

import "os/exec"

func startProcess(cmd *exec.Cmd) (*supervisorWaiter, error) {
	configureProcess(cmd)

	if err := cmd.Start(); err != nil {
		return nil, err
	}

	return newSupervisorWaiter(cmd, true), nil
}
