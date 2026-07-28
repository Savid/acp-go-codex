//go:build !windows

package codex

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestLoginNeverExecsABrowserLauncher(t *testing.T) {
	restoreAccountCommandHooks(t)

	marker := filepath.Join(t.TempDir(), "launched")
	probe := t.TempDir()

	for _, name := range browserLauncherNames {
		script := fmt.Sprintf("#!/bin/sh\necho \"$0 $*\" >> %q\nexit 0\n", marker)
		require.NoError(t, os.WriteFile(filepath.Join(probe, name), []byte(script), 0o700))
	}

	t.Setenv("PATH", probe+string(os.PathListSeparator)+os.Getenv("PATH"))

	for _, name := range browserLauncherNames {
		control := exec.Command(name, "https://example.invalid/")
		control.Env = os.Environ()
		require.NoError(t, control.Run())
		require.FileExists(t, marker)
		require.NoError(t, os.Remove(marker))
	}

	cli := writeAccountCommandScript(t, fmt.Sprintf(`#!/bin/sh
if [ "$1" = %q ]; then
  echo codex-cli 0.144.1
  exit 0
fi
for launcher in %s; do
  "$launcher" "https://example.invalid/"
done
exit 0
`, codexVersionArgument, strings.Join(browserLauncherNames, " ")))

	accountSupervisorCommand = func(_ context.Context, config supervisorConfig) (*exec.Cmd, *supervisorProof, error) {
		cmd := exec.Command(config.NativePath, config.NativeArgs...)
		cmd.Env = config.NativeEnv

		return cmd, &supervisorProof{}, nil
	}

	require.NoError(t, RunAccountCommand(context.Background(), AccountCommandOptions{
		CLIPath: cli, CodexHome: t.TempDir(), ScratchDir: t.TempDir(), Mode: accountCommandLogin,
	}))
	require.NoFileExists(t, marker)
}

func TestLogoutRunsWithoutABrowserShim(t *testing.T) {
	restoreAccountCommandHooks(t)
	restoreBrowserShimHooks(t)

	parent := t.TempDir()
	home := t.TempDir()

	browserShimMkdirTemp = func(string, string) (string, error) {
		t.Error("logout built a browser shim")

		return "", errors.New("unreachable")
	}

	var childEnv []string

	accountSupervisorCommand = func(_ context.Context, config supervisorConfig) (*exec.Cmd, *supervisorProof, error) {
		childEnv = config.NativeEnv

		entries, err := os.ReadDir(parent)
		require.NoError(t, err)

		for _, entry := range entries {
			require.False(t, strings.HasPrefix(entry.Name(), browserShimPrefix))
		}

		return exec.Command("/usr/bin/true"), &supervisorProof{}, nil
	}
	accountProbeVersion = func(_ context.Context, options VersionProbeOptions) (string, error) {
		require.Empty(t, options.Env)

		return minCodexVersion, nil
	}

	require.NoError(t, RunAccountCommand(context.Background(), AccountCommandOptions{
		CLIPath: "/usr/bin/true", CodexHome: home, ScratchDir: parent, Mode: accountCommandLogout,
	}))
	require.Equal(t, upsertEnv(os.Environ(), envCodexHome, home), childEnv)
}

func TestRunAccountCommandRefusesWithoutABrowserShim(t *testing.T) {
	restoreAccountCommandHooks(t)
	restoreBrowserShimHooks(t)

	browserShimMkdirTemp = func(string, string) (string, error) { return "", errors.New("shim parent") }
	require.ErrorContains(t, RunAccountCommand(context.Background(), AccountCommandOptions{
		CLIPath:    writeAccountCommandScript(t, "#!/bin/sh\necho codex-cli 0.144.1\n"),
		CodexHome:  t.TempDir(),
		ScratchDir: t.TempDir(),
		Mode:       accountCommandLogin,
	}), "create browser shim directory")
}

func TestNewBrowserShimMaterialisesNoOpLaunchers(t *testing.T) {
	restoreBrowserShimHooks(t)

	parent := t.TempDir()
	shim, err := newBrowserShim(parent)
	require.NoError(t, err)
	require.Equal(t, parent, filepath.Dir(shim.dir))
	require.True(t, strings.HasPrefix(filepath.Base(shim.dir), browserShimPrefix))

	info, err := os.Stat(shim.dir)
	require.NoError(t, err)
	require.Equal(t, os.FileMode(0o700), info.Mode().Perm())

	for _, name := range browserLauncherNames {
		launcher := filepath.Join(shim.dir, name)
		require.FileExists(t, launcher)

		cmd := exec.Command(launcher, "https://example.invalid/")
		cmd.Env = os.Environ()
		require.NoError(t, cmd.Run())
	}

	require.NoError(t, shim.remove())
	require.NoDirExists(t, shim.dir)
}

func TestNewBrowserShimFailureBranches(t *testing.T) {
	restoreBrowserShimHooks(t)

	browserShimMkdirTemp = func(string, string) (string, error) { return "", errors.New("mkdir") }
	shim, err := newBrowserShim(t.TempDir())
	require.Nil(t, shim)
	require.ErrorContains(t, err, "create browser shim directory")

	browserShimMkdirTemp = os.MkdirTemp

	var written string

	browserShimWriteFile = func(path string, _ []byte, _ os.FileMode) error {
		written = path

		return errors.New("write")
	}
	shim, err = newBrowserShim(t.TempDir())
	require.Nil(t, shim)
	require.ErrorContains(t, err, "write browser shim "+browserLauncherNames[0])
	require.NoDirExists(t, filepath.Dir(written))
}

func TestBrowserShimEnviron(t *testing.T) {
	shim := &browserShim{dir: "/scratch/shim"}

	require.Equal(t, []string{
		"malformed",
		"CODEX_HOME=/home",
		"PATH=/scratch/shim",
		"BROWSER=/scratch/shim/open",
	}, shim.environ([]string{"malformed", "CODEX_HOME=/home", "BROWSER=/usr/bin/open"}))

	require.Equal(t, []string{
		"PATH=/scratch/shim" + string(os.PathListSeparator) + "/usr/bin",
		"BROWSER=/scratch/shim/open",
	}, shim.environ([]string{"PATH=/usr/bin"}))
}

func TestBrowserShimRemoveNilReceiver(t *testing.T) {
	require.NoError(t, (*browserShim)(nil).remove())
}

func restoreBrowserShimHooks(t *testing.T) {
	t.Helper()

	mkdirTemp := browserShimMkdirTemp
	writeFile := browserShimWriteFile

	t.Cleanup(func() {
		browserShimMkdirTemp = mkdirTemp
		browserShimWriteFile = writeFile
	})
}
