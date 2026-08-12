//go:build unix

package codex

import (
	"context"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
)

type packagedCodexFixture struct {
	executable  string
	packagePath string
	root        string
}

func newPackagedCodexFixture(t *testing.T) packagedCodexFixture {
	t.Helper()

	root := t.TempDir()
	bin := filepath.Join(root, "bin")
	packagePath := filepath.Join(root, codexPackagePathDir)
	require.NoError(t, os.MkdirAll(bin, 0o755))
	require.NoError(t, os.MkdirAll(packagePath, 0o755))
	require.NoError(t, os.MkdirAll(filepath.Join(root, codexPackageResources), 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(root, codexPackageMetadata), []byte("fixture\n"), 0o600))

	executable := filepath.Join(bin, "codex")
	require.NoError(t, os.WriteFile(executable, []byte("#!/bin/sh\ncat\n"), 0o700))
	require.NoError(t, os.WriteFile(filepath.Join(bin, codexCodeModeHost), []byte("host\n"), 0o700))
	require.NoError(t, os.WriteFile(filepath.Join(packagePath, "rg"), []byte("rg\n"), 0o700))

	return packagedCodexFixture{executable: executable, packagePath: packagePath, root: root}
}

func preservePackagePathHooks(t *testing.T) {
	t.Helper()

	copyFile := stagePackagedCodexCopy
	chmod := stagePackagedCodexChmod
	link := stagePackagedCodexLink
	mkdir := stagePackagedCodexMkdir
	readDir := stagePackagedCodexReadDir
	t.Cleanup(func() {
		stagePackagedCodexCopy = copyFile
		stagePackagedCodexChmod = chmod
		stagePackagedCodexLink = link
		stagePackagedCodexMkdir = mkdir
		stagePackagedCodexReadDir = readDir
	})
}

func fixedPackageStage(t *testing.T, scratch string) string {
	t.Helper()

	preservePackagePathHooks(t)
	stageRoot := filepath.Join(scratch, "codex-package-fixed")
	require.NoError(t, os.MkdirAll(stageRoot, 0o700))
	stagePackagedCodexMkdir = func(string, string) (string, error) {
		return stageRoot, nil
	}

	return stageRoot
}

func TestStagePackagedCodexRemovesPrivatePathPrepend(t *testing.T) {
	fixture := newPackagedCodexFixture(t)
	invocation := filepath.Join(t.TempDir(), "codex")
	require.NoError(t, os.Symlink(fixture.executable, invocation))
	scratch := t.TempDir()

	staged, env, err := stagePackagedCodex(invocation, []string{"Z=value", "PATH=/native/first:/native/second"}, scratch)
	require.NoError(t, err)

	stageBin := filepath.Dir(staged)
	stageRoot := filepath.Dir(stageBin)
	require.Equal(t, filepath.Join(stageBin, "codex"), staged)
	require.Equal(t, stageBin+string(os.PathListSeparator)+"/native/first:/native/second", environmentMap(env)[pathEnvKey])
	require.Equal(t, "value", environmentMap(env)["Z"])
	metadata, err := os.ReadFile(filepath.Join(stageRoot, codexPackageMetadata))
	require.NoError(t, err)
	require.Equal(t, "{}\n", string(metadata))
	resourcesInfo, err := os.Stat(filepath.Join(stageRoot, codexPackageResources))
	require.NoError(t, err)
	require.True(t, resourcesInfo.IsDir())
	require.FileExists(t, filepath.Join(stageBin, codexCodeModeHost))
	require.FileExists(t, filepath.Join(stageBin, "rg"))
	require.NoDirExists(t, filepath.Join(stageRoot, codexPackagePathDir))

	sourceInfo, err := os.Stat(fixture.executable)
	require.NoError(t, err)
	stageInfo, err := os.Stat(staged)
	require.NoError(t, err)
	require.True(t, os.SameFile(sourceInfo, stageInfo))
	require.Equal(t, os.FileMode(0o755), resourcesInfo.Mode().Perm())
}

func TestStagePackagedCodexAllowsRelaunchInSameScratch(t *testing.T) {
	fixture := newPackagedCodexFixture(t)
	scratch := t.TempDir()

	first, _, err := stagePackagedCodex(fixture.executable, []string{"PATH=/native"}, scratch)
	require.NoError(t, err)
	second, _, err := stagePackagedCodex(fixture.executable, []string{"PATH=/native"}, scratch)
	require.NoError(t, err)
	require.NotEqual(t, filepath.Dir(filepath.Dir(first)), filepath.Dir(filepath.Dir(second)))
	require.FileExists(t, first)
	require.FileExists(t, second)
	require.NoDirExists(t, filepath.Join(filepath.Dir(filepath.Dir(first)), codexPackagePathDir))
	require.NoDirExists(t, filepath.Join(filepath.Dir(filepath.Dir(second)), codexPackagePathDir))
}

func TestStagePackagedCodexLeavesOrdinaryExecutableUnchanged(t *testing.T) {
	executable := filepath.Join(t.TempDir(), "codex")
	require.NoError(t, os.WriteFile(executable, []byte("ordinary\n"), 0o700))
	env := []string{"PATH=/native"}

	staged, gotEnv, err := stagePackagedCodex(executable, env, "")
	require.NoError(t, err)
	require.Equal(t, executable, staged)
	require.Equal(t, env, gotEnv)
}

func TestStagePackagedCodexErrors(t *testing.T) {
	t.Run("resolve executable", func(t *testing.T) {
		_, _, err := stagePackagedCodex(filepath.Join(t.TempDir(), "missing"), nil, t.TempDir())
		require.ErrorContains(t, err, "resolve packaged Codex executable")
	})

	t.Run("missing scratch", func(t *testing.T) {
		fixture := newPackagedCodexFixture(t)
		_, _, err := stagePackagedCodex(fixture.executable, nil, "")
		require.ErrorContains(t, err, "runtime scratch is required")
	})

	t.Run("create stage", func(t *testing.T) {
		fixture := newPackagedCodexFixture(t)
		scratch := filepath.Join(t.TempDir(), "scratch")
		require.NoError(t, os.WriteFile(scratch, []byte("blocked\n"), 0o600))
		_, _, err := stagePackagedCodex(fixture.executable, nil, scratch)
		require.ErrorContains(t, err, "create packaged Codex stage")
	})

	t.Run("make stage traversable", func(t *testing.T) {
		preservePackagePathHooks(t)
		fixture := newPackagedCodexFixture(t)
		stagePackagedCodexChmod = func(string, os.FileMode) error { return errors.New("chmod failed") }
		_, _, err := stagePackagedCodex(fixture.executable, nil, t.TempDir())
		require.ErrorContains(t, err, "make packaged Codex stage traversable")
	})

	t.Run("create stage bin", func(t *testing.T) {
		fixture := newPackagedCodexFixture(t)
		scratch := t.TempDir()
		stageRoot := fixedPackageStage(t, scratch)
		require.NoError(t, os.WriteFile(filepath.Join(stageRoot, "bin"), []byte("blocked\n"), 0o600))
		_, _, err := stagePackagedCodex(fixture.executable, nil, scratch)
		require.ErrorContains(t, err, "create packaged Codex stage bin")
	})

	t.Run("stage executable", func(t *testing.T) {
		fixture := newPackagedCodexFixture(t)
		scratch := t.TempDir()
		stageRoot := fixedPackageStage(t, scratch)
		require.NoError(t, os.MkdirAll(filepath.Join(stageRoot, "bin", "codex"), 0o755))
		_, _, err := stagePackagedCodex(fixture.executable, nil, scratch)
		require.ErrorContains(t, err, "stage packaged Codex executable")
	})

	t.Run("link code mode host", func(t *testing.T) {
		fixture := newPackagedCodexFixture(t)
		scratch := t.TempDir()
		stageRoot := fixedPackageStage(t, scratch)
		stageBin := filepath.Join(stageRoot, "bin")
		require.NoError(t, os.MkdirAll(stageBin, 0o755))
		require.NoError(t, os.WriteFile(filepath.Join(stageBin, codexCodeModeHost), []byte("blocked\n"), 0o600))
		_, _, err := stagePackagedCodex(fixture.executable, nil, scratch)
		require.ErrorContains(t, err, "link packaged Codex entry")
	})

	t.Run("link resources", func(t *testing.T) {
		fixture := newPackagedCodexFixture(t)
		scratch := t.TempDir()
		stageRoot := fixedPackageStage(t, scratch)
		require.NoError(t, os.MkdirAll(stageRoot, 0o755))
		require.NoError(t, os.WriteFile(filepath.Join(stageRoot, codexPackageResources), []byte("blocked\n"), 0o600))
		_, _, err := stagePackagedCodex(fixture.executable, nil, scratch)
		require.ErrorContains(t, err, "link packaged Codex entry")
	})

	t.Run("read private path", func(t *testing.T) {
		preservePackagePathHooks(t)
		fixture := newPackagedCodexFixture(t)
		stagePackagedCodexReadDir = func(string) ([]os.DirEntry, error) {
			return nil, errors.New("read failed")
		}
		_, _, err := stagePackagedCodex(fixture.executable, nil, t.TempDir())
		require.ErrorContains(t, err, "read packaged Codex PATH directory")
	})

	t.Run("link private path entry", func(t *testing.T) {
		fixture := newPackagedCodexFixture(t)
		require.NoError(t, os.WriteFile(filepath.Join(fixture.packagePath, codexCodeModeHost), []byte("collision\n"), 0o600))
		_, _, err := stagePackagedCodex(fixture.executable, nil, t.TempDir())
		require.ErrorContains(t, err, "link packaged Codex entry")
	})

	t.Run("write metadata", func(t *testing.T) {
		fixture := newPackagedCodexFixture(t)
		scratch := t.TempDir()
		stageRoot := fixedPackageStage(t, scratch)
		stageMetadata := filepath.Join(stageRoot, codexPackageMetadata)
		require.NoError(t, os.MkdirAll(stageMetadata, 0o755))
		_, _, err := stagePackagedCodex(fixture.executable, nil, scratch)
		require.ErrorContains(t, err, "write staged Codex package metadata")
	})
}

func TestLinkOrCopyExecutableFallbacks(t *testing.T) {
	preservePackagePathHooks(t)
	stagePackagedCodexLink = func(string, string) error { return errors.New("link failed") }

	t.Run("copy", func(t *testing.T) {
		source := filepath.Join(t.TempDir(), "source")
		target := filepath.Join(t.TempDir(), "target")
		require.NoError(t, os.WriteFile(source, []byte("executable\n"), 0o700))
		require.NoError(t, linkOrCopyExecutable(source, target))
		contents, err := os.ReadFile(target)
		require.NoError(t, err)
		require.Equal(t, "executable\n", string(contents))
	})

	t.Run("open source", func(t *testing.T) {
		err := linkOrCopyExecutable(filepath.Join(t.TempDir(), "missing"), filepath.Join(t.TempDir(), "target"))
		require.Error(t, err)
	})

	t.Run("open target", func(t *testing.T) {
		source := filepath.Join(t.TempDir(), "source")
		target := filepath.Join(t.TempDir(), "target")
		require.NoError(t, os.WriteFile(source, []byte("source\n"), 0o700))
		require.NoError(t, os.WriteFile(target, []byte("target\n"), 0o700))
		require.Error(t, linkOrCopyExecutable(source, target))
	})

	t.Run("copy failure removes target", func(t *testing.T) {
		source := filepath.Join(t.TempDir(), "source")
		target := filepath.Join(t.TempDir(), "target")
		require.NoError(t, os.WriteFile(source, []byte("source\n"), 0o700))
		stagePackagedCodexCopy = func(io.Writer, io.Reader) (int64, error) {
			return 0, errors.New("copy failed")
		}
		require.ErrorContains(t, linkOrCopyExecutable(source, target), "copy failed")
		require.NoFileExists(t, target)
	})
}

func TestLaunchAppServerStagesPackagedCodex(t *testing.T) {
	preserveLaunchHooks(t)
	fixture := newPackagedCodexFixture(t)
	scratch := t.TempDir()
	nativePath := "/native/first:/native/second"
	var launchedPaths []string
	execCommandContext = func(ctx context.Context, path string, _ ...string) *exec.Cmd {
		launchedPaths = append(launchedPaths, path)

		return exec.CommandContext(ctx, "/bin/cat")
	}

	for range 2 {
		transport, command, version, gotNativePath, err := launchAppServer(context.Background(), context.Background(), Options{
			CLIPath:        fixture.executable,
			SupervisorRoot: scratch,
			NativeVersion:  minCodexVersion,
			skipSupervisor: true,
			ImplicitEnvironment: map[string]string{
				"PATH": nativePath,
			},
		})
		require.NoError(t, err)
		require.NotNil(t, transport)
		require.NotNil(t, command)
		require.Equal(t, minCodexVersion, version)
		launchedPath := launchedPaths[len(launchedPaths)-1]
		stageBin := filepath.Dir(launchedPath)
		require.Equal(t, filepath.Join(stageBin, "codex"), launchedPath)
		require.Equal(t, stageBin+string(os.PathListSeparator)+nativePath, gotNativePath)
		require.NoError(t, transport.Close())
	}
	require.Len(t, launchedPaths, 2)
	require.NotEqual(t, filepath.Dir(filepath.Dir(launchedPaths[0])), filepath.Dir(filepath.Dir(launchedPaths[1])))
}

func TestLaunchAppServerReportsPackagedCodexStageFailure(t *testing.T) {
	fixture := newPackagedCodexFixture(t)
	transport, command, version, nativePath, err := launchAppServer(context.Background(), context.Background(), Options{
		CLIPath:        fixture.executable,
		NativeVersion:  minCodexVersion,
		skipSupervisor: true,
	})
	require.Nil(t, transport)
	require.Nil(t, command)
	require.Empty(t, version)
	require.Empty(t, nativePath)
	require.ErrorContains(t, err, "runtime scratch is required")
}
