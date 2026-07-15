package homelock

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
)

const (
	ClaimFileName    = "acp-go-codex-runtime.claim.lock"
	LivenessFileName = "acp-go-codex-runtime.liveness.lock"
)

var (
	mkdirAll       = os.MkdirAll
	openFile       = os.OpenFile
	statPath       = os.Stat
	statFile       = func(file *os.File) (os.FileInfo, error) { return file.Stat() }
	chmodFile      = func(file *os.File, mode os.FileMode) error { return file.Chmod(mode) }
	sameFile       = os.SameFile
	platformLock   = lockFile
	platformUnlock = unlockFile
	validateFS     = validateLockFilesystem
)

// Lock owns one or more never-unlinked writable-home lock files. Supervisor
// processes deliberately acquire claim and liveness independently so neither
// descriptor is inherited by the native process tree.
type Lock struct {
	files []*os.File
	once  sync.Once
	err   error
}

func Acquire(home string) (*Lock, error) {
	claim, err := AcquireClaim(home)
	if err != nil {
		return nil, err
	}

	liveness, err := AcquireLiveness(home)
	if err != nil {
		_ = claim.Release()

		return nil, err
	}

	return &Lock{files: append(claim.files, liveness.files...)}, nil
}

// AcquireClaim takes the primary guardian's claim lock.
func AcquireClaim(home string) (*Lock, error) {
	return acquire(home, ClaimFileName, "claim Codex writable home")
}

// AcquireLiveness takes the native-tree supervisor's independent liveness
// lock. It is held until that supervisor has reaped the native root and proved
// the contained tree quiescent.
func AcquireLiveness(home string) (*Lock, error) {
	return acquire(home, LivenessFileName, "claim Codex home liveness")
}

func acquire(home string, name string, action string) (*Lock, error) {
	if home == "" {
		return nil, errors.New("codex writable home is required for runtime exclusivity")
	}

	if err := mkdirAll(home, 0o700); err != nil {
		return nil, fmt.Errorf("create Codex home for runtime lock: %w", err)
	}

	path := filepath.Join(home, name)

	file, err := openLockFile(path)
	if err != nil {
		return nil, err
	}

	if err := validateFS(file); err != nil {
		_ = file.Close()

		return nil, fmt.Errorf("validate Codex writable-home filesystem: %w", err)
	}

	if err := platformLock(file); err != nil {
		_ = file.Close()

		return nil, fmt.Errorf("%s: %w", action, err)
	}

	if err := verifyLockedPath(file, path); err != nil {
		_ = platformUnlock(file)
		_ = file.Close()

		return nil, err
	}

	return &Lock{files: []*os.File{file}}, nil
}

func verifyLockedPath(file *os.File, path string) error {
	held, err := statFile(file)
	if err != nil {
		return fmt.Errorf("stat held runtime lock: %w", err)
	}

	current, err := statPath(path)
	if err != nil {
		return fmt.Errorf("stat runtime lock path: %w", err)
	}

	if !sameFile(held, current) {
		return fmt.Errorf("runtime lock path changed during acquisition")
	}

	return nil
}

func openLockFile(path string) (*os.File, error) {
	file, err := openFile(path, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, fmt.Errorf("open runtime lock %s: %w", filepath.Base(path), err)
	}

	if err := chmodFile(file, 0o600); err != nil {
		_ = file.Close()

		return nil, fmt.Errorf("chmod runtime lock %s: %w", filepath.Base(path), err)
	}

	return file, nil
}

func (l *Lock) Release() error {
	if l == nil {
		return nil
	}

	l.once.Do(func() {
		for index := len(l.files) - 1; index >= 0; index-- {
			file := l.files[index]
			l.err = errors.Join(l.err, platformUnlock(file), file.Close())
		}
	})

	return l.err
}
