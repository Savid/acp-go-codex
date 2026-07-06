package codexacp

import (
	"context"
	"encoding/json"
	"io"
	"testing"

	"github.com/coder/acp-go-sdk"
	"github.com/savid/acp-go-codex/internal/codex"
)

func TestRequestBuilderClones(t *testing.T) {
	meta := map[string]any{"x": []any{map[string]any{"y": "z"}}}
	env := map[string]string{"SECRET": "value"}
	approvalPolicy := map[string]any{"mode": "never"}
	sandboxPolicy := map[string]any{"type": "workspaceWrite"}
	req := NewSessionRequest("/repo",
		WithSessionAdditionalDirectories("/extra"),
		WithSessionRawEvents(true),
		WithSessionMeta(meta),
		WithSessionCodexOptions(NewCodexOptions(
			WithCodexModel("gpt"),
			WithCodexEffort("low"),
			WithCodexEnv(env),
			WithCodexApprovalPolicy(approvalPolicy),
			WithCodexSandboxPolicy(sandboxPolicy),
			WithCodexOutputSchema(map[string]any{"type": "object"}),
		)),
	)
	asType[map[string]any](t, asType[[]any](t, meta["x"])[0])["y"] = "changed"
	env["SECRET"] = "changed"
	approvalPolicy["mode"] = "changed"
	sandboxPolicy["type"] = "changed"
	if asType[map[string]any](t, asType[[]any](t, req.Meta["x"])[0])["y"] != "z" {
		t.Fatal("WithSessionMeta did not clone input")
	}
	options := asType[map[string]any](t, asType[map[string]any](t, req.Meta[codexMetaKey])[metaOptionsKey])
	if asType[map[string]string](t, options[metaEnvKey])["SECRET"] != "value" {
		t.Fatalf("Codex env was not cloned: %#v", options)
	}
	if asType[map[string]any](t, options[metaApprovalPolicyKey])["mode"] != "never" {
		t.Fatalf("Codex approval policy was not cloned: %#v", options)
	}
	if asType[map[string]any](t, options[metaSandboxPolicyKey])["type"] != "workspaceWrite" {
		t.Fatalf("Codex sandbox policy was not cloned: %#v", options)
	}
	if len(LoadSessionRequest("s", "/repo").McpServers) != 0 || ResumeSessionRequest("s", "/repo").SessionId != "s" || ForkSessionRequest("s", "/repo").SessionId != "s" {
		t.Fatal("request builders returned unexpected values")
	}
	if DeleteSessionRequest("s").SessionId != "s" {
		t.Fatal("delete session request returned unexpected value")
	}
	if SetConfigOptionRequest("s", configEffort, "high").ValueId.Value != "high" {
		t.Fatal("set config option request returned unexpected value")
	}
	if SetModelRequest("s", "gpt-next").ValueId.Value != "gpt-next" {
		t.Fatal("set model request returned unexpected value")
	}
	list := ListSessionsRequest(WithListSessionsCwd("/repo"), WithListSessionsCursor("c"), WithListSessionsMeta(map[string]any{"a": "b"}))
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
	asType[[]any](t, cloned["x"])[0] = "b"
	if asType[[]any](t, asType[map[string]any](t, meta[codexMetaKey])["x"])[0] != "b" {
		t.Fatal("ensureMetaMap did not install cloned map")
	}
	if cloneMCPServers(nil) != nil || cloneMCPServerStdio(nil) != nil || cloneEnvVariables(nil) != nil || unstableMCPServersFromStable(nil) != nil {
		t.Fatal("nil MCP clone helpers returned non-nil")
	}
	if cloneMCPServer(acp.McpServer{Sse: &acp.McpServerSseInline{Name: "sse", Headers: []acp.HttpHeader{{Name: "H"}}}}).Sse == nil {
		t.Fatal("SSE MCP server did not clone")
	}
	if cloneMCPServer(acp.McpServer{Http: &acp.McpServerHttpInline{Name: "http", Headers: []acp.HttpHeader{{Name: "H"}}}}).Http == nil {
		t.Fatal("HTTP MCP server did not clone")
	}
	if cloneMCPServer(acp.McpServer{Acp: &acp.McpServerAcpInline{Name: "acp"}}).Acp == nil {
		t.Fatal("ACP MCP server did not clone")
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
	if unstableMCPServerFromStable(acp.McpServer{Acp: &acp.McpServerAcpInline{Name: "acp"}}).Acp == nil {
		t.Fatal("stable ACP MCP did not convert")
	}
	if unstableMCPServerFromStable(acp.McpServer{}).Stdio != nil {
		t.Fatal("empty stable MCP converted to stdio")
	}

	stdio := StdioMCPServer("stdio", "cmd", []string{"arg"}, map[string]string{"A": "B"})
	if stdio.Stdio == nil || stdio.Stdio.Args[0] != "arg" || stdio.Stdio.Env[0].Value != "B" {
		t.Fatalf("StdioMCPServer = %#v", stdio)
	}
	httpServer := HTTPMCPServer("http", "https://example.com", map[string]string{"H": "V"})
	if httpServer.Http == nil || httpServer.Http.Headers[0].Value != "V" {
		t.Fatalf("HTTPMCPServer = %#v", httpServer)
	}
	options := NewCodexOptions(WithCodexMCPToolApprovalMode("prompt"))
	if options.MCPToolApprovalMode != "prompt" {
		t.Fatalf("MCP approval option = %#v", options)
	}
}

func TestCallForkSessionHelper(t *testing.T) {
	ctx := context.Background()
	c2aR, c2aW := io.Pipe()
	a2cR, a2cW := io.Pipe()
	t.Cleanup(func() {
		_ = c2aR.Close()
		_ = c2aW.Close()
		_ = a2cR.Close()
		_ = a2cW.Close()
	})

	clientConn := acp.NewClientSideConnection(&recordingClient{}, c2aW, a2cR)
	agent := NewAgent(withClientFactory(func(context.Context, codex.Options) (codex.Client, error) {
		return newSpyCodexClient(), nil
	}))
	agentConn := newLocalAgentConnection(agent, a2cW, c2aR)
	agent.setAgentClient(agentConn)

	if _, err := clientConn.Initialize(ctx, acp.InitializeRequest{}); err != nil {
		t.Fatalf("Initialize returned error: %v", err)
	}
	parent, err := clientConn.NewSession(ctx, NewSessionRequest("/tmp/project"))
	if err != nil {
		t.Fatalf("NewSession returned error: %v", err)
	}
	fork, err := CallForkSession(ctx, clientConn, ForkSessionRequest(parent.SessionId, "/tmp/project"))
	if err != nil {
		t.Fatalf("CallForkSession returned error: %v", err)
	}
	if fork.SessionId == "" {
		t.Fatalf("fork response = %#v", fork)
	}
	if _, err := CallForkSession(ctx, clientConn, acp.UnstableForkSessionRequest{}); err == nil {
		t.Fatal("CallForkSession accepted invalid request")
	}

	badC2AR, badC2AW := io.Pipe()
	badA2CR, badA2CW := io.Pipe()
	t.Cleanup(func() {
		_ = badC2AR.Close()
		_ = badC2AW.Close()
		_ = badA2CR.Close()
		_ = badA2CW.Close()
	})
	badClientConn := acp.NewClientSideConnection(&recordingClient{}, badC2AW, badA2CR)
	badPeer := acp.NewConnection(func(context.Context, string, json.RawMessage) (any, *acp.RequestError) {
		return "not a fork response", nil
	}, badA2CW, badC2AR)
	_ = badPeer
	if _, err := CallForkSession(ctx, badClientConn, ForkSessionRequest("parent", "/tmp/project")); err == nil {
		t.Fatal("CallForkSession accepted undecodable response")
	}
}
