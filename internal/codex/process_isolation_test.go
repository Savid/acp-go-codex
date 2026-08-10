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
	_, err := buildProcessEnvironment(&ProcessIsolation{UID: 0, GID: 2, BaseEnvironment: map[string]string{}})
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

func TestImplicitProcessEnvironmentIsCapturedAndScrubbed(t *testing.T) {
	original := processEnviron
	t.Cleanup(func() { processEnviron = original })
	processEnviron = func() []string {
		return []string{
			"PATH=/usr/bin:/bin",
			"AMBIENT=present",
			"CODEX_HOME=/ambient/codex",
			"home=/ambient/home",
			"Xdg_Cache_Home=/ambient/cache",
			"XDG_CONFIG_HOME=/ambient/config",
			"XDG_DATA_HOME=/ambient/data",
			"XDG_RUNTIME_DIR=/ambient/runtime",
			"XDG_STATE_HOME=/ambient/state",
			supervisorModeEnv + "=" + supervisorModeGuardian,
			privateAdapterEnvPrefix + "SPOOF=present",
			strings.ToLower(privateAdapterEnvPrefix) + "LOWER=present",
			DarwinRuntimeIDEnv + "=stale",
			DarwinScratchRootEnv + "=/stale",
		}
	}

	env, err := buildProcessEnvironment(nil)
	require.NoError(t, err)
	values := environmentMap(env)
	require.Equal(t, "present", values["AMBIENT"])
	for _, key := range []string{
		"CODEX_HOME",
		"home",
		"Xdg_Cache_Home",
		"XDG_CONFIG_HOME",
		"XDG_DATA_HOME",
		"XDG_RUNTIME_DIR",
		"XDG_STATE_HOME",
		supervisorModeEnv,
		privateAdapterEnvPrefix + "SPOOF",
		strings.ToLower(privateAdapterEnvPrefix) + "LOWER",
		DarwinRuntimeIDEnv,
		DarwinScratchRootEnv,
	} {
		require.NotContains(t, values, key)
	}
}

func TestOrdinaryEnvironmentRestoresOnlyManagedCodexHome(t *testing.T) {
	base := map[string]string{
		"PATH":            "relative-bin:/usr/bin",
		"AMBIENT":         "present",
		"CODEX_HOME":      "/ambient/codex",
		"HOME":            "/ambient/home",
		"XDG_CONFIG_HOME": "/ambient/config",
	}

	env, err := buildProcessEnvironmentFrom(nil, base, map[string]string{
		"CODEX_HOME": "/resolved/codex",
	})
	require.NoError(t, err)
	values := environmentMap(env)
	require.Equal(t, "relative-bin:/usr/bin", values["PATH"])
	require.Equal(t, "present", values["AMBIENT"])
	require.Equal(t, "/resolved/codex", values["CODEX_HOME"])
	require.NotContains(t, values, "HOME")
	require.NotContains(t, values, "XDG_CONFIG_HOME")
}

func TestOrdinaryExecutableLookupSupportsRelativePaths(t *testing.T) {
	root := t.TempDir()
	executable := filepath.Join(root, "codex")
	require.NoError(t, os.WriteFile(executable, []byte("ordinary executable"), 0o700))

	workingDirectory, err := os.Getwd()
	require.NoError(t, err)
	relativeExecutable, err := filepath.Rel(workingDirectory, executable)
	require.NoError(t, err)
	relativeDirectory, err := filepath.Rel(workingDirectory, root)
	require.NoError(t, err)

	resolved, err := resolveOrdinaryProcessExecutable(relativeExecutable, []string{"PATH=/unused"})
	require.NoError(t, err)
	require.Equal(t, executable, resolved)

	resolved, err = resolveOrdinaryProcessExecutable("codex", []string{"PATH=" + relativeDirectory})
	require.NoError(t, err)
	require.Equal(t, executable, resolved)

	_, err = resolveProcessExecutable(relativeExecutable, []string{"PATH=/usr/bin:/bin"})
	require.ErrorContains(t, err, "is not absolute")
	_, err = resolveProcessExecutable("codex", []string{"PATH=" + relativeDirectory})
	require.ErrorContains(t, err, "non-absolute entry")
}

func TestOrdinaryWindowsExecutableLookupUsesPathAndPathExtCaseInsensitively(t *testing.T) {
	originalGOOS := processIsolationGOOS
	t.Cleanup(func() { processIsolationGOOS = originalGOOS })
	processIsolationGOOS = processIsolationWindows

	root := t.TempDir()
	executable := filepath.Join(root, "codex.exe")
	require.NoError(t, os.WriteFile(executable, []byte("ordinary Windows executable"), 0o600))

	resolved, err := resolveOrdinaryProcessExecutable("codex", []string{
		"Path=" + root,
		"PathExt=.EXE;.CMD",
	})
	require.NoError(t, err)
	require.Equal(t, executable, resolved)
}

func TestOrdinaryExecutableLookupFailureBranches(t *testing.T) {
	originalGOOS := processIsolationGOOS
	originalAbs := ordinaryExecutableAbs
	t.Cleanup(func() {
		processIsolationGOOS = originalGOOS
		ordinaryExecutableAbs = originalAbs
	})

	_, err := resolveOrdinaryProcessExecutable(" ", []string{"PATH=/usr/bin"})
	require.ErrorContains(t, err, "empty")
	_, err = resolveOrdinaryProcessExecutable("codex", nil)
	require.ErrorContains(t, err, "PATH is empty")

	root := t.TempDir()
	executable := filepath.Join(root, "codex")
	require.NoError(t, os.WriteFile(executable, []byte("ordinary executable"), 0o700))
	resolved, err := resolveOrdinaryProcessExecutable("codex", []string{"PATH=" + string(os.PathListSeparator) + root})
	require.NoError(t, err)
	require.Equal(t, executable, resolved)

	directory := filepath.Join(root, "directory")
	require.NoError(t, os.Mkdir(directory, 0o700))
	_, err = resolveOrdinaryExecutableCandidate(directory, nil)
	require.ErrorIs(t, err, exec.ErrNotFound)
	nonExecutable := filepath.Join(root, "plain")
	require.NoError(t, os.WriteFile(nonExecutable, []byte("plain"), 0o600))
	_, err = resolveOrdinaryExecutableCandidate(nonExecutable, nil)
	require.ErrorIs(t, err, exec.ErrNotFound)

	blocker := filepath.Join(root, "blocker")
	require.NoError(t, os.WriteFile(blocker, []byte("blocker"), 0o600))
	_, err = resolveOrdinaryProcessExecutable("codex", []string{"PATH=" + blocker})
	require.Error(t, err)
	_, err = resolveOrdinaryProcessExecutable("missing", []string{"PATH=" + root})
	require.ErrorIs(t, err, exec.ErrNotFound)

	absErr := errors.New("absolute path unavailable")
	ordinaryExecutableAbs = func(string) (string, error) { return "", absErr }
	_, err = resolveOrdinaryProcessExecutable("./codex", []string{"PATH=" + root})
	require.ErrorIs(t, err, absErr)
	_, err = resolveOrdinaryProcessExecutable("codex", []string{"PATH=" + root})
	require.ErrorIs(t, err, absErr)
	ordinaryExecutableAbs = originalAbs

	processIsolationGOOS = processIsolationWindows
	require.Empty(t, ordinaryEnvironmentValue(map[string]string{"OTHER": "value"}, "PATH"))
	require.Equal(t, []string{
		ordinaryWindowsExtensionCOM,
		ordinaryWindowsExtensionEXE,
		ordinaryWindowsExtensionBAT,
		ordinaryWindowsExtensionCMD,
	}, ordinaryWindowsExecutableExtensions(""))
	require.Equal(t, []string{
		ordinaryWindowsExtensionEXE,
		ordinaryWindowsExtensionCMD,
	}, ordinaryWindowsExecutableExtensions("EXE;;.CMD"))

	windowsExecutable := filepath.Join(root, "tool.cmd")
	require.NoError(t, os.WriteFile(windowsExecutable, []byte("Windows executable"), 0o600))
	resolved, err = resolveOrdinaryExecutableCandidate(windowsExecutable, []string{"PATHEXT=.CMD"})
	require.NoError(t, err)
	require.Equal(t, windowsExecutable, resolved)
	_, err = resolveOrdinaryExecutableCandidate(filepath.Join(blocker, "tool"), []string{"PATHEXT=.EXE"})
	require.Error(t, err)
	_, err = resolveOrdinaryExecutableCandidate(filepath.Join(root, "missing"), []string{"PATHEXT=.EXE"})
	require.ErrorIs(t, err, exec.ErrNotFound)
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
