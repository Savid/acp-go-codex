package codex

import (
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
