//go:build unix

package codex

import (
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestProcessIsolationEnvironmentIsReplacementAndOverlay(t *testing.T) {
	t.Setenv("ACP_PROCESS_AMBIENT_CANARY", "must-not-leak")
	policy := &ProcessIsolation{UID: 123, GID: 456, BaseEnvironment: map[string]string{"PATH": "/usr/bin:/bin", "BASE": "yes", "OVERLAY": "base"}}
	env, err := buildProcessEnvironment(policy, map[string]string{"OVERLAY": "option", "ONLY_OPTION": "yes"})
	require.NoError(t, err)
	values := environmentMap(env)
	require.NotContains(t, values, "ACP_PROCESS_AMBIENT_CANARY")
	require.Equal(t, "yes", values["BASE"])
	require.Equal(t, "option", values["OVERLAY"])
	require.Equal(t, "yes", values["ONLY_OPTION"])
}

func TestProcessIsolationFailsClosedAndClearsGroups(t *testing.T) {
	_, err := buildProcessEnvironment(nil)
	require.ErrorContains(t, err, "required")
	_, err = buildProcessEnvironment(&ProcessIsolation{UID: 0, GID: 2, BaseEnvironment: map[string]string{}})
	require.ErrorContains(t, err, "nonzero")
	_, err = buildProcessEnvironment(&ProcessIsolation{UID: 1, GID: 2, BaseEnvironment: map[string]string{"PATH": "relative"}})
	require.ErrorContains(t, err, "non-absolute")

	cmd := exec.Command("/usr/bin/true")
	policy := &ProcessIsolation{UID: 123, GID: 456, BaseEnvironment: map[string]string{}}
	require.NoError(t, applyProcessCredential(cmd, policy))
	require.Equal(t, uint32(123), cmd.SysProcAttr.Credential.Uid)
	require.Equal(t, uint32(456), cmd.SysProcAttr.Credential.Gid)
	require.Empty(t, cmd.SysProcAttr.Credential.Groups)
}

func TestSupervisorConfigIsInheritedUnlinkedDescriptor(t *testing.T) {
	root := t.TempDir()
	file, err := writeSupervisorConfig(root, supervisorConfig{NativePath: "/usr/bin/true", Home: root, Scratch: root, IsolationUID: 1, IsolationGID: 2})
	require.NoError(t, err)
	t.Cleanup(func() { _ = file.Close() })
	_, err = os.Stat(file.Name())
	require.ErrorIs(t, err, os.ErrNotExist)
	config, err := readSupervisorConfig(file)
	require.NoError(t, err)
	require.Equal(t, filepath.Clean(root), config.Home)
}
