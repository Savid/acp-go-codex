//go:build integration

package integration

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

func TestRedactCodexRefreshTokens(t *testing.T) {
	t.Parallel()

	data, err := redactCodexRefreshTokens([]byte(`{
			"tokens": {
				"access_token": "access",
				"refresh_token": "refresh",
				"refreshToken": "camel-refresh"
			},
			"nested": [{"refresh_token": "nested-refresh", "refreshToken": "nested-camel-refresh"}]
		}`))
	if err != nil {
		t.Fatalf("redact Codex refresh tokens: %v", err)
	}

	var decoded map[string]any
	if err := json.Unmarshal(data, &decoded); err != nil {
		t.Fatalf("decode redacted auth: %v", err)
	}

	tokens, ok := decoded["tokens"].(map[string]any)
	if !ok {
		t.Fatalf("tokens = %#v", decoded["tokens"])
	}
	if tokens["access_token"] != "access" {
		t.Fatalf("access token changed: %#v", tokens)
	}
	requireEmptyJSONField(t, tokens, "refresh_token")
	requireEmptyJSONField(t, tokens, "refreshToken")

	nested, ok := decoded["nested"].([]any)
	if !ok || len(nested) != 1 {
		t.Fatalf("nested = %#v", decoded["nested"])
	}
	nestedObj, ok := nested[0].(map[string]any)
	if !ok {
		t.Fatalf("nested[0] = %#v", nested[0])
	}
	requireEmptyJSONField(t, nestedObj, "refresh_token")
	requireEmptyJSONField(t, nestedObj, "refreshToken")
}

func TestCodexAuthToken(t *testing.T) {
	t.Parallel()

	token := codexAuthToken(map[string]any{
		"tokens": map[string]any{"access_token": "access"},
	})
	if token != "access" {
		t.Fatalf("token = %q", token)
	}

	token = codexAuthToken(map[string]any{
		"nested": []any{map[string]any{"openai_api_key": "api-key"}},
	})
	if token != "api-key" {
		t.Fatalf("nested token = %q", token)
	}
}

func TestIsolatedCodexHomeUsesFreshHomeWithProcessAuth(t *testing.T) {
	t.Setenv(envOpenAIAPIKey, "api-key")
	t.Setenv(envCodexHome, "")
	t.Setenv("HOME", t.TempDir())

	home := isolatedCodexHome(t)
	if home == "" {
		t.Fatal("isolated Codex home is empty")
	}
	if _, err := os.Stat(home); err != nil {
		t.Fatalf("stat isolated Codex home: %v", err)
	}
	if _, err := os.Stat(filepath.Join(home, "auth.json")); !os.IsNotExist(err) {
		t.Fatalf("auth.json exists with process auth, err=%v", err)
	}
}

func TestIsolatedCodexHomePrefersPortableAuthOverProcessAuth(t *testing.T) {
	sourceParent := t.TempDir()
	source := filepath.Join(sourceParent, ".codex")
	if err := os.Mkdir(source, 0o700); err != nil {
		t.Fatalf("create source home: %v", err)
	}
	t.Setenv(envOpenAIAPIKey, "api-key")
	t.Setenv(envCodexHome, "")
	t.Setenv("HOME", sourceParent)
	if err := os.WriteFile(filepath.Join(source, "auth.json"), []byte(`{
			"tokens": {
				"access_token": "access",
				"refresh_token": "refresh"
			}
		}`), 0o600); err != nil {
		t.Fatalf("write auth.json: %v", err)
	}

	home := isolatedCodexHome(t)
	data, err := os.ReadFile(filepath.Join(home, "auth.json"))
	if err != nil {
		t.Fatalf("read copied auth.json: %v", err)
	}
	var decoded map[string]any
	if err := json.Unmarshal(data, &decoded); err != nil {
		t.Fatalf("decode copied auth.json: %v", err)
	}
	tokens, ok := decoded["tokens"].(map[string]any)
	if !ok {
		t.Fatalf("tokens = %#v", decoded["tokens"])
	}
	if tokens["access_token"] != "access" {
		t.Fatalf("access token changed: %#v", tokens)
	}
	requireEmptyJSONField(t, tokens, "refresh_token")
}

func TestIsolatedCodexHomeCopiesExplicitSource(t *testing.T) {
	source := t.TempDir()
	t.Setenv(envOpenAIAPIKey, "")
	t.Setenv(envCodexHome, source)
	if err := os.WriteFile(filepath.Join(source, "auth.json"), []byte(`{
			"tokens": {
				"access_token": "access",
				"refresh_token": "refresh"
			}
		}`), 0o600); err != nil {
		t.Fatalf("write auth.json: %v", err)
	}

	home := isolatedCodexHome(t)
	if home == "" || home == source {
		t.Fatalf("home = %q source = %q", home, source)
	}
	data, err := os.ReadFile(filepath.Join(home, "auth.json"))
	if err != nil {
		t.Fatalf("read copied auth.json: %v", err)
	}
	var decoded map[string]any
	if err := json.Unmarshal(data, &decoded); err != nil {
		t.Fatalf("decode copied auth.json: %v", err)
	}
	tokens, ok := decoded["tokens"].(map[string]any)
	if !ok {
		t.Fatalf("tokens = %#v", decoded["tokens"])
	}
	if tokens["access_token"] != "access" {
		t.Fatalf("access token changed: %#v", tokens)
	}
	requireEmptyJSONField(t, tokens, "refresh_token")
}

func requireEmptyJSONField(t *testing.T, values map[string]any, key string) {
	t.Helper()

	value, ok := values[key]
	if !ok {
		t.Fatalf("missing key %q in %#v", key, values)
	}
	if value != "" {
		t.Fatalf("%s = %#v, want empty string", key, value)
	}
}
