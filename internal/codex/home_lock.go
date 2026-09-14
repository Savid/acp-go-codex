package codex

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"syscall"
)

// HomeLock owns an exclusive native home lock.
type HomeLock struct {
	file *os.File
	once sync.Once
	err  error
}

// Close releases the lock explicitly before closing its descriptor. A child
// between fork and exec may still hold an inherited descriptor to this file.
func (l *HomeLock) Close() error {
	l.once.Do(func() {
		l.err = errors.Join(syscall.Flock(int(l.file.Fd()), syscall.LOCK_UN), l.file.Close())
	})

	return l.err
}

// LockHome holds the adapter's single-writer lock until the returned lock is
// closed. The lock file stays in place so every contender uses the same inode.
func LockHome(home string) (*HomeLock, error) {
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

	return &HomeLock{file: file}, nil
}
