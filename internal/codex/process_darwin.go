//go:build darwin

package codex

import (
	"os/exec"
	"syscall"
)

func configureProcess(cmd *exec.Cmd) {
	// Darwin has no Pdeathsig equivalent; parent-death cleanup is best-effort
	// via process-group signalling.
	var credential *syscall.Credential
	if cmd.SysProcAttr != nil {
		credential = cmd.SysProcAttr.Credential
	}

	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true, Credential: credential}
}
