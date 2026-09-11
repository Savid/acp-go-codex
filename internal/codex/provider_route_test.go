package codex

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestProviderRouteFromConfig(t *testing.T) {
	for _, tc := range []struct {
		name     string
		config   map[string]any
		provider string
		custom   bool
	}{
		{name: "default", config: map[string]any{}, provider: "openai"},
		{name: "selected", config: map[string]any{"model_provider": "openai"}, provider: "openai"},
		{name: "resolved ChatGPT default", config: map[string]any{"chatgpt_base_url": "https://chatgpt.com/backend-api/"}, provider: "openai"},
		{name: "ChatGPT default without trailing slash", config: map[string]any{"chatgpt_base_url": "https://chatgpt.com/backend-api"}, provider: "openai"},
		{name: "other provider", config: map[string]any{"model_provider": "other"}, provider: "other", custom: true},
		{name: "base URL", config: map[string]any{"openai_base_url": "https://example.test"}, provider: "openai", custom: true},
		{name: "ChatGPT custom host", config: map[string]any{"chatgpt_base_url": "https://example.test/backend-api/"}, provider: "openai", custom: true},
		{name: "ChatGPT insecure URL", config: map[string]any{"chatgpt_base_url": "http://chatgpt.com/backend-api/"}, provider: "openai", custom: true},
		{name: "ChatGPT invalid URL", config: map[string]any{"chatgpt_base_url": "/"}, provider: "openai", custom: true},
		{name: "ChatGPT custom path", config: map[string]any{"chatgpt_base_url": "https://chatgpt.com/backend-api/custom"}, provider: "openai", custom: true},
		{name: "ChatGPT custom query", config: map[string]any{"chatgpt_base_url": "https://chatgpt.com/backend-api/?account=other"}, provider: "openai", custom: true},
		{name: "ChatGPT user info", config: map[string]any{"chatgpt_base_url": "https://other@chatgpt.com/backend-api/"}, provider: "openai", custom: true},
		{name: "nested base URL", config: map[string]any{"model_providers": map[string]any{"openai": map[string]any{"base_url": "https://example.test"}}}, provider: "openai", custom: true},
		{name: "provider auth override", config: map[string]any{"model_providers": map[string]any{"openai": map[string]any{"env_key": "CUSTOM_OPENAI_KEY"}}}, provider: "openai", custom: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			require.Equal(t, ProviderRoute{ProviderID: tc.provider, Custom: tc.custom}, providerRouteFromConfig(tc.config))
		})
	}
}

func TestReadProviderRoute(t *testing.T) {
	for _, tc := range []struct {
		name               string
		config             any
		environment        map[string]string
		implicit           map[string]string
		custom             bool
		environmentBaseURL bool
		fail               bool
	}{
		{name: "native default", config: map[string]any{"model_provider": "openai"}},
		{name: "resolved default with ambient API key", config: map[string]any{"model_provider": nil, "chatgpt_base_url": "https://chatgpt.com/backend-api/", "model_providers": map[string]any{"openai": map[string]any{}}}, implicit: map[string]string{"OPENAI_API_KEY": "test-key"}},
		{name: "OpenAI API key override", config: map[string]any{"model_provider": "openai"}, environment: map[string]string{"OPENAI_API_KEY": "test-key"}},
		{name: "Codex API key override", config: map[string]any{"model_provider": "openai"}, environment: map[string]string{"CODEX_API_KEY": "test-key"}},
		{name: "configured endpoint override", config: map[string]any{"model_provider": "openai", "openai_base_url": "https://example.test"}, custom: true},
		{name: "launch-block base URL", config: map[string]any{"model_provider": "openai"}, environment: map[string]string{"OPENAI_BASE_URL": "https://example.test"}, environmentBaseURL: true},
		{name: "ambient base URL", config: map[string]any{"model_provider": "openai"}, implicit: map[string]string{"OPENAI_BASE_URL": "https://example.test"}, environmentBaseURL: true},
		{name: "missing config", fail: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			transport := &responseTransport{responses: map[string]any{"config/read": map[string]any{"config": tc.config}}}
			implicit := tc.implicit
			if implicit == nil {
				implicit = map[string]string{}
			}
			client := &AppServerClient{rpc: newRPCConn(transport, nil), options: Options{ImplicitEnvironment: implicit, Env: tc.environment}}
			defer client.Close(context.Background())
			configuration, err := client.ReadProviderRoute(context.Background(), "/workspace")
			if tc.fail {
				require.Error(t, err)

				return
			}
			require.NoError(t, err)
			require.Equal(t,
				ProviderRoute{ProviderID: "openai", Custom: tc.custom, EnvironmentBaseURL: tc.environmentBaseURL},
				configuration,
			)
			transport.mu.Lock()
			sent := transport.sent[0]
			transport.mu.Unlock()
			require.JSONEq(t, `{"includeLayers":false,"cwd":"/workspace"}`, string(sent.Params))
		})
	}
}
