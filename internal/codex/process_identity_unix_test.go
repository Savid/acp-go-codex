//go:build unix && !linux

package codex

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestCurrentProcessIdentityUnix(t *testing.T) {
	effectiveUID := processEffectiveUID
	effectiveGID := processEffectiveGID
	t.Cleanup(func() {
		processEffectiveUID = effectiveUID
		processEffectiveGID = effectiveGID
	})

	uid, gid, err := currentProcessIdentity()
	require.NoError(t, err)
	require.Equal(t, uint32(os.Geteuid()), uid)
	require.Equal(t, uint32(os.Getegid()), gid)

	processEffectiveGID = func() int { return -1 }
	_, _, err = currentProcessIdentity()
	require.ErrorContains(t, err, "unavailable")
}

func TestOrdinarySupervisorIdentityDispositionUnix(t *testing.T) {
	effectiveUID := processEffectiveUID
	effectiveGID := processEffectiveGID
	originalGOOS := processIsolationGOOS
	t.Cleanup(func() {
		processEffectiveUID = effectiveUID
		processEffectiveGID = effectiveGID
		processIsolationGOOS = originalGOOS
	})

	uid, gid, err := currentProcessIdentity()
	require.NoError(t, err)
	require.NoError(t, validateSupervisorIdentityDisposition(supervisorConfig{}))
	require.NoError(t, validateSupervisorIdentityDisposition(supervisorConfig{
		OrdinaryExecution: true,
		IsolationUID:      uid,
		IsolationGID:      gid,
	}))
	require.ErrorContains(t, validateSupervisorIdentityDisposition(supervisorConfig{
		OrdinaryExecution: true,
		IsolationUID:      uid,
		IsolationGID:      gid,
		AuthorityDomain:   true,
	}), "ordinary supervisor identity")

	root := t.TempDir()
	configFile, err := writeSupervisorConfig(root, supervisorConfig{
		NativePath:        "/usr/bin/true",
		Home:              filepath.Join(root, "home"),
		Scratch:           root,
		OrdinaryExecution: true,
		IsolationUID:      uid + 1,
		IsolationGID:      gid,
	})
	require.NoError(t, err)
	require.ErrorContains(t, runSupervisor("unknown", configFile), "ordinary supervisor identity")

	processIsolationGOOS = processIsolationLinux
	processEffectiveGID = func() int { return -1 }
	require.ErrorContains(t, validateSupervisorIdentityDisposition(supervisorConfig{OrdinaryExecution: true}), "unavailable")
	_, _, err = supervisorCommand(context.Background(), supervisorConfig{NativePath: "/usr/bin/true"})
	require.ErrorContains(t, err, "current process identity is unavailable")
}
