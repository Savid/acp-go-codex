//go:build windows

package homelock

import (
	"os"
	"syscall"

	"golang.org/x/sys/windows"
)

func lockFile(file *os.File) error {
	overlapped := new(windows.Overlapped)
	return windows.LockFileEx(
		windows.Handle(file.Fd()),
		windows.LOCKFILE_EXCLUSIVE_LOCK|windows.LOCKFILE_FAIL_IMMEDIATELY,
		0,
		1,
		0,
		overlapped,
	)
}

func unlockFile(file *os.File) error {
	overlapped := new(windows.Overlapped)
	return windows.UnlockFileEx(windows.Handle(syscall.Handle(file.Fd())), 0, 1, 0, overlapped)
}

// requireLockPrimitive reports that this platform carries the lock primitive.
func requireLockPrimitive() error { return nil }
