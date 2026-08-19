package codexacp

import (
	"context"
	"encoding/json"
	"strconv"

	"github.com/coder/acp-go-sdk"
	"github.com/savid/acp-go-codex/internal/lifecycle"
)

const (
	metaOptionsKey             = "options"
	metaModelKey               = "model"
	metaEffortKey              = "effort"
	metaServiceTierKey         = "serviceTier"
	metaPersonalityKey         = "personality"
	metaEnvKey                 = "env"
	metaExtraPathDirsKey       = "extraPathDirs"
	metaApprovalPolicyKey      = "approvalPolicy"
	metaSandboxPolicyKey       = "sandboxPolicy"
	metaOutputSchemaKey        = "outputSchema"
	metaMCPToolApprovalModeKey = "mcpToolApprovalMode"
)

// CodexOptions is the stable Codex-specific subset accepted at
// _meta.codex.options.
type CodexOptions struct {
	// Model selects the initial Codex model for this session.
	Model string `json:"model,omitempty"`
	// OutputSchema configures JSON Schema structured output for this session.
	OutputSchema map[string]any `json:"outputSchema,omitempty"`
	// Effort selects the Codex reasoning effort for turns in this session.
	Effort string `json:"effort,omitempty"`
	// ServiceTier selects the Codex service tier for turns in this session.
	ServiceTier string `json:"serviceTier,omitempty"`
	// Personality selects the Codex personality setting for turns in this session.
	Personality string `json:"personality,omitempty"`
	// Env is the native shell environment of this session's Codex thread. It is
	// thread scoped: peer sessions on the same app-server never observe it. A
	// raw PATH entry is rejected; ExtraPathDirs is the only session PATH
	// authority.
	Env map[string]string `json:"env,omitempty"`
	// ExtraPathDirs are ordered absolute directories placed, in this order,
	// ahead of the app-server's native PATH for this session's Codex thread.
	ExtraPathDirs []string `json:"extraPathDirs,omitempty"`
	// ApprovalPolicy configures Codex approval behavior for this session.
	ApprovalPolicy any `json:"approvalPolicy,omitempty"`
	// SandboxPolicy configures Codex sandbox behavior for turns in this session.
	SandboxPolicy any `json:"sandboxPolicy,omitempty"`
	// MCPToolApprovalMode declares the host trust mode for MCP tool calls.
	MCPToolApprovalMode string `json:"mcpToolApprovalMode,omitempty"`
}

// Meta returns an ACP _meta object for the supported Codex-specific options.
func (options CodexOptions) Meta() map[string]any {
	values := map[string]any{}
	if options.Model != "" {
		values[metaModelKey] = options.Model
	}

	if options.OutputSchema != nil {
		values[metaOutputSchemaKey] = cloneAnyMap(options.OutputSchema)
	}

	if options.Effort != "" {
		values[string(configEffort)] = options.Effort
	}

	if options.ServiceTier != "" {
		values["serviceTier"] = options.ServiceTier
	}

	if options.Personality != "" {
		values[string(configPersonality)] = options.Personality
	}

	if len(options.Env) > 0 {
		values[metaEnvKey] = cloneStringMap(options.Env)
	}

	if len(options.ExtraPathDirs) > 0 {
		values[metaExtraPathDirsKey] = cloneStrings(options.ExtraPathDirs)
	}

	if options.ApprovalPolicy != nil {
		values[metaApprovalPolicyKey] = cloneAny(options.ApprovalPolicy)
	}

	if options.SandboxPolicy != nil {
		values[metaSandboxPolicyKey] = cloneAny(options.SandboxPolicy)
	}

	if options.MCPToolApprovalMode != "" {
		values[metaMCPToolApprovalModeKey] = options.MCPToolApprovalMode
	}

	return map[string]any{
		codexMetaKey: map[string]any{
			metaOptionsKey: values,
		},
	}
}

// SessionRequestOption configures embedded-Go ACP session lifecycle requests.
type SessionRequestOption func(*sessionRequestConfig)

type sessionRequestConfig struct {
	additionalDirectories []string
	mcpServers            []acp.McpServer
	meta                  map[string]any
}

// NewSessionRequest constructs a session/new request with ACP-required empty
// slices initialized for embedded Go callers.
func NewSessionRequest(cwd string, opts ...SessionRequestOption) acp.NewSessionRequest {
	config := newSessionRequestConfig(opts...)

	return acp.NewSessionRequest{
		Cwd:                   cwd,
		McpServers:            config.stableMCPServers(),
		AdditionalDirectories: config.additionalDirectoriesClone(),
		Meta:                  cloneAnyMap(config.meta),
	}
}

// LoadSessionRequest constructs a session/load request with ACP-required empty
// slices initialized for embedded Go callers.
func LoadSessionRequest(sessionID acp.SessionId, cwd string, opts ...SessionRequestOption) acp.LoadSessionRequest {
	config := newSessionRequestConfig(opts...)

	return acp.LoadSessionRequest{
		SessionId:             sessionID,
		Cwd:                   cwd,
		McpServers:            config.stableMCPServers(),
		AdditionalDirectories: config.additionalDirectoriesClone(),
		Meta:                  cloneAnyMap(config.meta),
	}
}

// ResumeSessionRequest constructs a session/resume request.
func ResumeSessionRequest(sessionID acp.SessionId, cwd string, opts ...SessionRequestOption) acp.ResumeSessionRequest {
	config := newSessionRequestConfig(opts...)

	return acp.ResumeSessionRequest{
		SessionId:             sessionID,
		Cwd:                   cwd,
		McpServers:            config.stableMCPServers(),
		AdditionalDirectories: config.additionalDirectoriesClone(),
		Meta:                  cloneAnyMap(config.meta),
	}
}

// ForkSessionRequest constructs an unstable session/fork request.
func ForkSessionRequest(sessionID acp.SessionId, cwd string, opts ...SessionRequestOption) acp.UnstableForkSessionRequest {
	config := newSessionRequestConfig(opts...)

	return acp.UnstableForkSessionRequest{
		SessionId:             sessionID,
		Cwd:                   cwd,
		McpServers:            unstableMCPServersFromStable(config.stableMCPServers()),
		AdditionalDirectories: config.additionalDirectoriesClone(),
		Meta:                  cloneAnyMap(config.meta),
	}
}

// WithSessionMCPServers sets MCP servers for a session lifecycle request.
func WithSessionMCPServers(servers ...acp.McpServer) SessionRequestOption {
	cloned := cloneMCPServers(servers)

	return func(config *sessionRequestConfig) {
		config.mcpServers = cloneMCPServers(cloned)
	}
}

// WithSessionAdditionalDirectories sets additional workspace directories for a
// session lifecycle request.
func WithSessionAdditionalDirectories(paths ...string) SessionRequestOption {
	cloned := append([]string(nil), paths...)

	return func(config *sessionRequestConfig) {
		config.additionalDirectories = append([]string(nil), cloned...)
	}
}

// WithSessionMeta merges metadata into a session lifecycle request. Caller
// metadata is the host's own namespace and never the family's: a map naming a
// reserved literal is refused rather than merged.
func WithSessionMeta(meta map[string]any) SessionRequestOption {
	rejectReservedMeta("WithSessionMeta", meta)

	cloned := cloneAnyMap(meta)

	return func(config *sessionRequestConfig) {
		config.meta = mergeAnyMap(config.meta, cloned)
	}
}

// reservedMetaLiterals is the closed family-global set. Adding a fifth is a
// contract amendment, so the set is written out rather than matched by prefix: a
// prefix test would also reject a literal this family has not defined and report
// it as though the contract already did.
var reservedMetaLiterals = []string{
	routeMetaKey,
	mediaEnvelopeMetaKey,
	handoffMetaKey,
	lifecycle.MetaKey,
}

// rejectReservedMeta refuses a caller `_meta` map that names a family literal.
// The builders return values rather than errors, so the refusal is a panic: the
// only alternatives inside the signature the family fixes are merging the key or
// dropping it, and both would let a host's guess about a reserved envelope stand
// in for the one this adapter stamps.
func rejectReservedMeta(builder string, meta map[string]any) {
	for _, literal := range reservedMetaLiterals {
		if _, present := meta[literal]; present {
			panic(builder + ": caller metadata used the family-reserved key " + strconv.Quote(literal))
		}
	}
}

// WithSessionCodexOptions merges Codex-specific options into a session
// lifecycle request's _meta.codex.options object.
func WithSessionCodexOptions(options CodexOptions) SessionRequestOption {
	cloned := cloneCodexOptions(options)

	return func(config *sessionRequestConfig) {
		config.meta = mergeAnyMap(config.meta, cloned.Meta())
	}
}

// WithSessionRawEvents toggles raw Codex event emission for a session lifecycle request.
func WithSessionRawEvents(enabled bool) SessionRequestOption {
	return func(config *sessionRequestConfig) {
		if config.meta == nil {
			config.meta = map[string]any{}
		}

		codexMeta := ensureMetaMap(config.meta, codexMetaKey)
		codexMeta[rawEventKey] = map[string]any{rawEventEnabledKey: enabled}
		config.meta[codexMetaKey] = codexMeta
	}
}

// StdioMCPServer constructs an ACP stdio MCP server.
func StdioMCPServer(name string, command string, args []string, env map[string]string) acp.McpServer {
	variables := make([]acp.EnvVariable, 0, len(env))
	for key, value := range env {
		variables = append(variables, acp.EnvVariable{Name: key, Value: value})
	}

	return acp.McpServer{Stdio: &acp.McpServerStdio{
		Name:    name,
		Command: command,
		Args:    append([]string(nil), args...),
		Env:     variables,
	}}
}

// HTTPMCPServer constructs an ACP HTTP MCP server.
func HTTPMCPServer(name string, url string, headers map[string]string) acp.McpServer {
	values := make([]acp.HttpHeader, 0, len(headers))
	for key, value := range headers {
		values = append(values, acp.HttpHeader{Name: key, Value: value})
	}

	return acp.McpServer{Http: &acp.McpServerHttpInline{
		Name:    name,
		Url:     url,
		Headers: values,
	}}
}

// WithSessionOutputSchema configures Codex JSON Schema structured output.
func WithSessionOutputSchema(schema map[string]any) SessionRequestOption {
	cloned := cloneAnyMap(schema)

	return func(config *sessionRequestConfig) {
		config.meta = mergeAnyMap(config.meta, CodexOptions{OutputSchema: cloned}.Meta())
	}
}

func newSessionRequestConfig(opts ...SessionRequestOption) sessionRequestConfig {
	config := sessionRequestConfig{}
	for _, opt := range opts {
		opt(&config)
	}

	return config
}

func (config sessionRequestConfig) stableMCPServers() []acp.McpServer {
	if config.mcpServers == nil {
		return []acp.McpServer{}
	}

	return cloneMCPServers(config.mcpServers)
}

func (config sessionRequestConfig) additionalDirectoriesClone() []string {
	return append([]string(nil), config.additionalDirectories...)
}

// PromptRequest constructs a session/prompt request with a non-nil prompt
// slice for embedded Go callers.
func PromptRequest(sessionID acp.SessionId, turnNonce string, blocks ...acp.ContentBlock) acp.PromptRequest {
	return acp.PromptRequest{
		SessionId: sessionID,
		Prompt:    append([]acp.ContentBlock{}, blocks...),
		Meta:      requestInboundRouteMeta(turnNonce),
	}
}

// TextPromptRequest constructs a session/prompt request containing one text
// content block.
func TextPromptRequest(sessionID acp.SessionId, turnNonce, text string) acp.PromptRequest {
	return PromptRequest(sessionID, turnNonce, acp.TextBlock(text))
}

// CancelRequest constructs a session/cancel notification bound to the exact
// active turn nonce. An unstamped or stale cancellation is rejected.
func CancelRequest(sessionID acp.SessionId, turnNonce string) acp.CancelNotification {
	return acp.CancelNotification{SessionId: sessionID, Meta: requestInboundRouteMeta(turnNonce)}
}

// SetConfigOptionRequest constructs a session/set_config_option value-id request.
func SetConfigOptionRequest(sessionID acp.SessionId, configID acp.SessionConfigId, value acp.SessionConfigValueId) acp.SetSessionConfigOptionRequest {
	return acp.SetSessionConfigOptionRequest{
		ValueId: &acp.SetSessionConfigOptionValueId{
			SessionId: sessionID,
			ConfigId:  configID,
			Value:     value,
		},
	}
}

// SetModelRequest constructs a session/set_config_option request for the model selector.
func SetModelRequest(sessionID acp.SessionId, model string) acp.SetSessionConfigOptionRequest {
	return SetConfigOptionRequest(sessionID, configModel, acp.SessionConfigValueId(model))
}

// DeleteSessionRequest constructs a session/delete request.
func DeleteSessionRequest(sessionID acp.SessionId) acp.UnstableDeleteSessionRequest {
	return acp.UnstableDeleteSessionRequest{SessionId: sessionID}
}

// CallForkSession calls the Codex fork extension method and decodes the SDK payload shape.
func CallForkSession(ctx context.Context, conn *acp.ClientSideConnection, params acp.UnstableForkSessionRequest) (acp.UnstableForkSessionResponse, error) {
	raw, err := conn.CallExtension(ctx, ForkSessionMethod, params)
	if err != nil {
		return acp.UnstableForkSessionResponse{}, err
	}

	var resp acp.UnstableForkSessionResponse
	if err := json.Unmarshal(raw, &resp); err != nil {
		return acp.UnstableForkSessionResponse{}, err
	}

	return resp, nil
}

// ListSessionsRequestOption configures embedded-Go session/list requests.
type ListSessionsRequestOption func(*acp.ListSessionsRequest)

// ListSessionsRequest constructs a session/list request.
func ListSessionsRequest(opts ...ListSessionsRequestOption) acp.ListSessionsRequest {
	var req acp.ListSessionsRequest
	for _, opt := range opts {
		opt(&req)
	}

	return req
}

// WithListSessionsCwd filters session/list by cwd.
func WithListSessionsCwd(cwd string) ListSessionsRequestOption {
	return func(req *acp.ListSessionsRequest) {
		value := cwd
		req.Cwd = &value
	}
}

// WithListSessionsCursor sets the cursor for session/list pagination.
func WithListSessionsCursor(cursor string) ListSessionsRequestOption {
	return func(req *acp.ListSessionsRequest) {
		value := cursor
		req.Cursor = &value
	}
}

// WithListSessionsMeta sets metadata on a session/list request. A caller map
// naming a family-reserved literal is refused on the same terms as
// WithSessionMeta.
func WithListSessionsMeta(meta map[string]any) ListSessionsRequestOption {
	rejectReservedMeta("WithListSessionsMeta", meta)

	cloned := cloneAnyMap(meta)

	return func(req *acp.ListSessionsRequest) {
		req.Meta = mergeAnyMap(req.Meta, cloned)
	}
}

// CodexOption configures CodexOptions values.
type CodexOption func(*CodexOptions)

// NewCodexOptions constructs CodexOptions from functional options.
func NewCodexOptions(opts ...CodexOption) CodexOptions {
	options := CodexOptions{}
	for _, opt := range opts {
		opt(&options)
	}

	return cloneCodexOptions(options)
}

// WithCodexModel configures the initial Codex model.
func WithCodexModel(model string) CodexOption {
	return func(options *CodexOptions) {
		options.Model = model
	}
}

// WithCodexEffort configures Codex reasoning effort.
func WithCodexEffort(effort string) CodexOption {
	return func(options *CodexOptions) {
		options.Effort = effort
	}
}

// WithCodexServiceTier configures the Codex service tier.
func WithCodexServiceTier(tier string) CodexOption {
	return func(options *CodexOptions) {
		options.ServiceTier = tier
	}
}

// WithCodexPersonality configures the Codex personality setting.
func WithCodexPersonality(personality string) CodexOption {
	return func(options *CodexOptions) {
		options.Personality = personality
	}
}

// WithCodexEnv sets the native shell environment of this session's Codex
// thread. Values are applied to that thread only, so concurrent sessions on one
// app-server never share or overwrite each other's values. A raw PATH entry
// fails closed; use WithCodexExtraPathDirs.
func WithCodexEnv(env map[string]string) CodexOption {
	cloned := cloneStringMap(env)

	return func(options *CodexOptions) {
		options.Env = cloneStringMap(cloned)
	}
}

// WithCodexExtraPathDirs places ordered absolute directories ahead of the
// app-server's native PATH on this session's Codex thread. Order is
// significant and preserved; the first directory wins native lookup.
func WithCodexExtraPathDirs(paths ...string) CodexOption {
	cloned := cloneStrings(paths)

	return func(options *CodexOptions) {
		options.ExtraPathDirs = cloneStrings(cloned)
	}
}

// WithCodexApprovalPolicy configures Codex approval behavior.
func WithCodexApprovalPolicy(policy any) CodexOption {
	cloned := cloneAny(policy)

	return func(options *CodexOptions) {
		options.ApprovalPolicy = cloneAny(cloned)
	}
}

// WithCodexSandboxPolicy configures Codex sandbox behavior.
func WithCodexSandboxPolicy(policy any) CodexOption {
	cloned := cloneAny(policy)

	return func(options *CodexOptions) {
		options.SandboxPolicy = cloneAny(cloned)
	}
}

// WithCodexOutputSchema configures Codex JSON Schema structured output.
func WithCodexOutputSchema(schema map[string]any) CodexOption {
	cloned := cloneAnyMap(schema)

	return func(options *CodexOptions) {
		options.OutputSchema = cloneAnyMap(cloned)
	}
}

// WithCodexMCPToolApprovalMode configures host-declared MCP tool trust for the session.
func WithCodexMCPToolApprovalMode(mode string) CodexOption {
	return func(options *CodexOptions) {
		options.MCPToolApprovalMode = mode
	}
}

func cloneCodexOptions(options CodexOptions) CodexOptions {
	return CodexOptions{
		Model:         options.Model,
		OutputSchema:  cloneAnyMap(options.OutputSchema),
		Effort:        options.Effort,
		ServiceTier:   options.ServiceTier,
		Personality:   options.Personality,
		Env:           cloneStringMap(options.Env),
		ExtraPathDirs: cloneStrings(options.ExtraPathDirs),
		ApprovalPolicy: cloneAny(
			options.ApprovalPolicy,
		),
		SandboxPolicy:       cloneAny(options.SandboxPolicy),
		MCPToolApprovalMode: options.MCPToolApprovalMode,
	}
}

func mergeAnyMap(base map[string]any, overlay map[string]any) map[string]any {
	result := cloneAnyMap(base)
	if result == nil {
		result = map[string]any{}
	}

	for key, value := range overlay {
		if valueMap, ok := value.(map[string]any); ok {
			if existingMap, ok := result[key].(map[string]any); ok {
				result[key] = mergeAnyMap(existingMap, valueMap)

				continue
			}
		}

		result[key] = cloneAny(value)
	}

	return result
}

func ensureMetaMap(meta map[string]any, key string) map[string]any {
	current, _ := meta[key].(map[string]any)
	if current == nil {
		current = map[string]any{}
	} else {
		current = cloneAnyMap(current)
	}

	meta[key] = current

	return current
}

func cloneMCPServers(servers []acp.McpServer) []acp.McpServer {
	if servers == nil {
		return nil
	}

	cloned := make([]acp.McpServer, len(servers))
	for index, server := range servers {
		cloned[index] = cloneMCPServer(server)
	}

	return cloned
}

func cloneMCPServer(server acp.McpServer) acp.McpServer {
	switch {
	case server.Http != nil:
		value := *server.Http
		value.Meta = cloneAnyMap(value.Meta)
		value.Headers = cloneHTTPHeaders(value.Headers)

		return acp.McpServer{Http: &value}
	case server.Sse != nil:
		value := *server.Sse
		value.Meta = cloneAnyMap(value.Meta)
		value.Headers = cloneHTTPHeaders(value.Headers)

		return acp.McpServer{Sse: &value}
	case server.Acp != nil:
		value := *server.Acp
		value.Meta = cloneAnyMap(value.Meta)

		return acp.McpServer{Acp: &value}
	case server.Stdio != nil:
		return acp.McpServer{Stdio: cloneMCPServerStdio(server.Stdio)}
	default:
		return acp.McpServer{}
	}
}

func cloneMCPServerStdio(server *acp.McpServerStdio) *acp.McpServerStdio {
	if server == nil {
		return nil
	}

	value := *server
	value.Meta = cloneAnyMap(value.Meta)
	value.Args = append([]string(nil), value.Args...)
	value.Env = cloneEnvVariables(value.Env)

	return &value
}

func cloneHTTPHeaders(headers []acp.HttpHeader) []acp.HttpHeader {
	if headers == nil {
		return nil
	}

	cloned := make([]acp.HttpHeader, len(headers))
	for index, header := range headers {
		cloned[index] = header
		cloned[index].Meta = cloneAnyMap(header.Meta)
	}

	return cloned
}

func cloneEnvVariables(env []acp.EnvVariable) []acp.EnvVariable {
	if env == nil {
		return nil
	}

	cloned := make([]acp.EnvVariable, len(env))
	for index, variable := range env {
		cloned[index] = variable
		cloned[index].Meta = cloneAnyMap(variable.Meta)
	}

	return cloned
}

func unstableMCPServersFromStable(servers []acp.McpServer) []acp.UnstableMcpServer {
	if servers == nil {
		return nil
	}

	cloned := make([]acp.UnstableMcpServer, len(servers))
	for index, server := range servers {
		cloned[index] = unstableMCPServerFromStable(server)
	}

	return cloned
}

func unstableMCPServerFromStable(server acp.McpServer) acp.UnstableMcpServer {
	switch {
	case server.Http != nil:
		value := acp.UnstableMcpServerHttp{
			Meta:    cloneAnyMap(server.Http.Meta),
			Headers: cloneHTTPHeaders(server.Http.Headers),
			Name:    server.Http.Name,
			Type:    server.Http.Type,
			Url:     server.Http.Url,
		}

		return acp.UnstableMcpServer{Http: &value}
	case server.Sse != nil:
		value := acp.UnstableMcpServerSse{
			Meta:    cloneAnyMap(server.Sse.Meta),
			Headers: cloneHTTPHeaders(server.Sse.Headers),
			Name:    server.Sse.Name,
			Type:    server.Sse.Type,
			Url:     server.Sse.Url,
		}

		return acp.UnstableMcpServer{Sse: &value}
	case server.Acp != nil:
		value := acp.UnstableMcpServerAcpInline{
			Meta: cloneAnyMap(server.Acp.Meta),
			Id:   acp.UnstableMcpServerAcpId(server.Acp.Id),
			Name: server.Acp.Name,
			Type: server.Acp.Type,
		}

		return acp.UnstableMcpServer{Acp: &value}
	case server.Stdio != nil:
		return acp.UnstableMcpServer{Stdio: cloneMCPServerStdio(server.Stdio)}
	default:
		return acp.UnstableMcpServer{}
	}
}
