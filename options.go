package codexacp

import (
	"context"
	"log/slog"
	"time"

	"github.com/savid/acp-go-codex/internal/codex"
	"go.opentelemetry.io/otel/metric"
	"go.opentelemetry.io/otel/propagation"
	"go.opentelemetry.io/otel/trace"
)

// Option configures the Codex ACP agent.
type Option func(*Options)

// Options configures the ACP agent process and Codex sessions it starts.
type Options struct {
	// AgentName is the protocol identifier advertised during ACP initialize.
	AgentName string
	// AgentTitle is the human-readable agent name advertised during ACP initialize.
	AgentTitle string
	// AgentVersion is the agent version advertised during ACP initialize.
	AgentVersion string

	// CodexPath is the Codex CLI executable path. If empty, PATH will be used.
	CodexPath string
	// CodexHome sets CODEX_HOME for launched Codex CLI sessions.
	CodexHome string
	// DefaultModel is the model preference for newly created Codex threads.
	DefaultModel string

	// Logger receives structured diagnostic logs. If nil, the default logger is used.
	Logger *slog.Logger
	// Env is merged into launched Codex process environments.
	Env map[string]string
	// MCPProxyCommand is used to expose ACP-transport MCP servers to Codex as
	// stdio MCP servers.
	MCPProxyCommand string
	// MCPProxyArgs are inserted after MCPProxyCommand and before adapter-owned
	// proxy arguments.
	MCPProxyArgs []string
	// SessionStore mirrors/imports Codex rollout JSONL rows for durable remote
	// session restore. If nil, imports are stored in-process.
	SessionStore SessionStore
	// SessionStoreLoadTimeout bounds store-backed load/list operations. Values
	// <= 0 use the default.
	SessionStoreLoadTimeout time.Duration
	// ChatGPTAuthTokenRefresher handles Codex external-auth refresh callbacks
	// from the app-server.
	ChatGPTAuthTokenRefresher func(context.Context) (codex.ChatGPTAuthTokens, error)
	// AllowAccountLogout permits ACP logout to call Codex account/logout. Leave
	// false when CODEX_HOME points at a user's normal local Codex credentials.
	AllowAccountLogout bool
	// TracerProvider receives OpenTelemetry spans. If nil, tracing is no-op.
	TracerProvider trace.TracerProvider
	// MeterProvider receives OpenTelemetry metrics. If nil, metrics are no-op.
	MeterProvider metric.MeterProvider
	// TextMapPropagator extracts ACP trace metadata and injects Codex process env.
	TextMapPropagator propagation.TextMapPropagator

	clientFactory func(context.Context, codex.Options) (codex.Client, error)
}

func applyOptions(opts []Option) Options {
	options := Options{
		AgentName:               "acp-go-codex",
		AgentTitle:              "acp-go-codex",
		AgentVersion:            "0.1.0",
		SessionStoreLoadTimeout: 10 * time.Second,
		clientFactory: func(ctx context.Context, options codex.Options) (codex.Client, error) {
			return codex.NewAppServerClient(ctx, options)
		},
	}

	for _, opt := range opts {
		opt(&options)
	}

	return options
}

func withClientFactory(factory func(context.Context, codex.Options) (codex.Client, error)) Option {
	return func(options *Options) {
		options.clientFactory = factory
	}
}

// WithAgentName sets the protocol identifier advertised during ACP initialize.
func WithAgentName(name string) Option {
	return func(options *Options) {
		options.AgentName = name
	}
}

// WithAgentTitle sets the human-readable agent name advertised during ACP initialize.
func WithAgentTitle(title string) Option {
	return func(options *Options) {
		options.AgentTitle = title
	}
}

// WithAgentVersion sets the agent version advertised during ACP initialize.
func WithAgentVersion(version string) Option {
	return func(options *Options) {
		options.AgentVersion = version
	}
}

// WithCodexPath sets the Codex CLI executable path.
func WithCodexPath(path string) Option {
	return func(options *Options) {
		options.CodexPath = path
	}
}

// WithCodexHome sets CODEX_HOME for launched Codex CLI sessions.
func WithCodexHome(path string) Option {
	return func(options *Options) {
		options.CodexHome = path
	}
}

// WithDefaultModel selects a Codex model for newly created sessions.
func WithDefaultModel(model string) Option {
	return func(options *Options) {
		options.DefaultModel = model
	}
}

// WithLogger configures structured diagnostic logging.
func WithLogger(logger *slog.Logger) Option {
	return func(options *Options) {
		options.Logger = logger
	}
}

// WithEnv merges environment variables into launched Codex sessions.
func WithEnv(env map[string]string) Option {
	return func(options *Options) {
		options.Env = make(map[string]string, len(env))
		for key, value := range env {
			options.Env[key] = value
		}
	}
}

// WithMCPProxyCommand configures the command used for ACP-transport MCP
// servers. Normal stdio and HTTP MCP servers do not use this proxy.
func WithMCPProxyCommand(command string, args ...string) Option {
	copied := append([]string(nil), args...)

	return func(options *Options) {
		options.MCPProxyCommand = command
		options.MCPProxyArgs = append([]string(nil), copied...)
	}
}

// WithSessionStore configures durable storage for Codex rollout JSONL rows.
func WithSessionStore(store SessionStore) Option {
	return func(options *Options) {
		options.SessionStore = store
	}
}

// WithSessionStoreLoadTimeout configures the timeout for store-backed load/list
// operations.
func WithSessionStoreLoadTimeout(timeout time.Duration) Option {
	return func(options *Options) {
		options.SessionStoreLoadTimeout = timeout
	}
}

// WithChatGPTAuthTokenRefresher configures external ChatGPT token refresh for
// Codex app-server auth callbacks.
func WithChatGPTAuthTokenRefresher(refresher func(context.Context) (codex.ChatGPTAuthTokens, error)) Option {
	return func(options *Options) {
		options.ChatGPTAuthTokenRefresher = refresher
	}
}

// WithAllowAccountLogout permits ACP logout to call Codex account/logout.
func WithAllowAccountLogout(enabled bool) Option {
	return func(options *Options) {
		options.AllowAccountLogout = enabled
	}
}

// WithTracerProvider configures OpenTelemetry tracing.
func WithTracerProvider(provider trace.TracerProvider) Option {
	return func(options *Options) {
		options.TracerProvider = provider
	}
}

// WithMeterProvider configures OpenTelemetry metrics.
func WithMeterProvider(provider metric.MeterProvider) Option {
	return func(options *Options) {
		options.MeterProvider = provider
	}
}

// WithTextMapPropagator configures OpenTelemetry trace propagation.
func WithTextMapPropagator(propagator propagation.TextMapPropagator) Option {
	return func(options *Options) {
		options.TextMapPropagator = propagator
	}
}
