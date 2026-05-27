package codexacp

import (
	"encoding/json"
	"testing"

	"github.com/coder/acp-go-sdk"
)

func TestMCPConfigArgs(t *testing.T) {
	agent := NewAgent(WithMCPProxyCommand("/bin/proxy", "--debug"))
	args, err := agent.mcpServerConfigArgs([]acp.McpServer{
		{Stdio: &acp.McpServerStdio{Name: "Local Tool", Command: "tool", Args: []string{"--one"}, Env: []acp.EnvVariable{{Name: "A", Value: "B"}}}},
		{Http: &acp.McpServerHttpInline{Name: "HTTP", Url: "https://example.com/mcp", Headers: []acp.HttpHeader{{Name: "Authorization", Value: "Bearer x"}}}},
	})
	if err != nil {
		t.Fatalf("mcpServerConfigArgs returned error: %v", err)
	}
	joined := jsonString(args)
	if !containsAll(joined, "mcp_servers.Local_Tool.command", "streamable_http", "Authorization") {
		t.Fatalf("args = %v", args)
	}
	if !containsArg(args, `mcp_servers.Local_Tool.args=["--one"]`) ||
		!containsArg(args, `mcp_servers.Local_Tool.env={ A = "B" }`) {
		t.Fatalf("TOML literals were quoted as strings: %v", args)
	}

	_, err = agent.mcpServerConfigArgs([]acp.McpServer{{Acp: &acp.McpServerAcpInline{Name: "Client", Id: "client-1"}}})
	if err == nil {
		t.Fatal("unprepared ACP MCP config succeeded")
	}

	_, err = agent.mcpServerConfigArgs([]acp.McpServer{{Sse: &acp.McpServerSseInline{Name: "sse", Url: "https://example.com/sse"}}})
	if err == nil {
		t.Fatal("SSE MCP config succeeded")
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
	if _, err := NewAgent().mcpServerConfigArgs([]acp.McpServer{{}}); err == nil {
		t.Fatal("MCP config accepted server without transport")
	}
	if name := mcpServerConfigName(acp.McpServer{Stdio: &acp.McpServerStdio{Name: "!!!"}}, 4, map[string]int{}); name != "server_5" {
		t.Fatalf("sanitized empty MCP name = %q", name)
	}
}
