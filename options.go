package codexacp

import (
	"context"
	"log/slog"
	"maps"
	"strings"
	"time"

	"github.com/savid/acp-go-codex/internal/codex"
	"go.opentelemetry.io/otel/metric"
	"go.opentelemetry.io/otel/propagation"
	"go.opentelemetry.io/otel/trace"
)

const managedHomeEnv = "HOME"

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

const minSupportedCodexVersion = "0.153.4"

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
	// Env is merged into launched Codex process environments. Managed config
	// and identity root variables are rejected.
	Env map[string]string
	// AmbientEnvironment replaces the adapter's own process environment as the
	// block ordinary execution inherits from. Its names are judged exactly as
	// inherited names are; WithEnv and session environments overlay it. Nil
	// inherits from the adapter's process. Managed execution never reads it.
	AmbientEnvironment map[string]string
	// HostAuthority delegates managed native execution and tree ownership to the
	// embedding host. Nil runs Codex as the adapter's current identity.
	HostAuthority HostAuthority

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
	// ImageLimits bounds decoded image bytes accepted from prompts and emitted
	// in session updates.
	ImageLimits ImageLimits
	// InputHandoffRoot is the read-only root for the local handoff prompt
	// transport. Empty rejects the handoff form. It must be an absolute path,
	// and nothing under it is ever created, modified, or removed.
	InputHandoffRoot string
	// ProviderAuthRoot is the durable host-owned directory housing the
	// values-free provider-auth ledger. Empty leaves the provider-auth surface
	// unadvertised. It must be an absolute path outside session scratch, it
	// holds no credential material, and no session lifecycle ever sweeps it.
	ProviderAuthRoot string
	// ProviderAuthDirectHome is the exact canonical CODEX_HOME the host
	// consents to a provider-auth leg reading credentials from or clearing.
	// The gated legs are advertised only while it is set and equal to the
	// resolved Home; the gate authorizes that home and never a parent, a
	// child, or a symlink target of it.
	ProviderAuthDirectHome string
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
	TextMapPropagator     propagation.TextMapPropagator
	clientFactory         func(context.Context, codex.Options) (codex.Client, error)
	customClientFactory   bool
	hostAuthoritySupplied bool
	implicitEnvironment   map[string]string
}

func applyOptions(opts []Option) Options {
	options := Options{
		AgentName:               "acp-go-codex",
		AgentTitle:              "acp-go-codex",
		AgentVersion:            "0.1.0",
		SessionStoreLoadTimeout: 10 * time.Second,
		ImageLimits:             defaultImageLimits(),
		clientFactory: func(ctx context.Context, options codex.Options) (codex.Client, error) {
			return codex.NewAppServerClient(ctx, options)
		},
	}

	for _, opt := range opts {
		opt(&options)
	}

	options.implicitEnvironment = ambientEnvironmentSnapshot(options)

	return options
}

// ambientEnvironmentSnapshot folds the ambient block once, at construction.
// Codex derives its default CODEX_HOME from HOME, so the adapter's own process
// environment falls back to the account's home directory when it names none. A
// supplied block is taken as given.
func ambientEnvironmentSnapshot(options Options) map[string]string {
	environment := make(map[string]string)

	for _, entry := range ambientEnvironmentEntries(options) {
		key, value, ok := strings.Cut(entry, "=")
		if ok && key != "" {
			environment[key] = value
		}
	}

	if options.AmbientEnvironment == nil && environment[managedHomeEnv] == "" {
		if home, err := adapterHomeDir(); err == nil && home != "" {
			environment[managedHomeEnv] = home
		}
	}

	return environment
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

// WithHostAuthority routes native processes and tree ownership through authority.
func WithHostAuthority(authority HostAuthority) Option {
	return func(options *Options) {
		options.HostAuthority = authority
		options.hostAuthoritySupplied = true
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

// WithInputHandoffRoot enables the local handoff prompt transport and confines
// it to dir: a prompt image block with empty data, a `file://` URI under dir,
// and an `acp-go.dev/handoff` envelope declaring the file's sha256 and size is
// read from disk instead of from embedded base64. Every byte is digest-verified
// before the ordinary image gates run, and validated bytes are copied into the
// scratch directory, so the host's path never reaches Codex.
//
// dir is a read root: nothing under it is created, modified, or removed, and the
// host may delete a handoff file as soon as session/prompt returns. An absolute
// path is required. Unset, the handoff form is rejected and only embedded base64
// is accepted.
func WithInputHandoffRoot(dir string) Option {
	return func(options *Options) {
		options.InputHandoffRoot = dir
	}
}

// WithProviderAuthRoot supplies the durable host-owned directory that houses
// the values-free provider-auth ledger. The path must be absolute and on
// durable storage outside session scratch; a relative path fails the agent
// closed. The directory is created 0700 when missing and ledger entries are
// written 0600.
//
// The ledger records which native credential slot each connection generation
// owns and nothing else: no credential material, no authorization URLs, no user
// codes, and no prompt answers. Unset — or set to a path that cannot be
// prepared — every _codex/auth leg is absent from the initialize advertisement
// and returns method-not-found.
func WithProviderAuthRoot(path string) Option {
	return func(options *Options) {
		options.ProviderAuthRoot = path
	}
}

// WithProviderAuthDirectHome names the exact CODEX_HOME the host consents to
// the account-level provider-auth legs touching. The credential leg reads that
// home's configured credential store and the disconnect leg clears the account
// in it, so both are advertised and answered only while this equals the
// resolved Home after path cleaning; otherwise both are absent from the
// advertisement and return method-not-found.
//
// The gate authorizes exactly the named home — never a parent, a child, or a
// symlink target — and is independent of WithCodexAllowAccountLogout, which
// governs the ACP logout method instead. A relative path fails the agent closed.
func WithProviderAuthDirectHome(path string) Option {
	return func(options *Options) {
		options.ProviderAuthDirectHome = path
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

// WithEnv merges environment variables into launched Codex sessions. Native
// root variables and adapter-private keys are rejected before launch.
func WithEnv(env map[string]string) Option {
	return func(options *Options) {
		options.Env = make(map[string]string, len(env))
		maps.Copy(options.Env, env)
	}
}

// WithAmbientEnvironment supplies the block ordinary execution inherits from in
// place of the adapter's own process environment. Entries are filtered like
// inherited entries; an entry that could not be an environment entry fails
// Agent construction. Managed execution reads nothing from it.
func WithAmbientEnvironment(env map[string]string) Option {
	return func(options *Options) {
		options.AmbientEnvironment = cloneStringMap(env)
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

// WithSeedFiles writes relative-path files into the resolved CODEX_HOME before
// each Codex process launches, so Codex reads them as its own config (e.g.
// config.toml). Paths are confined to CODEX_HOME: absolute paths, ".." escapes,
// and empty keys fail closed at session start. Secrets belong in WithEnv and are
// referenced from seeded files by env-var indirection (Codex env_key).
func WithSeedFiles(files map[string]string) Option {
	return func(options *Options) {
		options.SeedFiles = make(map[string]string, len(files))
		maps.Copy(options.SeedFiles, files)
	}
}

// WithCodexConfigOverrides sets TOML config overrides passed to `codex
// app-server` as `-c key=value`. Keys may be dotted paths for nested values
// (e.g. model_providers.litellm.base_url). The session-scoped mcp_servers
// keyspace and the thread-owned shell_environment_policy keyspace are reserved
// whole, and either one causes initialization or an embedded lifecycle call to
// fail closed. String values are TOML-quoted automatically. Nothing is written
// to disk, so it is non-destructive and safe against a real ~/.codex. The input
// map is cloned.
func WithCodexConfigOverrides(overrides map[string]any) Option {
	return func(options *Options) {
		options.Config = make(map[string]any, len(overrides))
		maps.Copy(options.Config, overrides)
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
// Enabling it requires an explicit WithHome consent boundary.
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
