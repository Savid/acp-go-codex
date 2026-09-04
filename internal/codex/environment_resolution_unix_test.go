//go:build !windows

package codex

import (
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
)

// TestResolveOrdinaryProcessExecutableEdges is the posix half of executable
// resolution: an exec bit, a symlink loop, and a PATH search that resolves a
// name with no extension. Windows answers none of those questions, and what it
// does answer instead is pinned by TestWindowsExecutableResolutionEdges, which
// drives the same resolver with its platform switch flipped.
func TestResolveOrdinaryProcessExecutableEdges(t *testing.T) {
	_, err := resolveOrdinaryProcessExecutable(" ", nil)
	require.ErrorContains(t, err, "empty")

	originalAbs := ordinaryExecutableAbs
	t.Cleanup(func() { ordinaryExecutableAbs = originalAbs })
	absErr := errors.New("abs failed")
	ordinaryExecutableAbs = func(string) (string, error) { return "", absErr }
	_, err = resolveOrdinaryProcessExecutable("./codex", nil)
	require.ErrorIs(t, err, absErr)
	_, err = resolveOrdinaryProcessExecutable("codex", []string{"PATH=/bin"})
	require.ErrorIs(t, err, absErr)
	ordinaryExecutableAbs = originalAbs

	_, err = resolveOrdinaryProcessExecutable("codex", []string{"PATH="})
	require.ErrorContains(t, err, "PATH is empty")

	root := t.TempDir()
	executable := filepath.Join(root, "codex")
	require.NoError(t, os.WriteFile(executable, []byte("#!/bin/sh\n"), 0o700))
	resolved, err := resolveOrdinaryProcessExecutable("codex", []string{"PATH=" + root})
	require.NoError(t, err)
	require.Equal(t, executable, resolved)
	resolved, err = resolveOrdinaryProcessExecutable("codex", []string{"PATH=" + string(os.PathListSeparator) + root})
	require.NoError(t, err)
	require.Equal(t, executable, resolved)

	loop := filepath.Join(root, "loop")
	require.NoError(t, os.Symlink(loop, loop))
	_, err = resolveOrdinaryProcessExecutable("loop", []string{"PATH=" + root})
	require.Error(t, err)
	require.NotErrorIs(t, err, os.ErrNotExist)

	nonExecutable := filepath.Join(root, "plain")
	require.NoError(t, os.WriteFile(nonExecutable, []byte("plain"), 0o600))
	_, err = resolveOrdinaryProcessExecutable(nonExecutable, nil)
	require.Error(t, err)
	_, err = resolveOrdinaryProcessExecutable(root, nil)
	require.Error(t, err)
	_, err = resolveOrdinaryProcessExecutable(filepath.Join(root, "missing"), nil)
	require.ErrorIs(t, err, os.ErrNotExist)
	_, err = resolveOrdinaryProcessExecutable("missing", []string{"PATH=" + root})
	require.Error(t, err)
}
