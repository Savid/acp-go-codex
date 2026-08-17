//go:build darwin

package codex

import (
	"os/exec"
	"testing"
)

func TestConfigureProcessDarwin(t *testing.T) {
	cmd := exec.Command("true")
	configureProcess(cmd)
	if cmd.SysProcAttr == nil || !cmd.SysProcAttr.Setpgid {
		t.Fatalf("SysProcAttr = %#v, want Setpgid", cmd.SysProcAttr)
	}
}
