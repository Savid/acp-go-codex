package codex

import (
	"fmt"
	"os"
	"strings"
)

const (
	shellEnvironmentPolicyKey = "shell_environment_policy"
	shellEnvironmentSetKey    = "set"
	pathEnvKey                = "PATH"
)

// threadSessionConfig renders the thread config that carries one thread's
// shell environment at shell_environment_policy.set, which Codex applies after
// inheritance and its own secret filtering. The derived PATH places the
// ordered session directories ahead of the exact PATH the app-server process
// itself runs with, so two threads on one app-server never see each other's
// environment.
func threadSessionConfig(environment map[string]string, extraPathDirs []string, nativePath string) (map[string]any, error) {
	if len(environment) == 0 && len(extraPathDirs) == 0 {
		return map[string]any{}, nil
	}

	set := make(map[string]any, len(environment)+1)

	for key, value := range environment {
		if key == pathEnvKey {
			return nil, fmt.Errorf("codex thread environment must not set %s", key)
		}

		set[key] = value
	}

	if path := composeSearchPath(extraPathDirs, nativePath); len(extraPathDirs) > 0 && path != "" {
		set[pathEnvKey] = path
	}

	return map[string]any{shellEnvironmentPolicyKey: map[string]any{shellEnvironmentSetKey: set}}, nil
}

// composeSearchPath joins the ordered session directories ahead of the native
// path, dropping empty components because an empty PATH element means the
// current directory to some shells.
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
