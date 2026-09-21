package codex

import (
	"strings"

	"github.com/savid/acp-go-core/process"
)

// InternalEnvPrefix names the adapter's private child-process markers.
const InternalEnvPrefix = "ACP_GO_CODEX_INTERNAL_"

const shellEnvironmentSetKey = "set"
const shellEnvironmentPolicyKey = "shell_environment_policy"

// threadSessionConfig applies the session overlay after Codex's native shell
// policy. Session PATH replaces the process PATH before extra directories apply.
func threadSessionConfig(environment map[string]string, extraPathDirs []string, nativePath string) (map[string]any, error) {
	env, err := (process.Environment{
		Process:        []string{"PATH=" + nativePath},
		Session:        environment,
		ExtraPathDirs:  extraPathDirs,
		InternalPrefix: InternalEnvPrefix,
	}).Build()
	if err != nil {
		return nil, err
	}

	set := make(map[string]any, len(env))
	for _, entry := range env {
		key, value, _ := strings.Cut(entry, "=")
		set[key] = value
	}

	return map[string]any{shellEnvironmentPolicyKey: map[string]any{shellEnvironmentSetKey: set}}, nil
}
