package codexacp

import (
	"context"
	"fmt"
	"strings"

	"github.com/coder/acp-go-sdk"
)

func (a *Agent) prepareMCPServers(_ context.Context, _ acp.SessionId, servers []acp.McpServer) ([]acp.McpServer, error) {
	seen := make(map[string]struct{}, len(servers))

	for index, server := range servers {
		switch {
		case server.Stdio != nil, server.Http != nil:
			name := mcpServerName(server)
			if strings.TrimSpace(name) == "" {
				return nil, acp.NewInvalidParams(map[string]any{fmt.Sprintf("mcpServers[%d].name", index): validationRequired})
			}

			if _, duplicate := seen[name]; duplicate {
				return nil, acp.NewInvalidParams(map[string]any{fmt.Sprintf("mcpServers[%d].name", index): validationDuplicate})
			}

			seen[name] = struct{}{}

			if server.Stdio != nil {
				for envIndex, variable := range server.Stdio.Env {
					if reservedCodexEnvKey(variable.Name) {
						return nil, unsupportedField(fmt.Sprintf("mcpServers[%d].env[%d].name", index, envIndex))
					}
				}
			}
		case server.Sse != nil:
			return nil, acp.NewInvalidParams(map[string]any{
				jsonFieldError:  errValueUnsupported,
				jsonFieldField:  fmt.Sprintf("mcpServers[%d]", index),
				jsonFieldServer: server.Sse.Name,
			})
		case server.Acp != nil:
			return nil, acp.NewInvalidParams(map[string]any{
				jsonFieldError:  errValueUnsupported,
				jsonFieldField:  fmt.Sprintf("mcpServers[%d]", index),
				jsonFieldServer: server.Acp.Name,
			})
		default:
			return nil, acp.NewInvalidParams(map[string]any{
				jsonFieldError: "no_transport",
				jsonFieldField: fmt.Sprintf("mcpServers[%d]", index),
			})
		}
	}

	return cloneMCPServers(servers), nil
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
