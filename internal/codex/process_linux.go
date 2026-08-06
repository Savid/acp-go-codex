//go:build linux

package codex

import (
	"os/exec"
	"syscall"
)

func configureProcess(cmd *exec.Cmd) {
	var credential *syscall.Credential
	if cmd.SysProcAttr != nil {
		credential = cmd.SysProcAttr.Credential
	}

	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true, Pdeathsig: syscall.SIGKILL, Credential: credential}
}
