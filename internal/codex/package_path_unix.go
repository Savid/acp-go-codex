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
	"strings"

	"golang.org/x/sys/unix"
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
	codexPackageJSONMaxBytes  = 1 << 20
	codexPackageLayoutVersion = 1
	codexPackageVariant       = "codex"
	codexPackageEntrypoint    = "bin/codex"
)

var (
	stagePackagedCodexCopy          = io.Copy
	stagePackagedCodexChmod         = os.Chmod
	stagePackagedCodexLink          = os.Link
	stagePackagedCodexMkdir         = os.MkdirTemp
	stagePackagedCodexOpenFile      = os.OpenFile
	stagePackagedCodexRemove        = os.RemoveAll
	stagePackagedCodexReadDir       = os.ReadDir
	stagePackagedCodexTreeMkdir     = os.Mkdir
	stagePackagedCodexHandoff       = handoffGeneratedNativeTree
	stagePackagedCodexSourceOpen    = unix.Open
	stagePackagedCodexSourceOpenat  = unix.Openat
	stagePackagedCodexSourceFstat   = unix.Fstat
	stagePackagedCodexSourceReadDir = func(directory *os.File) ([]os.DirEntry, error) {
		return directory.ReadDir(-1)
	}
	npmCodexPackageReadDir  = os.ReadDir
	npmCodexPackageReadJSON = io.ReadAll
	npmCodexPackageStatJSON = func(file *os.File) (os.FileInfo, error) { return file.Stat() }
	npmCodexPackageLstat    = os.Lstat
	npmCodexRuntimeGOOS     = runtime.GOOS
	npmCodexRuntimeGOARCH   = runtime.GOARCH
)

func stagePackagedCodex(path string, nativeEnv []string, scratch string) (string, []string, error) {
	staged, env, _, err := stagePackagedCodexForProcess(path, nativeEnv, scratch, "", nil)

	return staged, env, err
}

// stagePackagedCodexForProcess gives an explicitly isolated process its own
// package stage beside, rather than beneath, the private supervisor root. The
// target identity cannot traverse that 0700 supervisor root, and making the
// root broadly traversable would expose unrelated runtime material. A sibling
// can instead be handed to exactly the configured identity while the private
// root remains untouched.
func stagePackagedCodexForProcess(
	path string,
	nativeEnv []string,
	scratch string,
	scratchParent string,
	isolation *ProcessIsolation,
) (staged string, env []string, ownedStageRoot string, returnErr error) {
	source, err := filepath.EvalSymlinks(path)
	if err != nil {
		return "", nil, "", fmt.Errorf("resolve packaged Codex executable: %w", err)
	}

	npmSource, recognizedNPM, err := resolveNPMCodexExecutable(source)
	if err != nil {
		return "", nil, "", fmt.Errorf("resolve npm packaged Codex executable: %w", err)
	}

	if recognizedNPM {
		source = npmSource
	}

	sourceBin := filepath.Dir(source)
	packageRoot := filepath.Dir(sourceBin)
	metadata := filepath.Join(packageRoot, codexPackageMetadata)
	packagePath := filepath.Join(packageRoot, codexPackagePathDir)

	if filepath.Base(sourceBin) != codexPackageBin || !regularFile(metadata) || !directory(packagePath) {
		return path, nativeEnv, "", nil
	}

	var sourcePackage *packagedCodexSource
	if isolation != nil {
		sourcePackage, err = openPackagedCodexSource(packageRoot)
		if err != nil {
			return "", nil, "", fmt.Errorf("open packaged Codex source: %w", err)
		}
		defer sourcePackage.close()

		layoutErr := sourcePackage.validateLayout()
		if layoutErr != nil {
			return "", nil, "", fmt.Errorf("validate packaged Codex source: %w", layoutErr)
		}
	}

	if scratch == "" {
		return "", nil, "", errors.New("stage packaged Codex executable: runtime scratch is required")
	}

	stageParent := scratch

	if isolation != nil {
		if scratchParent == "" {
			return "", nil, "", errors.New("stage packaged Codex executable: runtime scratch parent is required for process isolation")
		}

		stageParent = scratchParent
	}

	stageRoot, err := stagePackagedCodexMkdir(stageParent, "codex-package-")
	if err != nil {
		return "", nil, "", fmt.Errorf("create packaged Codex stage: %w", err)
	}
	defer func() {
		if returnErr != nil && isolation != nil {
			returnErr = errors.Join(
				returnErr,
				packageStageCleanupError(stagePackagedCodexRemove(stageRoot)),
			)
		}
	}()

	if isolation == nil {
		chmodErr := stagePackagedCodexChmod(stageRoot, 0o755)
		if chmodErr != nil {
			return "", nil, "", fmt.Errorf("make packaged Codex stage traversable: %w", chmodErr)
		}
	}

	stagedCodex, stageBin, stageErr := stagePackagedCodexContents(
		source,
		packageRoot,
		packagePath,
		stageRoot,
		sourcePackage,
	)
	if stageErr != nil {
		return "", nil, "", stageErr
	}

	if err := os.WriteFile(filepath.Join(stageRoot, codexPackageMetadata), []byte("{}\n"), 0o600); err != nil {
		return "", nil, "", fmt.Errorf("write staged Codex package metadata: %w", err)
	}

	if isolation != nil {
		if err := stagePackagedCodexHandoff(stageRoot, isolation); err != nil {
			return "", nil, "", fmt.Errorf("handoff packaged Codex stage: %w", err)
		}

		ownedStageRoot = stageRoot
	}

	values := environmentMap(nativeEnv)
	values[pathEnvKey] = composeSearchPath([]string{stageBin}, values[pathEnvKey])

	return stagedCodex, environmentList(values), ownedStageRoot, nil
}

func stagePackagedCodexContents(
	source string,
	packageRoot string,
	packagePath string,
	stageRoot string,
	sourcePackage *packagedCodexSource,
) (string, string, error) {
	stageBin := filepath.Join(stageRoot, codexPackageBin)
	stageMode := os.FileMode(0o755)

	if sourcePackage != nil {
		stageMode = 0o700
	}

	if err := os.MkdirAll(stageBin, stageMode); err != nil {
		return "", "", fmt.Errorf("create packaged Codex stage bin: %w", err)
	}

	stagedCodex := filepath.Join(stageBin, filepath.Base(source))
	if err := stagePackagedCodexExecutable(sourcePackage, source, stagedCodex); err != nil {
		return "", "", fmt.Errorf("stage packaged Codex executable: %w", err)
	}

	sourceBin := filepath.Dir(source)
	if err := stagePackagedCodexFileEntry(
		sourcePackage,
		filepath.Join(sourceBin, codexCodeModeHost),
		filepath.Join(codexPackageBin, codexCodeModeHost),
		filepath.Join(stageBin, codexCodeModeHost),
	); err != nil {
		return "", "", fmt.Errorf("stage packaged Codex entry %q: %w", codexCodeModeHost, err)
	}

	resourcesTarget := filepath.Join(stageRoot, codexPackageResources)
	if err := stagePackagedCodexResources(sourcePackage, packageRoot, resourcesTarget); err != nil {
		return "", "", fmt.Errorf("stage packaged Codex entry %q: %w", codexPackageResources, err)
	}

	entries, err := readPackagedCodexPath(sourcePackage, packagePath)
	if err != nil {
		return "", "", fmt.Errorf("read packaged Codex PATH directory: %w", err)
	}

	for _, entry := range entries {
		entryErr := stagePackagedCodexFileEntry(
			sourcePackage,
			filepath.Join(packagePath, entry.Name()),
			filepath.Join(codexPackagePathDir, entry.Name()),
			filepath.Join(stageBin, entry.Name()),
		)
		if entryErr != nil {
			return "", "", fmt.Errorf("stage packaged Codex entry %q: %w", entry.Name(), entryErr)
		}
	}

	return stagedCodex, stageBin, nil
}

func stagePackagedCodexExecutable(sourcePackage *packagedCodexSource, source string, target string) error {
	if sourcePackage == nil {
		return linkOrCopyExecutable(source, target)
	}

	return sourcePackage.copyRegular(filepath.Join(codexPackageBin, filepath.Base(source)), target, 0o700)
}

func stagePackagedCodexFileEntry(
	sourcePackage *packagedCodexSource,
	source string,
	relative string,
	target string,
) error {
	if sourcePackage == nil {
		return linkRequiredPackageEntry(source, target)
	}

	return sourcePackage.copyRegular(relative, target, 0o700)
}

func stagePackagedCodexResources(
	sourcePackage *packagedCodexSource,
	packageRoot string,
	target string,
) error {
	if sourcePackage == nil {
		return linkRequiredPackageEntry(filepath.Join(packageRoot, codexPackageResources), target)
	}

	return sourcePackage.copyTree(codexPackageResources, target)
}

func readPackagedCodexPath(sourcePackage *packagedCodexSource, packagePath string) ([]os.DirEntry, error) {
	if sourcePackage == nil {
		return stagePackagedCodexReadDir(packagePath)
	}

	return sourcePackage.readDir(codexPackagePathDir)
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
			alias: "codex-linux-x64", cpu: codexNPMCPUX64, distribution: "linux-x64", os: processIsolationLinux,
			target: "x86_64-unknown-linux-musl",
		}, true
	case "linux/arm64":
		return npmCodexPlatform{
			alias: "codex-linux-arm64", cpu: codexNPMCPUARM64, distribution: "linux-arm64", os: processIsolationLinux,
			target: "aarch64-unknown-linux-musl",
		}, true
	case "darwin/amd64":
		return npmCodexPlatform{
			alias: "codex-darwin-x64", cpu: codexNPMCPUX64, distribution: "darwin-x64", os: processIsolationDarwin,
			target: "x86_64-apple-darwin",
		}, true
	case "darwin/arm64":
		return npmCodexPlatform{
			alias: "codex-darwin-arm64", cpu: codexNPMCPUARM64, distribution: "darwin-arm64", os: processIsolationDarwin,
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

type packagedCodexSource struct {
	root *os.File
}

func openPackagedCodexSource(root string) (*packagedCodexSource, error) {
	if !filepath.IsAbs(root) {
		return nil, errors.New("packaged Codex source root must be absolute")
	}

	fd, err := stagePackagedCodexSourceOpen(
		string(filepath.Separator),
		unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC|unix.O_NOFOLLOW,
		0,
	)
	if err != nil {
		return nil, err
	}

	clean := filepath.Clean(root)

	components := strings.Split(strings.TrimPrefix(clean, string(filepath.Separator)), string(filepath.Separator))
	for _, component := range components {
		if component == "" {
			continue
		}

		next, openErr := stagePackagedCodexSourceOpenat(
			fd,
			component,
			unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC|unix.O_NOFOLLOW,
			0,
		)
		if openErr != nil {
			_ = unix.Close(fd)

			return nil, openErr
		}

		_ = unix.Close(fd)
		fd = next
	}

	var stat unix.Stat_t
	if err := stagePackagedCodexSourceFstat(fd, &stat); err != nil {
		_ = unix.Close(fd)

		return nil, err
	}

	if stat.Mode&unix.S_IFMT != unix.S_IFDIR {
		_ = unix.Close(fd)

		return nil, errors.New("packaged Codex source root is not a directory")
	}

	return &packagedCodexSource{root: os.NewFile(uintptr(fd), clean)}, nil
}

func (s *packagedCodexSource) close() {
	if s != nil && s.root != nil {
		_ = s.root.Close()
	}
}

func (s *packagedCodexSource) validateLayout() error {
	metadata, stat, err := s.open(codexPackageMetadata, false)
	if err != nil {
		return err
	}

	metadata.Close()

	if err := validatePackagedCodexRegular(stat); err != nil {
		return fmt.Errorf("validate %s: %w", codexPackageMetadata, err)
	}

	for _, relative := range []string{codexPackageBin, codexPackagePathDir, codexPackageResources} {
		directory, _, err := s.open(relative, true)
		if err != nil {
			return fmt.Errorf("validate %s: %w", relative, err)
		}

		directory.Close()
	}

	return nil
}

func (s *packagedCodexSource) open(relative string, directory bool) (*os.File, unix.Stat_t, error) {
	if s == nil || s.root == nil {
		return nil, unix.Stat_t{}, errors.New("packaged Codex source is unavailable")
	}

	clean := filepath.Clean(relative)
	if !filepath.IsLocal(clean) || clean == "." {
		return nil, unix.Stat_t{}, fmt.Errorf("packaged Codex source path is invalid %q", relative)
	}

	components := strings.Split(clean, string(filepath.Separator))
	parentFD := int(s.root.Fd())
	ownedParent := false

	for index, component := range components {
		last := index == len(components)-1

		flags := unix.O_RDONLY | unix.O_CLOEXEC | unix.O_NOFOLLOW
		if !last || directory {
			flags |= unix.O_DIRECTORY
		} else {
			flags |= unix.O_NONBLOCK
		}

		fd, err := stagePackagedCodexSourceOpenat(parentFD, component, flags, 0)
		if ownedParent {
			_ = unix.Close(parentFD)
		}

		if err != nil {
			return nil, unix.Stat_t{}, err
		}

		parentFD = fd
		ownedParent = true
	}

	var stat unix.Stat_t
	if err := stagePackagedCodexSourceFstat(parentFD, &stat); err != nil {
		_ = unix.Close(parentFD)

		return nil, unix.Stat_t{}, err
	}

	kindMatches := stat.Mode&unix.S_IFMT == unix.S_IFREG
	if directory {
		kindMatches = stat.Mode&unix.S_IFMT == unix.S_IFDIR
	}

	if !kindMatches {
		_ = unix.Close(parentFD)

		return nil, unix.Stat_t{}, fmt.Errorf("packaged Codex source has unsupported type %#o", stat.Mode&unix.S_IFMT)
	}

	return os.NewFile(uintptr(parentFD), clean), stat, nil
}

func validatePackagedCodexRegular(stat unix.Stat_t) error {
	if stat.Mode&unix.S_IFMT != unix.S_IFREG {
		return fmt.Errorf("packaged Codex source has unsupported type %#o", stat.Mode&unix.S_IFMT)
	}

	if stat.Nlink != 1 {
		return fmt.Errorf("packaged Codex source has %d links", stat.Nlink)
	}

	return nil
}

func (s *packagedCodexSource) copyRegular(relative, target string, mode os.FileMode) error {
	input, stat, err := s.open(relative, false)
	if err != nil {
		return err
	}
	defer input.Close()

	if err := validatePackagedCodexRegular(stat); err != nil {
		return err
	}

	return copyOpenPackagedCodexFile(input, target, mode)
}

func (s *packagedCodexSource) readDir(relative string) ([]os.DirEntry, error) {
	directory, _, err := s.open(relative, true)
	if err != nil {
		return nil, err
	}
	defer directory.Close()

	return stagePackagedCodexSourceReadDir(directory)
}

func (s *packagedCodexSource) copyTree(relative, target string) error {
	directory, _, err := s.open(relative, true)
	if err != nil {
		return err
	}
	defer directory.Close()

	return copyOpenPackagedCodexTree(directory, target)
}

func copyOpenPackagedCodexTree(source *os.File, target string) error {
	if err := stagePackagedCodexTreeMkdir(target, 0o700); err != nil {
		return err
	}

	entries, err := stagePackagedCodexSourceReadDir(source)
	if err != nil {
		return err
	}

	for _, entry := range entries {
		fd, openErr := stagePackagedCodexSourceOpenat(
			int(source.Fd()),
			entry.Name(),
			unix.O_RDONLY|unix.O_CLOEXEC|unix.O_NOFOLLOW|unix.O_NONBLOCK,
			0,
		)
		if openErr != nil {
			return fmt.Errorf("open packaged Codex source entry %q: %w", entry.Name(), openErr)
		}

		child := os.NewFile(uintptr(fd), entry.Name())

		var stat unix.Stat_t

		statErr := stagePackagedCodexSourceFstat(fd, &stat)
		if statErr != nil {
			_ = child.Close()

			return statErr
		}

		targetChild := filepath.Join(target, entry.Name())

		var childErr error

		switch stat.Mode & unix.S_IFMT {
		case unix.S_IFDIR:
			childErr = copyOpenPackagedCodexTree(child, targetChild)
		case unix.S_IFREG:
			if childErr = validatePackagedCodexRegular(stat); childErr == nil {
				mode := os.FileMode(0o600)
				if stat.Mode&0o111 != 0 {
					mode = 0o700
				}

				childErr = copyOpenPackagedCodexFile(child, targetChild, mode)
			}
		default:
			childErr = fmt.Errorf("packaged Codex source entry %q has unsupported type %#o", entry.Name(), stat.Mode&unix.S_IFMT)
		}

		closeErr := child.Close()
		if childErr != nil || closeErr != nil {
			return errors.Join(childErr, closeErr)
		}
	}

	return nil
}

func copyOpenPackagedCodexFile(input *os.File, target string, mode os.FileMode) error {
	output, err := stagePackagedCodexOpenFile(target, os.O_WRONLY|os.O_CREATE|os.O_EXCL, mode) // #nosec G304 -- target is inside a fresh package stage.
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
