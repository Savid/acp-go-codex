//go:build !linux

package codex

import (
	"fmt"
	"runtime"
)

func handoffGeneratedNativeTree(_ string, isolation *ProcessIsolation) error {
	if isolation == nil {
		return nil
	}

	return fmt.Errorf("native path ownership handoff is unsupported on %s", runtime.GOOS)
}

func validateNativeOwnedDirectory(_ string, isolation *ProcessIsolation) error {
	if isolation == nil {
		return nil
	}

	return fmt.Errorf("native path ownership validation is unsupported on %s", runtime.GOOS)
}
