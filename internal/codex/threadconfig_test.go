package codex

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func TestThreadEnvironmentUsesSessionPath(t *testing.T) {
	t.Parallel()
	config, err := threadSessionConfig(map[string]string{
		"PATH":                         ":/session/bin::/session/tools:",
		"EMPTY":                        "",
		"HOME":                         "/session/home",
		"ACP_GO_CODEX_INTERNAL_MARK":   "drop",
		"acp_go_codex_internal_caller": "keep",
		"ACP_GO_OTHER_INTERNAL_MARK":   "keep",
	}, []string{"/extra/first", "/extra/second"}, "/process/bin")
	require.NoError(t, err)
	require.Equal(t, map[string]any{"shell_environment_policy": map[string]any{"set": map[string]any{
		"PATH":  "/extra/first:/extra/second:/session/bin:/session/tools",
		"EMPTY": "", "HOME": "/session/home", "acp_go_codex_internal_caller": "keep", "ACP_GO_OTHER_INTERNAL_MARK": "keep",
	}}}, config)
	other, err := threadSessionConfig(map[string]string{"PATH": "/other/bin"}, nil, "/process/bin")
	require.NoError(t, err)
	require.Equal(t, map[string]any{"shell_environment_policy": map[string]any{"set": map[string]any{"PATH": "/other/bin"}}}, other)
}
