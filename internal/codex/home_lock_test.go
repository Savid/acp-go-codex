package codex

import (
	"syscall"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestHomeHasOneWriter(t *testing.T) {
	t.Parallel()
	home := t.TempDir()
	first, err := LockHome(home)
	require.NoError(t, err)
	t.Cleanup(func() { _ = first.Close() })
	second, err := LockHome(home)
	require.Error(t, err)
	require.Nil(t, second)
	require.NoError(t, first.Close())
	second, err = LockHome(home)
	require.NoError(t, err)
	require.NoError(t, second.Close())
}

func TestHomeUnlockReleasesInheritedDescriptor(t *testing.T) {
	t.Parallel()
	home := t.TempDir()
	first, err := LockHome(home)
	require.NoError(t, err)
	t.Cleanup(func() { _ = first.Close() })
	inherited, err := syscall.Dup(int(first.file.Fd()))
	require.NoError(t, err)
	t.Cleanup(func() { _ = syscall.Close(inherited) })
	require.NoError(t, first.Close())
	second, err := LockHome(home)
	require.NoError(t, err)
	require.NoError(t, second.Close())
}
