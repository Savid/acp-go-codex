package codex

import (
	"testing"

	"github.com/coder/acp-go-sdk"
	"github.com/stretchr/testify/require"
)

func TestMCPServerThreadConfig(t *testing.T) {
	require.Empty(t, MCPServerThreadConfig(nil, ""))

	config := MCPServerThreadConfig([]acp.McpServer{
		{Stdio: &acp.McpServerStdio{Name: "local", Command: "tool", Args: []string{"--one"}, Env: []acp.EnvVariable{{Name: "A", Value: "B"}}}},
		{Http: &acp.McpServerHttpInline{
			Name:    "HTTP",
			Url:     "https://example.com/mcp",
			Headers: []acp.HttpHeader{{Name: "Authorization", Value: "Bearer x"}},
		}},
	}, mcpApprovalModeApprove)
	servers, ok := config[mcpServersConfigKey].(map[string]any)
	require.True(t, ok)
	stdio, ok := servers["local"].(map[string]any)
	require.True(t, ok)
	http, ok := servers["HTTP"].(map[string]any)
	require.True(t, ok)
	require.Equal(t, "tool", stdio["command"])
	require.Equal(t, []string{"--one"}, stdio["args"])
	require.Equal(t, map[string]string{"A": "B"}, stdio["env"])
	require.Equal(t, "https://example.com/mcp", http["url"])
	require.Equal(t, map[string]string{"Authorization": "Bearer x"}, http["http_headers"])
	require.Equal(t, "approve", http["default_tools_approval_mode"])
}

// TestValidMCPApprovalMode pins the accepted tool-approval vocabulary. The ACP
// session-start validator is its only caller and rejects everything else with
// the uniform unsupported-field error.
func TestValidMCPApprovalMode(t *testing.T) {
	for mode, want := range map[string]bool{
		mcpApprovalModeAuto:    true,
		mcpApprovalModePrompt:  true,
		mcpApprovalModeApprove: true,
		"never":                false,
		"":                     false,
	} {
		require.Equal(t, want, ValidMCPApprovalMode(mode), "mode %q", mode)
	}
}

func TestMCPServerThreadConfigForwardsNameVerbatim(t *testing.T) {
	config := MCPServerThreadConfig([]acp.McpServer{
		{Stdio: &acp.McpServerStdio{Name: "Local Tool", Command: "tool", Args: []string{"--one"}}},
	}, "")
	servers, ok := config[mcpServersConfigKey].(map[string]any)
	require.True(t, ok)
	require.Contains(t, servers, "Local Tool")
}

func TestConfigArgRendersValueKinds(t *testing.T) {
	require.Equal(t, []string{"-c", `k="v"`}, ConfigArg("k", "v"))
	require.Equal(t, []string{"-c", "k=raw"}, ConfigArg("k", TOMLLiteral("raw")))
	require.Equal(t, []string{"-c", "k=true"}, ConfigArg("k", true))
}
