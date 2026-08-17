package codexacp

import (
	"os"
	"strings"

	"github.com/savid/acp-go-codex/internal/codex"
)

var osEnviron = os.Environ

func (a *Agent) codexOTELConfig(envOverlay map[string]string) (codex.OTELConfig, error) {
	return codex.OTELConfigFromEnv(codexOTELEffectiveEnv(a.options.Env, envOverlay))
}

func codexOTELEffectiveEnv(agentEnv map[string]string, sessionEnv map[string]string) map[string]string {
	env := envMapFromEnviron(osEnviron())
	overlayStringMap(env, agentEnv)
	overlayStringMap(env, sessionEnv)

	return env
}

func envMapFromEnviron(environ []string) map[string]string {
	env := make(map[string]string, len(environ))
	for _, entry := range environ {
		key, value, ok := strings.Cut(entry, "=")
		if !ok || key == "" {
			continue
		}

		env[key] = value
	}

	return env
}

func overlayStringMap(base map[string]string, overlay map[string]string) {
	for key, value := range overlay {
		base[key] = value
	}
}
