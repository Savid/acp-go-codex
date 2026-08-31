//go:build !linux

package codexacp

import (
	"runtime"
	"testing"

	"github.com/stretchr/testify/require"
)

// Off Linux every explicit isolation policy is refused. Ordinary current-
// identity execution reaches these helpers only with a nil policy.
func TestNativePathOwnershipOffLinux(t *testing.T) {
	root := t.TempDir()
	explicit := ProcessIsolation{UID: 1, GID: 1}

	require.NoError(t, handoffGeneratedNativeTree(root, nil))
	require.NoError(t, validateNativeOwnedDirectory(root, nil))

	require.ErrorContains(t, handoffGeneratedNativeTree(root, &explicit), "handoff is unsupported on "+runtime.GOOS)
	require.ErrorContains(t, validateNativeOwnedDirectory(root, &explicit), "validation is unsupported on "+runtime.GOOS)
}
