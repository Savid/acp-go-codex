//go:build darwin

package codex

import (
	"os/exec"
	"syscall"
)

func configureProcess(cmd *exec.Cmd) {
	// Darwin has no Pdeathsig equivalent; parent-death cleanup is best-effort
	// via process-group signalling.
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
}
