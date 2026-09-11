package codex

import (
	"context"
	"errors"
	"strings"
)

const (
	providerRouteOpenAI     = "openai"
	providerRouteChatGPTURL = "https://chatgpt.com/backend-api"
	providerRouteEnvBaseURL = "OPENAI_BASE_URL"
)

// ProviderRouteClient resolves where a workspace's native requests go without
// starting a thread or model turn. Only the relevant configuration is returned.
type ProviderRouteClient interface {
	ReadProviderRoute(context.Context, string) (ProviderRoute, error)
}

// ProviderRoute identifies the effective provider a workspace's requests reach
// and whether anything points it away from that provider's own endpoint. Both
// readers that care where a request lands — the subscription quota reader and
// the model menu — decide from this one answer.
type ProviderRoute struct {
	ProviderID string
	// Custom reports a configured endpoint override: a base URL, a credential
	// or header binding on the provider, or a replaced ChatGPT host. Codex
	// resolves these, so they move where requests actually land.
	Custom bool
	// EnvironmentBaseURL reports an OPENAI_BASE_URL in the app-server's own
	// environment. Codex reads its endpoint from configuration and ignores this
	// variable, so it moves no request; a reader that refuses on declared
	// intent rather than on the resolved route consults it separately.
	EnvironmentBaseURL bool
}

// ReadProviderRoute reads layered native config in the selected workspace.
func (c *AppServerClient) ReadProviderRoute(ctx context.Context, cwd string) (ProviderRoute, error) {
	params := map[string]any{"includeLayers": false}
	if cwd != "" {
		params["cwd"] = cwd
	}

	var response struct {
		Config map[string]any `json:"config"`
	}
	if err := c.rpc.Call(ctx, "config/read", params, &response); err != nil {
		return ProviderRoute{}, err
	}

	if response.Config == nil {
		return ProviderRoute{}, errors.New("native configuration response is missing its config")
	}

	configuration := providerRouteFromConfig(response.Config)

	environment, err := buildMergedEnv(c.options)
	if err != nil {
		return ProviderRoute{}, err
	}

	for _, entry := range environment {
		key, value, _ := strings.Cut(entry, "=")
		if strings.EqualFold(key, providerRouteEnvBaseURL) {
			configuration.EnvironmentBaseURL = configuration.EnvironmentBaseURL || value != ""
		}
	}

	return configuration, nil
}

func providerRouteFromConfig(config map[string]any) ProviderRoute {
	provider := stringValue(config, "model_provider")
	if provider == "" {
		provider = providerRouteOpenAI // Codex's built-in default when no provider is selected.
	}

	// config/read includes Codex's resolved ChatGPT URL even without an override.
	chatGPTBaseURL := strings.TrimSpace(stringValue(config, "chatgpt_base_url"))
	custom := provider != providerRouteOpenAI || strings.TrimSpace(stringValue(config, "openai_base_url")) != "" ||
		chatGPTBaseURL != "" && chatGPTBaseURL != providerRouteChatGPTURL && chatGPTBaseURL != providerRouteChatGPTURL+"/"

	providerConfig := mapValue(mapValue(config, "model_providers"), provider)
	for _, key := range []string{"base_url", "env_key", "experimental_bearer_token", "auth", "http_headers", "env_http_headers"} {
		custom = custom || providerConfig[key] != nil
	}

	return ProviderRoute{ProviderID: provider, Custom: custom}
}
