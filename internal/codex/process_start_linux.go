//go:build linux

package codex

import "os/exec"

func startProcess(cmd *exec.Cmd) (*supervisorWaiter, error) {
	configureProcess(cmd)

	beginWait := make(chan struct{})
	waitDone, err := startCommandOnCreatorThread(cmd.Start, func() error {
		<-beginWait

		return cmd.Wait()
	})
	if err != nil {
		return nil, err
	}

	return newSupervisorWaiterResult(waitDone, func() { close(beginWait) }, true), nil
}
