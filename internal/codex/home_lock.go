package codex

import (
	"fmt"
	"os"
	"path/filepath"
	"syscall"
)

// LockHome holds the adapter's single-writer lock until the returned file is
// closed. The lock file stays in place so every contender uses the same inode.
func LockHome(home string) (*os.File, error) {
	if err := os.MkdirAll(home, 0o700); err != nil {
		return nil, fmt.Errorf("create Codex home: %w", err)
	}

	file, err := os.OpenFile(filepath.Join(home, ".acp-go-codex.lock"), os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, fmt.Errorf("open Codex home lock: %w", err)
	}

	if err := syscall.Flock(int(file.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		_ = file.Close()

		return nil, fmt.Errorf("codex home is already in use: %w", err)
	}

	return file, nil
}
