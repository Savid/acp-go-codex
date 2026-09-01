//go:build !windows

package codex

import (
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
)

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
