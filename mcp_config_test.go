package codexacp

import (
	"context"
	"encoding/json"
	"fmt"
	"testing"

	"github.com/coder/acp-go-sdk"
	"github.com/stretchr/testify/require"
)

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

func TestMCPConfigRejectsMissingTransport(t *testing.T) {
	agent := NewAgent()
	ctx := context.Background()
	if servers, err := agent.prepareMCPServers(ctx, "s", []acp.McpServer{
		{Stdio: &acp.McpServerStdio{Name: "stdio", Command: "cmd"}},
		{Http: &acp.McpServerHttpInline{Name: "http", Url: "https://example.com"}},
	}); err != nil || len(servers) != 2 {
		t.Fatalf("prepareMCPServers = %#v err=%v", servers, err)
	}
	_, sseErr := agent.prepareMCPServers(ctx, "s", []acp.McpServer{{Sse: &acp.McpServerSseInline{Name: "sse"}}})
	var sseReqErr *acp.RequestError
	require.ErrorAs(t, sseErr, &sseReqErr)
	require.Equal(t, -32602, sseReqErr.Code)
	require.Equal(t, map[string]any{
		"error":  "unsupported",
		"field":  "mcpServers[0]",
		"server": "sse",
	}, sseReqErr.Data)

	_, acpErr := agent.prepareMCPServers(ctx, "s", []acp.McpServer{{Acp: &acp.McpServerAcpInline{Name: "acp"}}})
	var acpReqErr *acp.RequestError
	require.ErrorAs(t, acpErr, &acpReqErr)
	require.Equal(t, -32602, acpReqErr.Code)
	require.Equal(t, map[string]any{
		"error":  "unsupported",
		"field":  "mcpServers[0]",
		"server": "acp",
	}, acpReqErr.Data)
	if _, err := agent.prepareMCPServers(ctx, "s", []acp.McpServer{{}}); err == nil {
		t.Fatal("prepareMCPServers accepted missing transport")
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
