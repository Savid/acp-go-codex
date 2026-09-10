package codex

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestEnvironmentCoverageEdges(t *testing.T) {
	require.Error(t, validateEnvironmentMap(nil))

	environment, err := buildProcessEnvironmentFrom(
		map[string]string{"B": "2", "A": "1", privateAdapterEnvPrefix + "SECRET": "x"}, nil, map[string]string{"A": "overlaid"})
	require.NoError(t, err)
	require.Equal(t, []string{"A=overlaid", "B=2"}, environment)

	_, err = buildProcessEnvironmentFrom(map[string]string{}, map[string]string{"BAD=KEY": "value"})
	require.Error(t, err)
}

func TestWindowsExecutableResolutionEdges(t *testing.T) {
	originalProcessGOOS := processGOOS
	processGOOS = platformWindows
	t.Cleanup(func() { processGOOS = originalProcessGOOS })

	originalGOOS := Platform
	Platform = platformWindows
	t.Cleanup(func() { Platform = originalGOOS })

	require.Equal(t, "mixed", ordinaryEnvironmentValue(environmentMap([]string{"Path=mixed"}), "PATH"))
	require.Empty(t, ordinaryEnvironmentValue(map[string]string{}, "PATH"))
	require.Equal(t,
		[]string{ordinaryWindowsExtensionCOM, ordinaryWindowsExtensionEXE, ordinaryWindowsExtensionBAT, ordinaryWindowsExtensionCMD},
		ordinaryWindowsExecutableExtensions(""),
	)
	require.Equal(t, []string{".exe", ".cmd"}, ordinaryWindowsExecutableExtensions("EXE;;.CMD"))

	root := t.TempDir()
	executable := filepath.Join(root, "codex.cmd")
	require.NoError(t, os.WriteFile(executable, []byte("command"), 0o600))
	resolved, err := resolveOrdinaryProcessExecutable("codex", []string{"Path=" + root, "PathExt=.CMD"})
	require.NoError(t, err)
	require.Equal(t, executable, resolved)

	resolved, err = resolveOrdinaryExecutableCandidate(executable, nil)
	require.NoError(t, err)
	require.Equal(t, executable, resolved)
	_, err = resolveOrdinaryExecutableCandidate(root, nil)
	require.Error(t, err)
	loop := filepath.Join(root, "loop")
	require.NoError(t, os.Symlink(loop, loop))
	_, err = resolveOrdinaryExecutableCandidate(loop, nil)
	require.Error(t, err)
	_, err = resolveOrdinaryExecutableCandidate(filepath.Join(root, "missing"), nil)
	require.Error(t, err)
}

func TestWindowsEnvironmentOverlayAndLookupShareOnePath(t *testing.T) {
	originalPlatform, originalGOOS := Platform, processGOOS
	Platform, processGOOS = platformWindows, platformWindows
	t.Cleanup(func() { Platform, processGOOS = originalPlatform, originalGOOS })

	oldRoot, selectedRoot := t.TempDir(), t.TempDir()
	for _, root := range []string{oldRoot, selectedRoot} {
		require.NoError(t, os.WriteFile(filepath.Join(root, "codex.cmd"), []byte("command"), 0o600))
	}
	base := map[string]string{"Path": oldRoot, "PathExt": ".CMD", "Home": "native"}
	overlay := map[string]string{"PATH": selectedRoot, "home": "configured"}
	environment, err := buildProcessEnvironmentFrom(base, overlay)
	require.NoError(t, err)
	require.Equal(t, []string{"HOME=configured", "PATH=" + selectedRoot, "PATHEXT=.CMD"}, environment)
	require.Equal(t, selectedRoot, searchPathFromEnvironment(environment))
	executable, err := resolveOrdinaryProcessExecutable("codex", environment)
	require.NoError(t, err)
	require.Equal(t, filepath.Join(selectedRoot, "codex.cmd"), executable)
	require.Equal(t, oldRoot, base["Path"], "building an environment leaves caller maps unchanged")

	values := environmentMap([]string{"Path=first", "PATH=last"})
	require.Equal(t, map[string]string{"PATH": "last"}, values)
	Platform = "linux"
	values = environmentMap([]string{"Path=first", "PATH=last"})
	require.Equal(t, map[string]string{"Path": "first", "PATH": "last"}, values)
}
