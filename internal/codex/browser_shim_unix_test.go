//go:build !windows

package codex

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestManagedLoginBrowserShimUsesAuthority(t *testing.T) {
	originalScratch := accountScratchParent
	originalProbe := accountProbeVersion
	t.Cleanup(func() {
		accountScratchParent = originalScratch
		accountProbeVersion = originalProbe
	})
	accountScratchParent = func(string) (string, error) { return t.TempDir(), nil }
	accountProbeVersion = func(context.Context, VersionProbeOptions) (string, error) { return minCodexVersion, nil }

	process := newAuthorityTestProcess("")
	host := &authorityTestHost{
		environment: map[string]string{"PATH": "/host/bin", "HOME": "/host/home"},
		process:     process,
	}
	home := t.TempDir()
	require.NoError(t, RunAccountCommand(t.Context(), AccountCommandOptions{
		CLIPath: "host-codex", CodexHome: home, Mode: accountCommandLogin, HostAuthority: host,
	}))

	require.Len(t, host.requests, 1)
	environment := environmentMap(host.requests[0].Environment)
	shim := filepath.Dir(environment[browserShimBrowserEnv])
	require.True(t, strings.HasPrefix(environment[browserShimPathEnv], shim+string(os.PathListSeparator)))
	require.Contains(t, host.prepared, shim)
	require.Contains(t, host.reclaimed, shim)
	require.NoDirExists(t, shim)
}

func TestManagedLoginRetriesBusyReclaimBeforeShimRemoval(t *testing.T) {
	originalProbe := accountProbeVersion
	originalScratchParent := accountScratchParent
	t.Cleanup(func() {
		accountProbeVersion = originalProbe
		accountScratchParent = originalScratchParent
	})

	accountProbeVersion = func(context.Context, VersionProbeOptions) (string, error) { return minCodexVersion, nil }
	scratch := t.TempDir()
	accountScratchParent = func(string) (string, error) { return scratch, nil }
	home := t.TempDir()
	host := &authorityTestHost{
		environment: map[string]string{"PATH": "/host/bin", "HOME": "/host/home"},
		process:     newAuthorityTestProcess(""),
		reclaimErrs: []error{ErrNativeTreeBusy, nil},
	}

	err := RunAccountCommand(t.Context(), AccountCommandOptions{
		CLIPath: "host-pinned-codex", CodexHome: home, Mode: accountCommandLogin, HostAuthority: host,
	})
	require.ErrorIs(t, err, ErrNativeTreeBusy)
	require.NotErrorIs(t, err, ErrContainmentIncomplete)
	require.Len(t, host.prepared, 2)
	shimDir := host.prepared[1]
	require.DirExists(t, shimDir)

	require.NoError(t, cleanupAccountTrees(host, host.prepared, &browserShim{dir: shimDir}))
	require.NoDirExists(t, shimDir)
}

func TestNewBrowserShimMaterializesNoOpLaunchers(t *testing.T) {
	shim, err := newBrowserShim(t.TempDir())
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, shim.remove()) })

	for _, name := range browserLauncherNames {
		path := filepath.Join(shim.dir, name)
		info, statErr := os.Stat(path)
		require.NoError(t, statErr)
		require.NotZero(t, info.Mode().Perm()&0o100)
		raw, readErr := os.ReadFile(path)
		require.NoError(t, readErr)
		require.Equal(t, browserShimScript, raw)
	}
}

func TestNewBrowserShimFailureBranches(t *testing.T) {
	originalMkdir := browserShimMkdirTemp
	originalWrite := browserShimWriteFile
	t.Cleanup(func() {
		browserShimMkdirTemp = originalMkdir
		browserShimWriteFile = originalWrite
	})

	browserShimMkdirTemp = func(string, string) (string, error) { return "", errors.New("mkdir") }
	_, err := newBrowserShim(t.TempDir())
	require.ErrorContains(t, err, "mkdir")

	dir := t.TempDir()
	browserShimMkdirTemp = func(string, string) (string, error) { return dir, nil }
	browserShimWriteFile = func(string, []byte, os.FileMode) error { return errors.New("write") }
	_, err = newBrowserShim(t.TempDir())
	require.ErrorContains(t, err, "write")
}

func TestBrowserShimEnvironmentAndNilReceiver(t *testing.T) {
	dir := "/shim"
	environment := browserShimEnviron([]string{"PATH=/bin", "BROWSER=real", "KEEP=yes"}, dir)
	values := environmentMap(environment)
	require.Equal(t, dir+string(os.PathListSeparator)+"/bin", values["PATH"])
	require.Equal(t, browserShimCommand(dir), values["BROWSER"])
	require.Equal(t, "yes", values["KEEP"])

	var shim *browserShim
	require.Equal(t, []string{"A=B"}, shim.environ([]string{"A=B"}))
	require.NoError(t, shim.remove())
}
