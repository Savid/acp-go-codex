//go:build unix

package codex

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
)

type packagedCodexFixture struct {
	executable string
	root       string
}

func newPackagedCodexFixture(t *testing.T) packagedCodexFixture {
	t.Helper()

	root := t.TempDir()
	bin := filepath.Join(root, codexPackageBin)
	pathDir := filepath.Join(root, codexPackagePathDir)
	require.NoError(t, os.MkdirAll(bin, 0o755))
	require.NoError(t, os.MkdirAll(pathDir, 0o755))
	require.NoError(t, os.MkdirAll(filepath.Join(root, codexPackageResources), 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(root, codexPackageMetadata), []byte("fixture\n"), 0o600))

	executable := filepath.Join(bin, codexPackageExecutable)
	require.NoError(t, os.WriteFile(executable, []byte("#!/bin/sh\ncat\n"), 0o700))
	require.NoError(t, os.WriteFile(filepath.Join(bin, codexCodeModeHost), []byte("host\n"), 0o700))
	require.NoError(t, os.WriteFile(filepath.Join(pathDir, "rg"), []byte("rg\n"), 0o700))

	return packagedCodexFixture{executable: executable, root: root}
}

func TestStagePackagedCodexStagesAndCleansOrdinaryPackage(t *testing.T) {
	fixture := newPackagedCodexFixture(t)
	invocation := filepath.Join(t.TempDir(), "codex")
	require.NoError(t, os.Symlink(fixture.executable, invocation))
	scratch := t.TempDir()

	staged, environment, cleanup, err := stagePackagedCodex(
		invocation, []string{"Z=value", "PATH=/native/first:/native/second"}, scratch,
	)
	require.NoError(t, err)
	stageBin := filepath.Dir(staged)
	stageRoot := filepath.Dir(stageBin)
	require.Equal(t, stageBin+string(os.PathListSeparator)+"/native/first:/native/second", environmentMap(environment)[pathEnvKey])
	require.Equal(t, "value", environmentMap(environment)["Z"])
	require.FileExists(t, staged)
	require.FileExists(t, filepath.Join(stageBin, codexCodeModeHost))
	require.FileExists(t, filepath.Join(stageBin, "rg"))
	resources, err := os.Stat(filepath.Join(stageRoot, codexPackageResources))
	require.NoError(t, err)
	require.True(t, resources.IsDir())
	require.NoError(t, cleanup())
	require.NoDirExists(t, stageRoot)
}

func TestStagePackagedCodexLeavesOrdinaryExecutableUnchanged(t *testing.T) {
	executable := filepath.Join(t.TempDir(), "codex")
	require.NoError(t, os.WriteFile(executable, []byte("ordinary\n"), 0o700))
	environment := []string{"PATH=/native"}

	staged, gotEnvironment, cleanup, err := stagePackagedCodex(executable, environment, "")
	require.NoError(t, err)
	require.Equal(t, executable, staged)
	require.Equal(t, environment, gotEnvironment)
	require.NoError(t, cleanup())
}

func TestStagePackagedCodexRequiresScratchAndCleansFailure(t *testing.T) {
	fixture := newPackagedCodexFixture(t)
	staged, environment, cleanup, err := stagePackagedCodex(fixture.executable, nil, "")
	require.ErrorContains(t, err, "runtime scratch is required")
	require.Empty(t, staged)
	require.Nil(t, environment)
	require.Nil(t, cleanup)

	originalReadDir := stagePackagedCodexReadDir
	t.Cleanup(func() { stagePackagedCodexReadDir = originalReadDir })
	stagePackagedCodexReadDir = func(string) ([]os.DirEntry, error) { return nil, os.ErrPermission }
	scratch := t.TempDir()
	staged, environment, cleanup, err = stagePackagedCodex(fixture.executable, nil, scratch)
	require.ErrorIs(t, err, os.ErrPermission)
	require.Empty(t, staged)
	require.Nil(t, environment)
	require.Nil(t, cleanup)
	entries, readErr := os.ReadDir(scratch)
	require.NoError(t, readErr)
	require.Empty(t, entries)
}

func TestStagePackagedCodexStagesOfficialNPMDistribution(t *testing.T) {
	platform, supported := currentNPMCodexPlatform()
	if !supported {
		t.Skip("official Codex npm package unsupported on this platform")
	}

	const version = "1.2.3"
	install := t.TempDir()
	mainRoot := filepath.Join(install, "node_modules", "@openai", "codex")
	require.NoError(t, os.MkdirAll(filepath.Join(mainRoot, "bin"), 0o755))
	shim := filepath.Join(mainRoot, "bin", codexNPMShimName)
	require.NoError(t, os.WriteFile(shim, []byte("#!/usr/bin/env node\n"), 0o700))
	writeJSONFixture(t, filepath.Join(mainRoot, "package.json"), npmCodexPackage{
		Name: codexNPMMainPackageName, Version: version, Bin: map[string]string{"codex": "bin/codex.js"},
		OptionalDependencies: map[string]string{
			"@openai/" + platform.alias: "npm:" + codexNPMMainPackageName + "@" + version + "-" + platform.distribution,
		},
	})

	aliasRoot := filepath.Join(filepath.Dir(mainRoot), platform.alias)
	require.NoError(t, os.MkdirAll(aliasRoot, 0o755))
	writeJSONFixture(t, filepath.Join(aliasRoot, "package.json"), npmCodexPackage{
		Name: codexNPMMainPackageName, Version: version + "-" + platform.distribution,
		OS: []string{platform.os}, CPU: []string{platform.cpu},
	})
	nativeRoot := filepath.Join(aliasRoot, "vendor", platform.target)
	native := newNativePackageFixture(t, nativeRoot, version, platform)

	invocation := filepath.Join(t.TempDir(), "codex")
	require.NoError(t, os.Symlink(shim, invocation))
	staged, _, cleanup, err := stagePackagedCodex(invocation, nil, t.TempDir())
	require.NoError(t, err)
	stagedInfo, err := os.Stat(staged)
	require.NoError(t, err)
	nativeInfo, err := os.Stat(native)
	require.NoError(t, err)
	require.True(t, os.SameFile(stagedInfo, nativeInfo))
	require.NoError(t, cleanup())
}

func newNativePackageFixture(t *testing.T, root, version string, platform npmCodexPlatform) string {
	t.Helper()

	bin := filepath.Join(root, codexPackageBin)
	require.NoError(t, os.MkdirAll(bin, 0o755))
	require.NoError(t, os.MkdirAll(filepath.Join(root, codexPackagePathDir), 0o755))
	require.NoError(t, os.MkdirAll(filepath.Join(root, codexPackageResources), 0o755))
	native := filepath.Join(bin, codexPackageExecutable)
	require.NoError(t, os.WriteFile(native, []byte("native-codex\n"), 0o700))
	require.NoError(t, os.WriteFile(filepath.Join(bin, codexCodeModeHost), []byte("host\n"), 0o700))
	require.NoError(t, os.WriteFile(filepath.Join(root, codexPackagePathDir, "rg"), []byte("rg\n"), 0o700))
	writeJSONFixture(t, filepath.Join(root, codexPackageMetadata), codexPackageManifest{
		LayoutVersion: codexPackageLayoutVersion, Version: version, Target: platform.target,
		Variant: codexPackageVariant, Entrypoint: codexPackageEntrypoint,
		ResourcesDir: codexPackageResources, PathDir: codexPackagePathDir,
	})

	return native
}

func writeJSONFixture(t *testing.T, path string, value any) {
	t.Helper()
	raw, err := json.Marshal(value)
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(path, append(raw, '\n'), 0o600))
}
