//go:build darwin || freebsd || openbsd

package codex

import (
	"fmt"
	"os/exec"
	"runtime"
	"syscall"
	"time"
)

// Process groups are not an authoritative tree-containment primitive: a
// descendant can call setsid(2) and escape. These platforms must remain
// unavailable until they have a kernel-backed containment and no-child proof.
type guardianContainment struct{}

type livenessContainment struct{}

func (*livenessContainment) DescendantCount() (int, bool) { return 0, false }

func unsupportedContainment() error {
	return fmt.Errorf("Codex runtime containment is unsupported on %s", runtime.GOOS)
}

func newGuardianContainment() (*guardianContainment, error) {
	return nil, unsupportedContainment()
}

func (*guardianContainment) Name() string { return "" }

func (*guardianContainment) Close() error { return nil }

func (*guardianContainment) Quiesce(int, time.Duration) error {
	return unsupportedContainment()
}

func openLivenessContainment(string) (*livenessContainment, error) {
	return nil, unsupportedContainment()
}

func (*livenessContainment) Start(*exec.Cmd) error { return unsupportedContainment() }

func (*livenessContainment) Close() error { return nil }

func (*livenessContainment) Quiesce(int, time.Duration) error {
	return unsupportedContainment()
}

func configureIndependentSupervisor(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
}

func terminateIndependentSupervisor(cmd *exec.Cmd) error {
	return signalProcess(cmd, syscall.SIGKILL)
}
