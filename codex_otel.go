package codexacp

import (
	"maps"

	"github.com/savid/acp-go-codex/internal/codex"
)

func (a *Agent) codexOTELConfig(envOverlay map[string]string) (codex.OTELConfig, error) {
	return codex.OTELConfigFromEnv(
		codexOTELEffectiveEnv(a.nativeAmbientEnvironment(), a.options.Env, envOverlay),
	)
}

func codexOTELEffectiveEnv(ambient, agentEnv, sessionEnv map[string]string) map[string]string {
	env := make(map[string]string, len(ambient)+len(agentEnv)+len(sessionEnv))
	maps.Copy(env, ambient)
	maps.Copy(env, agentEnv)
	maps.Copy(env, sessionEnv)

	return env
}
