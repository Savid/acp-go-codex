//go:build linux

package codex

import (
	"os/exec"
	"runtime"
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

func TestProviderCreatorLinuxProcessStartRetainsCreatorThreadThroughWait(t *testing.T) {
	cmd := exec.Command("/bin/sh", "-c", "while :; do sleep 1; done")
	waiter, err := startProcess(cmd)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL) })

	for range 512 {
		runtime.Gosched()
		if err := syscall.Kill(cmd.Process.Pid, 0); err != nil {
			t.Fatalf("runtime child exited while its creator-thread Wait was active: %v", err)
		}
	}

	waiter.start()
	if err := syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL); err != nil {
		t.Fatal(err)
	}
	if err := <-waiter.result(); err == nil {
		t.Fatal("killed runtime child returned a successful wait result")
	}
}
