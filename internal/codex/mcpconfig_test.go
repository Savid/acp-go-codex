package codex

import (
	"strings"
	"testing"

	"github.com/coder/acp-go-sdk"
	"github.com/stretchr/testify/require"
)

func hasArg(args []string, needle string) bool {
	for _, arg := range args {
		if arg == needle {
			return true
		}
	}

	return false
}

func hasAll(value string, needles ...string) bool {
	for _, needle := range needles {
		if !strings.Contains(value, needle) {
			return false
		}
	}

	return true
}

func TestMCPServerConfigArgs(t *testing.T) {
	empty, env, err := MCPServerConfigArgs(nil, "")
	require.NoError(t, err)
	require.Nil(t, empty)
	require.Nil(t, env)

	args, env, err := MCPServerConfigArgs([]acp.McpServer{
		{Stdio: &acp.McpServerStdio{Name: "local", Command: "tool", Args: []string{"--one"}, Env: []acp.EnvVariable{{Name: "A", Value: "B"}}}},
		{Http: &acp.McpServerHttpInline{
			Name:    "HTTP",
			Url:     "https://example.com/mcp",
			Headers: []acp.HttpHeader{{Name: "Authorization", Value: "Bearer x"}},
		}},
	}, mcpApprovalModeApprove)
	require.NoError(t, err)
	joined := strings.Join(args, " ")
	require.True(t, hasAll(joined, "mcp_servers.local.command", "mcp_servers.HTTP.url", "env_http_headers", "Authorization", "default_tools_approval_mode"), "args = %v", args)
	require.True(t, hasArg(args, `mcp_servers.local.args=["--one"]`), "args = %v", args)
	require.True(t, hasArg(args, `mcp_servers.local.env={ A = "B" }`), "args = %v", args)
	require.Equal(t, "Bearer x", env["CODEX_MCP_HEADER_HTTP_AUTHORIZATION"])
	require.True(t, hasArg(args, `mcp_servers.HTTP.default_tools_approval_mode="approve"`), "args = %v", args)

	_, _, err = MCPServerConfigArgs([]acp.McpServer{{Acp: &acp.McpServerAcpInline{Name: "Client", Id: "client-1"}}}, "")
	require.Error(t, err)

	_, _, err = MCPServerConfigArgs([]acp.McpServer{{Sse: &acp.McpServerSseInline{Name: "sse", Url: "https://example.com/sse"}}}, "")
	require.Error(t, err)

	_, _, err = MCPServerConfigArgs([]acp.McpServer{{}}, "")
	require.Error(t, err)
}

// TestMCPServerConfigArgsForwardsNameVerbatim proves accepted server names are
// forwarded unchanged into the Codex config key: a bare-key-safe name is emitted
// as-is, and a name with characters that are not valid in a TOML bare key is
// quoted (never rewritten, deduplicated, or fabricated).
func TestMCPServerConfigArgsForwardsNameVerbatim(t *testing.T) {
	args, _, err := MCPServerConfigArgs([]acp.McpServer{
		{Stdio: &acp.McpServerStdio{Name: "Local Tool", Command: "tool", Args: []string{"--one"}}},
	}, "")
	require.NoError(t, err)
	require.True(t, hasArg(args, `mcp_servers."Local Tool".command="tool"`), "args = %v", args)
	require.True(t, hasArg(args, `mcp_servers."Local Tool".args=["--one"]`), "args = %v", args)
}

func TestMCPServerConfigArgsRejectsInvalidMode(t *testing.T) {
	_, _, err := MCPServerConfigArgs([]acp.McpServer{{Stdio: &acp.McpServerStdio{Name: "mcp", Command: "mcp"}}}, "never")
	require.Error(t, err)
}

func TestMCPApprovalConfigArgsSkipsEmptyMode(t *testing.T) {
	args, err := mcpApprovalConfigArgs("remote", "")
	require.NoError(t, err)
	require.Empty(t, args)
}

func TestConfigArgRendersValueKinds(t *testing.T) {
	require.Equal(t, []string{"-c", `k="v"`}, ConfigArg("k", "v"))
	require.Equal(t, []string{"-c", "k=raw"}, ConfigArg("k", TOMLLiteral("raw")))
	require.Equal(t, []string{"-c", "k=true"}, ConfigArg("k", true))
}

func TestTOMLEnvTableSkipsEmptyNames(t *testing.T) {
	require.Equal(t, `{ A = "B" }`, TOMLEnvTable([]acp.EnvVariable{{Name: "", Value: "skip"}, {Name: "A", Value: "B"}}))
}

func TestMCPHeaderEnvTable(t *testing.T) {
	env := map[string]string{}
	table := mcpHeaderEnvTable("remote", []acp.HttpHeader{
		{Name: "Authorization", Value: "Bearer one"},
		{Name: "traceparent", Value: "00-trace-span-01"},
		{Name: "Authorization", Value: "Bearer two"},
	}, env)

	require.True(t, hasAll(table,
		`"Authorization" = "CODEX_MCP_HEADER_REMOTE_AUTHORIZATION"`,
		`"traceparent" = "CODEX_MCP_HEADER_REMOTE_TRACEPARENT"`,
		`"Authorization" = "CODEX_MCP_HEADER_REMOTE_AUTHORIZATION_2"`,
	), "table = %s", table)
	require.Equal(t, "Bearer one", env["CODEX_MCP_HEADER_REMOTE_AUTHORIZATION"])
	require.Equal(t, "00-trace-span-01", env["CODEX_MCP_HEADER_REMOTE_TRACEPARENT"])
	require.Equal(t, "Bearer two", env["CODEX_MCP_HEADER_REMOTE_AUTHORIZATION_2"])

	env = map[string]string{}
	fallback := mcpHeaderEnvTable("!!!", []acp.HttpHeader{{}, {Name: "###", Value: "v"}}, env)
	require.True(t, hasAll(fallback, `"###" = "CODEX_MCP_HEADER_SERVER_2_HEADER"`), "table = %s", fallback)
	require.Equal(t, "v", env["CODEX_MCP_HEADER_SERVER_2_HEADER"])

	require.Empty(t, mcpHeaderEnvTable("empty", []acp.HttpHeader{{}}, map[string]string{}))
}
