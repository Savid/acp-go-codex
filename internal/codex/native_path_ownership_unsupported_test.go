//go:build !linux

package codex

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func TestNativePathOwnershipOffLinux(t *testing.T) {
	root := t.TempDir()
	explicit := &ProcessIsolation{UID: 1, GID: 1}

	require.NoError(t, handoffGeneratedNativeTree(root, nil))
	require.ErrorContains(t, handoffGeneratedNativeTree(root, explicit), "ownership handoff is unsupported")

	require.NoError(t, validateNativeOwnedDirectory(root, nil))
	require.ErrorContains(t, validateNativeOwnedDirectory(root, explicit), "ownership validation is unsupported")
}

func TestDuplicateAgentIdentityLockIsLinuxOnly(t *testing.T) {
	file, err := duplicateLinuxAgentIdentityLock(nil)
	require.Nil(t, file)
	require.ErrorContains(t, err, "supported only on Linux")
}

func TestBrowserShimHandoffDelegatesToNativeOwnership(t *testing.T) {
	shim := &browserShim{dir: t.TempDir()}

	require.NoError(t, (*browserShim)(nil).handoff(&ProcessIsolation{UID: 1, GID: 1}))
	require.ErrorContains(t, shim.handoff(&ProcessIsolation{UID: 1, GID: 1}),
		"ownership handoff is unsupported")
}
