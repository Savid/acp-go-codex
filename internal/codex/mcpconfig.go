package codex

import (
	"fmt"
	"regexp"
	"strings"

	"github.com/coder/acp-go-sdk"
)

const (
	mcpHeaderEnvPrefix     = "CODEX_MCP_HEADER"
	mcpApprovalModeAuto    = "auto"
	mcpApprovalModePrompt  = "prompt"
	mcpApprovalModeApprove = "approve"
	mcpServersKey          = "mcpServers"
	mcpServersPrefix       = "mcp_servers"
)

var (
	mcpBareKeyRE = regexp.MustCompile(`^[A-Za-z0-9_-]+$`)
	mcpEnvPartRE = regexp.MustCompile(`[^A-Za-z0-9]+`)
)

// MCPServerConfigArgs renders the native Codex `-c mcp_servers.*` overrides (and
// any header env vars) for the given ACP MCP servers. SSE and ACP transports are
// rejected; callers validate transports up front, so those branches are
// defensive.
func MCPServerConfigArgs(servers []acp.McpServer, defaultApprovalMode string) ([]string, map[string]string, error) {
	if len(servers) == 0 {
		return nil, nil, nil
	}

	args := make([]string, 0, len(servers)*8)
	env := map[string]string{}

	for index, server := range servers {
		name := mcpServerName(server)
		switch {
		case server.Stdio != nil:
			args = append(args, ConfigArg(mcpServerConfigKey(name, "command"), server.Stdio.Command)...)
			if len(server.Stdio.Args) > 0 {
				args = append(args, ConfigArg(mcpServerConfigKey(name, "args"), TOMLLiteral(TOMLStringArray(server.Stdio.Args)))...)
			}

			if len(server.Stdio.Env) > 0 {
				args = append(args, ConfigArg(mcpServerConfigKey(name, "env"), TOMLLiteral(TOMLEnvTable(server.Stdio.Env)))...)
			}
		case server.Http != nil:
			args = append(args, ConfigArg(mcpServerConfigKey(name, "url"), server.Http.Url)...)
			if len(server.Http.Headers) > 0 {
				headerEnv := mcpHeaderEnvTable(name, server.Http.Headers, env)
				if headerEnv != "" {
					args = append(args, ConfigArg(mcpServerConfigKey(name, "env_http_headers"), TOMLLiteral(headerEnv))...)
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

// mcpServerConfigKey builds a Codex `-c` dotted TOML key for an MCP server,
// forwarding the ACP server name verbatim. The name is quoted as a TOML basic
// string when it is not a valid bare key so any accepted (non-empty, unique)
// name maps to a well-formed config path without being rewritten.
func mcpServerConfigKey(name string, suffix string) string {
	return mcpServersPrefix + "." + tomlKeySegment(name) + "." + suffix
}

func tomlKeySegment(segment string) string {
	if mcpBareKeyRE.MatchString(segment) {
		return segment
	}

	return TOMLString(segment)
}

func mcpApprovalConfigArgs(serverName string, defaultMode string) ([]string, error) {
	if defaultMode == "" {
		return nil, nil
	}

	if !ValidMCPApprovalMode(defaultMode) {
		return nil, acp.NewInvalidParams(map[string]any{mcpServersKey: "_meta.codex.options.mcpToolApprovalMode must be one of auto, prompt, approve"})
	}

	return ConfigArg(mcpServerConfigKey(serverName, "default_tools_approval_mode"), defaultMode), nil
}

func ValidMCPApprovalMode(mode string) bool {
	switch mode {
	case mcpApprovalModeAuto, mcpApprovalModePrompt, mcpApprovalModeApprove:
		return true
	default:
		return false
	}
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
		items = append(items, TOMLString(header.Name)+" = "+TOMLString(envName))
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
