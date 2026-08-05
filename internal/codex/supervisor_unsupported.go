//go:build !linux && !darwin && !freebsd && !openbsd && !netbsd

package codex

import (
	"fmt"
	"os/exec"
	"runtime"
	"time"
)

type guardianContainment struct{}
type livenessContainment struct {
	beforeStart func() error
}

func (*livenessContainment) DescendantCount() (int, bool) { return 0, false }

func unsupportedContainment() error {
	return fmt.Errorf("codex runtime containment is unsupported on %s", runtime.GOOS)
}

func newGuardianContainment(supervisorConfig) (*guardianContainment, error) {
	return nil, unsupportedContainment()
}
func (*guardianContainment) Name() string { return "" }
func (*guardianContainment) Close() error { return nil }
func (*guardianContainment) Quiesce(int, time.Duration) error {
	return unsupportedContainment()
}
func openLivenessContainment(supervisorConfig) (*livenessContainment, error) {
	return nil, unsupportedContainment()
}
func (*livenessContainment) Start(*exec.Cmd) error { return unsupportedContainment() }
func (*livenessContainment) Wait() <-chan error {
	result := make(chan error, 1)
	result <- unsupportedContainment()

	return result
}
func (*livenessContainment) Close() error { return nil }
func (*livenessContainment) Quiesce(int, time.Duration) error {
	return unsupportedContainment()
}
func configureIndependentSupervisor(*exec.Cmd) {}

func startIndependentSupervisor(*exec.Cmd) error { return unsupportedContainment() }
func terminateIndependentSupervisor(*exec.Cmd) error {
	return unsupportedContainment()
}
