package codexacp

import (
	"testing"

	"github.com/savid/acp-go-codex/internal/codex"
	"github.com/stretchr/testify/require"
)

func simulateCaseInsensitiveEnv(t *testing.T, enabled bool) {
	t.Helper()

	previous := codex.Platform
	t.Cleanup(func() { codex.Platform = previous })

	codex.Platform = "linux"
	if enabled {
		codex.Platform = "windows"
	}
}

func requireAmbiguousField(t *testing.T, err error, field string) {
	t.Helper()

	require.Equal(t, ambiguousField(field), err)
}

func TestSessionEnvAcceptsEveryStructurallyValidName(t *testing.T) {
	simulateCaseInsensitiveEnv(t, false)

	env := map[string]string{
		"https_proxy":     "",
		"no_proxy":        "",
		"WAGIE_API_URL":   "http://127.0.0.1:1",
		"BASH_FUNC_x%%":   "() { :; }",
		"path":            "/not/the/search/path",
		"env":             "/not/the/shell/init",
		"ld_preload":      "/not/the/loader",
		"codex_home":      "/not/the/managed/root",
		"Xdg_Config_Home": "/not/the/managed/root",
	}

	parsed, err := sessionMetaFromLifecycle(CodexOptions{Env: env}.Meta())
	require.NoError(t, err)
	require.Equal(t, env, parsed.Env)
	require.NoError(t, ValidateCodexSessionMeta(CodexOptions{Env: env}.Meta()))
}

func TestSessionEnvRefusesStructurallyInvalidEntries(t *testing.T) {
	tests := []struct {
		name string
		env  map[string]string
		key  string
	}{
		{"empty name", map[string]string{"": "x"}, ""},
		{"name carries an equals sign", map[string]string{"A=B": "x"}, "A=B"},
		{"name carries a NUL", map[string]string{"A\x00B": "x"}, "A\x00B"},
		{"value carries a NUL", map[string]string{"A": "x\x00y"}, "A"},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			_, err := sessionMetaFromLifecycle(CodexOptions{Env: test.env}.Meta())
			requireInvalidParamsField(t, err, envOptionPath+"."+test.key)
		})
	}
}

func TestSessionEnvRefusesBlockedNamesUnderThePlatformIdentity(t *testing.T) {
	blocked := []string{
		"PATH", "NODE_OPTIONS", "BASH_ENV", "ENV",
		"LD_PRELOAD", "DYLD_INSERT_LIBRARIES",
		managedCodexHomeEnvironment, managedHomeEnv, envXDGConfigHomeKey,
	}

	for _, fold := range []bool{false, true} {
		simulateCaseInsensitiveEnv(t, fold)

		for _, key := range blocked {
			_, err := sessionMetaFromLifecycle(CodexOptions{Env: map[string]string{key: "x"}}.Meta())
			requireInvalidParamsField(t, err, envOptionPath+"."+key)
		}

		for _, key := range []string{privateAdapterEnvPrefix + "TOKEN", "acp_go_codex_internal_token"} {
			_, err := sessionMetaFromLifecycle(CodexOptions{Env: map[string]string{key: "x"}}.Meta())
			requireInvalidParamsField(t, err, envOptionPath+"."+key)
		}
	}

	simulateCaseInsensitiveEnv(t, true)

	for _, key := range []string{"path", "Node_Options", "ld_preload", "home", "codex_home", "xdg_state_home"} {
		_, err := sessionMetaFromLifecycle(CodexOptions{Env: map[string]string{key: "x"}}.Meta())
		requireInvalidParamsField(t, err, envOptionPath+"."+key)
	}
}

func TestSessionEnvReportsTheFirstKeyInSortedOrder(t *testing.T) {
	simulateCaseInsensitiveEnv(t, false)

	_, err := sessionMetaFromLifecycle(CodexOptions{Env: map[string]string{
		"ZZ_LAST":   "x\x00y",
		"AA_FIRST=": "x",
		"MM_MID":    "x",
	}}.Meta())
	requireInvalidParamsField(t, err, envOptionPath+".AA_FIRST=")
}

func TestSessionEnvRefusesTwoSpellingsOfOneWindowsVariable(t *testing.T) {
	env := map[string]string{"Https_Proxy": "a", "https_proxy": "b"}

	simulateCaseInsensitiveEnv(t, false)

	parsed, err := sessionMetaFromLifecycle(CodexOptions{Env: env}.Meta())
	require.NoError(t, err)
	require.Len(t, parsed.Env, 2)

	simulateCaseInsensitiveEnv(t, true)

	_, err = sessionMetaFromLifecycle(CodexOptions{Env: env}.Meta())
	requireAmbiguousField(t, err, envOptionPath+".https_proxy")
	requireAmbiguousField(t, ValidateCodexSessionMeta(CodexOptions{Env: env}.Meta()), envOptionPath+".https_proxy")
	require.ErrorContains(t, validateAgentEnv(env), "name the same variable")
}

func TestValidateCodexSessionMetaMirrorsTheSessionParser(t *testing.T) {
	require.NoError(t, ValidateCodexSessionMeta(nil))
	require.NoError(t, ValidateCodexSessionMeta(CodexOptions{
		Env:           map[string]string{"https_proxy": "", "WAGIE_API_TOKEN": "bearer"},
		ExtraPathDirs: []string{absTestPath("session", "bin")},
	}.Meta()))
	requireInvalidParamsField(t, ValidateCodexSessionMeta(CodexOptions{Env: map[string]string{"PATH": "/bin"}}.Meta()), envOptionPath+".PATH")
	requireInvalidParamsField(t, ValidateCodexSessionMeta(CodexOptions{ExtraPathDirs: []string{"relative"}}.Meta()), extraPathDirField(0))
}
