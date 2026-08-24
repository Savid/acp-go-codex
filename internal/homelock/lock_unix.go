//go:build linux || darwin || freebsd || openbsd

package homelock

import (
	"os"

	"golang.org/x/sys/unix"
)

func lockFile(file *os.File) error {
	return unix.Flock(int(file.Fd()), unix.LOCK_EX|unix.LOCK_NB)
}

func unlockFile(file *os.File) error {
	return unix.Flock(int(file.Fd()), unix.LOCK_UN)
}

// requireLockPrimitive reports that this platform carries the lock primitive.
func requireLockPrimitive() error { return nil }
