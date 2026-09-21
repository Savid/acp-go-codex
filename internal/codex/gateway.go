package codex

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"github.com/pelletier/go-toml/v2"

	"github.com/savid/acp-go-core/usage/gateway"
)

// modelProvider is one model_providers entry as config.toml or a launch
// override declares it.
type modelProvider struct {
	BaseURL string `toml:"base_url"`
	EnvKey  string `toml:"env_key"`
}

// GatewayRoutes lists the model providers declared with their own base URL,
// by config.toml under home and by the launch overrides, which win: the
// routes Codex sends requests through, each with the key the environment
// holds under the provider's env_key.
func GatewayRoutes(home string, overrides map[string]any, lookup func(string) (string, bool)) ([]gateway.Route, error) {
	config, err := configuredProviders(home)
	if err != nil {
		return nil, err
	}

	providers := config.ModelProviders

	for key, value := range overrides {
		name, field, ok := strings.Cut(strings.TrimPrefix(key, "model_providers."), ".")
		text, isText := value.(string)

		if !ok || !strings.HasPrefix(key, "model_providers.") || !isText {
			continue
		}

		provider := providers[name]

		switch field {
		case "base_url":
			provider.BaseURL = text
		case "env_key":
			provider.EnvKey = text
		default:
			continue
		}

		providers[name] = provider
	}

	routes := make([]gateway.Route, 0, len(providers))

	for _, name := range slices.Sorted(func(yield func(string) bool) {
		for name := range providers {
			if !yield(name) {
				return
			}
		}
	}) {
		provider := providers[name]
		if provider.BaseURL == "" {
			continue
		}

		token := ""
		if provider.EnvKey != "" {
			token, _ = lookup(provider.EnvKey)
		}

		routes = append(routes, gateway.Route{Provider: name, BaseURL: provider.BaseURL, Token: token})
	}

	return routes, nil
}

// ActiveGatewayRoute is the route of the model provider Codex sends turns
// to: the launch override's model_provider, else config.toml's, when that
// provider declares its own base URL.
func ActiveGatewayRoute(home string, overrides map[string]any, lookup func(string) (string, bool)) (gateway.Route, bool, error) {
	config, err := configuredProviders(home)
	if err != nil {
		return gateway.Route{}, false, err
	}

	active := config.ModelProvider
	if name, ok := overrides["model_provider"].(string); ok {
		active = name
	}

	if active == "" {
		return gateway.Route{}, false, nil
	}

	routes, err := GatewayRoutes(home, overrides, lookup)
	if err != nil {
		return gateway.Route{}, false, err
	}

	for _, route := range routes {
		if route.Provider == active {
			return route, true, nil
		}
	}

	return gateway.Route{}, false, nil
}

type providerConfig struct {
	ModelProvider  string                   `toml:"model_provider"`
	ModelProviders map[string]modelProvider `toml:"model_providers"`
}

func configuredProviders(home string) (providerConfig, error) {
	config := providerConfig{ModelProviders: map[string]modelProvider{}}

	data, err := os.ReadFile(filepath.Join(home, "config.toml"))
	if errors.Is(err, fs.ErrNotExist) {
		return config, nil
	}

	if err != nil {
		return providerConfig{}, fmt.Errorf("native config: %w", err)
	}

	if err := toml.Unmarshal(data, &config); err != nil {
		return providerConfig{}, fmt.Errorf("native config: %w", err)
	}

	if config.ModelProviders == nil {
		config.ModelProviders = map[string]modelProvider{}
	}

	return config, nil
}
