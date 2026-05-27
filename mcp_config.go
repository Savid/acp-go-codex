package codexacp

import (
	"fmt"
	"regexp"
	"strconv"
	"strings"

	"github.com/coder/acp-go-sdk"
)

const (
	mcpTransportStreamableHTTP = "streamable_http"
)

var mcpNamePartRE = regexp.MustCompile(`[^A-Za-z0-9_-]+`)

type tomlLiteral string

func (a *Agent) mcpServerConfigArgs(servers []acp.McpServer) ([]string, error) {
	if len(servers) == 0 {
		return nil, nil
	}

	args := make([]string, 0, len(servers)*8)
	seen := map[string]int{}

	for index, server := range servers {
		name := mcpServerConfigName(server, index, seen)
		switch {
		case server.Stdio != nil:
			args = append(args, codexConfigArg("mcp_servers."+name+".command", server.Stdio.Command)...)
			if len(server.Stdio.Args) > 0 {
				args = append(args, codexConfigArg("mcp_servers."+name+".args", tomlLiteral(tomlStringArray(server.Stdio.Args)))...)
			}
			if len(server.Stdio.Env) > 0 {
				args = append(args, codexConfigArg("mcp_servers."+name+".env", tomlLiteral(tomlEnvTable(server.Stdio.Env)))...)
			}
		case server.Http != nil:
			args = append(args,
				codexConfigArg("mcp_servers."+name+".type", mcpTransportStreamableHTTP)...,
			)
			args = append(args, codexConfigArg("mcp_servers."+name+".url", server.Http.Url)...)
			if len(server.Http.Headers) > 0 {
				// Codex only accepts process-scoped MCP config today. Header values
				// therefore ride in -c argv and may be visible to same-user process
				// inspection; prefer ACP-transport MCP for sensitive credentials.
				args = append(args, codexConfigArg("mcp_servers."+name+".headers", tomlLiteral(tomlHeaderTable(server.Http.Headers)))...)
			}
		case server.Acp != nil:
			return nil, acp.NewInvalidParams(map[string]any{"mcpServers": "ACP MCP servers must be rewritten by the session MCP bridge before Codex config generation"})
		case server.Sse != nil:
			return nil, acp.NewInvalidParams(map[string]any{"mcpServers": "SSE MCP is not supported by Codex"})
		default:
			return nil, acp.NewInvalidParams(map[string]any{"mcpServers": fmt.Sprintf("server %d has no transport", index)})
		}
	}

	return args, nil
}

func mcpServerConfigName(server acp.McpServer, index int, seen map[string]int) string {
	name := ""
	switch {
	case server.Stdio != nil:
		name = server.Stdio.Name
	case server.Http != nil:
		name = server.Http.Name
	case server.Acp != nil:
		name = server.Acp.Name
	case server.Sse != nil:
		name = server.Sse.Name
	}
	if strings.TrimSpace(name) == "" {
		name = fmt.Sprintf("server_%d", index+1)
	}

	name = strings.Trim(mcpNamePartRE.ReplaceAllString(name, "_"), "_")
	if name == "" {
		name = fmt.Sprintf("server_%d", index+1)
	}

	count := seen[name]
	seen[name] = count + 1
	if count > 0 {
		name = fmt.Sprintf("%s_%d", name, count+1)
	}

	return name
}

func codexConfigArg(key string, value any) []string {
	switch typed := value.(type) {
	case tomlLiteral:
		return []string{"-c", key + "=" + string(typed)}
	case string:
		return []string{"-c", key + "=" + tomlString(typed)}
	default:
		return []string{"-c", key + "=" + fmt.Sprint(typed)}
	}
}

func tomlString(value string) string {
	return strconv.Quote(value)
}

func tomlStringArray(values []string) string {
	items := make([]string, 0, len(values))
	for _, value := range values {
		items = append(items, tomlString(value))
	}

	return "[" + strings.Join(items, ", ") + "]"
}

func tomlEnvTable(env []acp.EnvVariable) string {
	items := make([]string, 0, len(env))
	for _, variable := range env {
		if variable.Name == "" {
			continue
		}
		items = append(items, variable.Name+" = "+tomlString(variable.Value))
	}

	return "{ " + strings.Join(items, ", ") + " }"
}

func tomlHeaderTable(headers []acp.HttpHeader) string {
	items := make([]string, 0, len(headers))
	for _, header := range headers {
		if header.Name == "" {
			continue
		}
		items = append(items, tomlString(header.Name)+" = "+tomlString(header.Value))
	}

	return "{ " + strings.Join(items, ", ") + " }"
}

func stableMCPServersFromUnstable(servers []acp.UnstableMcpServer) []acp.McpServer {
	if servers == nil {
		return nil
	}
	out := make([]acp.McpServer, 0, len(servers))
	for _, server := range servers {
		switch {
		case server.Stdio != nil:
			out = append(out, acp.McpServer{Stdio: cloneMCPServerStdio(server.Stdio)})
		case server.Http != nil:
			value := acp.McpServerHttpInline{
				Meta:    cloneAnyMap(server.Http.Meta),
				Headers: cloneHTTPHeaders(server.Http.Headers),
				Name:    server.Http.Name,
				Type:    server.Http.Type,
				Url:     server.Http.Url,
			}
			out = append(out, acp.McpServer{Http: &value})
		case server.Acp != nil:
			value := acp.McpServerAcpInline{
				Meta: cloneAnyMap(server.Acp.Meta),
				Id:   acp.McpServerAcpId(server.Acp.Id),
				Name: server.Acp.Name,
				Type: server.Acp.Type,
			}
			out = append(out, acp.McpServer{Acp: &value})
		case server.Sse != nil:
			value := acp.McpServerSseInline{
				Meta:    cloneAnyMap(server.Sse.Meta),
				Headers: cloneHTTPHeaders(server.Sse.Headers),
				Name:    server.Sse.Name,
				Type:    server.Sse.Type,
				Url:     server.Sse.Url,
			}
			out = append(out, acp.McpServer{Sse: &value})
		}
	}

	return out
}
