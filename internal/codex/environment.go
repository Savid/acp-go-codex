package codex

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
)

const (
	privateAdapterEnvPrefix     = "ACP_GO_CODEX_INTERNAL_"
	platformWindows             = "windows"
	ordinaryWindowsExtensionCOM = ".com"
	ordinaryWindowsExtensionEXE = ".exe"
	ordinaryWindowsExtensionBAT = ".bat"
	ordinaryWindowsExtensionCMD = ".cmd"
)

var processGOOS = runtime.GOOS
var processEnviron = os.Environ
var ordinaryExecutableAbs = filepath.Abs

func validateEnvironmentMap(environment map[string]string) error {
	if environment == nil {
		return errors.New("native base environment is unavailable")
	}

	for key, value := range environment {
		if key == "" || strings.ContainsAny(key, "=\x00") || strings.ContainsRune(value, '\x00') {
			return fmt.Errorf("invalid environment entry for %q", key)
		}
	}

	return nil
}

func environmentMap(entries []string) map[string]string {
	values := make(map[string]string, len(entries))
	for _, entry := range entries {
		key, value, ok := strings.Cut(entry, "=")
		if ok && key != "" {
			values[key] = value
		}
	}

	return values
}

func environmentList(values map[string]string) []string {
	keys := make([]string, 0, len(values))
	for key := range values {
		keys = append(keys, key)
	}

	sort.Strings(keys)

	environment := make([]string, 0, len(keys))
	for _, key := range keys {
		environment = append(environment, key+"="+values[key])
	}

	return environment
}

func buildProcessEnvironmentFrom(base map[string]string, overlays ...map[string]string) ([]string, error) {
	if base == nil {
		base = captureProcessEnvironment()
	}

	if err := validateEnvironmentMap(base); err != nil {
		return nil, err
	}

	values := cloneEnvironment(base)

	for _, overlay := range overlays {
		if overlay == nil {
			continue
		}

		if err := validateEnvironmentMap(overlay); err != nil {
			return nil, err
		}

		for key, value := range overlay {
			values[key] = value
		}
	}

	for key := range values {
		if privateProcessEnvironmentKey(key) {
			delete(values, key)
		}
	}

	return environmentList(values), nil
}

func privateProcessEnvironmentKey(key string) bool {
	return strings.HasPrefix(strings.ToUpper(key), privateAdapterEnvPrefix)
}

func captureProcessEnvironment() map[string]string { return environmentMap(processEnviron()) }

func cloneEnvironment(environment map[string]string) map[string]string {
	if environment == nil {
		return nil
	}

	cloned := make(map[string]string, len(environment))
	for key, value := range environment {
		cloned[key] = value
	}

	return cloned
}

func withoutManagedRootOverrides(environment map[string]string) map[string]string {
	filtered := make(map[string]string, len(environment))
	for key, value := range environment {
		switch strings.ToUpper(key) {
		case envCodexHome, envHome, "XDG_CACHE_HOME", envXDGConfigHome, "XDG_DATA_HOME", "XDG_RUNTIME_DIR", "XDG_STATE_HOME":
			continue
		default:
			filtered[key] = value
		}
	}

	return filtered
}

func resolveOrdinaryProcessExecutable(path string, environment []string) (string, error) {
	if strings.TrimSpace(path) == "" {
		return "", errors.New("executable path is empty")
	}

	if strings.ContainsAny(path, `/\`) {
		absolute, err := ordinaryExecutableAbs(path)
		if err != nil {
			return "", err
		}

		return resolveOrdinaryExecutableCandidate(absolute, environment)
	}

	values := environmentMap(environment)

	search := ordinaryEnvironmentValue(values, "PATH")
	if search == "" {
		return "", fmt.Errorf("find %s: PATH is empty", path)
	}

	for _, directory := range filepath.SplitList(search) {
		if directory == "" {
			directory = "."
		}

		candidate, err := ordinaryExecutableAbs(filepath.Join(directory, path))
		if err != nil {
			return "", err
		}

		resolved, err := resolveOrdinaryExecutableCandidate(candidate, environment)
		if err == nil {
			return resolved, nil
		}

		if !errors.Is(err, os.ErrNotExist) && !errors.Is(err, exec.ErrNotFound) {
			return "", err
		}
	}

	return "", fmt.Errorf("find %s in PATH: %w", path, exec.ErrNotFound)
}

func ordinaryEnvironmentValue(values map[string]string, key string) string {
	if value, ok := values[key]; ok || processGOOS != platformWindows {
		return value
	}

	for candidate, value := range values {
		if strings.EqualFold(candidate, key) {
			return value
		}
	}

	return ""
}

func resolveOrdinaryExecutableCandidate(path string, environment []string) (string, error) {
	if processGOOS != platformWindows {
		info, err := os.Stat(path)
		if err != nil {
			return "", err
		}

		if info.IsDir() || info.Mode()&0o111 == 0 {
			return "", exec.ErrNotFound
		}

		return path, nil
	}

	values := environmentMap(environment)

	candidates := []string{path}
	if filepath.Ext(path) == "" {
		for _, extension := range ordinaryWindowsExecutableExtensions(ordinaryEnvironmentValue(values, "PATHEXT")) {
			candidates = append(candidates, path+extension)
		}
	}

	for _, candidate := range candidates {
		info, err := os.Stat(candidate)
		if err == nil && !info.IsDir() {
			return candidate, nil
		}

		if err != nil && !errors.Is(err, os.ErrNotExist) {
			return "", err
		}
	}

	return "", exec.ErrNotFound
}

func ordinaryWindowsExecutableExtensions(value string) []string {
	if value == "" {
		return []string{ordinaryWindowsExtensionCOM, ordinaryWindowsExtensionEXE, ordinaryWindowsExtensionBAT, ordinaryWindowsExtensionCMD}
	}

	extensions := make([]string, 0)

	for _, extension := range strings.Split(value, ";") {
		if extension == "" {
			continue
		}

		if extension[0] != '.' {
			extension = "." + extension
		}

		extensions = append(extensions, strings.ToLower(extension))
	}

	return extensions
}
