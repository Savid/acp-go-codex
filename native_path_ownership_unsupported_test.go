//go:build !linux

package codexacp

import (
	"os"
	"runtime"
	"testing"

	"github.com/stretchr/testify/require"
)

// Off Linux the adapter cannot prove or transfer native inode ownership, so a
// distinct native identity must be refused rather than launched unprotected.
// An identity equal to the calling process needs no transfer and is accepted.
func TestNativePathOwnershipOffLinux(t *testing.T) {
	root := t.TempDir()
	caller := ProcessIsolation{UID: uint32(os.Geteuid()), GID: uint32(os.Getegid())}
	distinct := ProcessIsolation{UID: caller.UID + 1, GID: caller.GID + 1}

	require.NoError(t, handoffGeneratedNativeTree(root, nil))
	require.NoError(t, validateNativeOwnedDirectory(root, nil))

	require.NoError(t, handoffGeneratedNativeTree(root, &caller))
	require.NoError(t, validateNativeOwnedDirectory(root, &caller))

	require.ErrorContains(t, handoffGeneratedNativeTree(root, &distinct), "handoff is unsupported on "+runtime.GOOS)
	require.ErrorContains(t, validateNativeOwnedDirectory(root, &distinct), "validation is unsupported on "+runtime.GOOS)

	// A half-matching identity is still a distinct identity.
	require.Error(t, handoffGeneratedNativeTree(root, &ProcessIsolation{UID: caller.UID, GID: caller.GID + 1}))
	require.Error(t, validateNativeOwnedDirectory(root, &ProcessIsolation{UID: caller.UID + 1, GID: caller.GID}))
}
