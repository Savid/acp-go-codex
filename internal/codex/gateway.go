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
	providers, err := configuredModelProviders(home)
	if err != nil {
		return nil, err
	}

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

func configuredModelProviders(home string) (map[string]modelProvider, error) {
	data, err := os.ReadFile(filepath.Join(home, "config.toml"))
	if errors.Is(err, fs.ErrNotExist) {
		return map[string]modelProvider{}, nil
	}

	if err != nil {
		return nil, fmt.Errorf("native config: %w", err)
	}

	var config struct {
		ModelProviders map[string]modelProvider `toml:"model_providers"`
	}
	if err := toml.Unmarshal(data, &config); err != nil {
		return nil, fmt.Errorf("native config: %w", err)
	}

	if config.ModelProviders == nil {
		return map[string]modelProvider{}, nil
	}

	return config.ModelProviders, nil
}
