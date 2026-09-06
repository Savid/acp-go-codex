package codexacp

import (
	"fmt"
	"maps"
	"slices"
	"strings"

	"github.com/coder/acp-go-sdk"

	"github.com/savid/acp-go-codex/internal/codex"
)

const (
	valAmbiguous = "ambiguous"

	envOptionPath = "_meta.codex.options." + metaEnvKey

	privateAdapterEnvPrefix = "ACP_GO_CODEX_INTERNAL_"

	envPathKey        = "PATH"
	envNodeOptionsKey = "NODE_OPTIONS"
	envBashEnvKey     = "BASH_ENV"
	envShellEnvKey    = "ENV"

	envXDGCacheHomeKey  = "XDG_CACHE_HOME"
	envXDGConfigHomeKey = "XDG_CONFIG_HOME"
	envXDGDataHomeKey   = "XDG_DATA_HOME"
	envXDGRuntimeDirKey = "XDG_RUNTIME_DIR"
	envXDGStateHomeKey  = "XDG_STATE_HOME"
)

func validEnvName(key string) bool {
	return key != "" && !strings.ContainsAny(key, "=\x00")
}

// blockedAgentEnvKey reports whether a caller-supplied env key names a
// variable the adapter refuses on every surface. The private adapter
// namespace is refused under every spelling; the managed roots and the
// loader, node, and shell injection names are read by the native process
// under an exact platform spelling, so those compare through the platform
// identity.
func blockedAgentEnvKey(key string) bool {
	if strings.HasPrefix(strings.ToUpper(key), privateAdapterEnvPrefix) {
		return true
	}

	name := codex.EnvironmentKey(key)
	if managedCodexRootEnvKey(name) {
		return true
	}

	switch name {
	case envNodeOptionsKey, envBashEnvKey, envShellEnvKey:
		return true
	default:
		return strings.HasPrefix(name, "LD_") || strings.HasPrefix(name, "DYLD_")
	}
}

// blockedSessionEnvKey additionally refuses PATH in a session env. The thread
// PATH is derived from extraPathDirs and the app-server's own search path, so
// a raw session PATH would be a second, silently losing owner of the same
// value.
func blockedSessionEnvKey(key string) bool {
	return blockedAgentEnvKey(key) || codex.EnvironmentKey(key) == envPathKey
}

// validateAgentEnv applies the session name rule to the static Agent-scoped
// environment, with PATH allowed because that surface establishes the
// app-server's native base search path. A refusal fails Agent construction.
func validateAgentEnv(env map[string]string) error {
	seen := make(map[string]string, len(env))

	for _, key := range slices.Sorted(maps.Keys(env)) {
		if !validEnvName(key) || strings.ContainsRune(env[key], '\x00') {
			return fmt.Errorf("environment key %q is not a variable name", key)
		}

		if blockedAgentEnvKey(key) {
			return fmt.Errorf("environment key %q is reserved", key)
		}

		identity := codex.EnvironmentKey(key)
		if previous, duplicate := seen[identity]; duplicate {
			return fmt.Errorf("environment keys %q and %q name the same variable", previous, key)
		}

		seen[identity] = key
	}

	return nil
}

// validatedSessionEnv checks a session environment in sorted key order, so the
// first refusal is the same on every call. A key that cannot be a variable
// name, a value carrying a NUL, and a blocked name each fail as unsupported at
// the key exactly as the host sent it. Two keys that name one variable under
// the platform identity fail as ambiguous at the later key: a Go map carries
// no order, so the value such a map would deliver is unknowable.
func validatedSessionEnv(env map[string]string) (map[string]string, error) {
	seen := make(map[string]struct{}, len(env))

	for _, key := range slices.Sorted(maps.Keys(env)) {
		if !validEnvName(key) || strings.ContainsRune(env[key], '\x00') || blockedSessionEnvKey(key) {
			return nil, unsupportedField(envOptionPath + "." + key)
		}

		identity := codex.EnvironmentKey(key)
		if _, duplicate := seen[identity]; duplicate {
			return nil, ambiguousField(envOptionPath + "." + key)
		}

		seen[identity] = struct{}{}
	}

	return env, nil
}

func ambiguousField(path string) error {
	return acp.NewInvalidParams(map[string]any{
		jsonFieldError: valAmbiguous,
		jsonFieldField: path,
	})
}

// ValidateCodexSessionMeta reports the refusal a session/new, session/load,
// session/resume, or fork request carrying meta receives from this package's
// _meta.codex parsing, or nil when the vendor namespace is accepted.
func ValidateCodexSessionMeta(meta map[string]any) error {
	_, err := sessionMetaFromLifecycle(meta)

	return err
}
