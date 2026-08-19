//go:build !linux && !darwin && !freebsd && !openbsd && !windows

package homelock

import (
	"fmt"
	"os"
	"runtime"
)

// requireLockPrimitive refuses construction on a platform with no lock
// primitive. The refusal carries ErrRuntimeLockUnsupported so a host classifies
// it the same way on every such platform.
func requireLockPrimitive() error {
	return fmt.Errorf("%w: %s", ErrRuntimeLockUnsupported, runtime.GOOS)
}

func lockFile(*os.File) error {
	return requireLockPrimitive()
}

func unlockFile(*os.File) error { return nil }
