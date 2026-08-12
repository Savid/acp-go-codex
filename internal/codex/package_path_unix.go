//go:build unix

package codex

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
)

const (
	codexPackageMetadata  = "codex-package.json"
	codexPackagePathDir   = "codex-path"
	codexPackageResources = "codex-resources"
	codexCodeModeHost     = "codex-code-mode-host"
)

var (
	stagePackagedCodexCopy    = io.Copy
	stagePackagedCodexChmod   = os.Chmod
	stagePackagedCodexLink    = os.Link
	stagePackagedCodexMkdir   = os.MkdirTemp
	stagePackagedCodexReadDir = os.ReadDir
)

func stagePackagedCodex(path string, nativeEnv []string, scratch string) (string, []string, error) {
	source, err := filepath.EvalSymlinks(path)
	if err != nil {
		return "", nil, fmt.Errorf("resolve packaged Codex executable: %w", err)
	}

	sourceBin := filepath.Dir(source)
	packageRoot := filepath.Dir(sourceBin)
	metadata := filepath.Join(packageRoot, codexPackageMetadata)
	packagePath := filepath.Join(packageRoot, codexPackagePathDir)

	if filepath.Base(sourceBin) != "bin" || !regularFile(metadata) || !directory(packagePath) {
		return path, nativeEnv, nil
	}

	if scratch == "" {
		return "", nil, errors.New("stage packaged Codex executable: runtime scratch is required")
	}

	stageRoot, err := stagePackagedCodexMkdir(scratch, "codex-package-")
	if err != nil {
		return "", nil, fmt.Errorf("create packaged Codex stage: %w", err)
	}

	chmodErr := stagePackagedCodexChmod(stageRoot, 0o755)
	if chmodErr != nil {
		return "", nil, fmt.Errorf("make packaged Codex stage traversable: %w", chmodErr)
	}

	stageBin := filepath.Join(stageRoot, "bin")

	mkdirErr := os.MkdirAll(stageBin, 0o755)
	if mkdirErr != nil {
		return "", nil, fmt.Errorf("create packaged Codex stage bin: %w", mkdirErr)
	}

	stagedCodex := filepath.Join(stageBin, filepath.Base(source))

	stageErr := linkOrCopyExecutable(source, stagedCodex)
	if stageErr != nil {
		return "", nil, fmt.Errorf("stage packaged Codex executable: %w", stageErr)
	}

	codeModeErr := linkRequiredPackageEntry(filepath.Join(sourceBin, codexCodeModeHost), filepath.Join(stageBin, codexCodeModeHost))
	if codeModeErr != nil {
		return "", nil, codeModeErr
	}

	resourcesErr := linkRequiredPackageEntry(filepath.Join(packageRoot, codexPackageResources), filepath.Join(stageRoot, codexPackageResources))
	if resourcesErr != nil {
		return "", nil, resourcesErr
	}

	entries, err := stagePackagedCodexReadDir(packagePath)
	if err != nil {
		return "", nil, fmt.Errorf("read packaged Codex PATH directory: %w", err)
	}

	for _, entry := range entries {
		if err := linkRequiredPackageEntry(filepath.Join(packagePath, entry.Name()), filepath.Join(stageBin, entry.Name())); err != nil {
			return "", nil, err
		}
	}

	if err := os.WriteFile(filepath.Join(stageRoot, codexPackageMetadata), []byte("{}\n"), 0o600); err != nil {
		return "", nil, fmt.Errorf("write staged Codex package metadata: %w", err)
	}

	values := environmentMap(nativeEnv)
	values[pathEnvKey] = composeSearchPath([]string{stageBin}, values[pathEnvKey])

	return stagedCodex, environmentList(values), nil
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
