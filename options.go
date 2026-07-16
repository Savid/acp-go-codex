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

// ChatGPTAuthTokens are externally supplied ChatGPT auth credentials for Codex.
type ChatGPTAuthTokens struct {
	AccessToken      string
	RefreshToken     string
	AccountID        string
	PlanType         string
	ExpiresAtUnixSec int64
}

// ConcurrencyLimits bounds work accepted by one Agent.
type ConcurrencyLimits struct {
	MaxActiveSessions        int
	MaxConcurrentClientCalls int
}

// RuntimeResourceKind identifies the reason an adapter-owned native process or
// scratch root is being acquired. Hosts use it to enforce independent global
// resource bounds without coupling the adapter to a particular registry.
type RuntimeResourceKind string

const (
	RuntimeResourceRuntime   RuntimeResourceKind = "runtime"
	RuntimeResourceSession   RuntimeResourceKind = "session"
	RuntimeResourcePrompt    RuntimeResourceKind = "prompt"
	RuntimeResourceDiscovery RuntimeResourceKind = "discovery"
)

type RuntimeProcessKind string

const (
	RuntimeProcessHomeLockSupervisor RuntimeProcessKind = "home_lock_supervisor"
	RuntimeProcessProviderDescendant RuntimeProcessKind = "provider_descendant"
)

type RuntimeStartupStage string

const (
	RuntimeStartupSpawn         RuntimeStartupStage = "spawn"
	RuntimeStartupReadiness     RuntimeStartupStage = "readiness"
	RuntimeStartupConfiguration RuntimeStartupStage = "configuration"
	RuntimeStartupSession       RuntimeStartupStage = "session"
)

// RuntimeResourceHooks let an embedding host account for native roots and
// adapter-created scratch roots at their exact lifetime boundaries. A nil
// callback selects standalone, sibling-owned unbounded accounting.
type RuntimeResourceHooks struct {
	AcquireNativeRoot      func(context.Context, RuntimeResourceKind) (release func(), err error)
	ReserveScratchRoot     func(context.Context, RuntimeResourceKind) (release func(), err error)
	ObserveProcess         func(context.Context, RuntimeProcessKind, int64)
	ObserveProcessSnapshot func(context.Context, RuntimeProcessKind, int)
	ObserveStartupStage    func(context.Context, RuntimeResourceKind, RuntimeStartupStage, time.Duration, error)
}

// Options configures the ACP agent process and Codex sessions it starts.
type Options struct {
	// AgentName is the protocol identifier advertised during ACP initialize.
	AgentName string
	// AgentTitle is the human-readable agent name advertised during ACP initialize.
	AgentTitle string
	// AgentVersion is the agent version advertised during ACP initialize.
	AgentVersion string

	// ExecutablePath is the Codex CLI executable path. If empty, PATH will be used.
	ExecutablePath string
	// Home sets CODEX_HOME for launched Codex CLI sessions.
	Home string
	// ScratchDir is the parent directory for all ephemeral on-disk
	// materialization (per-session roots, hydration temp files, probe dirs).
	// Empty means the system temp directory. The directory is created 0700
	// when missing.
	ScratchDir string
	// DefaultModel is the model preference for newly created Codex threads.
	DefaultModel string
	// Env is merged into launched Codex process environments.
	Env map[string]string

	// Logger receives structured diagnostic logs. If nil, the default logger is used.
	Logger *slog.Logger
	// SessionStore mirrors Codex rollout JSONL rows for durable remote session
	// restore. If nil, an in-process store is used.
	SessionStore SessionStore
	// SessionStoreLoadTimeout bounds store-backed load/list operations. Values
	// <= 0 use the default.
	SessionStoreLoadTimeout time.Duration
	// ConcurrencyLimits bounds active sessions, prompts, and server-to-client calls.
	ConcurrencyLimits ConcurrencyLimits
	// TurnTimeout bounds a single session/prompt turn. When a turn exceeds it,
	// the native Codex turn is aborted and session/prompt fails with the
	// codex_turn_failed error (cause "timeout"). The default of 0 disables the
	// deadline.
	TurnTimeout time.Duration
	// SeedFiles maps relative paths to file contents written into the resolved
	// CODEX_HOME before each Codex process launches, so Codex reads them as its
	// own config (e.g. config.toml). Paths are confined to CODEX_HOME.
	SeedFiles map[string]string
	// Config holds TOML config overrides passed to `codex app-server` as
	// `-c key=value`. Keys may be dotted paths for nested values; string values
	// are TOML-quoted automatically. Nothing is written to disk.
	Config map[string]any
	// ChatGPTAuthTokenRefresher handles Codex external-auth refresh callbacks
	// from the app-server.
	ChatGPTAuthTokenRefresher func(context.Context) (ChatGPTAuthTokens, error)
	// AllowAccountLogout permits ACP logout to call Codex account/logout. Leave
	// false when CODEX_HOME points at a user's normal local Codex credentials.
	AllowAccountLogout bool
	// TracerProvider receives OpenTelemetry spans. If nil, tracing is no-op.
	TracerProvider trace.TracerProvider
	// MeterProvider receives OpenTelemetry metrics. If nil, metrics are no-op.
	MeterProvider metric.MeterProvider
	// TextMapPropagator extracts ACP trace metadata and injects Codex process env.
	TextMapPropagator propagation.TextMapPropagator
	// RuntimeResourceHooks account for native roots and scratch roots at their
	// exact creation/deletion boundaries.
	RuntimeResourceHooks RuntimeResourceHooks

	clientFactory       func(context.Context, codex.Options) (codex.Client, error)
	customClientFactory bool
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
		options.customClientFactory = true
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

// WithExecutablePath sets the Codex CLI executable path.
func WithExecutablePath(path string) Option {
	return func(options *Options) {
		options.ExecutablePath = path
	}
}

// WithHome sets CODEX_HOME for launched Codex CLI sessions.
func WithHome(path string) Option {
	return func(options *Options) {
		options.Home = path
	}
}

// WithScratchDir sets the parent directory for all ephemeral on-disk
// materialization (per-session roots, hydration temp files, probe dirs).
// Empty means the system temp directory. The directory is created 0700
// when missing.
func WithScratchDir(dir string) Option {
	return func(options *Options) {
		options.ScratchDir = dir
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

// WithSessionStore replaces the default in-memory authority for Codex rollout
// JSONL rows and inactive session lifecycle operations.
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

// WithConcurrencyLimits configures agent concurrency limits. Zero fields use defaults.
func WithConcurrencyLimits(limits ConcurrencyLimits) Option {
	return func(options *Options) {
		options.ConcurrencyLimits = limits
	}
}

// WithTurnTimeout bounds a single session/prompt turn. On expiry the native
// Codex turn is aborted and session/prompt fails with the codex_turn_failed
// error (cause "timeout"), not a cancellation. The default of 0 disables the
// deadline.
func WithTurnTimeout(timeout time.Duration) Option {
	return func(options *Options) {
		options.TurnTimeout = timeout
	}
}

// WithRuntimeResourceHooks configures exact-lifetime native-root and scratch-
// root accounting for an embedding host. Rejection is propagated fail closed.
func WithRuntimeResourceHooks(hooks RuntimeResourceHooks) Option {
	return func(options *Options) {
		options.RuntimeResourceHooks = hooks
	}
}

// WithSeedFiles writes relative-path files into the resolved CODEX_HOME before
// each Codex process launches, so Codex reads them as its own config (e.g.
// config.toml). Paths are confined to CODEX_HOME: absolute paths, ".." escapes,
// and empty keys fail closed at session start. Secrets belong in WithEnv and are
// referenced from seeded files by env-var indirection (Codex env_key).
func WithSeedFiles(files map[string]string) Option {
	return func(options *Options) {
		options.SeedFiles = make(map[string]string, len(files))
		for path, contents := range files {
			options.SeedFiles[path] = contents
		}
	}
}

// WithCodexConfigOverrides sets TOML config overrides passed to `codex
// app-server` as `-c key=value`. Keys may be dotted paths for nested values
// (e.g. model_providers.litellm.base_url). The session-scoped mcp_servers
// keyspace is reserved and causes Initialize to fail closed. String values are
// TOML-quoted automatically. Nothing is written to disk, so it is
// non-destructive and safe against a real ~/.codex. The input map is cloned.
func WithCodexConfigOverrides(overrides map[string]any) Option {
	return func(options *Options) {
		options.Config = make(map[string]any, len(overrides))
		for key, value := range overrides {
			options.Config[key] = value
		}
	}
}

// WithCodexChatGPTAuthTokenRefresher configures external ChatGPT token refresh for
// Codex app-server auth callbacks.
func WithCodexChatGPTAuthTokenRefresher(refresher func(context.Context) (ChatGPTAuthTokens, error)) Option {
	return func(options *Options) {
		options.ChatGPTAuthTokenRefresher = refresher
	}
}

// WithCodexAllowAccountLogout permits ACP logout to call Codex account/logout.
func WithCodexAllowAccountLogout(enabled bool) Option {
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
