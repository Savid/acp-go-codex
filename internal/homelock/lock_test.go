package homelock

import (
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestAcquireFailsClosedForSecondClaimAndNeverUnlinksFiles(t *testing.T) {
	home := t.TempDir()
	first, err := Acquire(home)
	require.NoError(t, err)

	_, err = Acquire(home)
	require.Error(t, err)
	require.NoError(t, first.Release())

	for _, name := range []string{ClaimFileName, LivenessFileName} {
		info, statErr := os.Stat(filepath.Join(home, name))
		require.NoError(t, statErr)
		require.Equal(t, hostFilePerm(0o600), info.Mode().Perm())
	}

	second, err := Acquire(home)
	require.NoError(t, err)
	require.NoError(t, second.Release())
}

func TestLockFailureBranches(t *testing.T) {
	require.Error(t, func() error {
		_, err := Acquire("")

		return err
	}())
	require.NoError(t, (*Lock)(nil).Release())

	t.Run("mkdir", func(t *testing.T) {
		restoreLockHooks(t)
		mkdirAll = func(string, os.FileMode) error { return errors.New("mkdir") }
		_, err := AcquireClaim(t.TempDir())
		require.Error(t, err)
	})

	t.Run("open", func(t *testing.T) {
		restoreLockHooks(t)
		openFile = func(string, int, os.FileMode) (*os.File, error) { return nil, errors.New("open") }
		_, err := AcquireClaim(t.TempDir())
		require.Error(t, err)
	})

	t.Run("chmod", func(t *testing.T) {
		restoreLockHooks(t)
		chmodFile = func(*os.File, os.FileMode) error { return errors.New("chmod") }
		_, err := AcquireClaim(t.TempDir())
		require.Error(t, err)
	})

	t.Run("platform lock", func(t *testing.T) {
		restoreLockHooks(t)
		platformLock = func(*os.File) error { return errors.New("lock") }
		_, err := AcquireClaim(t.TempDir())
		require.Error(t, err)
	})

	t.Run("filesystem", func(t *testing.T) {
		restoreLockHooks(t)
		validateFS = func(*os.File) error { return errors.New("filesystem") }
		_, err := AcquireClaim(t.TempDir())
		require.ErrorContains(t, err, "filesystem")
	})

	t.Run("held stat", func(t *testing.T) {
		restoreLockHooks(t)
		statFile = func(*os.File) (os.FileInfo, error) { return nil, errors.New("stat file") }
		_, err := AcquireClaim(t.TempDir())
		require.Error(t, err)
	})

	t.Run("path stat", func(t *testing.T) {
		restoreLockHooks(t)
		statPath = func(string) (os.FileInfo, error) { return nil, errors.New("stat path") }
		_, err := AcquireClaim(t.TempDir())
		require.Error(t, err)
	})

	t.Run("replacement", func(t *testing.T) {
		restoreLockHooks(t)
		sameFile = func(os.FileInfo, os.FileInfo) bool { return false }
		_, err := AcquireClaim(t.TempDir())
		require.Error(t, err)
	})

	t.Run("second lock", func(t *testing.T) {
		restoreLockHooks(t)
		calls := 0
		original := platformLock
		platformLock = func(file *os.File) error {
			calls++
			if calls == 2 {
				return errors.New("liveness")
			}

			return original(file)
		}
		_, err := Acquire(t.TempDir())
		require.Error(t, err)
	})

	// A platform with no lock primitive is unreachable from the platforms this
	// package builds tests for, so the refusal is driven through the hook the
	// build-tagged files install. What is pinned is the contract a host reads:
	// construction fails, it fails with the family sentinel, and it leaves no
	// home directory or lock file behind to suggest a claim nobody holds.
	t.Run("unsupported platform", func(t *testing.T) {
		restoreLockHooks(t)

		home := filepath.Join(t.TempDir(), "home")
		requireLock = func() error { return ErrRuntimeLockUnsupported }

		_, err := Acquire(home)
		require.ErrorIs(t, err, ErrRuntimeLockUnsupported)

		_, err = AcquireClaim(home)
		require.ErrorIs(t, err, ErrRuntimeLockUnsupported)

		_, err = AcquireLiveness(home)
		require.ErrorIs(t, err, ErrRuntimeLockUnsupported)

		require.NoDirExists(t, home)
	})

	t.Run("release", func(t *testing.T) {
		restoreLockHooks(t)
		lock, err := Acquire(t.TempDir())
		require.NoError(t, err)
		platformUnlock = func(*os.File) error { return errors.New("unlock") }
		require.Error(t, lock.Release())
		require.Error(t, lock.Release())
	})
}

func restoreLockHooks(t *testing.T) {
	t.Helper()
	originalMkdirAll := mkdirAll
	originalOpenFile := openFile
	originalStatPath := statPath
	originalStatFile := statFile
	originalChmodFile := chmodFile
	originalSameFile := sameFile
	originalRequireLock := requireLock
	originalPlatformLock := platformLock
	originalPlatformUnlock := platformUnlock
	originalValidateFS := validateFS
	t.Cleanup(func() {
		mkdirAll = originalMkdirAll
		openFile = originalOpenFile
		statPath = originalStatPath
		statFile = originalStatFile
		chmodFile = originalChmodFile
		sameFile = originalSameFile
		requireLock = originalRequireLock
		platformLock = originalPlatformLock
		platformUnlock = originalPlatformUnlock
		validateFS = originalValidateFS
	})
}
