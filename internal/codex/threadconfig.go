package codex

import (
	"fmt"
	"maps"
	"os"
	"runtime"
	"strings"
)

const (
	shellEnvironmentPolicyKey = "shell_environment_policy"
	shellEnvironmentSetKey    = "set"
	pathEnvKey                = "PATH"
)

// caseInsensitiveEnvKeys reports whether the running platform treats
// environment names case insensitively. Windows does, so "Path" owns the same
// value as "PATH" there; elsewhere only the exact name does.
var caseInsensitiveEnvKeys = runtime.GOOS == "windows"

func isPathEnvKey(key string) bool {
	return key == pathEnvKey || (caseInsensitiveEnvKeys && strings.EqualFold(key, pathEnvKey))
}

// threadSessionConfig deep-clones the caller's thread config and installs the
// operation's shell environment at shell_environment_policy.set, which Codex
// applies after inheritance and secret filtering. That ordering is load
// bearing: a secret-shaped session value survives the native filter only
// because it is set rather than inherited.
//
// The derived PATH places the ordered operation directories ahead of the exact
// PATH the app-server process itself was launched with. Nothing here mutates
// the caller's map, so two threads on one app-server cannot observe each
// other's environment.
func threadSessionConfig(
	config map[string]any,
	environment map[string]string,
	extraPathDirs []string,
	nativePath string,
) (map[string]any, error) {
	cloned := cloneConfigMap(config)

	_, hasShellPolicy := cloned[shellEnvironmentPolicyKey]
	if !hasShellPolicy && len(environment) == 0 && len(extraPathDirs) == 0 && nativePath == "" {
		return cloned, nil
	}

	if cloned == nil {
		cloned = map[string]any{}
	}

	policy, err := ownedConfigSection(cloned, shellEnvironmentPolicyKey, shellEnvironmentPolicyKey)
	if err != nil {
		return nil, err
	}

	set, err := ownedConfigSection(
		policy,
		shellEnvironmentSetKey,
		shellEnvironmentPolicyKey+"."+shellEnvironmentSetKey,
	)
	if err != nil {
		return nil, err
	}

	for key := range set {
		if isPathEnvKey(key) {
			return nil, fmt.Errorf(
				"codex thread config %s.%s.%s already owns the session search path",
				shellEnvironmentPolicyKey, shellEnvironmentSetKey, key,
			)
		}
	}

	for key, value := range environment {
		if isPathEnvKey(key) {
			return nil, fmt.Errorf("codex thread environment must not set %s", key)
		}

		set[key] = value
	}

	if path := composeSearchPath(extraPathDirs, nativePath); path != "" {
		set[pathEnvKey] = path
	}

	policy[shellEnvironmentSetKey] = set
	cloned[shellEnvironmentPolicyKey] = policy

	return cloned, nil
}

// composeSearchPath joins the ordered operation directories ahead of the
// native path. Empty components are dropped because an empty PATH element
// means the current directory to some shells.
func composeSearchPath(extraPathDirs []string, nativePath string) string {
	separator := string(os.PathListSeparator)

	components := make([]string, 0, len(extraPathDirs)+1)
	components = append(components, extraPathDirs...)
	components = append(components, strings.Split(nativePath, separator)...)

	kept := make([]string, 0, len(components))

	for _, component := range components {
		if component != "" {
			kept = append(kept, component)
		}
	}

	return strings.Join(kept, separator)
}

// ownedConfigSection returns the named object this adapter is about to author.
// A present value of another shape is an operator-owned section the adapter
// refuses to silently overwrite.
func ownedConfigSection(parent map[string]any, key string, field string) (map[string]any, error) {
	raw, ok := parent[key]
	if !ok || raw == nil {
		return map[string]any{}, nil
	}

	section, ok := raw.(map[string]any)
	if !ok {
		return nil, fmt.Errorf("codex thread config %s must be an object", field)
	}

	return section, nil
}

// searchPathFromEnvironment reads the exact PATH out of the environment list
// that was actually built for the app-server process, in either supervised or
// direct launch mode. Ambient os.Getenv is never consulted.
func searchPathFromEnvironment(entries []string) string {
	for _, entry := range entries {
		key, value, ok := strings.Cut(entry, "=")
		if ok && isPathEnvKey(key) {
			return value
		}
	}

	return ""
}

func cloneConfigMap(values map[string]any) map[string]any {
	if values == nil {
		return nil
	}

	cloned := make(map[string]any, len(values))
	for key, value := range values {
		cloned[key] = cloneConfigValue(value)
	}

	return cloned
}

func cloneConfigValue(value any) any {
	switch typed := value.(type) {
	case map[string]any:
		return cloneConfigMap(typed)
	case map[string]string:
		cloned := make(map[string]string, len(typed))
		maps.Copy(cloned, typed)

		return cloned
	case []any:
		cloned := make([]any, len(typed))
		for index, item := range typed {
			cloned[index] = cloneConfigValue(item)
		}

		return cloned
	case []string:
		return append([]string{}, typed...)
	default:
		return value
	}
}
