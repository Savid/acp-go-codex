package codex

import (
	"github.com/coder/acp-go-sdk"
)

const (
	mcpApprovalModeAuto    = "auto"
	mcpApprovalModePrompt  = "prompt"
	mcpApprovalModeApprove = "approve"
	mcpServersConfigKey    = "mcp_servers"
)

// MCPServerThreadConfig renders ACP MCP servers into Codex's thread-scoped
// config.mcp_servers object. Credentials travel only inside the app-server
// JSON-RPC stream; they are never placed in process arguments or environment.
//
// It renders and never validates. The ACP session-start validator owns every
// verdict over these inputs (accepted transport, non-empty unique name,
// approval mode) and is the single place a caller learns which field it got
// wrong; a second opinion here could only disagree with it.
func MCPServerThreadConfig(servers []acp.McpServer, defaultApprovalMode string) map[string]any {
	if len(servers) == 0 {
		return map[string]any{}
	}

	native := make(map[string]any, len(servers))

	for _, server := range servers {
		name := ""
		config := map[string]any{}

		switch {
		case server.Stdio != nil:
			name = server.Stdio.Name
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
			name = server.Http.Name
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
		}

		if defaultApprovalMode != "" {
			config["default_tools_approval_mode"] = defaultApprovalMode
		}

		native[name] = config
	}

	return map[string]any{mcpServersConfigKey: native}
}

func ValidMCPApprovalMode(mode string) bool {
	switch mode {
	case mcpApprovalModeAuto, mcpApprovalModePrompt, mcpApprovalModeApprove:
		return true
	default:
		return false
	}
}
