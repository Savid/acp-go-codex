//go:build !linux

package codexacp

import (
	"os"
	"os/exec"
)

type fakeCodexChildIdentity struct {
	PID int `json:"pid"`
}

func newFakeCodexDelayedChildCommand() *exec.Cmd {
	return exec.Command(os.Args[0], fakeCodexDelayedChildArg) // #nosec G204 -- fixed test binary and fixed argument.
}

func ignoreFakeCodexChildTerminationSignals() {}

func currentFakeCodexChildIdentity() fakeCodexChildIdentity {
	return fakeCodexChildIdentity{PID: os.Getpid()}
}

func fakeCodexChildIsDetached(path string) bool {
	_, err := os.Stat(path)

	return err == nil
}
