package codex

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"slices"

	"github.com/pelletier/go-toml/v2"

	"github.com/savid/acp-go-core/usage/gateway"
)

// GatewayRoutes lists the model providers config.toml under home declares
// with their own base URL: the routes Codex sends requests through, each with
// the key the environment holds under the provider's env_key.
func GatewayRoutes(home string, lookup func(string) (string, bool)) ([]gateway.Route, error) {
	data, err := os.ReadFile(filepath.Join(home, "config.toml"))
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}

	if err != nil {
		return nil, fmt.Errorf("native config: %w", err)
	}

	var config struct {
		ModelProviders map[string]struct {
			BaseURL string `toml:"base_url"`
			EnvKey  string `toml:"env_key"`
		} `toml:"model_providers"`
	}
	if err := toml.Unmarshal(data, &config); err != nil {
		return nil, fmt.Errorf("native config: %w", err)
	}

	routes := make([]gateway.Route, 0, len(config.ModelProviders))

	for _, name := range slices.Sorted(func(yield func(string) bool) {
		for name := range config.ModelProviders {
			if !yield(name) {
				return
			}
		}
	}) {
		provider := config.ModelProviders[name]
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
