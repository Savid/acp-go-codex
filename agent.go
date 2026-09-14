package codexacp

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"

	"github.com/coder/acp-go-sdk"

	acpcore "github.com/savid/acp-go-core"
	"github.com/savid/acp-go-core/image"
	"github.com/savid/acp-go-core/lifecycle"
	"github.com/savid/acp-go-core/observer"
	"github.com/savid/acp-go-core/process"
	"github.com/savid/acp-go-core/wire"
)

const (
	// RawEventMethod is the notification carrying one raw app-server event
	// when a session opted in through _meta.codex.rawEvent.enabled.
	RawEventMethod = "_codex/rawEvent"
	// SessionStoreFormat identifies the store layout this package writes: raw
	// Codex rollout rows under the main subpath plus the adapter's session
	// record under the config subpath.
	SessionStoreFormat = "codex-rollout-jsonl-v1"

	vendor = "codex"

	capabilityMethodKey = "method"
)

// client is the host side of the connection, as the sessions use it.
type client interface {
	SessionUpdate(ctx context.Context, params acp.SessionNotification) error
	RequestPermission(ctx context.Context, params acp.RequestPermissionRequest) (acp.RequestPermissionResponse, error)
	UnstableCreateElicitation(ctx context.Context, params acp.UnstableCreateElicitationRequest) (acp.UnstableCreateElicitationResponse, error)
	NotifyExtension(ctx context.Context, method string, params any) error
}

// Agent exposes the Codex app-server through ACP. One app-server serves every
// session; each session owns one thread on it.
type Agent struct {
	options   Options
	log       *slog.Logger
	observe   *observer.Observer
	optionErr *acp.RequestError
	// processEnv is the adapter's own environment, read once at construction.
	processEnv []string
	store      acpcore.SessionStore

	mu                 sync.Mutex
	conn               client
	transport          *wire.Transport
	closed             bool
	clientCapabilities acp.ClientCapabilities
	positionEncoding   acp.PositionEncodingKind
	// lifecycle is the answer this connection gave at initialize. An absent
	// answer leaves the extension dormant for every session on it.
	lifecycle    lifecycle.Negotiated
	sessions     map[acp.SessionId]*session
	deleted      map[acp.SessionId]struct{}
	clientCalls  chan struct{}
	incarnations uint64

	// runtimeMu serializes starting and replacing the shared app-server.
	runtimeMu sync.Mutex
	runtime   *runtime

	versionOnce sync.Once
	versionErr  error
	executable  string
}

var (
	_ acp.Agent                  = (*Agent)(nil)
	_ acp.AgentLoader            = (*Agent)(nil)
	_ acp.ExtensionMethodHandler = (*Agent)(nil)
)

// NewAgent creates an ACP agent for the Codex CLI. Construction never fails; a
// refused option is reported by Initialize and every session-establishing
// method as codex_invalid_options.
func NewAgent(opts ...Option) *Agent {
	options := applyOptions(opts)

	log := options.Logger
	if log == nil {
		log = slog.Default()
	}

	store := options.SessionStore
	if store == nil {
		store = acpcore.NewInMemorySessionStore()
	}

	agent := &Agent{
		options: options,
		log:     log,
		observe: observer.New(observer.Config{
			Vendor: vendor, NativeClient: "codex-app-server",
			MeterProvider:  options.MeterProvider,
			Propagator:     options.TextMapPropagator,
			TracerProvider: options.TracerProvider,
			Version:        options.AgentVersion,
		}),
		processEnv:       os.Environ(),
		store:            store,
		sessions:         make(map[acp.SessionId]*session),
		deleted:          make(map[acp.SessionId]struct{}),
		clientCalls:      make(chan struct{}, max(0, options.ConcurrencyLimits.MaxConcurrentClientCalls)),
		positionEncoding: acp.PositionEncodingKindUtf16,
	}
	agent.optionErr = agent.validateOptions()

	return agent
}

// validateOptions reports the first refused option. The reason goes to the
// log; the wire answer names only the option.
func (a *Agent) validateOptions() *acp.RequestError {
	options := a.options

	checks := []struct {
		field string
		err   error
	}{
		{"home", validateOptionalAbsolute(options.Home)},
		{"inputHandoffRoot", validateHandoffRoot(options.InputHandoffRoot)},
		{"configuredModels", validateConfiguredModels(options.ConfiguredModels)},
		{metaEnvKey, process.ValidateNames(options.Env)},
		{"codexConfigOverrides", validateConfigOverrides(options.CodexConfigOverrides)},
		{"concurrencyLimits", validateConcurrencyLimits(options.ConcurrencyLimits)},
		{"imageLimits", options.ImageLimits.core().Validate()},
	}

	for _, check := range checks {
		if check.err == nil {
			continue
		}

		a.log.Error("codex agent option rejected", slog.String("field", check.field), slog.String("reason", check.err.Error()))

		return wire.InvalidOptions(vendor, check.field)
	}

	return nil
}

func validateOptionalAbsolute(path string) error {
	if path == "" || filepath.IsAbs(path) {
		return nil
	}

	return errors.New("path must be absolute")
}

func validateHandoffRoot(root string) error {
	if root == "" {
		return nil
	}

	return image.ValidateHandoffRoot(root)
}

func validateConfiguredModels(ids []string) error {
	seen := make(map[string]struct{}, len(ids))

	for index, id := range ids {
		if id == "" || id != strings.TrimSpace(id) {
			return fmt.Errorf("configured model %d %q is not a model id", index, id)
		}

		if _, duplicate := seen[id]; duplicate {
			return fmt.Errorf("configured model %q is listed twice", id)
		}

		seen[id] = struct{}{}
	}

	return nil
}

// validateConfigOverrides refuses the keyspace the adapter authors per thread
// and keys that cannot be spelled on the command line.
func validateConfigOverrides(overrides map[string]any) error {
	for key := range overrides {
		trimmed := strings.TrimSpace(key)
		if trimmed == "" || trimmed != key || strings.ContainsAny(key, "=\n") {
			return fmt.Errorf("config override %q is not a key", key)
		}

		if key == "shell_environment_policy" || strings.HasPrefix(key, "shell_environment_policy.") {
			return fmt.Errorf("config override %q is owned by the session environment", key)
		}
	}

	return nil
}

func validateConcurrencyLimits(limits ConcurrencyLimits) error {
	if limits.MaxActiveSessions < 0 || limits.MaxConcurrentClientCalls < 0 {
		return errors.New("concurrency limits must not be negative")
	}

	return nil
}

// Serve runs an ACP agent over the provided streams. It blocks until the
// context is cancelled or the peer closes the connection, then closes the
// agent.
func Serve(ctx context.Context, input io.Reader, output io.Writer, opts ...Option) (returnErr error) {
	if err := ctx.Err(); err != nil {
		return err
	}

	agent := NewAgent(opts...)
	defer func() {
		if closeErr := agent.Close(); closeErr != nil {
			returnErr = closeErr
		}
	}()

	transport := wire.NewTransport(input, output)
	conn := acp.NewAgentSideConnection(agent, transport.Writer(), transport.Reader())
	conn.SetLogger(agent.log)
	agent.attach(conn, transport)
	transport.Start()

	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-conn.Done():
		return nil
	}
}

// attach binds the host connection the sessions emit through.
func (a *Agent) attach(conn client, transport *wire.Transport) {
	a.mu.Lock()
	defer a.mu.Unlock()

	a.conn = conn
	a.transport = transport
}

func (a *Agent) connection() client {
	a.mu.Lock()
	defer a.mu.Unlock()

	return a.conn
}

// Close runs the shutdown ladder for every session, stops the shared
// app-server, and refuses every later request.
func (a *Agent) Close() error {
	a.mu.Lock()
	if a.closed {
		a.mu.Unlock()

		return nil
	}

	a.closed = true
	sessions := slices.Collect(func(yield func(*session) bool) {
		for _, s := range a.sessions {
			if !yield(s) {
				return
			}
		}
	})
	a.conn = nil
	a.mu.Unlock()

	var errs []error

	for _, s := range sessions {
		if err := s.close(context.Background()); err != nil {
			errs = append(errs, err)
		}
	}

	a.mu.Lock()
	clear(a.sessions)
	a.mu.Unlock()

	a.stopRuntime(context.Background())

	return errors.Join(errs...)
}

func (a *Agent) ensureOpen() error {
	a.mu.Lock()
	defer a.mu.Unlock()

	if a.closed {
		return errAgentClosed()
	}

	return nil
}

// errAgentClosed answers every request after Close.
func errAgentClosed() *acp.RequestError {
	return acp.NewInvalidRequest(map[string]any{wire.FieldError: "agent closed"})
}

// Initialize implements ACP initialize.
func (a *Agent) Initialize(ctx context.Context, params acp.InitializeRequest) (resp acp.InitializeResponse, err error) {
	_, finish := a.observe.StartACP(ctx, params.Meta, acp.AgentMethodInitialize)
	defer func() { finish(err) }()

	if a.optionErr != nil {
		return acp.InitializeResponse{}, a.optionErr
	}

	meta := params.Meta
	if t := a.transportRef(); t != nil {
		meta = lifecycle.RetainRequestMetadata(meta, t.TakeRaw(acp.AgentMethodInitialize))
	}

	offer, present, paramErr := lifecycle.DecodeOffer(meta)
	if paramErr != nil {
		return acp.InitializeResponse{}, invalidParam(paramErr)
	}

	var negotiated lifecycle.Negotiated
	if present {
		negotiated = offer.Answer(lifecycle.Negotiated{UpdatesOutsidePrompt: true, ActivityKinds: []lifecycle.ActivityKind{}})
	}

	encoding := selectPositionEncoding(params.ClientCapabilities.PositionEncodings)

	a.mu.Lock()
	a.clientCapabilities = params.ClientCapabilities
	a.positionEncoding = encoding
	a.lifecycle = negotiated
	a.mu.Unlock()

	title := a.options.AgentTitle

	capabilityMeta := map[string]any{
		vendor: map[string]any{
			"elicitation": map[string]any{"unstable": true, "scope": "session", "tracks": "ACP v1 elicitation"},
			metaRawEventKey: map[string]any{
				capabilityMethodKey: RawEventMethod, "enabledBy": "_meta.codex.rawEvent.enabled",
				"maxBytes": wire.RawEventMaxBytes, "defaultEnabled": false,
			},
			"sessionStore": map[string]any{"format": SessionStoreFormat, "key": []string{"sessionId", "subpath"}},
			"structuredOutput": map[string]any{
				"config": metaOptionPath(metaOutputSchemaKey), nativeResultKey: "_meta.codex." + structuredOutputKey, "schema": "json_schema",
			},
		},
		wire.MediaEnvelopeKey: image.MediaEnvelope(a.options.ImageLimits.core(), image.Envelope{DocumentFormats: []string{}}),
	}
	if a.options.InputHandoffRoot != "" {
		capabilityMeta[wire.HandoffKey] = image.HandoffAdvertisement()
	}

	var responseMeta map[string]any
	if negotiated.Present() {
		responseMeta = map[string]any{wire.LifecycleKey: negotiated.Advertisement()}
	}

	return acp.InitializeResponse{
		Meta:            responseMeta,
		ProtocolVersion: acp.ProtocolVersionNumber,
		AgentInfo: &acp.Implementation{
			Name:    a.options.AgentName,
			Title:   &title,
			Version: a.options.AgentVersion,
		},
		AuthMethods: []acp.AuthMethod{},
		AgentCapabilities: acp.AgentCapabilities{
			Meta:             capabilityMeta,
			LoadSession:      true,
			PositionEncoding: &encoding,
			PromptCapabilities: acp.PromptCapabilities{
				EmbeddedContext: true,
				Image:           true,
			},
			SessionCapabilities: acp.SessionCapabilities{
				Close:                 &acp.SessionCloseCapabilities{},
				Delete:                &acp.SessionDeleteCapabilities{},
				List:                  &acp.SessionListCapabilities{},
				Resume:                &acp.SessionResumeCapabilities{},
				AdditionalDirectories: &acp.SessionAdditionalDirectoriesCapabilities{},
			},
		},
	}, nil
}

func selectPositionEncoding(encodings []acp.PositionEncodingKind) acp.PositionEncodingKind {
	if slices.Contains(encodings, acp.PositionEncodingKindUtf8) {
		return acp.PositionEncodingKindUtf8
	}

	return acp.PositionEncodingKindUtf16
}

// Authenticate exists because the SDK interface requires it. The harness
// authenticates itself in its own home, outside ACP.
func (a *Agent) Authenticate(_ context.Context, params acp.AuthenticateRequest) (acp.AuthenticateResponse, error) {
	if refusal := lifecycle.RejectKey(params.Meta); refusal != nil {
		return acp.AuthenticateResponse{}, invalidParam(refusal)
	}

	return acp.AuthenticateResponse{}, acp.NewInvalidParams(map[string]any{"methodId": params.MethodId})
}

// Logout exists because the SDK interface requires it.
func (a *Agent) Logout(_ context.Context, params acp.LogoutRequest) (acp.LogoutResponse, error) {
	if refusal := lifecycle.RejectKey(params.Meta); refusal != nil {
		return acp.LogoutResponse{}, invalidParam(refusal)
	}

	return acp.LogoutResponse{}, acp.NewMethodNotFound(acp.AgentMethodLogout)
}

// SetSessionMode exists because the SDK interface requires it. Native modes
// are config options, never ACP session modes.
func (a *Agent) SetSessionMode(_ context.Context, params acp.SetSessionModeRequest) (acp.SetSessionModeResponse, error) {
	if refusal := lifecycle.RejectKey(params.Meta); refusal != nil {
		return acp.SetSessionModeResponse{}, invalidParam(refusal)
	}

	return acp.SetSessionModeResponse{}, acp.NewMethodNotFound(acp.AgentMethodSessionSetMode)
}

// HandleExtensionMethod answers every extension method with method-not-found.
// The only extension surface is the outbound RawEventMethod notification.
func (a *Agent) HandleExtensionMethod(_ context.Context, method string, params json.RawMessage) (any, error) {
	var envelope struct {
		Meta map[string]any `json:"_meta"` //nolint:tagliatelle // ACP reserves this wire spelling.
	}

	if err := json.Unmarshal(params, &envelope); err == nil {
		if refusal := lifecycle.RejectKey(envelope.Meta); refusal != nil {
			return nil, invalidParam(refusal)
		}
	}

	return nil, acp.NewMethodNotFound(method)
}

func (a *Agent) transportRef() *wire.Transport {
	a.mu.Lock()
	defer a.mu.Unlock()

	return a.transport
}

func (a *Agent) lifecycleNegotiated() lifecycle.Negotiated {
	a.mu.Lock()
	defer a.mu.Unlock()

	return a.lifecycle
}

func (a *Agent) elicitationModes() (form bool, url bool) {
	a.mu.Lock()
	defer a.mu.Unlock()

	capabilities := a.clientCapabilities.Elicitation
	if capabilities == nil {
		return false, false
	}

	return capabilities.Form != nil, capabilities.Url != nil
}

// nextIncarnation mints a stream identity no earlier incarnation of any
// session on this agent used.
func (a *Agent) nextIncarnation() uint64 {
	a.mu.Lock()
	defer a.mu.Unlock()

	a.incarnations++

	return a.incarnations
}

// acquireClientCall takes one slot of the server-to-client call budget
// without waiting.
func (a *Agent) acquireClientCall() (func(), error) {
	select {
	case a.clientCalls <- struct{}{}:
		return func() { <-a.clientCalls }, nil
	default:
		return nil, wire.Backpressure("client_calls")
	}
}

// invalidParam renders a lifecycle negotiation refusal as the uniform
// invalid-params verdict.
func invalidParam(err *lifecycle.ParamError) *acp.RequestError {
	if err.Verdict == lifecycle.VerdictMissing {
		return wire.Missing(err.Field)
	}

	return wire.Unsupported(err.Field)
}

// environment builds the merge for the app-server launch: the inherited
// process environment, the agent overlay, then the home when configured.
func (a *Agent) environment() process.Environment {
	owned := map[string]string{}
	if a.options.Home != "" {
		owned[codexHomeEnv] = a.options.Home
	}

	return process.Environment{
		Process:        a.processEnv,
		Agent:          a.options.Env,
		Owned:          owned,
		InternalPrefix: internalEnvPrefix,
	}
}

// internalClassNativeStart is the one documented codex_internal_failure
// class: a native thread that could not be started or configured for a
// session.
const internalClassNativeStart = "native_start"
