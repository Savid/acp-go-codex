//go:build unix

package codex

import (
	"context"
	"encoding/json"
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

type npmPackagedCodexFixture struct {
	executable            string
	invocation            string
	mainPackageMetadata   string
	manifest              string
	nativePackageMetadata string
	nativePackageRoot     string
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

func newNPMPackagedCodexFixture(t *testing.T) npmPackagedCodexFixture {
	t.Helper()

	return newNPMPackagedCodexFixtureWithLayout(t, false)
}

func newNPMPackagedCodexFixtureWithLayout(t *testing.T, nested bool) npmPackagedCodexFixture {
	t.Helper()

	platform, supported := currentNPMCodexPlatform()
	if !supported {
		t.Skip("official Codex npm packages do not support this Unix platform")
	}

	const version = "1.2.3"
	installRoot := t.TempDir()
	mainPackageRoot := filepath.Join(installRoot, "node_modules", "@openai", "codex")
	mainBin := filepath.Join(mainPackageRoot, "bin")
	require.NoError(t, os.MkdirAll(mainBin, 0o755))
	shim := filepath.Join(mainBin, codexNPMShimName)
	require.NoError(t, os.WriteFile(shim, []byte("#!/usr/bin/env node\n"), 0o700))
	mainPackageMetadata := filepath.Join(mainPackageRoot, "package.json")
	writeCodexJSONFixture(t, mainPackageMetadata, npmCodexPackage{
		Name: codexNPMMainPackageName, Version: version,
		Bin: map[string]string{"codex": "bin/codex.js"},
		OptionalDependencies: map[string]string{
			"@openai/" + platform.alias: "npm:" + codexNPMMainPackageName + "@" + version + "-" + platform.distribution,
		},
	})

	nativeAliasRoot := filepath.Join(filepath.Dir(mainPackageRoot), platform.alias)
	if nested {
		nativeAliasRoot = filepath.Join(mainPackageRoot, "node_modules", "@openai", platform.alias)
	}
	require.NoError(t, os.MkdirAll(nativeAliasRoot, 0o755))
	nativePackageMetadata := filepath.Join(nativeAliasRoot, "package.json")
	writeCodexJSONFixture(t, nativePackageMetadata, npmCodexPackage{
		Name: codexNPMMainPackageName, Version: version + "-" + platform.distribution,
		OS: []string{platform.os}, CPU: []string{platform.cpu},
	})

	nativePackageRoot := filepath.Join(nativeAliasRoot, "vendor", platform.target)
	nativeBin := filepath.Join(nativePackageRoot, "bin")
	require.NoError(t, os.MkdirAll(nativeBin, 0o755))
	require.NoError(t, os.MkdirAll(filepath.Join(nativePackageRoot, codexPackagePathDir), 0o755))
	require.NoError(t, os.MkdirAll(filepath.Join(nativePackageRoot, codexPackageResources), 0o755))
	executable := filepath.Join(nativeBin, "codex")
	require.NoError(t, os.WriteFile(executable, []byte("native-codex\n"), 0o700))
	require.NoError(t, os.WriteFile(filepath.Join(nativeBin, codexCodeModeHost), []byte("host\n"), 0o700))
	require.NoError(t, os.WriteFile(
		filepath.Join(nativePackageRoot, codexPackagePathDir, "rg"), []byte("rg\n"), 0o700,
	))
	manifest := filepath.Join(nativePackageRoot, codexPackageMetadata)
	writeCodexJSONFixture(t, manifest, codexPackageManifest{
		LayoutVersion: codexPackageLayoutVersion,
		Version:       version,
		Target:        platform.target,
		Variant:       codexPackageVariant,
		Entrypoint:    codexPackageEntrypoint,
		ResourcesDir:  codexPackageResources,
		PathDir:       codexPackagePathDir,
	})

	invocation := filepath.Join(t.TempDir(), "codex")
	require.NoError(t, os.Symlink(shim, invocation))
	resolvedNativePackageRoot, err := filepath.EvalSymlinks(nativePackageRoot)
	require.NoError(t, err)

	return npmPackagedCodexFixture{
		executable: filepath.Join(resolvedNativePackageRoot, "bin", "codex"), invocation: invocation,
		mainPackageMetadata: mainPackageMetadata, manifest: manifest, nativePackageMetadata: nativePackageMetadata,
		nativePackageRoot: resolvedNativePackageRoot,
	}
}

func writeCodexJSONFixture(t *testing.T, path string, value any) {
	t.Helper()

	data, err := json.Marshal(value)
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(path, append(data, '\n'), 0o600))
}

func preservePackagePathHooks(t *testing.T) {
	t.Helper()

	copyFile := stagePackagedCodexCopy
	chmod := stagePackagedCodexChmod
	link := stagePackagedCodexLink
	mkdir := stagePackagedCodexMkdir
	readDir := stagePackagedCodexReadDir
	npmReadDir := npmCodexPackageReadDir
	npmReadJSON := npmCodexPackageReadJSON
	npmStatJSON := npmCodexPackageStatJSON
	npmLstat := npmCodexPackageLstat
	npmGOOS := npmCodexRuntimeGOOS
	npmGOARCH := npmCodexRuntimeGOARCH
	t.Cleanup(func() {
		stagePackagedCodexCopy = copyFile
		stagePackagedCodexChmod = chmod
		stagePackagedCodexLink = link
		stagePackagedCodexMkdir = mkdir
		stagePackagedCodexReadDir = readDir
		npmCodexPackageReadDir = npmReadDir
		npmCodexPackageReadJSON = npmReadJSON
		npmCodexPackageStatJSON = npmStatJSON
		npmCodexPackageLstat = npmLstat
		npmCodexRuntimeGOOS = npmGOOS
		npmCodexRuntimeGOARCH = npmGOARCH
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

func TestStagePackagedCodexLeavesOtherJavaScriptShimUnchanged(t *testing.T) {
	packageRoot := filepath.Join(t.TempDir(), "node_modules", "example", "tool")
	require.NoError(t, os.MkdirAll(filepath.Join(packageRoot, "bin"), 0o755))
	shim := filepath.Join(packageRoot, "bin", codexNPMShimName)
	require.NoError(t, os.WriteFile(shim, []byte("#!/usr/bin/env node\n"), 0o700))
	invocation := filepath.Join(t.TempDir(), "codex")
	require.NoError(t, os.Symlink(shim, invocation))
	env := []string{"PATH=/native"}

	staged, gotEnv, err := stagePackagedCodex(invocation, env, "")
	require.NoError(t, err)
	require.Equal(t, invocation, staged)
	require.Equal(t, env, gotEnv)
}

func TestStagePackagedCodexStagesOfficialNPMDistribution(t *testing.T) {
	for _, nested := range []bool{false, true} {
		name := "sibling dependency"
		if nested {
			name = "nested dependency"
		}
		t.Run(name, func(t *testing.T) {
			fixture := newNPMPackagedCodexFixtureWithLayout(t, nested)
			scratch := t.TempDir()

			staged, env, err := stagePackagedCodex(
				fixture.invocation, []string{"Z=value", "PATH=/native/first:/native/second"}, scratch,
			)
			require.NoError(t, err)

			stageBin := filepath.Dir(staged)
			stageRoot := filepath.Dir(stageBin)
			require.Equal(t, filepath.Join(stageBin, "codex"), staged)
			require.Equal(t, stageBin+string(os.PathListSeparator)+"/native/first:/native/second", environmentMap(env)[pathEnvKey])
			require.Equal(t, "value", environmentMap(env)["Z"])
			sourceInfo, err := os.Stat(fixture.executable)
			require.NoError(t, err)
			stageInfo, err := os.Stat(staged)
			require.NoError(t, err)
			require.True(t, os.SameFile(sourceInfo, stageInfo))
			codeModeTarget, err := os.Readlink(filepath.Join(stageBin, codexCodeModeHost))
			require.NoError(t, err)
			require.Equal(t, filepath.Join(fixture.nativePackageRoot, "bin", codexCodeModeHost), codeModeTarget)
			rgTarget, err := os.Readlink(filepath.Join(stageBin, "rg"))
			require.NoError(t, err)
			require.Equal(t, filepath.Join(fixture.nativePackageRoot, codexPackagePathDir, "rg"), rgTarget)
			resourcesTarget, err := os.Readlink(filepath.Join(stageRoot, codexPackageResources))
			require.NoError(t, err)
			require.Equal(t, filepath.Join(fixture.nativePackageRoot, codexPackageResources), resourcesTarget)
			require.NotEqual(t, codexNPMShimName, filepath.Base(staged),
				"the npm JavaScript shim was staged instead of its native executable")
		})
	}
}

func TestStagePackagedCodexStagesPNPMSiblingDependencyLink(t *testing.T) {
	fixture := newNPMPackagedCodexFixture(t)
	aliasRoot := filepath.Dir(filepath.Dir(fixture.nativePackageRoot))
	storeRoot := filepath.Join(t.TempDir(), "node_modules", "@openai", filepath.Base(aliasRoot))
	require.NoError(t, os.MkdirAll(filepath.Dir(storeRoot), 0o755))
	require.NoError(t, os.Rename(aliasRoot, storeRoot))
	require.NoError(t, os.Symlink(storeRoot, aliasRoot))
	fixture.nativePackageRoot = filepath.Join(storeRoot, "vendor", filepath.Base(fixture.nativePackageRoot))
	fixture.executable = filepath.Join(fixture.nativePackageRoot, "bin", "codex")

	staged, _, err := stagePackagedCodex(fixture.invocation, []string{"PATH=/usr/bin"}, t.TempDir())
	require.NoError(t, err)
	require.FileExists(t, staged)
}

func TestStagePackagedCodexFailsClosedForInvalidOfficialNPMDistribution(t *testing.T) {
	t.Run("missing optional dependency pin", func(t *testing.T) {
		fixture := newNPMPackagedCodexFixture(t)
		writeCodexJSONFixture(t, fixture.mainPackageMetadata, npmCodexPackage{
			Name: codexNPMMainPackageName, Version: "1.2.3",
			Bin: map[string]string{codexPackageExecutable: "bin/codex.js"},
		})
		requireNPMCodexStageError(t, fixture)
	})

	t.Run("nested invalid shadows sibling", func(t *testing.T) {
		fixture := newNPMPackagedCodexFixture(t)
		platform, supported := currentNPMCodexPlatform()
		require.True(t, supported)
		shadow := filepath.Join(
			filepath.Dir(fixture.mainPackageMetadata), "node_modules", "@openai", platform.alias,
		)
		require.NoError(t, os.MkdirAll(shadow, 0o755))
		requireNPMCodexStageError(t, fixture)
	})

	t.Run("missing main package metadata", func(t *testing.T) {
		fixture := newNPMPackagedCodexFixture(t)
		require.NoError(t, os.Remove(fixture.mainPackageMetadata))
		requireNPMCodexStageError(t, fixture)
	})

	t.Run("malformed main package metadata", func(t *testing.T) {
		fixture := newNPMPackagedCodexFixture(t)
		require.NoError(t, os.WriteFile(fixture.mainPackageMetadata, []byte("{\n"), 0o600))
		requireNPMCodexStageError(t, fixture)
	})

	t.Run("wrong main package metadata", func(t *testing.T) {
		fixture := newNPMPackagedCodexFixture(t)
		writeCodexJSONFixture(t, fixture.mainPackageMetadata, npmCodexPackage{
			Name: "example.invalid/codex", Version: "1.2.3",
			Bin: map[string]string{codexPackageExecutable: "bin/codex.js"},
		})
		requireNPMCodexStageError(t, fixture)
	})

	t.Run("unsupported runtime", func(t *testing.T) {
		preservePackagePathHooks(t)
		fixture := newNPMPackagedCodexFixture(t)
		npmCodexRuntimeGOOS = "plan9"
		npmCodexRuntimeGOARCH = "amd64"
		requireNPMCodexStageError(t, fixture)
	})

	t.Run("missing native package metadata", func(t *testing.T) {
		fixture := newNPMPackagedCodexFixture(t)
		require.NoError(t, os.Remove(fixture.nativePackageMetadata))
		requireNPMCodexStageError(t, fixture)
	})

	t.Run("missing native package", func(t *testing.T) {
		fixture := newNPMPackagedCodexFixture(t)
		require.NoError(t, os.RemoveAll(filepath.Dir(fixture.nativePackageMetadata)))
		requireNPMCodexStageError(t, fixture)
	})

	t.Run("malformed native package metadata", func(t *testing.T) {
		fixture := newNPMPackagedCodexFixture(t)
		require.NoError(t, os.WriteFile(fixture.nativePackageMetadata, []byte("{\n"), 0o600))
		requireNPMCodexStageError(t, fixture)
	})

	t.Run("wrong native package metadata", func(t *testing.T) {
		fixture := newNPMPackagedCodexFixture(t)
		platform, supported := currentNPMCodexPlatform()
		require.True(t, supported)
		writeCodexJSONFixture(t, fixture.nativePackageMetadata, npmCodexPackage{
			Name: codexNPMMainPackageName, Version: "1.2.3-wrong",
			OS: []string{platform.os}, CPU: []string{platform.cpu},
		})
		requireNPMCodexStageError(t, fixture)
	})

	t.Run("missing native manifest", func(t *testing.T) {
		fixture := newNPMPackagedCodexFixture(t)
		require.NoError(t, os.Remove(fixture.manifest))
		requireNPMCodexStageError(t, fixture)
	})

	t.Run("malformed native manifest", func(t *testing.T) {
		fixture := newNPMPackagedCodexFixture(t)
		require.NoError(t, os.WriteFile(fixture.manifest, []byte("{\n"), 0o600))
		requireNPMCodexStageError(t, fixture)
	})

	t.Run("wrong native target", func(t *testing.T) {
		fixture := newNPMPackagedCodexFixture(t)
		platform, supported := currentNPMCodexPlatform()
		require.True(t, supported)
		writeCodexJSONFixture(t, fixture.manifest, codexPackageManifest{
			LayoutVersion: codexPackageLayoutVersion,
			Version:       "1.2.3",
			Target:        platform.target + "-lookalike",
			Variant:       codexPackageVariant,
			Entrypoint:    codexPackageEntrypoint,
			ResourcesDir:  codexPackageResources,
			PathDir:       codexPackagePathDir,
		})
		requireNPMCodexStageError(t, fixture)
	})

	t.Run("native package root escape", func(t *testing.T) {
		fixture := newNPMPackagedCodexFixture(t)
		outside := filepath.Join(t.TempDir(), "native-package")
		require.NoError(t, os.Rename(fixture.nativePackageRoot, outside))
		require.NoError(t, os.Symlink(outside, fixture.nativePackageRoot))
		requireNPMCodexStageError(t, fixture)
	})

	t.Run("missing native package directory", func(t *testing.T) {
		fixture := newNPMPackagedCodexFixture(t)
		require.NoError(t, os.RemoveAll(filepath.Join(fixture.nativePackageRoot, codexPackageResources)))
		requireNPMCodexStageError(t, fixture)
	})

	t.Run("missing code-mode host", func(t *testing.T) {
		fixture := newNPMPackagedCodexFixture(t)
		require.NoError(t, os.Remove(filepath.Join(fixture.nativePackageRoot, codexPackageBin, codexCodeModeHost)))
		requireNPMCodexStageError(t, fixture)
	})

	t.Run("unreadable package path", func(t *testing.T) {
		preservePackagePathHooks(t)
		fixture := newNPMPackagedCodexFixture(t)
		npmCodexPackageReadDir = func(string) ([]os.DirEntry, error) {
			return nil, errors.New("read failed")
		}
		requireNPMCodexStageError(t, fixture)
	})

	t.Run("invalid package path entry", func(t *testing.T) {
		fixture := newNPMPackagedCodexFixture(t)
		require.NoError(t, os.Chmod(
			filepath.Join(fixture.nativePackageRoot, codexPackagePathDir, "rg"), 0o600,
		))
		requireNPMCodexStageError(t, fixture)
	})

	t.Run("native executable escape", func(t *testing.T) {
		fixture := newNPMPackagedCodexFixture(t)
		outside := filepath.Join(t.TempDir(), "codex")
		require.NoError(t, os.WriteFile(outside, []byte("outside\n"), 0o700))
		require.NoError(t, os.Remove(fixture.executable))
		require.NoError(t, os.Symlink(outside, fixture.executable))
		requireNPMCodexStageError(t, fixture)
	})

	t.Run("missing native executable", func(t *testing.T) {
		fixture := newNPMPackagedCodexFixture(t)
		require.NoError(t, os.Remove(fixture.executable))
		requireNPMCodexStageError(t, fixture)
	})

	t.Run("non-executable native executable", func(t *testing.T) {
		fixture := newNPMPackagedCodexFixture(t)
		require.NoError(t, os.Chmod(fixture.executable, 0o600))
		requireNPMCodexStageError(t, fixture)
	})
}

func requireNPMCodexStageError(t *testing.T, fixture npmPackagedCodexFixture) {
	t.Helper()

	staged, env, err := stagePackagedCodex(fixture.invocation, []string{"PATH=/native"}, t.TempDir())
	require.Empty(t, staged)
	require.Nil(t, env)
	require.ErrorContains(t, err, "resolve npm packaged Codex executable")
}

func TestCurrentNPMCodexPlatform(t *testing.T) {
	preservePackagePathHooks(t)

	for _, test := range []struct {
		name     string
		goos     string
		goarch   string
		want     npmCodexPlatform
		wantOkay bool
	}{
		{
			name: "linux amd64", goos: "linux", goarch: "amd64", wantOkay: true,
			want: npmCodexPlatform{
				alias: "codex-linux-x64", cpu: "x64", distribution: "linux-x64", os: "linux",
				target: "x86_64-unknown-linux-musl",
			},
		},
		{
			name: "linux arm64", goos: "linux", goarch: "arm64", wantOkay: true,
			want: npmCodexPlatform{
				alias: "codex-linux-arm64", cpu: "arm64", distribution: "linux-arm64", os: "linux",
				target: "aarch64-unknown-linux-musl",
			},
		},
		{
			name: "darwin amd64", goos: "darwin", goarch: "amd64", wantOkay: true,
			want: npmCodexPlatform{
				alias: "codex-darwin-x64", cpu: "x64", distribution: "darwin-x64", os: "darwin",
				target: "x86_64-apple-darwin",
			},
		},
		{
			name: "darwin arm64", goos: "darwin", goarch: "arm64", wantOkay: true,
			want: npmCodexPlatform{
				alias: "codex-darwin-arm64", cpu: "arm64", distribution: "darwin-arm64", os: "darwin",
				target: "aarch64-apple-darwin",
			},
		},
		{name: "unsupported", goos: "plan9", goarch: "amd64"},
	} {
		t.Run(test.name, func(t *testing.T) {
			npmCodexRuntimeGOOS = test.goos
			npmCodexRuntimeGOARCH = test.goarch
			got, okay := currentNPMCodexPlatform()
			require.Equal(t, test.wantOkay, okay)
			require.Equal(t, test.want, got)
		})
	}
}

func TestResolveExactCodexPackagePathErrors(t *testing.T) {
	missingRoot := filepath.Join(t.TempDir(), "missing")
	_, err := resolveExactCodexPackagePath(missingRoot, filepath.Join(missingRoot, "file"), "file")
	require.Error(t, err)

	root := t.TempDir()
	_, err = resolveExactCodexPackagePath(root, filepath.Join(root, "missing"), "missing")
	require.Error(t, err)
}

func TestResolveNPMCodexAliasRootErrors(t *testing.T) {
	_, err := resolveNPMCodexAliasRoot(t.TempDir(), "codex-linux-x64")
	require.Error(t, err)

	preservePackagePathHooks(t)
	wantErr := errors.New("lstat failed")
	npmCodexPackageLstat = func(string) (os.FileInfo, error) { return nil, wantErr }
	_, err = resolveNPMCodexAliasRoot(t.TempDir(), "codex-linux-x64")
	require.ErrorIs(t, err, wantErr)
}

func TestReadCodexPackageJSONErrors(t *testing.T) {
	t.Run("open", func(t *testing.T) {
		var value map[string]any
		require.Error(t, readCodexPackageJSON(filepath.Join(t.TempDir(), "missing"), &value))
	})

	t.Run("stat", func(t *testing.T) {
		preservePackagePathHooks(t)
		path := filepath.Join(t.TempDir(), "metadata.json")
		require.NoError(t, os.WriteFile(path, []byte("{}\n"), 0o600))
		npmCodexPackageStatJSON = func(*os.File) (os.FileInfo, error) {
			return nil, errors.New("stat failed")
		}
		var value map[string]any
		require.ErrorContains(t, readCodexPackageJSON(path, &value), "stat failed")
	})

	t.Run("not regular", func(t *testing.T) {
		var value map[string]any
		require.ErrorContains(t, readCodexPackageJSON(t.TempDir(), &value), "not a regular file")
	})

	t.Run("stat size", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "metadata.json")
		file, err := os.Create(path)
		require.NoError(t, err)
		require.NoError(t, file.Truncate(codexPackageJSONMaxBytes+1))
		require.NoError(t, file.Close())
		var value map[string]any
		require.ErrorContains(t, readCodexPackageJSON(path, &value), "exceeds the size limit")
	})

	t.Run("read", func(t *testing.T) {
		preservePackagePathHooks(t)
		path := filepath.Join(t.TempDir(), "metadata.json")
		require.NoError(t, os.WriteFile(path, []byte("{}\n"), 0o600))
		npmCodexPackageReadJSON = func(io.Reader) ([]byte, error) {
			return nil, errors.New("read failed")
		}
		var value map[string]any
		require.ErrorContains(t, readCodexPackageJSON(path, &value), "read failed")
	})

	t.Run("read size", func(t *testing.T) {
		preservePackagePathHooks(t)
		path := filepath.Join(t.TempDir(), "metadata.json")
		require.NoError(t, os.WriteFile(path, []byte("{}\n"), 0o600))
		npmCodexPackageReadJSON = func(io.Reader) ([]byte, error) {
			return make([]byte, codexPackageJSONMaxBytes+1), nil
		}
		var value map[string]any
		require.ErrorContains(t, readCodexPackageJSON(path, &value), "exceeds the size limit")
	})

	t.Run("decode", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "metadata.json")
		require.NoError(t, os.WriteFile(path, []byte("{\n"), 0o600))
		var value map[string]any
		require.Error(t, readCodexPackageJSON(path, &value))
	})
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
