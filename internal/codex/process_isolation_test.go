//go:build unix

package codex

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

type testProcessIdentityCapability struct{}

func (testProcessIdentityCapability) Duplicate() (*os.File, error) {
	return nil, errors.New("test capability cannot be duplicated")
}

func TestProcessIdentityDispositionValidation(t *testing.T) {
	capability := testProcessIdentityCapability{}
	validStandalone := ProcessIsolation{StandaloneOwnerID: "deployment-1", StandaloneStateRoot: "/var/lib/acp-go-codex"}
	validBorrowed := ProcessIsolation{IdentityLock: capability, AuthorityDomain: capability}

	for name, isolation := range map[string]ProcessIsolation{
		"standalone":             validStandalone,
		"borrowed":               validBorrowed,
		"mixed capabilities":     {IdentityLock: capability},
		"borrowed owner":         {IdentityLock: capability, AuthorityDomain: capability, StandaloneOwnerID: "deployment-1"},
		"missing owner":          {StandaloneStateRoot: "/var/lib/acp-go-codex"},
		"invalid owner prefix":   {StandaloneOwnerID: "-deployment", StandaloneStateRoot: "/var/lib/acp-go-codex"},
		"invalid owner byte":     {StandaloneOwnerID: "deployment 1", StandaloneStateRoot: "/var/lib/acp-go-codex"},
		"long owner":             {StandaloneOwnerID: "a" + strings.Repeat("b", 256), StandaloneStateRoot: "/var/lib/acp-go-codex"},
		"relative root":          {StandaloneOwnerID: "deployment-1", StandaloneStateRoot: "relative"},
		"filesystem root":        {StandaloneOwnerID: "deployment-1", StandaloneStateRoot: "/"},
		"authority root":         {StandaloneOwnerID: "deployment-1", StandaloneStateRoot: "/var/lib/acp-go/agent-identities"},
		"beneath authority root": {StandaloneOwnerID: "deployment-1", StandaloneStateRoot: "/var/lib/acp-go/agent-identities/provider"},
		"control in root":        {StandaloneOwnerID: "deployment-1", StandaloneStateRoot: "/var/lib/provider\u0085"},
		"invalid utf8 in root":   {StandaloneOwnerID: "deployment-1", StandaloneStateRoot: string([]byte{'/', 0xff})},
	} {
		t.Run(name, func(t *testing.T) {
			err := validateStandaloneIdentityDisposition(&isolation)
			if name == "standalone" || name == "borrowed" {
				require.NoError(t, err)
			} else {
				require.Error(t, err)
			}
		})
	}
}

func TestProcessIsolationEnvironmentIsReplacementAndOverlay(t *testing.T) {
	t.Setenv("ACP_PROCESS_AMBIENT_CANARY", "must-not-leak")
	policy := &ProcessIsolation{UID: 123, GID: 456, BaseEnvironment: map[string]string{"PATH": "/usr/bin:/bin", "BASE": "yes", "OVERLAY": "base"}, StandaloneOwnerID: "test-owner", StandaloneStateRoot: "/var/lib/acp-go-test"}
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
	_, err = buildProcessEnvironment(&ProcessIsolation{UID: 1, GID: 2, BaseEnvironment: map[string]string{"PATH": "relative"}, StandaloneOwnerID: "test-owner", StandaloneStateRoot: "/var/lib/acp-go-test"})
	require.ErrorContains(t, err, "non-absolute")

	cmd := exec.Command("/usr/bin/true")
	policy := &ProcessIsolation{UID: 123, GID: 456, BaseEnvironment: map[string]string{}, StandaloneOwnerID: "test-owner", StandaloneStateRoot: "/var/lib/acp-go-test"}
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
