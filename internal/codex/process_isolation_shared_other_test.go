//go:build !linux

package codex

import (
	"testing"

	"github.com/stretchr/testify/require"
)

// TestSharedProcessIdentityBelongsToTheLinuxSupervisor proves no other platform
// selects the arm. Only Linux descends from a trusted root identity to the
// native one, so only Linux has a descent to skip; anywhere else the launch
// keeps whatever boundary that platform states for itself, even when the
// identity it is handed is the one the process already runs as.
func TestSharedProcessIdentityBelongsToTheLinuxSupervisor(t *testing.T) {
	isolation := &ProcessIsolation{UID: 1000, GID: 1000}

	require.False(t, sharedProcessIdentity(isolation))
	require.False(t, sharedSupervisorIdentity(1000))

	shared, err := sharedProcessCredential(isolation)
	require.NoError(t, err)
	require.False(t, shared)

	require.NoError(t, validateSupervisorIdentityDisposition(supervisorConfig{IsolationUID: 1000, SharedIdentity: true}))
}
