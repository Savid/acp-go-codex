//go:build unix

package codex

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
)

const (
	codexPackageMetadata      = "codex-package.json"
	codexPackagePathDir       = "codex-path"
	codexPackageResources     = "codex-resources"
	codexCodeModeHost         = "codex-code-mode-host"
	codexPackageBin           = "bin"
	codexPackageExecutable    = "codex"
	codexNPMMainPackageName   = "@openai/codex"
	codexNPMShimName          = "codex.js"
	codexNPMCPUX64            = "x64"
	codexNPMCPUARM64          = "arm64"
	codexNPMOSLinux           = "linux"
	codexNPMOSDarwin          = "darwin"
	codexPackageJSONMaxBytes  = 1 << 20
	codexPackageLayoutVersion = 1
	codexPackageVariant       = "codex"
	codexPackageEntrypoint    = "bin/codex"
)

var (
	stagePackagedCodexCopy    = io.Copy
	stagePackagedCodexChmod   = os.Chmod
	stagePackagedCodexLink    = os.Link
	stagePackagedCodexMkdir   = os.MkdirTemp
	stagePackagedCodexReadDir = os.ReadDir
	npmCodexPackageReadDir    = os.ReadDir
	npmCodexPackageReadJSON   = io.ReadAll
	npmCodexPackageStatJSON   = func(file *os.File) (os.FileInfo, error) { return file.Stat() }
	npmCodexPackageLstat      = os.Lstat
	npmCodexRuntimeGOOS       = runtime.GOOS
	npmCodexRuntimeGOARCH     = runtime.GOARCH
)

func stagePackagedCodex(path string, nativeEnv []string, scratch string) (string, []string, func() error, error) {
	source, err := filepath.EvalSymlinks(path)
	if err != nil {
		return "", nil, nil, fmt.Errorf("resolve packaged Codex executable: %w", err)
	}

	npmSource, recognizedNPM, err := resolveNPMCodexExecutable(source)
	if err != nil {
		return "", nil, nil, fmt.Errorf("resolve npm packaged Codex executable: %w", err)
	}

	if recognizedNPM {
		source = npmSource
	}

	sourceBin := filepath.Dir(source)
	packageRoot := filepath.Dir(sourceBin)
	metadata := filepath.Join(packageRoot, codexPackageMetadata)
	packagePath := filepath.Join(packageRoot, codexPackagePathDir)

	if filepath.Base(sourceBin) != codexPackageBin || !regularFile(metadata) || !directory(packagePath) {
		return path, nativeEnv, func() error { return nil }, nil
	}

	if scratch == "" {
		return "", nil, nil, errors.New("stage packaged Codex executable: runtime scratch is required")
	}

	stageRoot, err := stagePackagedCodexMkdir(scratch, "codex-package-")
	if err != nil {
		return "", nil, nil, fmt.Errorf("create packaged Codex stage: %w", err)
	}

	cleanup := func() error { return os.RemoveAll(stageRoot) }

	if chmodErr := stagePackagedCodexChmod(stageRoot, 0o755); chmodErr != nil {
		return "", nil, nil, errors.Join(
			fmt.Errorf("make packaged Codex stage traversable: %w", chmodErr), cleanup(),
		)
	}

	stagedCodex, stageBin, stageErr := stagePackagedCodexContents(
		source,
		packageRoot,
		packagePath,
		stageRoot,
	)
	if stageErr != nil {
		return "", nil, nil, errors.Join(stageErr, cleanup())
	}

	if err := os.WriteFile(filepath.Join(stageRoot, codexPackageMetadata), []byte("{}\n"), 0o600); err != nil {
		return "", nil, nil, errors.Join(
			fmt.Errorf("write staged Codex package metadata: %w", err), cleanup(),
		)
	}

	values := environmentMap(nativeEnv)
	values[pathEnvKey] = composeSearchPath([]string{stageBin}, values[pathEnvKey])

	return stagedCodex, environmentList(values), cleanup, nil
}

func stagePackagedCodexContents(
	source string,
	packageRoot string,
	packagePath string,
	stageRoot string,
) (string, string, error) {
	stageBin := filepath.Join(stageRoot, codexPackageBin)

	if err := os.MkdirAll(stageBin, 0o755); err != nil {
		return "", "", fmt.Errorf("create packaged Codex stage bin: %w", err)
	}

	stagedCodex := filepath.Join(stageBin, filepath.Base(source))
	if err := linkOrCopyExecutable(source, stagedCodex); err != nil {
		return "", "", fmt.Errorf("stage packaged Codex executable: %w", err)
	}

	sourceBin := filepath.Dir(source)
	if err := stagePackagedCodexFileEntry(
		filepath.Join(sourceBin, codexCodeModeHost),
		filepath.Join(stageBin, codexCodeModeHost),
	); err != nil {
		return "", "", fmt.Errorf("stage packaged Codex entry %q: %w", codexCodeModeHost, err)
	}

	resourcesTarget := filepath.Join(stageRoot, codexPackageResources)
	if err := linkRequiredPackageEntry(filepath.Join(packageRoot, codexPackageResources), resourcesTarget); err != nil {
		return "", "", fmt.Errorf("stage packaged Codex entry %q: %w", codexPackageResources, err)
	}

	entries, err := stagePackagedCodexReadDir(packagePath)
	if err != nil {
		return "", "", fmt.Errorf("read packaged Codex PATH directory: %w", err)
	}

	for _, entry := range entries {
		entryErr := stagePackagedCodexFileEntry(
			filepath.Join(packagePath, entry.Name()),
			filepath.Join(stageBin, entry.Name()),
		)
		if entryErr != nil {
			return "", "", fmt.Errorf("stage packaged Codex entry %q: %w", entry.Name(), entryErr)
		}
	}

	return stagedCodex, stageBin, nil
}

func stagePackagedCodexFileEntry(
	source string,
	target string,
) error {
	return linkRequiredPackageEntry(source, target)
}

type npmCodexPlatform struct {
	alias        string
	cpu          string
	distribution string
	os           string
	target       string
}

type npmCodexPackage struct {
	Name                 string            `json:"name"`
	Version              string            `json:"version"`
	Bin                  map[string]string `json:"bin"`
	CPU                  []string          `json:"cpu"`
	OS                   []string          `json:"os"`
	OptionalDependencies map[string]string `json:"optionalDependencies"`
}

type codexPackageManifest struct {
	LayoutVersion int    `json:"layoutVersion"`
	Version       string `json:"version"`
	Target        string `json:"target"`
	Variant       string `json:"variant"`
	Entrypoint    string `json:"entrypoint"`
	ResourcesDir  string `json:"resourcesDir"`
	PathDir       string `json:"pathDir"`
}

func resolveNPMCodexExecutable(source string) (string, bool, error) {
	packageRoot, recognized := npmCodexShimPackageRoot(source)
	if !recognized {
		return "", false, nil
	}

	mainPackagePath, err := resolveExactCodexPackagePath(
		packageRoot, filepath.Join(packageRoot, "package.json"), "package.json",
	)
	if err != nil {
		return "", true, fmt.Errorf("resolve npm Codex package metadata: %w", err)
	}

	mainPackage := npmCodexPackage{}

	readErr := readCodexPackageJSON(mainPackagePath, &mainPackage)
	if readErr != nil {
		return "", true, fmt.Errorf("read npm Codex package metadata: %w", readErr)
	}

	if validationErr := validateNPMCodexShimPackage(mainPackage); validationErr != nil {
		return "", true, validationErr
	}

	platform, supported := currentNPMCodexPlatform()
	if !supported {
		return "", true, fmt.Errorf("npm Codex package is unsupported on %s/%s", npmCodexRuntimeGOOS, npmCodexRuntimeGOARCH)
	}

	if validationErr := validateNPMCodexPlatformPin(mainPackage, platform); validationErr != nil {
		return "", true, validationErr
	}

	aliasRoot, err := resolveNPMCodexAliasRoot(packageRoot, platform.alias)
	if err != nil {
		return "", true, fmt.Errorf("resolve npm Codex native package: %w", err)
	}

	optionalPackagePath, err := resolveExactCodexPackagePath(
		aliasRoot, filepath.Join(aliasRoot, "package.json"), "package.json",
	)
	if err != nil {
		return "", true, fmt.Errorf("resolve npm Codex native package metadata: %w", err)
	}

	optionalPackage := npmCodexPackage{}

	readErr = readCodexPackageJSON(optionalPackagePath, &optionalPackage)
	if readErr != nil {
		return "", true, fmt.Errorf("read npm Codex native package metadata: %w", readErr)
	}

	if optionalPackage.Name != codexNPMMainPackageName ||
		optionalPackage.Version != mainPackage.Version+"-"+platform.distribution ||
		len(optionalPackage.OS) != 1 || optionalPackage.OS[0] != platform.os ||
		len(optionalPackage.CPU) != 1 || optionalPackage.CPU[0] != platform.cpu {
		return "", true, errors.New("npm Codex native package metadata does not match the official platform package")
	}

	nativeRelative := filepath.Join("vendor", platform.target)

	nativeRoot, err := resolveExactCodexPackagePath(
		aliasRoot, filepath.Join(aliasRoot, nativeRelative), nativeRelative,
	)
	if err != nil {
		return "", true, fmt.Errorf("resolve npm Codex native package root: %w", err)
	}

	manifestPath, err := resolveExactCodexPackagePath(
		nativeRoot, filepath.Join(nativeRoot, codexPackageMetadata), codexPackageMetadata,
	)
	if err != nil {
		return "", true, fmt.Errorf("resolve npm Codex native package manifest: %w", err)
	}

	manifest := codexPackageManifest{}

	readErr = readCodexPackageJSON(manifestPath, &manifest)
	if readErr != nil {
		return "", true, fmt.Errorf("read npm Codex native package manifest: %w", readErr)
	}

	if manifest.LayoutVersion != codexPackageLayoutVersion || manifest.Version != mainPackage.Version ||
		manifest.Target != platform.target || manifest.Variant != codexPackageVariant ||
		manifest.Entrypoint != codexPackageEntrypoint || manifest.ResourcesDir != codexPackageResources ||
		manifest.PathDir != codexPackagePathDir {
		return "", true, errors.New("npm Codex native package manifest does not match the official layout")
	}

	layoutErr := validateNPMCodexPackageLayout(nativeRoot)
	if layoutErr != nil {
		return "", true, layoutErr
	}

	executable, err := resolveExactCodexPackagePath(
		nativeRoot, filepath.Join(nativeRoot, filepath.FromSlash(manifest.Entrypoint)),
		filepath.FromSlash(manifest.Entrypoint),
	)
	if err != nil {
		return "", true, fmt.Errorf("resolve npm Codex native executable: %w", err)
	}

	if err := validateCodexPackageExecutable(executable); err != nil {
		return "", true, fmt.Errorf("validate npm Codex native executable: %w", err)
	}

	return executable, true, nil
}

func validateNPMCodexShimPackage(metadata npmCodexPackage) error {
	if metadata.Name != codexNPMMainPackageName || metadata.Version == "" ||
		metadata.Bin[codexPackageExecutable] != filepath.ToSlash(filepath.Join(codexPackageBin, codexNPMShimName)) {
		return errors.New("npm Codex package metadata does not describe the official shim")
	}

	return nil
}

func validateNPMCodexPlatformPin(metadata npmCodexPackage, platform npmCodexPlatform) error {
	want := "npm:" + codexNPMMainPackageName + "@" + metadata.Version + "-" + platform.distribution
	if metadata.OptionalDependencies["@openai/"+platform.alias] != want {
		return errors.New("npm Codex package metadata does not pin the official platform package")
	}

	return nil
}

func resolveNPMCodexAliasRoot(packageRoot, alias string) (string, error) {
	candidates := []string{
		filepath.Join(packageRoot, "node_modules", "@openai", alias),
		filepath.Join(filepath.Dir(packageRoot), alias),
	}
	for _, candidate := range candidates {
		_, err := npmCodexPackageLstat(candidate)
		if err == nil {
			return filepath.EvalSymlinks(candidate)
		}

		if !errors.Is(err, os.ErrNotExist) {
			return "", err
		}
	}

	return "", fmt.Errorf("official platform package %q is unavailable", alias)
}

func npmCodexShimPackageRoot(source string) (string, bool) {
	shimBin := filepath.Dir(source)

	packageRoot := filepath.Dir(shimBin)
	if filepath.Base(source) != codexNPMShimName || filepath.Base(shimBin) != codexPackageBin ||
		filepath.Base(packageRoot) != codexPackageExecutable || filepath.Base(filepath.Dir(packageRoot)) != "@openai" {
		return "", false
	}

	return packageRoot, true
}

func currentNPMCodexPlatform() (npmCodexPlatform, bool) {
	switch npmCodexRuntimeGOOS + "/" + npmCodexRuntimeGOARCH {
	case "linux/amd64":
		return npmCodexPlatform{
			alias: "codex-linux-x64", cpu: codexNPMCPUX64, distribution: "linux-x64", os: codexNPMOSLinux,
			target: "x86_64-unknown-linux-musl",
		}, true
	case "linux/arm64":
		return npmCodexPlatform{
			alias: "codex-linux-arm64", cpu: codexNPMCPUARM64, distribution: "linux-arm64", os: codexNPMOSLinux,
			target: "aarch64-unknown-linux-musl",
		}, true
	case "darwin/amd64":
		return npmCodexPlatform{
			alias: "codex-darwin-x64", cpu: codexNPMCPUX64, distribution: "darwin-x64", os: codexNPMOSDarwin,
			target: "x86_64-apple-darwin",
		}, true
	case "darwin/arm64":
		return npmCodexPlatform{
			alias: "codex-darwin-arm64", cpu: codexNPMCPUARM64, distribution: "darwin-arm64", os: codexNPMOSDarwin,
			target: "aarch64-apple-darwin",
		}, true
	default:
		return npmCodexPlatform{}, false
	}
}

func resolveExactCodexPackagePath(root, path, expectedRelative string) (string, error) {
	resolvedRoot, err := filepath.EvalSymlinks(root)
	if err != nil {
		return "", err
	}

	resolved, err := filepath.EvalSymlinks(path)
	if err != nil {
		return "", err
	}

	// filepath.Rel cannot fail for two resolved absolute paths on Unix.
	relative, _ := filepath.Rel(resolvedRoot, resolved)

	if relative != filepath.Clean(expectedRelative) || !filepath.IsLocal(relative) {
		return "", fmt.Errorf("package path escapes its declared layout %q", relative)
	}

	return resolved, nil
}

func readCodexPackageJSON(path string, target any) error {
	file, err := os.Open(path) // #nosec G304 -- callers first confine derived npm paths to the configured package.
	if err != nil {
		return err
	}
	defer func() { _ = file.Close() }()

	info, statErr := npmCodexPackageStatJSON(file)
	if statErr != nil || !info.Mode().IsRegular() {
		return errors.Join(statErr, fmt.Errorf("package metadata is not a regular file %q", path))
	}

	if info.Size() > codexPackageJSONMaxBytes {
		return fmt.Errorf("package metadata exceeds the size limit %q", path)
	}

	data, readErr := npmCodexPackageReadJSON(io.LimitReader(file, codexPackageJSONMaxBytes+1))
	if readErr != nil {
		return readErr
	}

	if len(data) > codexPackageJSONMaxBytes {
		return fmt.Errorf("package metadata exceeds the size limit %q", path)
	}

	if err := json.Unmarshal(data, target); err != nil {
		return err
	}

	return nil
}

func validateNPMCodexPackageLayout(root string) error {
	for _, path := range []string{codexPackageBin, codexPackagePathDir, codexPackageResources} {
		info, err := os.Lstat(filepath.Join(root, path))
		if err != nil || !info.IsDir() {
			return errors.Join(err, fmt.Errorf("npm Codex native package entry is not a directory %q", path))
		}
	}

	if err := validateCodexPackageExecutable(filepath.Join(root, codexPackageBin, codexCodeModeHost)); err != nil {
		return fmt.Errorf("validate npm Codex code-mode host: %w", err)
	}

	entries, err := npmCodexPackageReadDir(filepath.Join(root, codexPackagePathDir))
	if err != nil {
		return err
	}

	for _, entry := range entries {
		if err := validateCodexPackageExecutable(filepath.Join(root, codexPackagePathDir, entry.Name())); err != nil {
			return fmt.Errorf("validate npm Codex PATH entry %q: %w", entry.Name(), err)
		}
	}

	return nil
}

func validateCodexPackageExecutable(path string) error {
	info, err := os.Lstat(path)
	if err != nil {
		return err
	}

	if !info.Mode().IsRegular() || info.Mode()&0o111 == 0 {
		return fmt.Errorf("package executable is not a regular executable %q", path)
	}

	return nil
}

func regularFile(path string) bool {
	info, err := os.Stat(path)

	return err == nil && info.Mode().IsRegular()
}

func directory(path string) bool {
	info, err := os.Stat(path)

	return err == nil && info.IsDir()
}

func linkRequiredPackageEntry(source, target string) error {
	if err := os.Symlink(source, target); err != nil {
		return fmt.Errorf("link packaged Codex entry %q: %w", filepath.Base(source), err)
	}

	return nil
}

func linkOrCopyExecutable(source, target string) error {
	if err := stagePackagedCodexLink(source, target); err == nil {
		return nil
	}

	input, err := os.Open(source) // #nosec G304 -- source is the validated configured Codex executable.
	if err != nil {
		return err
	}
	defer input.Close()

	output, err := os.OpenFile(target, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o755) // #nosec G304 -- target is inside the private runtime scratch root.
	if err != nil {
		return err
	}

	_, copyErr := stagePackagedCodexCopy(output, input)

	closeErr := output.Close()

	if err := errors.Join(copyErr, closeErr); err != nil {
		_ = os.Remove(target)

		return err
	}

	return nil
}
