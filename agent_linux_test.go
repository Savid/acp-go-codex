//go:build linux

package codexacp

import (
	"encoding/json"
	"os"
	"os/exec"
	"os/signal"
	"syscall"

	"golang.org/x/sys/unix"
)

type fakeCodexChildIdentity struct {
	PID  int `json:"pid"`
	PGID int `json:"pgid"`
	SID  int `json:"sid"`
}

func newFakeCodexDelayedChildCommand() *exec.Cmd {
	command := exec.Command(os.Args[0], fakeCodexDelayedChildArg) // #nosec G204 -- fixed test binary and fixed argument.
	command.SysProcAttr = &syscall.SysProcAttr{Setsid: true}

	return command
}

func ignoreFakeCodexChildTerminationSignals() {
	signal.Ignore(syscall.SIGHUP, syscall.SIGINT, syscall.SIGTERM)
}

func currentFakeCodexChildIdentity() fakeCodexChildIdentity {
	pid := os.Getpid()
	pgid, _ := syscall.Getpgid(pid)
	sid, _ := unix.Getsid(pid)

	return fakeCodexChildIdentity{PID: pid, PGID: pgid, SID: sid}
}

func fakeCodexChildIsDetached(path string) bool {
	raw, err := os.ReadFile(path)
	if err != nil {
		return false
	}

	var identity fakeCodexChildIdentity
	if json.Unmarshal(raw, &identity) != nil {
		return false
	}

	return identity.PID > 0 && identity.PID == identity.PGID && identity.PID == identity.SID
}
