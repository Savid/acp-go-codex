package codex

import (
	"testing"

	"github.com/coder/acp-go-sdk"
	"github.com/stretchr/testify/require"
)

func TestMCPServerThreadConfig(t *testing.T) {
	empty, err := MCPServerThreadConfig(nil, "")
	require.NoError(t, err)
	require.Empty(t, empty)

	config, err := MCPServerThreadConfig([]acp.McpServer{
		{Stdio: &acp.McpServerStdio{Name: "local", Command: "tool", Args: []string{"--one"}, Env: []acp.EnvVariable{{Name: "A", Value: "B"}}}},
		{Http: &acp.McpServerHttpInline{
			Name:    "HTTP",
			Url:     "https://example.com/mcp",
			Headers: []acp.HttpHeader{{Name: "Authorization", Value: "Bearer x"}},
		}},
	}, mcpApprovalModeApprove)
	require.NoError(t, err)
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

	_, err = MCPServerThreadConfig([]acp.McpServer{{Acp: &acp.McpServerAcpInline{Name: "Client", Id: "client-1"}}}, "")
	require.Error(t, err)
	_, err = MCPServerThreadConfig([]acp.McpServer{{Sse: &acp.McpServerSseInline{Name: "sse", Url: "https://example.com/sse"}}}, "")
	require.Error(t, err)
	_, err = MCPServerThreadConfig([]acp.McpServer{{}}, "")
	require.Error(t, err)
}

func TestMCPServerThreadConfigForwardsNameVerbatim(t *testing.T) {
	config, err := MCPServerThreadConfig([]acp.McpServer{
		{Stdio: &acp.McpServerStdio{Name: "Local Tool", Command: "tool", Args: []string{"--one"}}},
	}, "")
	require.NoError(t, err)
	servers, ok := config[mcpServersConfigKey].(map[string]any)
	require.True(t, ok)
	require.Contains(t, servers, "Local Tool")
}

func TestMCPServerThreadConfigRejectsInvalidMode(t *testing.T) {
	_, err := MCPServerThreadConfig([]acp.McpServer{{Stdio: &acp.McpServerStdio{Name: "mcp", Command: "mcp"}}}, "never")
	require.Error(t, err)
}

func TestConfigArgRendersValueKinds(t *testing.T) {
	require.Equal(t, []string{"-c", `k="v"`}, ConfigArg("k", "v"))
	require.Equal(t, []string{"-c", "k=raw"}, ConfigArg("k", TOMLLiteral("raw")))
	require.Equal(t, []string{"-c", "k=true"}, ConfigArg("k", true))
}
