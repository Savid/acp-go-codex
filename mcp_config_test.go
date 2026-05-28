package codexacp

import (
	"encoding/json"
	"testing"

	"github.com/coder/acp-go-sdk"
)

func TestMCPConfigArgs(t *testing.T) {
	agent := NewAgent(WithMCPProxyCommand("/bin/proxy", "--debug"))
	args, env, err := agent.mcpServerConfigArgs([]acp.McpServer{
		{Stdio: &acp.McpServerStdio{Name: "Local Tool", Command: "tool", Args: []string{"--one"}, Env: []acp.EnvVariable{{Name: "A", Value: "B"}}}},
		{Http: &acp.McpServerHttpInline{
			Name:    "HTTP",
			Url:     "https://example.com/mcp",
			Headers: []acp.HttpHeader{{Name: "Authorization", Value: "Bearer x"}},
			Meta: map[string]any{
				codexMetaKey: map[string]any{
					mcpDefaultApprovalModeKey: mcpApprovalModeApprove,
					mcpToolsKey: map[string]any{
						"search": map[string]any{mcpToolApprovalModeKey: mcpApprovalModeAuto},
					},
				},
			},
		}},
	})
	if err != nil {
		t.Fatalf("mcpServerConfigArgs returned error: %v", err)
	}
	joined := jsonString(args)
	if !containsAll(joined, "mcp_servers.Local_Tool.command", "mcp_servers.HTTP.url", "env_http_headers", "Authorization", "default_tools_approval_mode", "tools.search.approval_mode") {
		t.Fatalf("args = %v", args)
	}
	if !containsArg(args, `mcp_servers.Local_Tool.args=["--one"]`) ||
		!containsArg(args, `mcp_servers.Local_Tool.env={ A = "B" }`) {
		t.Fatalf("TOML literals were quoted as strings: %v", args)
	}
	if env["CODEX_MCP_HEADER_HTTP_AUTHORIZATION"] != "Bearer x" {
		t.Fatalf("header env = %#v", env)
	}
	if !containsArg(args, `mcp_servers.HTTP.default_tools_approval_mode="approve"`) ||
		!containsArg(args, `mcp_servers.HTTP.tools.search.approval_mode="auto"`) {
		t.Fatalf("MCP approval args missing: %v", args)
	}

	_, _, err = agent.mcpServerConfigArgs([]acp.McpServer{{Acp: &acp.McpServerAcpInline{Name: "Client", Id: "client-1"}}})
	if err == nil {
		t.Fatal("unprepared ACP MCP config succeeded")
	}

	_, _, err = agent.mcpServerConfigArgs([]acp.McpServer{{Sse: &acp.McpServerSseInline{Name: "sse", Url: "https://example.com/sse"}}})
	if err == nil {
		t.Fatal("SSE MCP config succeeded")
	}
}

func TestMCPApprovalConfigArgsRejectsInvalidMeta(t *testing.T) {
	tests := []struct {
		name   string
		server acp.McpServer
	}{
		{
			name: "bad default mode type",
			server: acp.McpServer{Stdio: &acp.McpServerStdio{Name: "mcp", Meta: map[string]any{
				codexMetaKey: map[string]any{mcpDefaultApprovalModeKey: 1},
			}}},
		},
		{
			name: "bad default mode value",
			server: acp.McpServer{Stdio: &acp.McpServerStdio{Name: "mcp", Meta: map[string]any{
				codexMetaKey: map[string]any{mcpDefaultApprovalModeKey: "never"},
			}}},
		},
		{
			name: "bad tools type",
			server: acp.McpServer{Stdio: &acp.McpServerStdio{Name: "mcp", Meta: map[string]any{
				codexMetaKey: map[string]any{mcpToolsKey: []any{}},
			}}},
		},
		{
			name: "empty tool name",
			server: acp.McpServer{Stdio: &acp.McpServerStdio{Name: "mcp", Meta: map[string]any{
				codexMetaKey: map[string]any{mcpToolsKey: map[string]any{" ": map[string]any{mcpToolApprovalModeKey: mcpApprovalModeApprove}}},
			}}},
		},
		{
			name: "bad tool config type",
			server: acp.McpServer{Stdio: &acp.McpServerStdio{Name: "mcp", Meta: map[string]any{
				codexMetaKey: map[string]any{mcpToolsKey: map[string]any{"execute": mcpApprovalModeApprove}},
			}}},
		},
		{
			name: "bad tool mode value",
			server: acp.McpServer{Stdio: &acp.McpServerStdio{Name: "mcp", Meta: map[string]any{
				codexMetaKey: map[string]any{mcpToolsKey: map[string]any{"execute": map[string]any{mcpToolApprovalModeKey: "never"}}},
			}}},
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if _, _, err := NewAgent().mcpServerConfigArgs([]acp.McpServer{test.server}); err == nil {
				t.Fatal("invalid MCP approval metadata was accepted")
			}
		})
	}
}

func TestMCPApprovalConfigArgsSkipsPartialMeta(t *testing.T) {
	args, err := mcpApprovalConfigArgs("remote", acp.McpServer{Http: &acp.McpServerHttpInline{
		Meta: map[string]any{
			codexMetaKey: map[string]any{
				mcpToolsKey: map[string]any{
					"execute": map[string]any{"unrelated": true},
				},
			},
		},
	}})
	if err != nil {
		t.Fatalf("partial MCP approval metadata returned error: %v", err)
	}
	if len(args) != 0 {
		t.Fatalf("partial MCP approval metadata emitted args: %#v", args)
	}
}

func TestMCPServerMeta(t *testing.T) {
	if mcpServerMeta(acp.McpServer{Stdio: &acp.McpServerStdio{Meta: map[string]any{"kind": "stdio"}}})["kind"] != "stdio" {
		t.Fatal("stdio MCP metadata not returned")
	}
	if mcpServerMeta(acp.McpServer{Http: &acp.McpServerHttpInline{Meta: map[string]any{"kind": "http"}}})["kind"] != "http" {
		t.Fatal("HTTP MCP metadata not returned")
	}
	if mcpServerMeta(acp.McpServer{Acp: &acp.McpServerAcpInline{Meta: map[string]any{"kind": "acp"}}})["kind"] != "acp" {
		t.Fatal("ACP MCP metadata not returned")
	}
	if mcpServerMeta(acp.McpServer{Sse: &acp.McpServerSseInline{Meta: map[string]any{"kind": "sse"}}})["kind"] != "sse" {
		t.Fatal("SSE MCP metadata not returned")
	}
	if mcpServerMeta(acp.McpServer{}) != nil {
		t.Fatal("empty MCP server returned metadata")
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
	if _, _, err := NewAgent().mcpServerConfigArgs([]acp.McpServer{{}}); err == nil {
		t.Fatal("MCP config accepted server without transport")
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
