package codexacp

import (
	"testing"

	"github.com/coder/acp-go-sdk"
)

func TestRequestBuilderClones(t *testing.T) {
	meta := map[string]any{"x": []any{map[string]any{"y": "z"}}}
	req := NewSessionRequest("/repo",
		WithSessionAdditionalDirectories("/extra"),
		WithSessionRawSDKMessages(true),
		WithSessionMeta(meta),
		WithSessionCodexOptions(NewCodexOptions(WithCodexModel("gpt"), WithCodexEffort("low"), WithCodexOutputSchema(map[string]any{"type": "object"}))),
	)
	meta["x"].([]any)[0].(map[string]any)["y"] = "changed"
	if req.Meta["x"].([]any)[0].(map[string]any)["y"] != "z" {
		t.Fatal("WithSessionMeta did not clone input")
	}
	if len(LoadSessionRequest("s", "/repo").McpServers) != 0 || ResumeSessionRequest("s", "/repo").SessionId != "s" || ForkSessionRequest("s", "/repo").SessionId != "s" {
		t.Fatal("request builders returned unexpected values")
	}
	list := ListSessionsRequest(WithListSessionsCwd("/repo"), WithListSessionsCursor("c"), WithListSessionsAdditionalDirectories("/extra"), WithListSessionsMeta(map[string]any{"a": "b"}))
	if *list.Cwd != "/repo" || *list.Cursor != "c" || list.Meta["a"] != "b" {
		t.Fatalf("list request = %#v", list)
	}

	stdio := acp.McpServer{Stdio: &acp.McpServerStdio{
		Name: "stdio", Command: "tool", Args: []string{"a"},
		Env:  []acp.EnvVariable{{Name: "A", Value: "B", Meta: map[string]any{"x": "y"}}},
		Meta: map[string]any{"s": "m"},
	}}
	httpServer := acp.McpServer{Http: &acp.McpServerHttpInline{Name: "http", Url: "https://example.com", Headers: []acp.HttpHeader{{Name: "H", Value: "V", Meta: map[string]any{"h": "m"}}}, Meta: map[string]any{"h": "m"}}}
	sse := acp.McpServer{Sse: &acp.McpServerSseInline{Name: "sse", Url: "https://example.com/sse", Headers: []acp.HttpHeader{{Name: "S", Value: "V"}}, Meta: map[string]any{"s": "m"}}}
	acpServer := acp.McpServer{Acp: &acp.McpServerAcpInline{Name: "acp", Id: "a1", Meta: map[string]any{"a": "m"}}}
	sessionReq := NewSessionRequest("/repo", WithSessionMCPServers(stdio, httpServer, sse, acpServer), WithSessionOutputSchema(map[string]any{"type": "object"}))
	if len(sessionReq.McpServers) != 4 {
		t.Fatalf("mcp servers = %#v", sessionReq.McpServers)
	}
	stdio.Stdio.Args[0] = "mutated"
	stdio.Stdio.Env[0].Value = "mutated"
	httpServer.Http.Headers[0].Value = "mutated"
	if sessionReq.McpServers[0].Stdio.Args[0] != "a" || sessionReq.McpServers[0].Stdio.Env[0].Value != "B" || sessionReq.McpServers[1].Http.Headers[0].Value != "V" {
		t.Fatalf("MCP servers were not cloned: %#v", sessionReq.McpServers)
	}
	unstable := unstableMCPServersFromStable(sessionReq.McpServers)
	if len(unstable) != 4 || unstable[0].Stdio == nil || unstable[1].Http == nil || unstable[2].Sse == nil || unstable[3].Acp == nil {
		t.Fatalf("unstable MCP servers = %#v", unstable)
	}
}

func TestRequestBuilderHelperBranches(t *testing.T) {
	if cloneAnySlice(nil) != nil {
		t.Fatal("nil slice clone returned non-nil")
	}

	meta := map[string]any{codexMetaKey: map[string]any{"x": []any{"a"}}}
	cloned := ensureMetaMap(meta, codexMetaKey)
	cloned["x"].([]any)[0] = "b"
	if meta[codexMetaKey].(map[string]any)["x"].([]any)[0] != "b" {
		t.Fatal("ensureMetaMap did not install cloned map")
	}
	if cloneMCPServers(nil) != nil || cloneMCPServerStdio(nil) != nil || cloneEnvVariables(nil) != nil || unstableMCPServersFromStable(nil) != nil {
		t.Fatal("nil MCP clone helpers returned non-nil")
	}
	if cloneMCPServer(acp.McpServer{Sse: &acp.McpServerSseInline{Name: "sse", Headers: []acp.HttpHeader{{Name: "H"}}}}).Sse == nil {
		t.Fatal("SSE MCP server did not clone")
	}
	if cloneMCPServer(acp.McpServer{Stdio: &acp.McpServerStdio{Name: "stdio"}}).Stdio == nil {
		t.Fatal("stdio MCP server did not clone")
	}
	if cloneMCPServer(acp.McpServer{}) != (acp.McpServer{}) {
		t.Fatal("empty MCP server clone changed value")
	}
	if unstableMCPServerFromStable(acp.McpServer{Sse: &acp.McpServerSseInline{Name: "sse"}}).Sse == nil {
		t.Fatal("stable SSE MCP did not convert")
	}
	if unstableMCPServerFromStable(acp.McpServer{}).Stdio != nil {
		t.Fatal("empty stable MCP converted to stdio")
	}
}
