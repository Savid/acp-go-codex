//go:build !linux

package codex

import (
	"os"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestNativePathOwnershipOffLinux(t *testing.T) {
	root := t.TempDir()
	current := &ProcessIsolation{UID: uint32(os.Geteuid()), GID: uint32(os.Getegid())}
	foreign := &ProcessIsolation{UID: current.UID + 1, GID: current.GID + 1}

	require.NoError(t, handoffGeneratedNativeTree(root, nil))
	require.NoError(t, handoffGeneratedNativeTree(root, current))
	require.ErrorContains(t, handoffGeneratedNativeTree(root, foreign), "ownership handoff is unsupported")

	require.NoError(t, validateNativeOwnedDirectory(root, nil))
	require.NoError(t, validateNativeOwnedDirectory(root, current))
	require.ErrorContains(t, validateNativeOwnedDirectory(root, foreign), "ownership validation is unsupported")
}

func TestDuplicateAgentIdentityLockIsLinuxOnly(t *testing.T) {
	file, err := duplicateLinuxAgentIdentityLock(nil)
	require.Nil(t, file)
	require.ErrorContains(t, err, "supported only on Linux")
}

func TestBrowserShimHandoffDelegatesToNativeOwnership(t *testing.T) {
	shim := &browserShim{dir: t.TempDir()}

	require.NoError(t, (*browserShim)(nil).handoff(&ProcessIsolation{UID: 1, GID: 1}))
	require.NoError(t, shim.handoff(&ProcessIsolation{UID: uint32(os.Geteuid()), GID: uint32(os.Getegid())}))
	require.ErrorContains(t, shim.handoff(&ProcessIsolation{UID: uint32(os.Geteuid()) + 1, GID: uint32(os.Getegid()) + 1}),
		"ownership handoff is unsupported")
}
