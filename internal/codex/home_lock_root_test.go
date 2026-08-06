package codex

import (
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestHomeLockRootDerivesAProtectedRoot(t *testing.T) {
	parent := t.TempDir()
	home := t.TempDir()

	root, err := HomeLockRoot(parent, home)
	require.NoError(t, err)
	require.Equal(t, filepath.Join(parent, "acp-go-codex-home-locks"), filepath.Dir(root))

	info, err := os.Stat(root)
	require.NoError(t, err)
	require.Equal(t, os.FileMode(0o700), info.Mode().Perm())

	again, err := HomeLockRoot(parent, home)
	require.NoError(t, err)
	require.Equal(t, root, again)
}

func TestHomeLockRootFailureBranches(t *testing.T) {
	_, err := HomeLockRoot("", t.TempDir())
	require.ErrorContains(t, err, "scratch parent is required")

	original := homeLockAbsolutePath
	t.Cleanup(func() { homeLockAbsolutePath = original })

	homeLockAbsolutePath = func(string) (string, error) { return "", errors.New("no working directory") }
	_, err = HomeLockRoot(t.TempDir(), "relative-home")
	require.ErrorContains(t, err, "resolve codex writable home for locking")

	homeLockAbsolutePath = original

	parent := t.TempDir()
	blocked := filepath.Join(parent, "acp-go-codex-home-locks")
	require.NoError(t, os.WriteFile(blocked, []byte("x"), 0o600))
	_, err = HomeLockRoot(parent, t.TempDir())
	require.ErrorContains(t, err, "create codex trusted home-lock root")

	// The protection step must fail closed: a root whose mode cannot be
	// asserted is not handed out as trusted. The kernel never refuses a chmod
	// from the owner of a directory it just created, so the fault arrives
	// through the seam.
	previousChmod := homeLockChmod
	t.Cleanup(func() { homeLockChmod = previousChmod })

	homeLockChmod = func(string, os.FileMode) error { return errors.New("mode lost") }
	root, err := HomeLockRoot(t.TempDir(), t.TempDir())
	require.ErrorContains(t, err, "protect codex trusted home-lock root")
	require.Empty(t, root, "an unprotected root must not be handed out")
}
