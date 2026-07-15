//go:build !linux && !darwin && !freebsd && !openbsd && !windows

package homelock

import (
	"fmt"
	"os"
	"runtime"
)

func lockFile(*os.File) error {
	return fmt.Errorf("Codex writable-home locking is unsupported on %s", runtime.GOOS)
}

func unlockFile(*os.File) error { return nil }
