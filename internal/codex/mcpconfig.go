package codex

import (
	"fmt"

	"github.com/coder/acp-go-sdk"
)

const (
	mcpApprovalModeAuto    = "auto"
	mcpApprovalModePrompt  = "prompt"
	mcpApprovalModeApprove = "approve"
	mcpServersKey          = "mcpServers"
	mcpServersConfigKey    = "mcp_servers"
)

// MCPServerThreadConfig renders ACP MCP servers into Codex's thread-scoped
// config.mcp_servers object. Credentials travel only inside the app-server
// JSON-RPC stream; they are never placed in process arguments or environment.
func MCPServerThreadConfig(servers []acp.McpServer, defaultApprovalMode string) (map[string]any, error) {
	if defaultApprovalMode != "" && !ValidMCPApprovalMode(defaultApprovalMode) {
		return nil, acp.NewInvalidParams(map[string]any{mcpServersKey: "_meta.codex.options.mcpToolApprovalMode must be one of auto, prompt, approve"})
	}

	if len(servers) == 0 {
		return map[string]any{}, nil
	}

	native := make(map[string]any, len(servers))
	for index, server := range servers {
		name := mcpServerName(server)
		config := map[string]any{}

		switch {
		case server.Stdio != nil:
			config["command"] = server.Stdio.Command
			if len(server.Stdio.Args) > 0 {
				config["args"] = append([]string(nil), server.Stdio.Args...)
			}

			if len(server.Stdio.Env) > 0 {
				env := make(map[string]string, len(server.Stdio.Env))
				for _, variable := range server.Stdio.Env {
					env[variable.Name] = variable.Value
				}

				config["env"] = env
			}
		case server.Http != nil:
			config["url"] = server.Http.Url
			if len(server.Http.Headers) > 0 {
				headers := make(map[string]string, len(server.Http.Headers))
				for _, header := range server.Http.Headers {
					if header.Name != "" {
						headers[header.Name] = header.Value
					}
				}

				if len(headers) > 0 {
					config["http_headers"] = headers
				}
			}
		case server.Acp != nil:
			return nil, acp.NewInvalidParams(map[string]any{mcpServersKey: "ACP MCP transport is not supported"})
		case server.Sse != nil:
			return nil, acp.NewInvalidParams(map[string]any{mcpServersKey: "SSE MCP is not supported by Codex"})
		default:
			return nil, acp.NewInvalidParams(map[string]any{mcpServersKey: fmt.Sprintf("server %d has no transport", index)})
		}

		if defaultApprovalMode != "" {
			config["default_tools_approval_mode"] = defaultApprovalMode
		}

		native[name] = config
	}

	return map[string]any{mcpServersConfigKey: native}, nil
}

func mcpServerName(server acp.McpServer) string {
	switch {
	case server.Http != nil:
		return server.Http.Name
	case server.Sse != nil:
		return server.Sse.Name
	case server.Acp != nil:
		return server.Acp.Name
	case server.Stdio != nil:
		return server.Stdio.Name
	default:
		return ""
	}
}

func ValidMCPApprovalMode(mode string) bool {
	switch mode {
	case mcpApprovalModeAuto, mcpApprovalModePrompt, mcpApprovalModeApprove:
		return true
	default:
		return false
	}
}
