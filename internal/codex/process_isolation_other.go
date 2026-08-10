//go:build !unix

package codex

import (
	"errors"
	"os"
	"os/exec"
)

func applyProcessCredential(_ *exec.Cmd, isolation *ProcessIsolation) error {
	if isolation == nil {
		return nil
	}

	return errors.New("process isolation is unsupported on this platform")
}

func closeInheritedOnExec(*os.File) error {
	return errors.New("process isolation is unsupported on this platform")
}
