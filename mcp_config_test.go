package codexacp

import (
	"context"
	"encoding/json"
	"fmt"
	"testing"

	"github.com/coder/acp-go-sdk"
	"github.com/stretchr/testify/require"
)

func TestMCPConfigArgs(t *testing.T) {
	agent := NewAgent()
	args, env, err := agent.mcpServerConfigArgs([]acp.McpServer{
		{Stdio: &acp.McpServerStdio{Name: "local", Command: "tool", Args: []string{"--one"}, Env: []acp.EnvVariable{{Name: "A", Value: "B"}}}},
		{Http: &acp.McpServerHttpInline{
			Name:    "HTTP",
			Url:     "https://example.com/mcp",
			Headers: []acp.HttpHeader{{Name: "Authorization", Value: "Bearer x"}},
		}},
	}, mcpApprovalModeApprove)
	if err != nil {
		t.Fatalf("mcpServerConfigArgs returned error: %v", err)
	}
	joined := jsonString(args)
	if !containsAll(joined, "mcp_servers.local.command", "mcp_servers.HTTP.url", "env_http_headers", "Authorization", "default_tools_approval_mode") {
		t.Fatalf("args = %v", args)
	}
	if !containsArg(args, `mcp_servers.local.args=["--one"]`) ||
		!containsArg(args, `mcp_servers.local.env={ A = "B" }`) {
		t.Fatalf("TOML literals were quoted as strings: %v", args)
	}
	if env["CODEX_MCP_HEADER_HTTP_AUTHORIZATION"] != "Bearer x" {
		t.Fatalf("header env = %#v", env)
	}
	if !containsArg(args, `mcp_servers.HTTP.default_tools_approval_mode="approve"`) {
		t.Fatalf("MCP approval args missing: %v", args)
	}

	_, _, err = agent.mcpServerConfigArgs([]acp.McpServer{{Acp: &acp.McpServerAcpInline{Name: "Client", Id: "client-1"}}}, "")
	if err == nil {
		t.Fatal("unprepared ACP MCP config succeeded")
	}

	_, _, err = agent.mcpServerConfigArgs([]acp.McpServer{{Sse: &acp.McpServerSseInline{Name: "sse", Url: "https://example.com/sse"}}}, "")
	if err == nil {
		t.Fatal("SSE MCP config succeeded")
	}
}

// TestMCPConfigForwardsNameVerbatim proves accepted server names are forwarded
// unchanged into the Codex config key: a bare-key-safe name is emitted as-is,
// and a name with characters that are not valid in a TOML bare key is quoted
// (never rewritten, deduplicated, or fabricated).
func TestMCPConfigForwardsNameVerbatim(t *testing.T) {
	agent := NewAgent()
	args, _, err := agent.mcpServerConfigArgs([]acp.McpServer{
		{Stdio: &acp.McpServerStdio{Name: "Local Tool", Command: "tool", Args: []string{"--one"}}},
	}, "")
	require.NoError(t, err)
	require.True(t, containsArg(args, `mcp_servers."Local Tool".command="tool"`), "args = %v", args)
	require.True(t, containsArg(args, `mcp_servers."Local Tool".args=["--one"]`), "args = %v", args)
}

func TestTOMLEnvTableSkipsEmptyNames(t *testing.T) {
	got := tomlEnvTable([]acp.EnvVariable{{Name: "", Value: "skip"}, {Name: "A", Value: "B"}})
	if got != `{ A = "B" }` {
		t.Fatalf("tomlEnvTable = %q", got)
	}
}

func TestMCPApprovalConfigArgsRejectsInvalidMode(t *testing.T) {
	tests := []struct {
		name string
		mode string
	}{
		{name: "bad mode", mode: "never"},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if _, _, err := NewAgent().mcpServerConfigArgs([]acp.McpServer{{Stdio: &acp.McpServerStdio{Name: "mcp", Command: "mcp"}}}, test.mode); err == nil {
				t.Fatal("invalid MCP approval mode was accepted")
			}
		})
	}
}

func TestMCPApprovalConfigArgsSkipsEmptyMode(t *testing.T) {
	args, err := mcpApprovalConfigArgs("remote", "")
	if err != nil {
		t.Fatalf("empty MCP approval mode returned error: %v", err)
	}
	if len(args) != 0 {
		t.Fatalf("empty MCP approval mode emitted args: %#v", args)
	}
}

func jsonString(value any) string {
	raw, _ := json.Marshal(value)

	return string(raw)
}

func containsAll(value string, needles ...string) bool {
	for _, needle := range needles {
		if !contains(value, needle) {
			return false
		}
	}

	return true
}

func contains(value string, needle string) bool {
	for i := 0; i+len(needle) <= len(value); i++ {
		if value[i:i+len(needle)] == needle {
			return true
		}
	}

	return false
}

func containsArg(args []string, needle string) bool {
	for _, arg := range args {
		if arg == needle {
			return true
		}
	}

	return false
}

func TestMCPConfigRejectsMissingTransport(t *testing.T) {
	agent := NewAgent()
	ctx := context.Background()
	if servers, err := agent.prepareMCPServers(ctx, "s", []acp.McpServer{
		{Stdio: &acp.McpServerStdio{Name: "stdio", Command: "cmd"}},
		{Http: &acp.McpServerHttpInline{Name: "http", Url: "https://example.com"}},
	}); err != nil || len(servers) != 2 {
		t.Fatalf("prepareMCPServers = %#v err=%v", servers, err)
	}
	if _, err := agent.prepareMCPServers(ctx, "s", []acp.McpServer{{Sse: &acp.McpServerSseInline{Name: "sse"}}}); err == nil {
		t.Fatal("prepareMCPServers accepted SSE")
	}
	if _, err := agent.prepareMCPServers(ctx, "s", []acp.McpServer{{Acp: &acp.McpServerAcpInline{Name: "acp"}}}); err == nil {
		t.Fatal("prepareMCPServers accepted ACP")
	}
	if _, err := agent.prepareMCPServers(ctx, "s", []acp.McpServer{{}}); err == nil {
		t.Fatal("prepareMCPServers accepted missing transport")
	}
	if _, _, err := NewAgent().mcpServerConfigArgs([]acp.McpServer{{}}, ""); err == nil {
		t.Fatal("MCP config accepted server without transport")
	}
}

// TestMCPServerNameRequired pins R6-1: an accepted (stdio or http) MCP server
// with an empty or whitespace-only name is rejected with invalid params
// (-32602) whose data is exactly {"mcpServers[<i>].name": "required"}.
func TestMCPServerNameRequired(t *testing.T) {
	agent := NewAgent()
	ctx := context.Background()

	tests := map[string]struct {
		servers []acp.McpServer
		index   int
	}{
		"stdio empty at 0": {
			servers: []acp.McpServer{{Stdio: &acp.McpServerStdio{Name: "", Command: "cmd"}}},
			index:   0,
		},
		"http empty after valid stdio": {
			servers: []acp.McpServer{
				{Stdio: &acp.McpServerStdio{Name: "ok", Command: "cmd"}},
				{Http: &acp.McpServerHttpInline{Name: "", Url: "https://example.com"}},
			},
			index: 1,
		},
		"stdio whitespace-only at 0": {
			servers: []acp.McpServer{{Stdio: &acp.McpServerStdio{Name: "   ", Command: "cmd"}}},
			index:   0,
		},
	}

	for name, test := range tests {
		t.Run(name, func(t *testing.T) {
			_, err := agent.prepareMCPServers(ctx, "s", test.servers)

			var reqErr *acp.RequestError
			require.ErrorAs(t, err, &reqErr)
			require.Equal(t, -32602, reqErr.Code)
			require.Equal(t, map[string]any{
				fmtIndex("mcpServers[%d].name", test.index): "required",
			}, reqErr.Data)
		})
	}
}

// TestMCPServerNameDuplicate pins R6-1: a duplicate name is rejected with
// invalid params (-32602) whose data is exactly
// {"mcpServers[<i>].name": "duplicate"}, where <i> is the index of the LATER
// (offending) entry.
func TestMCPServerNameDuplicate(t *testing.T) {
	agent := NewAgent()
	ctx := context.Background()

	_, err := agent.prepareMCPServers(ctx, "s", []acp.McpServer{
		{Stdio: &acp.McpServerStdio{Name: "dup", Command: "cmd"}},
		{Http: &acp.McpServerHttpInline{Name: "unique", Url: "https://example.com"}},
		{Http: &acp.McpServerHttpInline{Name: "dup", Url: "https://example.com"}},
	})

	var reqErr *acp.RequestError
	require.ErrorAs(t, err, &reqErr)
	require.Equal(t, -32602, reqErr.Code)
	require.Equal(t, map[string]any{
		"mcpServers[2].name": "duplicate",
	}, reqErr.Data)
}

func fmtIndex(format string, index int) string {
	return fmt.Sprintf(format, index)
}

func TestMCPHeaderEnvTable(t *testing.T) {
	env := map[string]string{}
	table := mcpHeaderEnvTable("remote", []acp.HttpHeader{
		{Name: "Authorization", Value: "Bearer one"},
		{Name: "traceparent", Value: "00-trace-span-01"},
		{Name: "Authorization", Value: "Bearer two"},
	}, env)

	if !containsAll(table,
		`"Authorization" = "CODEX_MCP_HEADER_REMOTE_AUTHORIZATION"`,
		`"traceparent" = "CODEX_MCP_HEADER_REMOTE_TRACEPARENT"`,
		`"Authorization" = "CODEX_MCP_HEADER_REMOTE_AUTHORIZATION_2"`,
	) {
		t.Fatalf("env_http_headers table = %s", table)
	}
	if env["CODEX_MCP_HEADER_REMOTE_AUTHORIZATION"] != "Bearer one" ||
		env["CODEX_MCP_HEADER_REMOTE_TRACEPARENT"] != "00-trace-span-01" ||
		env["CODEX_MCP_HEADER_REMOTE_AUTHORIZATION_2"] != "Bearer two" {
		t.Fatalf("env = %#v", env)
	}

	env = map[string]string{}
	if table := mcpHeaderEnvTable("!!!", []acp.HttpHeader{{}, {Name: "###", Value: "v"}}, env); !containsAll(table, `"###" = "CODEX_MCP_HEADER_SERVER_2_HEADER"`) {
		t.Fatalf("fallback env_http_headers table = %s", table)
	}
	if env["CODEX_MCP_HEADER_SERVER_2_HEADER"] != "v" {
		t.Fatalf("fallback env = %#v", env)
	}
	if table := mcpHeaderEnvTable("empty", []acp.HttpHeader{{}}, map[string]string{}); table != "" {
		t.Fatalf("empty env_http_headers table = %s", table)
	}
}

func TestStableMCPServersFromUnstable(t *testing.T) {
	if stableMCPServersFromUnstable(nil) != nil {
		t.Fatal("nil unstable MCP conversion returned non-nil")
	}
	servers := stableMCPServersFromUnstable([]acp.UnstableMcpServer{
		{Stdio: &acp.McpServerStdio{Name: "stdio", Command: "cmd"}},
		{Http: &acp.UnstableMcpServerHttp{Name: "http", Url: "https://example.com", Headers: []acp.HttpHeader{{Name: "H", Value: "V"}}, Meta: map[string]any{"m": "h"}}},
		{Acp: &acp.UnstableMcpServerAcpInline{Name: "acp", Id: "id", Meta: map[string]any{"m": "a"}}},
		{Sse: &acp.UnstableMcpServerSse{Name: "sse", Url: "https://example.com/sse", Headers: []acp.HttpHeader{{Name: "S", Value: "V"}}, Meta: map[string]any{"m": "s"}}},
		{},
	})
	if len(servers) != 4 || servers[0].Stdio == nil || servers[1].Http == nil || servers[2].Acp == nil || servers[3].Sse == nil {
		t.Fatalf("stable MCP servers = %#v", servers)
	}
}
