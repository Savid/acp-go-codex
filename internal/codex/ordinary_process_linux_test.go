//go:build linux

package codex

import (
	"os/exec"
	"syscall"
	"testing"
)

func TestConfigureProcessLinux(t *testing.T) {
	cmd := exec.Command("true")
	configureProcess(cmd)
	if cmd.SysProcAttr == nil || !cmd.SysProcAttr.Setpgid || cmd.SysProcAttr.Pdeathsig != syscall.SIGKILL {
		t.Fatalf("SysProcAttr = %#v, want Setpgid and Pdeathsig SIGKILL", cmd.SysProcAttr)
	}
}
