//go:build unix

package codex

import (
	"encoding/json"
	"errors"
	"io"
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

func requireStagePackagedCodexError(t *testing.T, path, scratch string) error {
	t.Helper()

	staged, environment, cleanup, err := stagePackagedCodex(path, nil, scratch)
	require.Empty(t, staged)
	require.Nil(t, environment)
	require.Nil(t, cleanup)
	require.Error(t, err)

	return err
}

func TestStagePackagedCodexFailureEdges(t *testing.T) {
	err := requireStagePackagedCodexError(t, filepath.Join(t.TempDir(), "missing"), t.TempDir())
	require.ErrorContains(t, err, "resolve packaged")

	shimRoot := filepath.Join(t.TempDir(), "@openai", codexPackageExecutable, codexPackageBin)
	require.NoError(t, os.MkdirAll(shimRoot, 0o755))
	shim := filepath.Join(shimRoot, codexNPMShimName)
	require.NoError(t, os.WriteFile(shim, []byte("shim"), 0o700))
	err = requireStagePackagedCodexError(t, shim, t.TempDir())
	require.ErrorContains(t, err, "resolve npm packaged")

	fixture := newPackagedCodexFixture(t)

	t.Run("mkdir", func(t *testing.T) {
		original := stagePackagedCodexMkdir
		stagePackagedCodexMkdir = func(string, string) (string, error) { return "", os.ErrPermission }
		t.Cleanup(func() { stagePackagedCodexMkdir = original })
		err := requireStagePackagedCodexError(t, fixture.executable, t.TempDir())
		require.ErrorIs(t, err, os.ErrPermission)
	})

	t.Run("chmod", func(t *testing.T) {
		original := stagePackagedCodexChmod
		stagePackagedCodexChmod = func(string, os.FileMode) error { return os.ErrPermission }
		t.Cleanup(func() { stagePackagedCodexChmod = original })
		err := requireStagePackagedCodexError(t, fixture.executable, t.TempDir())
		require.ErrorIs(t, err, os.ErrPermission)
	})

	t.Run("metadata write", func(t *testing.T) {
		original := stagePackagedCodexMkdir
		stageRoot := filepath.Join(t.TempDir(), "stage")
		require.NoError(t, os.MkdirAll(filepath.Join(stageRoot, codexPackageMetadata), 0o755))
		stagePackagedCodexMkdir = func(string, string) (string, error) { return stageRoot, nil }
		t.Cleanup(func() { stagePackagedCodexMkdir = original })
		err := requireStagePackagedCodexError(t, fixture.executable, t.TempDir())
		require.Error(t, err)
	})
}

func TestStagePackagedCodexContentsFailureEdges(t *testing.T) {
	fixture := newPackagedCodexFixture(t)

	parentFile := filepath.Join(t.TempDir(), "file")
	require.NoError(t, os.WriteFile(parentFile, []byte("x"), 0o600))
	_, _, err := stagePackagedCodexContents(fixture.executable, fixture.root, filepath.Join(fixture.root, codexPackagePathDir), filepath.Join(parentFile, "stage"))
	require.ErrorContains(t, err, "stage bin")

	t.Run("source", func(t *testing.T) {
		original := stagePackagedCodexLink
		stagePackagedCodexLink = func(string, string) error { return os.ErrPermission }
		t.Cleanup(func() { stagePackagedCodexLink = original })
		_, _, err := stagePackagedCodexContents(filepath.Join(t.TempDir(), "missing"), fixture.root, filepath.Join(fixture.root, codexPackagePathDir), t.TempDir())
		require.Error(t, err)
	})

	t.Run("code mode host target", func(t *testing.T) {
		stageRoot := t.TempDir()
		stageBin := filepath.Join(stageRoot, codexPackageBin)
		require.NoError(t, os.MkdirAll(stageBin, 0o755))
		require.NoError(t, os.WriteFile(filepath.Join(stageBin, codexCodeModeHost), []byte("occupied"), 0o600))
		_, _, err := stagePackagedCodexContents(fixture.executable, fixture.root, filepath.Join(fixture.root, codexPackagePathDir), stageRoot)
		require.ErrorContains(t, err, codexCodeModeHost)
	})

	t.Run("resources target", func(t *testing.T) {
		stageRoot := t.TempDir()
		require.NoError(t, os.Mkdir(filepath.Join(stageRoot, codexPackageResources), 0o755))
		_, _, err := stagePackagedCodexContents(fixture.executable, fixture.root, filepath.Join(fixture.root, codexPackagePathDir), stageRoot)
		require.ErrorContains(t, err, codexPackageResources)
	})

	t.Run("path entry target", func(t *testing.T) {
		pathDir := filepath.Join(fixture.root, codexPackagePathDir)
		require.NoError(t, os.WriteFile(filepath.Join(pathDir, codexPackageExecutable), []byte("duplicate"), 0o700))
		_, _, err := stagePackagedCodexContents(fixture.executable, fixture.root, pathDir, t.TempDir())
		require.ErrorContains(t, err, codexPackageExecutable)
	})
}

type officialNPMFixture struct {
	shim       string
	mainRoot   string
	aliasRoot  string
	nativeRoot string
	native     string
	platform   npmCodexPlatform
	version    string
}

func newOfficialNPMFixture(t *testing.T) officialNPMFixture {
	t.Helper()
	platform, supported := currentNPMCodexPlatform()
	require.True(t, supported)
	const version = "1.2.3"
	install := t.TempDir()
	mainRoot := filepath.Join(install, "node_modules", "@openai", codexPackageExecutable)
	require.NoError(t, os.MkdirAll(filepath.Join(mainRoot, codexPackageBin), 0o755))
	shim := filepath.Join(mainRoot, codexPackageBin, codexNPMShimName)
	require.NoError(t, os.WriteFile(shim, []byte("shim"), 0o700))
	writeJSONFixture(t, filepath.Join(mainRoot, "package.json"), npmCodexPackage{
		Name: codexNPMMainPackageName, Version: version,
		Bin: map[string]string{codexPackageExecutable: filepath.ToSlash(filepath.Join(codexPackageBin, codexNPMShimName))},
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

	return officialNPMFixture{shim: shim, mainRoot: mainRoot, aliasRoot: aliasRoot, nativeRoot: nativeRoot, native: native, platform: platform, version: version}
}

func TestResolveNPMCodexExecutableFailureEdges(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(*testing.T, officialNPMFixture)
	}{
		{"main path", func(t *testing.T, f officialNPMFixture) {
			t.Helper()
			require.NoError(t, os.Remove(filepath.Join(f.mainRoot, "package.json")))
		}},
		{"main read", func(t *testing.T, f officialNPMFixture) {
			t.Helper()
			require.NoError(t, os.WriteFile(filepath.Join(f.mainRoot, "package.json"), []byte("{"), 0o600))
		}},
		{"shim metadata", func(t *testing.T, f officialNPMFixture) {
			t.Helper()
			writeJSONFixture(t, filepath.Join(f.mainRoot, "package.json"), npmCodexPackage{})
		}},
		{"unsupported platform", func(t *testing.T, _ officialNPMFixture) {
			t.Helper()
			oldOS, oldArch := npmCodexRuntimeGOOS, npmCodexRuntimeGOARCH
			npmCodexRuntimeGOOS, npmCodexRuntimeGOARCH = "unsupported", "unsupported"
			t.Cleanup(func() { npmCodexRuntimeGOOS, npmCodexRuntimeGOARCH = oldOS, oldArch })
		}},
		{"platform pin", func(t *testing.T, f officialNPMFixture) {
			t.Helper()
			writeJSONFixture(t, filepath.Join(f.mainRoot, "package.json"), npmCodexPackage{Name: codexNPMMainPackageName, Version: f.version, Bin: map[string]string{codexPackageExecutable: "bin/codex.js"}})
		}},
		{"alias root", func(t *testing.T, f officialNPMFixture) {
			t.Helper()
			require.NoError(t, os.RemoveAll(f.aliasRoot))
		}},
		{"optional path", func(t *testing.T, f officialNPMFixture) {
			t.Helper()
			require.NoError(t, os.Remove(filepath.Join(f.aliasRoot, "package.json")))
		}},
		{"optional read", func(t *testing.T, f officialNPMFixture) {
			t.Helper()
			require.NoError(t, os.WriteFile(filepath.Join(f.aliasRoot, "package.json"), []byte("{"), 0o600))
		}},
		{"optional metadata", func(t *testing.T, f officialNPMFixture) {
			t.Helper()
			writeJSONFixture(t, filepath.Join(f.aliasRoot, "package.json"), npmCodexPackage{})
		}},
		{"native root", func(t *testing.T, f officialNPMFixture) {
			t.Helper()
			require.NoError(t, os.RemoveAll(filepath.Join(f.aliasRoot, "vendor")))
		}},
		{"manifest path", func(t *testing.T, f officialNPMFixture) {
			t.Helper()
			require.NoError(t, os.Remove(filepath.Join(f.nativeRoot, codexPackageMetadata)))
		}},
		{"manifest read", func(t *testing.T, f officialNPMFixture) {
			t.Helper()
			require.NoError(t, os.WriteFile(filepath.Join(f.nativeRoot, codexPackageMetadata), []byte("{"), 0o600))
		}},
		{"manifest metadata", func(t *testing.T, f officialNPMFixture) {
			t.Helper()
			writeJSONFixture(t, filepath.Join(f.nativeRoot, codexPackageMetadata), codexPackageManifest{})
		}},
		{"layout", func(t *testing.T, f officialNPMFixture) {
			t.Helper()
			require.NoError(t, os.RemoveAll(filepath.Join(f.nativeRoot, codexPackageResources)))
		}},
		{"executable escapes", func(t *testing.T, f officialNPMFixture) {
			t.Helper()
			require.NoError(t, os.Remove(f.native))
			outside := filepath.Join(t.TempDir(), "codex")
			require.NoError(t, os.WriteFile(outside, []byte("outside"), 0o700))
			require.NoError(t, os.Symlink(outside, f.native))
		}},
		{"executable invalid", func(t *testing.T, f officialNPMFixture) {
			t.Helper()
			require.NoError(t, os.Chmod(f.native, 0o600))
		}},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			fixture := newOfficialNPMFixture(t)
			test.mutate(t, fixture)
			_, recognized, err := resolveNPMCodexExecutable(fixture.shim)
			require.True(t, recognized)
			require.Error(t, err)
		})
	}
}

func TestNPMCodexHelperEdges(t *testing.T) {
	require.Error(t, validateNPMCodexShimPackage(npmCodexPackage{}))
	require.Error(t, validateNPMCodexPlatformPin(npmCodexPackage{}, npmCodexPlatform{alias: "alias"}))

	originalLstat := npmCodexPackageLstat
	npmCodexPackageLstat = func(string) (os.FileInfo, error) { return nil, os.ErrPermission }
	t.Cleanup(func() { npmCodexPackageLstat = originalLstat })
	_, err := resolveNPMCodexAliasRoot(t.TempDir(), "alias")
	require.ErrorIs(t, err, os.ErrPermission)
	npmCodexPackageLstat = originalLstat
	_, err = resolveNPMCodexAliasRoot(t.TempDir(), "alias")
	require.ErrorContains(t, err, "unavailable")

	oldOS, oldArch := npmCodexRuntimeGOOS, npmCodexRuntimeGOARCH
	t.Cleanup(func() { npmCodexRuntimeGOOS, npmCodexRuntimeGOARCH = oldOS, oldArch })
	for _, pair := range [][2]string{{"linux", "amd64"}, {"linux", "arm64"}, {codexNPMOSDarwin, "amd64"}, {codexNPMOSDarwin, "arm64"}, {"other", "other"}} {
		npmCodexRuntimeGOOS, npmCodexRuntimeGOARCH = pair[0], pair[1]
		_, _ = currentNPMCodexPlatform()
	}

	root := t.TempDir()
	_, err = resolveExactCodexPackagePath(filepath.Join(root, "missing"), root, ".")
	require.Error(t, err)
	_, err = resolveExactCodexPackagePath(root, filepath.Join(root, "missing"), "missing")
	require.Error(t, err)
	outside := filepath.Join(t.TempDir(), "outside")
	require.NoError(t, os.WriteFile(outside, []byte("x"), 0o600))
	_, err = resolveExactCodexPackagePath(root, outside, "inside")
	require.ErrorContains(t, err, "escapes")
}

func TestReadAndValidateNPMCodexPackageEdges(t *testing.T) {
	var target npmCodexPackage
	_, missing := filepath.Split(filepath.Join(t.TempDir(), "missing"))
	require.Error(t, readCodexPackageJSON(filepath.Join(t.TempDir(), missing), &target))

	file := filepath.Join(t.TempDir(), "package.json")
	require.NoError(t, os.WriteFile(file, []byte("{}"), 0o600))
	originalStat := npmCodexPackageStatJSON
	npmCodexPackageStatJSON = func(*os.File) (os.FileInfo, error) { return nil, os.ErrPermission }
	t.Cleanup(func() { npmCodexPackageStatJSON = originalStat })
	require.ErrorIs(t, readCodexPackageJSON(file, &target), os.ErrPermission)
	npmCodexPackageStatJSON = originalStat
	require.Error(t, readCodexPackageJSON(t.TempDir(), &target))

	large := filepath.Join(t.TempDir(), "large.json")
	require.NoError(t, os.WriteFile(large, make([]byte, codexPackageJSONMaxBytes+1), 0o600))
	require.ErrorContains(t, readCodexPackageJSON(large, &target), "size limit")

	originalRead := npmCodexPackageReadJSON
	npmCodexPackageReadJSON = func(io.Reader) ([]byte, error) { return nil, os.ErrPermission }
	t.Cleanup(func() { npmCodexPackageReadJSON = originalRead })
	require.ErrorIs(t, readCodexPackageJSON(file, &target), os.ErrPermission)
	npmCodexPackageReadJSON = func(io.Reader) ([]byte, error) { return make([]byte, codexPackageJSONMaxBytes+1), nil }
	require.ErrorContains(t, readCodexPackageJSON(file, &target), "size limit")
	npmCodexPackageReadJSON = originalRead
	require.NoError(t, os.WriteFile(file, []byte("{"), 0o600))
	require.Error(t, readCodexPackageJSON(file, &target))

	root := t.TempDir()
	require.Error(t, validateNPMCodexPackageLayout(root))
	for _, path := range []string{codexPackageBin, codexPackagePathDir, codexPackageResources} {
		require.NoError(t, os.Mkdir(filepath.Join(root, path), 0o755))
	}
	require.ErrorContains(t, validateNPMCodexPackageLayout(root), "code-mode host")
	host := filepath.Join(root, codexPackageBin, codexCodeModeHost)
	require.NoError(t, os.WriteFile(host, []byte("host"), 0o700))
	originalReadDir := npmCodexPackageReadDir
	npmCodexPackageReadDir = func(string) ([]os.DirEntry, error) { return nil, os.ErrPermission }
	t.Cleanup(func() { npmCodexPackageReadDir = originalReadDir })
	require.ErrorIs(t, validateNPMCodexPackageLayout(root), os.ErrPermission)
	npmCodexPackageReadDir = originalReadDir
	require.NoError(t, os.WriteFile(filepath.Join(root, codexPackagePathDir, "bad"), []byte("bad"), 0o600))
	require.ErrorContains(t, validateNPMCodexPackageLayout(root), "PATH entry")
	require.Error(t, validateCodexPackageExecutable(filepath.Join(root, "missing")))
	require.Error(t, validateCodexPackageExecutable(filepath.Join(root, codexPackageResources)))
}

func TestPackageEntryLinkAndCopyEdges(t *testing.T) {
	target := filepath.Join(t.TempDir(), "target")
	require.NoError(t, os.WriteFile(target, []byte("occupied"), 0o600))
	require.Error(t, linkRequiredPackageEntry("source", target))

	originalLink := stagePackagedCodexLink
	stagePackagedCodexLink = func(string, string) error { return errors.New("cross-device") }
	t.Cleanup(func() { stagePackagedCodexLink = originalLink })
	_, err := os.Stat(filepath.Join(t.TempDir(), "missing"))
	require.Error(t, err)
	require.Error(t, linkOrCopyExecutable(filepath.Join(t.TempDir(), "missing"), filepath.Join(t.TempDir(), "target")))

	source := filepath.Join(t.TempDir(), "source")
	require.NoError(t, os.WriteFile(source, []byte("payload"), 0o700))
	require.Error(t, linkOrCopyExecutable(source, target))

	copyTarget := filepath.Join(t.TempDir(), "copy")
	originalCopy := stagePackagedCodexCopy
	stagePackagedCodexCopy = func(io.Writer, io.Reader) (int64, error) { return 0, os.ErrPermission }
	t.Cleanup(func() { stagePackagedCodexCopy = originalCopy })
	require.ErrorIs(t, linkOrCopyExecutable(source, copyTarget), os.ErrPermission)
	require.NoFileExists(t, copyTarget)
	stagePackagedCodexCopy = originalCopy
	require.NoError(t, linkOrCopyExecutable(source, copyTarget))
	require.FileExists(t, copyTarget)
}
