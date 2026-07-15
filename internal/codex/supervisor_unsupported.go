//go:build !linux && !darwin && !freebsd && !openbsd && !windows

package codex

import (
	"fmt"
	"os/exec"
	"runtime"
	"time"
)

type guardianContainment struct{}
type livenessContainment struct{}

func (*livenessContainment) DescendantCount() (int, bool) { return 0, false }

func unsupportedContainment() error {
	return fmt.Errorf("Codex runtime containment is unsupported on %s", runtime.GOOS)
}

func newGuardianContainment() (*guardianContainment, error) { return nil, unsupportedContainment() }
func (*guardianContainment) Name() string                   { return "" }
func (*guardianContainment) Close() error                   { return nil }
func (*guardianContainment) Quiesce(int, time.Duration) error {
	return unsupportedContainment()
}
func openLivenessContainment(string) (*livenessContainment, error) {
	return nil, unsupportedContainment()
}
func (*livenessContainment) Start(*exec.Cmd) error { return unsupportedContainment() }
func (*livenessContainment) Close() error          { return nil }
func (*livenessContainment) Quiesce(int, time.Duration) error {
	return unsupportedContainment()
}
func configureIndependentSupervisor(*exec.Cmd) {}
func terminateIndependentSupervisor(*exec.Cmd) error {
	return unsupportedContainment()
}
