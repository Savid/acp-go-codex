package codexacp

import (
	"context"
	"fmt"
	"regexp"
	"strconv"
	"strings"

	"github.com/coder/acp-go-sdk"
)

const (
	mcpHeaderEnvPrefix     = "CODEX_MCP_HEADER"
	mcpApprovalModeAuto    = "auto"
	mcpApprovalModePrompt  = "prompt"
	mcpApprovalModeApprove = "approve"
	codexConfigFlag        = "-c"
	mcpServersKey          = "mcpServers"
)

var mcpNamePartRE = regexp.MustCompile(`[^A-Za-z0-9_-]+`)
var mcpEnvPartRE = regexp.MustCompile(`[^A-Za-z0-9]+`)

type tomlLiteral string

func (a *Agent) prepareMCPServers(_ context.Context, _ acp.SessionId, servers []acp.McpServer) ([]acp.McpServer, error) {
	for index, server := range servers {
		switch {
		case server.Stdio != nil, server.Http != nil:
			continue
		case server.Sse != nil:
			return nil, acp.NewInvalidParams(map[string]any{mcpServersKey: map[string]any{"index": index, jsonFieldName: mcpServerName(server), jsonFieldError: "SSE MCP is not supported"}})
		case server.Acp != nil:
			return nil, acp.NewInvalidParams(map[string]any{mcpServersKey: map[string]any{"index": index, jsonFieldName: mcpServerName(server), jsonFieldError: "ACP MCP transport is not supported"}})
		default:
			return nil, acp.NewInvalidParams(map[string]any{mcpServersKey: fmt.Sprintf("server %d has no transport", index)})
		}
	}

	return cloneMCPServers(servers), nil
}

func (a *Agent) mcpServerConfigArgs(servers []acp.McpServer, defaultApprovalMode string) ([]string, map[string]string, error) {
	if len(servers) == 0 {
		return nil, nil, nil
	}

	args := make([]string, 0, len(servers)*8)
	env := map[string]string{}
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
			args = append(args, codexConfigArg("mcp_servers."+name+".url", server.Http.Url)...)
			if len(server.Http.Headers) > 0 {
				headerEnv := mcpHeaderEnvTable(name, server.Http.Headers, env)
				if headerEnv != "" {
					args = append(args, codexConfigArg("mcp_servers."+name+".env_http_headers", tomlLiteral(headerEnv))...)
				}
			}
		case server.Acp != nil:
			return nil, nil, acp.NewInvalidParams(map[string]any{mcpServersKey: "ACP MCP transport is not supported"})
		case server.Sse != nil:
			return nil, nil, acp.NewInvalidParams(map[string]any{mcpServersKey: "SSE MCP is not supported by Codex"})
		default:
			return nil, nil, acp.NewInvalidParams(map[string]any{mcpServersKey: fmt.Sprintf("server %d has no transport", index)})
		}

		approvalArgs, err := mcpApprovalConfigArgs(name, defaultApprovalMode)
		if err != nil {
			return nil, nil, err
		}

		args = append(args, approvalArgs...)
	}

	return args, env, nil
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

func mcpApprovalConfigArgs(serverConfigName string, defaultMode string) ([]string, error) {
	if defaultMode == "" {
		return nil, nil
	}

	if !validMCPApprovalMode(defaultMode) {
		return nil, acp.NewInvalidParams(map[string]any{mcpServersKey: "_meta.codex.options.mcpToolApprovalMode must be one of auto, prompt, approve"})
	}

	return codexConfigArg("mcp_servers."+serverConfigName+".default_tools_approval_mode", defaultMode), nil
}

func validMCPApprovalMode(mode string) bool {
	switch mode {
	case mcpApprovalModeAuto, mcpApprovalModePrompt, mcpApprovalModeApprove:
		return true
	default:
		return false
	}
}

func codexConfigArg(key string, value any) []string {
	switch typed := value.(type) {
	case tomlLiteral:
		return []string{codexConfigFlag, key + "=" + string(typed)}
	case string:
		return []string{codexConfigFlag, key + "=" + tomlString(typed)}
	default:
		return []string{codexConfigFlag, key + "=" + fmt.Sprint(typed)}
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

func mcpHeaderEnvTable(serverName string, headers []acp.HttpHeader, env map[string]string) string {
	items := make([]string, 0, len(headers))
	seen := map[string]int{}

	for index, header := range headers {
		if header.Name == "" {
			continue
		}

		envName := mcpHeaderEnvName(serverName, header.Name, index, seen)
		env[envName] = header.Value
		items = append(items, tomlString(header.Name)+" = "+tomlString(envName))
	}

	if len(items) == 0 {
		return ""
	}

	return "{ " + strings.Join(items, ", ") + " }"
}

func mcpHeaderEnvName(serverName string, headerName string, index int, seen map[string]int) string {
	base := strings.Trim(mcpEnvPartRE.ReplaceAllString(strings.ToUpper(serverName+"_"+headerName), "_"), "_")
	if base == "" {
		base = fmt.Sprintf("SERVER_%d_HEADER", index+1)
	}

	name := mcpHeaderEnvPrefix + "_" + base
	count := seen[name]

	seen[name] = count + 1
	if count > 0 {
		name = fmt.Sprintf("%s_%d", name, count+1)
	}

	return name
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
