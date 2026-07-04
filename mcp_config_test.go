package codexacp

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/coder/acp-go-sdk"
)

func TestMCPConfigArgs(t *testing.T) {
	agent := NewAgent()
	args, env, err := agent.mcpServerConfigArgs([]acp.McpServer{
		{Stdio: &acp.McpServerStdio{Name: "Local Tool", Command: "tool", Args: []string{"--one"}, Env: []acp.EnvVariable{{Name: "A", Value: "B"}}}},
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
	if !containsAll(joined, "mcp_servers.Local_Tool.command", "mcp_servers.HTTP.url", "env_http_headers", "Authorization", "default_tools_approval_mode") {
		t.Fatalf("args = %v", args)
	}
	if !containsArg(args, `mcp_servers.Local_Tool.args=["--one"]`) ||
		!containsArg(args, `mcp_servers.Local_Tool.env={ A = "B" }`) {
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

func TestMCPConfigRejectsMissingTransportAndSanitizesName(t *testing.T) {
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
	seen := map[string]int{}
	if name := mcpServerConfigName(acp.McpServer{Stdio: &acp.McpServerStdio{Name: "Same"}}, 0, seen); name != "Same" {
		t.Fatalf("first duplicate name = %q", name)
	}
	if name := mcpServerConfigName(acp.McpServer{Http: &acp.McpServerHttpInline{Name: "Same"}}, 1, seen); name != "Same_2" {
		t.Fatalf("second duplicate name = %q", name)
	}
	if name := mcpServerConfigName(acp.McpServer{Acp: &acp.McpServerAcpInline{Name: "Client"}}, 2, map[string]int{}); name != "Client" {
		t.Fatalf("ACP name = %q", name)
	}
	if name := mcpServerConfigName(acp.McpServer{Sse: &acp.McpServerSseInline{Name: "Events"}}, 3, map[string]int{}); name != "Events" {
		t.Fatalf("SSE name = %q", name)
	}
	if name := mcpServerConfigName(acp.McpServer{Stdio: &acp.McpServerStdio{Name: "!!!"}}, 4, map[string]int{}); name != "server_5" {
		t.Fatalf("sanitized empty MCP name = %q", name)
	}
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
