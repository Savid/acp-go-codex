package codexacp

import (
	"os"
	"os/exec"
	"os/signal"
	"syscall"
)

type fakeCodexChildIdentity struct {
	PID  int `json:"pid"`
	PGID int `json:"pgid"`
	SID  int `json:"sid"`
}

func newFakeCodexDelayedChildCommand() *exec.Cmd {
	return exec.Command(os.Args[0], fakeCodexDelayedChildArg) // #nosec G204 -- fixed test binary and argument.
}

func ignoreFakeCodexChildTerminationSignals() {
	signal.Ignore(syscall.SIGHUP, syscall.SIGINT, syscall.SIGTERM)
}

func currentFakeCodexChildIdentity() fakeCodexChildIdentity {
	pid := os.Getpid()
	pgid, _ := syscall.Getpgid(pid)

	return fakeCodexChildIdentity{PID: pid, PGID: pgid, SID: pgid}
}

// runtimeGenerationSnapshot reads the shared app-server generation a test wants
// to prove survived, or did not. The epoch is what distinguishes a generation
// that kept serving from a replacement started after one was fenced.
type runtimeGeneration struct {
	epoch uint64
	dead  bool
}

func (a *Agent) runtimeGenerationSnapshot() runtimeGeneration {
	a.mu.Lock()
	defer a.mu.Unlock()

	return runtimeGeneration{epoch: a.runtimeEpoch, dead: a.runtimeDead}
}
