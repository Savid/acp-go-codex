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
	"unicode"
	"unicode/utf8"
)

const standaloneAuthorityRoot = "/var/lib/acp-go/agent-identities"

const processIsolationLinux = "linux"
const processIsolationWindows = "windows"
const processIsolationDarwin = "darwin"

const (
	ordinaryWindowsExtensionCOM = ".com"
	ordinaryWindowsExtensionEXE = ".exe"
	ordinaryWindowsExtensionBAT = ".bat"
	ordinaryWindowsExtensionCMD = ".cmd"
)

var (
	processIsolationGOOS  = runtime.GOOS
	processEnviron        = os.Environ
	ordinaryExecutableAbs = filepath.Abs
)

func validateProcessIsolation(isolation *ProcessIsolation) error {
	if isolation == nil {
		return nil
	}

	if isolation.UID == 0 || isolation.GID == 0 {
		return errors.New("process isolation uid and gid must be nonzero")
	}

	if isolation.BaseEnvironment == nil {
		return errors.New("process isolation base environment is required")
	}

	if err := validateEnvironmentMap(isolation.BaseEnvironment); err != nil {
		return fmt.Errorf("validate process isolation base environment: %w", err)
	}

	for key := range isolation.BaseEnvironment {
		if privateProcessEnvironmentKey(key) {
			return fmt.Errorf("process isolation base environment key %q is reserved", key)
		}
	}

	return errors.Join(processIsolationValidateIdentity(isolation), validateProcessIsolationPlatform())
}

func validateProcessIsolationPlatform() error {
	if processIsolationGOOS != processIsolationLinux {
		return errors.New("explicit process isolation is supported only on linux")
	}

	return nil
}

// Seam for the platform's identity disposition validator. Only Linux owns an
// agent identity lock and an authority domain, so only the Linux build has a
// disposition to enforce; every other platform accepts whatever it is handed.
// supervisorCommand makes its own check that the pair arrives together, and on
// Linux this validator has always refused first, so the supervisor's own check
// is the only one there is off Linux and cannot be reached here. Tests swap
// this for the answer every other platform gives so that check can be proved.
var processIsolationValidateIdentity = validateProcessIsolationIdentity

func validateStandaloneIdentityDisposition(isolation *ProcessIsolation) error {
	identityLock := isolation.IdentityLock != nil

	authorityDomain := isolation.AuthorityDomain != nil

	if isolation.identityAuthorityAdopted {
		if identityLock || authorityDomain {
			return errors.New("adopted process identity authority cannot carry duplicable capabilities")
		}

		identityLock = true
		authorityDomain = true
	}

	if identityLock != authorityDomain {
		return errors.New("process identity lock and authority domain must be provided together")
	}

	if identityLock {
		if isolation.StandaloneOwnerID != "" || isolation.StandaloneStateRoot != "" {
			return errors.New("borrowed process identity forbids standalone owner fields")
		}

		return nil
	}

	if !validStandaloneOwnerID(isolation.StandaloneOwnerID) {
		return errors.New("standalone owner id must be 1..256 canonical ASCII bytes")
	}

	if !validStandaloneStateRootPath(isolation.StandaloneStateRoot) {
		return errors.New("standalone state root must be a clean absolute path outside the authority root")
	}

	return nil
}

func validStandaloneOwnerID(value string) bool {
	if value == "" || len(value) > 256 || !standaloneOwnerIDAlphanumeric(value[0]) {
		return false
	}

	for index := 1; index < len(value); index++ {
		if !standaloneOwnerIDAlphanumeric(value[index]) && !strings.ContainsRune("._:@/-", rune(value[index])) {
			return false
		}
	}

	return true
}

func standaloneOwnerIDAlphanumeric(value byte) bool {
	return value >= 'A' && value <= 'Z' || value >= 'a' && value <= 'z' || value >= '0' && value <= '9'
}

func validStandaloneStateRootPath(value string) bool {
	if value == "" || len(value) > 4096 || !utf8.ValidString(value) || !filepath.IsAbs(value) ||
		filepath.Clean(value) != value || value == "/" || strings.IndexByte(value, 0) >= 0 {
		return false
	}

	if value == standaloneAuthorityRoot || strings.HasPrefix(value, standaloneAuthorityRoot+string(filepath.Separator)) {
		return false
	}

	for _, character := range value {
		if unicode.IsControl(character) {
			return false
		}
	}

	return true
}

func validateEnvironmentMap(environment map[string]string) error {
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

	env := make([]string, 0, len(keys))
	for _, key := range keys {
		env = append(env, key+"="+values[key])
	}

	return env
}

func buildProcessEnvironment(isolation *ProcessIsolation, overlays ...map[string]string) ([]string, error) {
	return buildProcessEnvironmentFrom(isolation, nil, overlays...)
}

func buildProcessEnvironmentFrom(
	isolation *ProcessIsolation,
	implicitEnvironment map[string]string,
	overlays ...map[string]string,
) ([]string, error) {
	if err := validateProcessIsolation(isolation); err != nil {
		return nil, err
	}

	base := implicitEnvironment
	if isolation != nil {
		base = isolation.BaseEnvironment
	} else if base == nil {
		base = captureProcessEnvironment()
	}

	if isolation == nil {
		base = withoutManagedRootOverrides(base)
	}

	values := cloneEnvironment(base)

	for _, overlay := range overlays {
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

	if isolation != nil {
		if err := validateProcessSearchPath(values["PATH"]); err != nil {
			return nil, err
		}
	}

	return environmentList(values), nil
}

func privateProcessEnvironmentKey(key string) bool {
	upperKey := strings.ToUpper(key)

	return strings.HasPrefix(upperKey, privateAdapterEnvPrefix) ||
		upperKey == DarwinRuntimeIDEnv || upperKey == DarwinScratchRootEnv
}

func captureProcessEnvironment() map[string]string {
	return environmentMap(processEnviron())
}

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

func ordinaryProcessBackend(isolation *ProcessIsolation, darwinBestEffort bool) bool {
	return isolation == nil && processIsolationGOOS != processIsolationLinux && !darwinBestEffort
}

func validateSupervisorIdentityDisposition(config supervisorConfig) error {
	if !config.OrdinaryExecution {
		return nil
	}

	uid, gid, err := currentProcessIdentity()
	if err != nil {
		return err
	}

	if config.IsolationUID != uid || config.IsolationGID != gid ||
		config.IdentityLock || config.AuthorityDomain || config.StandaloneAuthority ||
		config.StandaloneOwnerID != "" || config.StandaloneStateRoot != "" {
		return errors.New("codex ordinary supervisor identity disposition is invalid")
	}

	return nil
}

func withoutManagedRootOverrides(env map[string]string) map[string]string {
	filtered := make(map[string]string, len(env))
	for key, value := range env {
		switch strings.ToUpper(key) {
		case envCodexHome, envHome,
			"XDG_CACHE_HOME", envXDGConfigHome, "XDG_DATA_HOME", "XDG_RUNTIME_DIR", "XDG_STATE_HOME":
			continue
		default:
			filtered[key] = value
		}
	}

	return filtered
}

func validateProcessSearchPath(search string) error {
	if search == "" {
		return nil
	}

	for _, directory := range filepath.SplitList(search) {
		if directory == "" || !filepath.IsAbs(directory) {
			return fmt.Errorf("process isolation PATH contains non-absolute entry %q", directory)
		}
	}

	return nil
}

func resolveProcessExecutable(path string, env []string) (string, error) {
	if strings.TrimSpace(path) == "" {
		return "", errors.New("executable path is empty")
	}

	if strings.ContainsRune(path, filepath.Separator) {
		if !filepath.IsAbs(path) {
			return "", fmt.Errorf("executable path %q is not absolute", path)
		}

		info, err := os.Stat(path)
		if err != nil {
			return "", fmt.Errorf("stat executable %q: %w", path, err)
		}

		if info.IsDir() || info.Mode()&0o111 == 0 {
			return "", fmt.Errorf("executable %q is not executable", path)
		}

		return path, nil
	}

	search := environmentMap(env)["PATH"]
	if search == "" {
		return "", fmt.Errorf("find %s: process isolation PATH is empty", path)
	}

	if err := validateProcessSearchPath(search); err != nil {
		return "", fmt.Errorf("find %s: %w", path, err)
	}

	for _, directory := range filepath.SplitList(search) {
		candidate := filepath.Join(directory, path)

		info, err := os.Stat(candidate)
		if err == nil && !info.IsDir() && info.Mode()&0o111 != 0 {
			return candidate, nil
		}

		if err != nil && !errors.Is(err, os.ErrNotExist) {
			return "", fmt.Errorf("find %s in process isolation PATH: %w", path, err)
		}
	}

	return "", fmt.Errorf("find %s in process isolation PATH: %w", path, exec.ErrNotFound)
}

func resolveOrdinaryProcessExecutable(path string, env []string) (string, error) {
	if strings.TrimSpace(path) == "" {
		return "", errors.New("executable path is empty")
	}

	if strings.ContainsAny(path, `/\`) {
		absolute, err := ordinaryExecutableAbs(path)
		if err != nil {
			return "", fmt.Errorf("resolve executable %q: %w", path, err)
		}

		return resolveOrdinaryExecutableCandidate(absolute, env)
	}

	values := environmentMap(env)

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
			return "", fmt.Errorf("find %s in PATH: %w", path, err)
		}

		resolved, err := resolveOrdinaryExecutableCandidate(candidate, env)
		if err == nil {
			return resolved, nil
		}

		if !errors.Is(err, os.ErrNotExist) && !errors.Is(err, exec.ErrNotFound) {
			return "", fmt.Errorf("find %s in PATH: %w", path, err)
		}
	}

	return "", fmt.Errorf("find %s in PATH: %w", path, exec.ErrNotFound)
}

func ordinaryEnvironmentValue(values map[string]string, key string) string {
	if value, ok := values[key]; ok || processIsolationGOOS != processIsolationWindows {
		return value
	}

	for candidate, value := range values {
		if strings.EqualFold(candidate, key) {
			return value
		}
	}

	return ""
}

func resolveOrdinaryExecutableCandidate(path string, env []string) (string, error) {
	if processIsolationGOOS != processIsolationWindows {
		info, err := os.Stat(path)
		if err != nil {
			return "", err
		}

		if info.IsDir() || info.Mode()&0o111 == 0 {
			return "", exec.ErrNotFound
		}

		return path, nil
	}

	values := environmentMap(env)
	extensions := ordinaryWindowsExecutableExtensions(ordinaryEnvironmentValue(values, "PATHEXT"))

	candidates := []string{path}
	if filepath.Ext(path) == "" {
		for _, extension := range extensions {
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
		return []string{
			ordinaryWindowsExtensionCOM,
			ordinaryWindowsExtensionEXE,
			ordinaryWindowsExtensionBAT,
			ordinaryWindowsExtensionCMD,
		}
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
