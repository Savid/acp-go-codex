package codex

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestEnvironmentCoverageEdges(t *testing.T) {
	require.Error(t, validateEnvironmentMap(nil))
	require.Nil(t, cloneEnvironment(nil))

	originalEnviron := processEnviron
	processEnviron = func() []string { return []string{"B=2", "A=1", privateAdapterEnvPrefix + "SECRET=x"} }
	t.Cleanup(func() { processEnviron = originalEnviron })

	environment, err := buildProcessEnvironmentFrom(nil, nil, map[string]string{"A": "overlaid"})
	require.NoError(t, err)
	require.Equal(t, []string{"A=overlaid", "B=2"}, environment)

	_, err = buildProcessEnvironmentFrom(map[string]string{}, map[string]string{"BAD=KEY": "value"})
	require.Error(t, err)
}

func TestWindowsExecutableResolutionEdges(t *testing.T) {
	originalGOOS := processGOOS
	processGOOS = platformWindows
	t.Cleanup(func() { processGOOS = originalGOOS })

	require.Equal(t, "mixed", ordinaryEnvironmentValue(map[string]string{"Path": "mixed"}, "PATH"))
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
