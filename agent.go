package codexacp

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/coder/acp-go-sdk"
	"github.com/savid/acp-go-codex/internal/codex"
	"github.com/savid/acp-go-codex/internal/lifecycle"
	"github.com/savid/acp-go-codex/internal/observer"
)

const (
	listSessionsPageSize            = 50
	defaultMaxActiveSessions        = 32
	sessionTurnCapacity             = 1
	defaultMaxConcurrentClientCalls = 16
	closeTimeout                    = 5 * time.Second

	jsonFieldError      = "error"
	jsonFieldMessage    = "message"
	jsonFieldCwd        = "cwd"
	jsonFieldEntries    = "entries"
	jsonFieldIndex      = "index"
	jsonFieldSessionID  = "sessionId"
	jsonFieldField      = "field"
	validationRequired  = "required"
	validationDuplicate = "duplicate"
	errValueUnsupported = "unsupported"

	jsonFieldSource        = "source"
	jsonFieldSequence      = "sequence"
	jsonFieldEvent         = "event"
	jsonFieldScope         = "scope"
	jsonFieldName          = "name"
	jsonFieldServer        = "server"
	jsonFieldPrompt        = "prompt"
	jsonFieldText          = "text"
	jsonFieldType          = "type"
	jsonFieldURL           = "url"
	jsonFieldMode          = "mode"
	jsonFieldTitle         = "title"
	jsonFieldMeta          = "_meta"
	jsonFieldAction        = "action"
	jsonFieldReason        = "reason"
	jsonFieldPath          = "path"
	jsonFieldConfigID      = "configId"
	jsonFieldAccessToken   = "accessToken"
	jsonFieldNetworkAccess = "networkAccess"
	jsonFieldResult        = "result"

	valueBackpressure        = "backpressure"
	valueSession             = "session"
	valueForm                = "form"
	valueReasoning           = "reasoning"
	valueCommand             = "command"
	valueAgentMessage        = "agent_message"
	valueAgentReasoning      = "agent_reasoning"
	valueEventMsg            = "event_msg"
	valueResponseItem        = "response_item"
	valueStored              = "stored"
	valueLocalImage          = "localImage"
	valueImage               = "image"
	valueImageGenerationCall = "image_generation_call"
	valueImageGeneration     = "imageGeneration"
	roleUser                 = "user"
	roleAgent                = "agent"
	statusDone               = "done"
	statusCompleted          = "completed"
	statusErrored            = "errored"
	statusFailed             = "failed"
	valueDefault             = "default"
	roleAssistant            = "assistant"
	eventUserMessage         = "user_message"

	jsonFieldLimit        = "limit"
	jsonFieldValue        = "value"
	jsonFieldContent      = "content"
	jsonFieldCause        = "cause"
	jsonFieldStatusCode   = "statusCode"
	jsonFieldProviderCode = "providerCode"

	valueTurnFailed = "codex_turn_failed"

	modeDefault acp.SessionModeId = "default"
	modePlan    acp.SessionModeId = "plan"

	configModel       acp.SessionConfigId = "model"
	configMode        acp.SessionConfigId = "mode"
	configEffort      acp.SessionConfigId = "effort"
	configServiceTier acp.SessionConfigId = "service_tier"
	configPersonality acp.SessionConfigId = "personality"

	configTypeSelect = "select"
)

// Agent exposes Codex through ACP.
type Agent struct {
	options      Options
	log          *slog.Logger
	observe      *observer.Observer
	optionsErr   error
	providerAuth *providerAuth

	mu                sync.Mutex
	closed            bool
	closeDone         chan struct{}
	closeErr          error
	conn              agentClient
	sessions          map[acp.SessionId]*session
	deleted           map[acp.SessionId]struct{}
	deleting          map[acp.SessionId]int
	clientCalls       chan struct{}
	authTokens        *ChatGPTAuthTokens
	providerProcesses *providerProcessSnapshotTracker
	containmentMode   RuntimeContainmentMode

	runtimeClient         codex.Client
	runtimeEpoch          uint64
	runtimeDead           bool
	runtimeStarting       chan struct{}
	runtimeStartCancel    context.CancelFunc
	runtimeClosing        chan struct{}
	runtimeNativeRelease  func()
	runtimeScratchRoot    string
	runtimeScratchRelease func()
	runtimeCleanupErr     error
	retainedThreads       map[acp.SessionId]*retainedRuntimeThread

	clientCapabilities acp.ClientCapabilities
	positionEncoding   acp.PositionEncodingKind
	// lifecycle is the answer this connection settled on at initialize. An
	// absent answer makes every envelope, prompt correlation, and action
	// correlation illegal on the connection.
	lifecycle lifecycle.Negotiated

	rateLimitsMu   sync.Mutex
	rateLimitsSnap *codex.RateLimitSnapshot
}

type codexClientEventSink struct {
	agent *Agent
	epoch uint64

	mu      sync.Mutex
	client  codex.Client
	pending []codex.Event
}

var (
	_ acp.Agent                  = (*Agent)(nil)
	_ acp.AgentLoader            = (*Agent)(nil)
	_ acp.ExtensionMethodHandler = (*Agent)(nil)
)

// NewAgent creates an ACP agent for Codex.
func NewAgent(opts ...Option) *Agent {
	options := applyOptions(opts)

	homeErr := normalizeStandaloneHome(&options)
	if options.SessionStore == nil {
		options.SessionStore = NewInMemorySessionStore()
	}

	limits, optionsErr := normalizeConcurrencyLimits(options.ConcurrencyLimits)
	optionsErr = errors.Join(optionsErr, homeErr)
	optionsErr = errors.Join(optionsErr, validateCodexConfigOverrides(options.Config))
	optionsErr = errors.Join(optionsErr, validateContainmentOptions(options))
	optionsErr = errors.Join(optionsErr, validateImageLimits(options.ImageLimits))
	optionsErr = errors.Join(optionsErr, validateInputHandoffRoot(options.InputHandoffRoot))
	optionsErr = errors.Join(optionsErr, validateProviderAuthOptions(options))
	options.ConcurrencyLimits = limits

	clientCallLimit := limits.MaxConcurrentClientCalls
	if clientCallLimit < 0 {
		clientCallLimit = 0
	}

	log := options.Logger
	if log == nil {
		log = slog.Default()
	}

	observe := observer.New(observer.Config{
		MeterProvider:  options.MeterProvider,
		Propagator:     options.TextMapPropagator,
		TracerProvider: options.TracerProvider,
		Version:        options.AgentVersion,
	})
	options.RuntimeResourceHooks = instrumentRuntimeResourceHooks(options.RuntimeResourceHooks, observe)
	mode := containmentMode(options)

	providerProcesses := newProviderProcessSnapshotTracker(options.RuntimeResourceHooks, mode.provesWholeTreeLifecycle())
	if options.RuntimeResourceHooks.ObserveContainment != nil {
		options.RuntimeResourceHooks.ObserveContainment(context.Background(), mode)
	}

	if mode == RuntimeContainmentBestEffort {
		log.Warn("Darwin best-effort process containment is enabled; escaped descendants may survive, numeric PGID reuse can cause collateral signalling, marker correlation is not ownership, markers can be scrubbed, and native-root permits do not bound escaped provider work",
			slog.String("containment", string(mode)),
		)
	}

	agent := &Agent{
		options:           options,
		log:               log,
		optionsErr:        optionsErr,
		observe:           observe,
		sessions:          make(map[acp.SessionId]*session),
		deleted:           make(map[acp.SessionId]struct{}),
		deleting:          make(map[acp.SessionId]int),
		clientCalls:       make(chan struct{}, clientCallLimit),
		providerProcesses: providerProcesses,
		containmentMode:   mode,
		retainedThreads:   make(map[acp.SessionId]*retainedRuntimeThread),
	}

	if optionsErr == nil {
		agent.providerAuth = newProviderAuth(agent)
	}

	return agent
}

func (a *Agent) ContainmentMode() RuntimeContainmentMode {
	if a == nil {
		return RuntimeContainmentUnavailable
	}

	return a.containmentMode
}

// codexReservedConfigRoots names the app-server config roots the adapter
// authors per thread, mapped to the owner a rejected override names. A `-c`
// override applies to every thread of the app-server process at once, which is
// outside the per-thread ownership entirely: reserving only a dotted child
// would leave its siblings open, so each root is reserved whole.
//
// The reservation belongs to the keyspace, not to this entry point. Every route
// that can define these roots reads this one list: `-c` overrides here, and a
// seeded CODEX_HOME config.toml through codexConfigRootIsReserved.
var codexReservedConfigRoots = map[string]string{
	"mcp_servers":              "session-scoped MCP",
	"shell_environment_policy": "the thread-owned shell environment",
}

func validateCodexConfigOverrides(config map[string]any) error {
	for key := range config {
		if owner, reserved := codexReservedConfigRoots[codexConfigRootKey(key)]; reserved {
			return fmt.Errorf("%s config override %q is reserved for %s", codexMetaKey, key, owner)
		}
	}

	return nil
}

func codexConfigRootKey(key string) string {
	key = strings.TrimSpace(key)
	if key == "" {
		return ""
	}

	switch key[0] {
	case '\'':
		if end := strings.IndexByte(key[1:], '\''); end >= 0 {
			return key[1 : end+1]
		}
	case '"':
		for index := 1; index < len(key); index++ {
			if key[index] != '"' || key[index-1] == '\\' {
				continue
			}

			if root, err := strconv.Unquote(key[:index+1]); err == nil {
				return root
			}

			break
		}
	default:
		if end := strings.IndexAny(key, ". \t\r\n"); end >= 0 {
			return key[:end]
		}

		return key
	}

	return key
}

func (a *Agent) setAgentClient(conn agentClient) {
	a.mu.Lock()
	defer a.mu.Unlock()

	a.conn = conn
}

// Serve runs an ACP agent over the provided streams.
func Serve(ctx context.Context, input io.Reader, output io.Writer, opts ...Option) (serveErr error) {
	if err := ctx.Err(); err != nil {
		return err
	}

	agent := NewAgent(opts...)
	defer func() {
		if closeErr := agent.Close(); closeErr != nil {
			agent.log.DebugContext(context.Background(), "close Codex ACP agent failed", slog.String("error", closeErr.Error()))
			serveErr = closeErr
		}
	}()

	conn := newLocalAgentConnection(agent, output, input)
	agent.setAgentClient(conn)

	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-conn.Done():
		return nil
	}
}

// Close cancels and closes all resources owned by the agent.
func (a *Agent) Close() error {
	a.mu.Lock()
	if closeDone := a.closeDone; closeDone != nil {
		a.mu.Unlock()
		<-closeDone

		a.mu.Lock()
		closeErr := a.closeErr
		a.mu.Unlock()

		return closeErr
	}

	closeDone := make(chan struct{})
	a.closeDone = closeDone

	if a.providerAuth != nil {
		defer a.providerAuth.closeAll()
	}

	sessions := make([]*session, 0, len(a.sessions))
	for _, session := range a.sessions {
		sessions = append(sessions, session)
	}

	a.sessions = make(map[acp.SessionId]*session)
	a.closed = true
	a.conn = nil
	a.mu.Unlock()

	var err error

	for _, session := range sessions {
		ctx, cancel := context.WithTimeout(context.Background(), closeTimeout)
		err = errors.Join(err, session.Close(ctx))

		cancel()
	}

	ctx, cancel := context.WithTimeout(context.Background(), closeTimeout)
	err = errors.Join(err, a.closeSharedRuntime(ctx))

	cancel()

	a.observe.AddActiveSession(context.Background(), -int64(len(sessions)))

	a.mu.Lock()
	a.closeErr = err

	close(closeDone)
	a.mu.Unlock()

	return err
}

func (a *Agent) setExternalAuthTokens(tokens ChatGPTAuthTokens) {
	a.mu.Lock()
	defer a.mu.Unlock()

	copied := tokens
	a.authTokens = &copied
}

func (a *Agent) clearExternalAuthTokens() {
	a.mu.Lock()
	defer a.mu.Unlock()

	a.authTokens = nil
}

func (a *Agent) externalAuthTokens() (ChatGPTAuthTokens, bool) {
	a.mu.Lock()
	defer a.mu.Unlock()

	if a.authTokens == nil {
		return ChatGPTAuthTokens{}, false
	}

	return *a.authTokens, true
}

// optionsError answers the construction-time option verdict. An agent built
// from options it refuses cannot serve anything, so every entry point asks
// this and not just initialize: an embedded host can open a session and prompt
// without ever handshaking. The code is internal error rather than invalid
// params because the caller's params are fine — what is broken is the agent
// the embedding host built. The joined Go text is the whole payload because
// there is no wire field to name, and the prose is all the operator has.
func (a *Agent) optionsError() error {
	if a.optionsErr == nil {
		return nil
	}

	return acp.NewInternalError(map[string]any{jsonFieldError: a.optionsErr.Error()})
}

// Initialize implements ACP initialize.
func (a *Agent) Initialize(_ context.Context, params acp.InitializeRequest) (acp.InitializeResponse, error) {
	if err := a.optionsError(); err != nil {
		return acp.InitializeResponse{}, err
	}

	negotiated, err := a.negotiateLifecycle(params.Meta)
	if err != nil {
		return acp.InitializeResponse{}, err
	}

	title := a.options.AgentTitle
	positionEncoding := selectPositionEncoding(params.ClientCapabilities.PositionEncodings)

	a.mu.Lock()
	a.clientCapabilities = cloneClientCapabilities(params.ClientCapabilities)
	a.positionEncoding = positionEncoding
	a.lifecycle = negotiated
	a.mu.Unlock()

	codexMeta := map[string]any{
		"fork": map[string]any{
			"unstable":      true,
			jsonFieldMethod: ForkSessionMethod,
			"request":       "acp.UnstableForkSessionRequest JSON payload only",
			"response":      "acp.UnstableForkSessionResponse JSON payload only",
		},
		"elicitation": map[string]any{
			"unstable":     true,
			jsonFieldScope: valueSession,
			"tracks":       "ACP v1 elicitation",
		},
		rawEventCapabilityKey: map[string]any{
			jsonFieldMethod:  RawEventMethod,
			"enabledBy":      rawEventEnabledByPath,
			"maxBytes":       rawEventMaxBytes,
			"defaultEnabled": false,
		},
		"sessionStore": map[string]any{
			"format": SessionStoreFormat,
			"key":    []string{jsonFieldSessionID, "subpath"},
		},
		structuredOutputCapabilityKey: map[string]any{
			"config":        outputSchemaConfigPath,
			jsonFieldResult: "_meta.codex.structuredOutput",
			"schema":        "json_schema",
		},
	}

	if a.providerAuth != nil {
		codexMeta[providerAuthCapabilityKey] = a.providerAuth.capability()
	}

	return acp.InitializeResponse{
		ProtocolVersion: acp.ProtocolVersionNumber,
		Meta:            lifecycleResponseMeta(negotiated),
		AgentInfo: &acp.Implementation{
			Name:    a.options.AgentName,
			Title:   &title,
			Version: a.options.AgentVersion,
		},
		AuthMethods: a.authMethods(params),
		AgentCapabilities: acp.AgentCapabilities{
			Meta:        a.capabilityMeta(codexMeta),
			LoadSession: true,
			Auth:        a.authCapabilities(),
			McpCapabilities: acp.McpCapabilities{
				Http: true,
			},
			PositionEncoding: &positionEncoding,
			PromptCapabilities: acp.PromptCapabilities{
				EmbeddedContext: true,
				Image:           true,
			},
			SessionCapabilities: acp.SessionCapabilities{
				AdditionalDirectories: &acp.SessionAdditionalDirectoriesCapabilities{},
				Close:                 &acp.SessionCloseCapabilities{},
				Delete:                &acp.SessionDeleteCapabilities{},
				List:                  &acp.SessionListCapabilities{},
				Resume:                &acp.SessionResumeCapabilities{},
			},
		},
	}, nil
}

func (a *Agent) launchRuntimeClient(ctx context.Context, epoch uint64, supervisorRoot string, nativeVersion string) (codex.Client, error) {
	env := a.staticRuntimeEnv()
	home := a.resolvedCodexHomeForEnv(env)

	if a.options.ProcessIsolation != nil && len(a.options.SeedFiles) > 0 {
		return nil, errors.New("codex seed files are unsupported with process isolation")
	}

	if err := validateNativeOwnedDirectory(home, a.options.ProcessIsolation); err != nil {
		return nil, err
	}

	factory := a.options.clientFactory
	if factory == nil {
		factory = func(ctx context.Context, options codex.Options) (codex.Client, error) {
			return codex.NewAppServerClient(ctx, options)
		}
	}

	configurationStarted := time.Now()
	if err := writeSeedFiles(a.options.Home, a.options.SeedFiles); err != nil {
		observeRuntimeStartupStage(ctx, a.options.RuntimeResourceHooks, RuntimeResourceRuntime, RuntimeStartupConfiguration, configurationStarted, err)

		return nil, err
	}

	otelConfig, err := a.codexOTELConfig(env)
	if err != nil {
		observeRuntimeStartupStage(ctx, a.options.RuntimeResourceHooks, RuntimeResourceRuntime, RuntimeStartupConfiguration, configurationStarted, err)

		return nil, err
	}

	extraArgs := append([]string(nil), otelConfig.ExtraArgs...)

	a.observe.RecordCodexProcessStart(ctx)
	eventSink := &codexClientEventSink{agent: a, epoch: epoch}

	observeRuntimeStartupStage(ctx, a.options.RuntimeResourceHooks, RuntimeResourceRuntime, RuntimeStartupConfiguration, configurationStarted, nil)

	client, err := factory(ctx, codex.Options{
		CLIPath:             a.options.ExecutablePath,
		CodexHome:           home,
		WritableHome:        home,
		SupervisorRoot:      supervisorRoot,
		SupervisorParent:    filepath.Dir(supervisorRoot),
		DarwinBestEffort:    a.containmentMode == RuntimeContainmentBestEffort,
		NativeVersion:       nativeVersion,
		DefaultModel:        a.options.DefaultModel,
		Env:                 a.observe.InjectTraceEnv(ctx, env),
		ImplicitEnvironment: cloneStringMap(a.options.implicitEnvironment),
		ProcessIsolation:    codexProcessIsolation(a.options.ProcessIsolation),
		Config:              a.codexConfig(),
		ExtraArgs:           extraArgs,
		Logger:              a.log,
		EventHandler:        eventSink.Handle,
		RequestHandler: func(ctx context.Context, req codex.ServerRequest) (any, error) {
			return a.handleCodexServerRequestForEpoch(ctx, epoch, req)
		},
		ObserveProcess: func(processCtx context.Context, kind string, delta int64) {
			observeRuntimeProcess(processCtx, a.options.RuntimeResourceHooks, RuntimeProcessKind(kind), delta)
		},
		NewProcessSnapshotObserver: a.newProcessSnapshotObserver,
		ObserveStartupStage: func(stageCtx context.Context, lifecycle, stage string, elapsed time.Duration, stageErr error) {
			observe := a.options.RuntimeResourceHooks.ObserveStartupStage
			if observe != nil {
				observe(stageCtx, RuntimeResourceKind(lifecycle), RuntimeStartupStage(stage), elapsed, stageErr)
			}
		},
	})
	if err != nil {
		return nil, err
	}

	eventSink.SetClient(client)

	if tokens, ok := a.externalAuthTokens(); ok {
		if err := client.LoginWithChatGPTTokens(ctx, toCodexAuthTokens(tokens)); err != nil {
			_ = client.Close(context.Background())

			return nil, err
		}
	}

	return client, nil
}

func codexProcessIsolation(value *ProcessIsolation) *codex.ProcessIsolation {
	if value == nil {
		return nil
	}

	return &codex.ProcessIsolation{
		UID: value.UID, GID: value.GID, BaseEnvironment: cloneStringMap(value.BaseEnvironment),
		StandaloneOwnerID: value.StandaloneOwnerID, StandaloneStateRoot: value.StandaloneStateRoot,
		IdentityLock: value.IdentityLock, AuthorityDomain: value.AuthorityDomain,
	}
}

func (a *Agent) codexConfig() map[string]any {
	if a.options.Config == nil {
		return nil
	}

	config := make(map[string]any, len(a.options.Config))
	for key, value := range a.options.Config {
		config[key] = value
	}

	return config
}

func (s *codexClientEventSink) Handle(_ context.Context, event codex.Event) {
	if !s.agent.runtimeEpochIsCurrent(s.epoch) {
		return
	}

	switch event.Kind {
	case codex.EventAccountUpdated, codex.EventLoginCompleted, codex.EventRateLimitsUpdated, codex.EventError:
	default:
		return
	}

	s.mu.Lock()

	client := s.client
	if client == nil {
		s.pending = append(s.pending, event)
		s.mu.Unlock()

		return
	}
	s.mu.Unlock()

	s.agent.applyCodexClientEvent(context.Background(), client, event)
}

func (s *codexClientEventSink) SetClient(client codex.Client) {
	s.mu.Lock()
	s.client = client
	pending := append([]codex.Event(nil), s.pending...)
	s.pending = nil
	s.mu.Unlock()

	for i := range pending {
		s.agent.applyCodexClientEvent(context.Background(), client, pending[i])
	}
}

func (a *Agent) applyCodexClientEvent(ctx context.Context, client codex.Client, event codex.Event) {
	switch event.Kind {
	case codex.EventAccountUpdated:
		a.updateAccountForClient(client, event.ThreadID, event.Account)
	case codex.EventLoginCompleted:
		if a.providerAuth != nil {
			a.providerAuth.loginCompleted(ctx, event.Login)
		}
	case codex.EventRateLimitsUpdated:
		if event.RateLimits != nil {
			a.cacheRateLimits(*event.RateLimits)
		}
	case codex.EventError:
		// Native error notifications also carry ordinary turn failures such as
		// provider quota exhaustion. Only errors that prove the app-server
		// transport or process died may poison the shared runtime generation;
		// treating a provider rejection as process death needlessly relaunches
		// the app-server and attempts to resume every otherwise-live thread.
		if codexRuntimeDied(event.Err) {
			a.markRuntimeDead(client)
		}
	}
}

func codexRuntimeDied(err error) bool {
	if err == nil {
		return false
	}

	var processExit *codex.ProcessExitError

	return errors.Is(err, codex.ErrConnectionClosed) || errors.As(err, &processExit)
}

// cacheRateLimits records the latest harness-reported rate-limit snapshot.
// The cache is agent-level and latest-wins across every session: any newer
// snapshot from any client replaces the previous one.
func (a *Agent) cacheRateLimits(snapshot codex.RateLimitSnapshot) {
	if !snapshot.HasData() {
		return
	}

	a.rateLimitsMu.Lock()
	defer a.rateLimitsMu.Unlock()

	a.rateLimitsSnap = &snapshot
}

// cachedRateLimits returns the latest cached snapshot, if any.
func (a *Agent) cachedRateLimits() (codex.RateLimitSnapshot, bool) {
	a.rateLimitsMu.Lock()
	defer a.rateLimitsMu.Unlock()

	if a.rateLimitsSnap == nil {
		return codex.RateLimitSnapshot{}, false
	}

	return *a.rateLimitsSnap, true
}

func (a *Agent) updateAccountForClient(client codex.Client, threadID string, account codex.Account) {
	meta := redactedAccountMeta(account)
	if len(meta) == 0 {
		return
	}

	a.mu.Lock()

	sessions := make([]*session, 0, len(a.sessions))
	for _, session := range a.sessions {
		if session.client != client {
			continue
		}

		if threadID != "" && session.codexThreadID != threadID {
			continue
		}

		sessions = append(sessions, session)
	}
	a.mu.Unlock()

	for _, session := range sessions {
		session.setAccount(meta)
	}
}

func (a *Agent) ensureOpen() error {
	a.mu.Lock()
	defer a.mu.Unlock()

	if a.closed {
		return newAgentClosedError()
	}

	return a.optionsError()
}

func (a *Agent) acquireClientCall(ctx context.Context) (func(), error) {
	if a.clientCalls == nil {
		return func() {}, nil
	}

	select {
	case a.clientCalls <- struct{}{}:
		return func() { <-a.clientCalls }, nil
	default:
		return nil, acp.NewInvalidRequest(map[string]any{jsonFieldError: valueBackpressure, jsonFieldLimit: "client_calls"})
	}
}

func (a *Agent) session(id acp.SessionId) (*session, error) {
	a.mu.Lock()
	defer a.mu.Unlock()

	if a.closed {
		return nil, newAgentClosedError()
	}

	session, ok := a.sessions[id]
	if !ok {
		return nil, newUnknownSession()
	}

	session.mu.Lock()
	closing := session.closing
	session.mu.Unlock()

	if closing {
		return nil, newSessionCloseInProgress()
	}

	return session, nil
}

// acquireSessionLifecycle admits a resume or load against an active wrapper.
// The double check makes close admission linearizable without holding Agent.mu
// across native calls. Agent.mu is always acquired before session.mu.
func (a *Agent) acquireSessionLifecycle(id acp.SessionId) (func(), error) {
	a.mu.Lock()
	if a.closed {
		a.mu.Unlock()

		return nil, newAgentClosedError()
	}

	if a.deletePendingLocked(id) {
		a.mu.Unlock()

		return nil, newUnknownSession()
	}

	session := a.sessions[id]
	if session == nil {
		a.mu.Unlock()

		return func() {}, nil
	}

	session.mu.Lock()
	closing := session.closing
	session.mu.Unlock()
	a.mu.Unlock()

	if closing {
		return nil, newSessionCloseInProgress()
	}

	session.sessionOps.RLock()

	if err := a.validateSessionLifecycle(id, session); err != nil {
		session.sessionOps.RUnlock()

		return nil, err
	}

	return session.sessionOps.RUnlock, nil
}

func (a *Agent) validateSessionLifecycle(id acp.SessionId, session *session) error {
	a.mu.Lock()
	closed := a.closed
	current := a.sessions[id]
	deleting := a.deletePendingLocked(id)

	session.mu.Lock()
	closing := session.closing
	session.mu.Unlock()
	a.mu.Unlock()

	switch {
	case closed:
		return newAgentClosedError()
	case closing:
		return newSessionCloseInProgress()
	case deleting:
		return newUnknownSession()
	case current != session:
		return newUnknownSession()
	default:
		return nil
	}
}

// deletePendingLocked reports that a delete of this id is still running. Delete
// claims the id before it inspects the active wrapper, so a store-only delete —
// which has no wrapper whose close flag could carry the fence — is still visible
// to load and resume admission. Agent.mu is held by the caller.
func (a *Agent) deletePendingLocked(id acp.SessionId) bool { return a.deleting[id] > 0 }

// deleteFencedLocked reports that an id is barred from coming back at all: a
// committed tombstone bars it forever, and a running delete bars it for the
// duration. Agent.mu is held by the caller.
func (a *Agent) deleteFencedLocked(id acp.SessionId) bool {
	if _, ok := a.deleted[id]; ok {
		return true
	}

	return a.deletePendingLocked(id)
}

// claimSessionDelete fences an id for one delete. The claim is counted because
// two concurrent deletes of the same id are both legal and idempotent, and the
// fence must outlive the later of them.
func (a *Agent) claimSessionDelete(id acp.SessionId) func() {
	a.mu.Lock()
	a.deleting[id]++
	a.mu.Unlock()

	return func() {
		a.mu.Lock()
		defer a.mu.Unlock()

		if a.deleting[id] <= 1 {
			delete(a.deleting, id)

			return
		}

		a.deleting[id]--
	}
}

func (a *Agent) storeStartedSession(session *session) error {
	a.mu.Lock()
	if a.closed {
		a.mu.Unlock()

		if err := session.Close(context.Background()); err != nil {
			a.log.DebugContext(context.Background(), "close rejected Codex session failed", slog.String(jsonFieldError, err.Error()))
		}

		return newAgentClosedError()
	}

	// The admission check at the head of load and resume happens before the
	// native resume it blocks in, so the tombstone is re-read here, where the
	// wrapper would actually become reachable. A resume that raced a delete
	// hands back a native thread nobody may address, so it is closed rather
	// than registered.
	if a.deleteFencedLocked(session.id) {
		a.mu.Unlock()

		if err := session.Close(context.Background()); err != nil {
			a.log.DebugContext(context.Background(), "close deleted Codex session failed", slog.String(jsonFieldError, err.Error()))
		}

		return newUnknownSession()
	}

	previous := a.sessions[session.id]
	if previous == nil && len(a.sessions) >= a.options.ConcurrencyLimits.MaxActiveSessions {
		a.mu.Unlock()

		if err := session.Close(context.Background()); err != nil {
			a.log.DebugContext(context.Background(), "close backpressured Codex session failed", slog.String(jsonFieldError, err.Error()))
		}

		return acp.NewInvalidRequest(map[string]any{jsonFieldError: valueBackpressure, jsonFieldLimit: "active_sessions"})
	}

	a.sessions[session.id] = session
	a.mu.Unlock()

	a.readmitProviderAuth(session.id)

	if previous != nil {
		if err := previous.Close(context.Background()); err != nil {
			a.log.WarnContext(context.Background(), "close replaced Codex session failed", slog.String(jsonFieldError, err.Error()))
		}

		return nil
	}

	a.observe.AddActiveSession(context.Background(), 1)

	return nil
}

// readmitProviderAuth tells the provider-auth broker that a session id is live
// again. The broker refuses every leg naming a session it has swept, and codex
// names a session by the thread it drives, so an id can come back through
// session/load and must not stay refused for the rest of the agent's life.
func (a *Agent) readmitProviderAuth(id acp.SessionId) {
	if a.providerAuth == nil {
		return
	}

	a.providerAuth.openSession(id)
}

func newAgentClosedError() *acp.RequestError {
	return acp.NewInvalidRequest(map[string]any{jsonFieldError: "agent is closed"})
}

func (a *Agent) removeSession(id acp.SessionId) *session {
	a.mu.Lock()
	defer a.mu.Unlock()

	session := a.sessions[id]
	delete(a.sessions, id)

	return session
}

func newSessionCloseInProgress() *acp.RequestError {
	return acp.NewInvalidRequest(map[string]any{jsonFieldError: "session close in progress"})
}

var errNoActiveSessionForDelete = errors.New("no active session for delete")

// beginSessionClose prevents new lifecycle requests from entering before the
// native unsubscribe begins. The caller acquires session.lifecycle for the
// native operation after this method returns.
func (a *Agent) beginSessionClose(id acp.SessionId) (*session, error) {
	a.mu.Lock()
	defer a.mu.Unlock()

	if a.closed {
		return nil, newAgentClosedError()
	}

	session := a.sessions[id]
	if session == nil {
		return nil, newUnknownSession()
	}

	session.mu.Lock()
	defer session.mu.Unlock()

	if session.closing {
		return nil, newSessionCloseInProgress()
	}

	session.closing = true

	return session, nil
}

// beginSessionDelete closes prompt admission for an active wrapper while still
// permitting deletion of a store-only session. Agent.mu then session.mu is the
// same lock order used by ordinary session lookup, so prompt admission and
// delete admission have one linearization point at session.closing.
func (a *Agent) beginSessionDelete(id acp.SessionId) (*session, error) {
	a.mu.Lock()
	defer a.mu.Unlock()

	if a.closed {
		return nil, newAgentClosedError()
	}

	session := a.sessions[id]
	if session == nil {
		return nil, errNoActiveSessionForDelete
	}

	session.mu.Lock()
	defer session.mu.Unlock()

	if session.closing {
		return nil, newSessionCloseInProgress()
	}

	session.closing = true

	return session, nil
}

func (a *Agent) abortSessionClose(id acp.SessionId, session *session) {
	a.mu.Lock()
	defer a.mu.Unlock()

	if a.sessions[id] != session {
		return
	}

	session.mu.Lock()
	session.closing = false
	session.mu.Unlock()
}

// finishSessionCloseRetainingThread publishes native-thread ownership only
// after unsubscribe succeeded. Materialized rollout cleanup moves with that
// ownership so the canonical path remains valid until rebind or runtime end.
func (a *Agent) finishSessionCloseRetainingThread(id acp.SessionId, session *session) (bool, bool) {
	a.mu.Lock()
	defer a.mu.Unlock()

	if a.sessions[id] != session {
		return false, false
	}

	session.mu.Lock()
	defer session.mu.Unlock()

	delete(a.sessions, id)

	if a.runtimeClient != session.client || a.runtimeDead || session.clientDead || session.codexThreadID == "" {
		return true, false
	}

	if a.retainedThreads == nil {
		a.retainedThreads = make(map[acp.SessionId]*retainedRuntimeThread)
	}

	a.retainedThreads[id] = &retainedRuntimeThread{
		sessionID:           id,
		threadID:            session.codexThreadID,
		path:                session.rolloutPath,
		client:              session.client,
		epoch:               a.runtimeEpoch,
		materializedPath:    session.materializedPath,
		materializedRelease: session.materializedRelease,
	}
	session.materializedPath = ""
	session.materializedRelease = nil

	return true, true
}

func (a *Agent) connection() agentClient {
	a.mu.Lock()
	defer a.mu.Unlock()

	return a.conn
}

func (a *Agent) clientElicitationCapabilities() *acp.ElicitationCapabilities {
	a.mu.Lock()
	defer a.mu.Unlock()

	return a.clientCapabilities.Elicitation
}

func (a *Agent) clientSupportsFormElicitation() bool {
	caps := a.clientElicitationCapabilities()
	if caps == nil {
		return false
	}

	return caps.Form != nil
}

func (a *Agent) clientSupportsURLElicitation() bool {
	caps := a.clientElicitationCapabilities()

	return caps != nil && caps.Url != nil
}

func (a *Agent) emitUpdate(ctx context.Context, sessionID acp.SessionId, update acp.SessionUpdate) error {
	conn := a.connection()
	if conn == nil {
		return nil
	}

	return conn.SessionUpdate(ctx, acp.SessionNotification{
		Meta:      turnRouteMetaFromContext(ctx),
		SessionId: sessionID,
		Update:    update,
	})
}

func (a *Agent) sessionStore() SessionStore {
	return a.options.SessionStore
}

func selectPositionEncoding(encodings []acp.PositionEncodingKind) acp.PositionEncodingKind {
	if slices.Contains(encodings, acp.PositionEncodingKindUtf8) {
		return acp.PositionEncodingKindUtf8
	}

	if slices.Contains(encodings, acp.PositionEncodingKindUtf16) {
		return acp.PositionEncodingKindUtf16
	}

	return acp.PositionEncodingKindUtf16
}

func normalizeConcurrencyLimits(limits ConcurrencyLimits) (ConcurrencyLimits, error) {
	if limits.MaxActiveSessions == 0 {
		limits.MaxActiveSessions = defaultMaxActiveSessions
	}

	if limits.MaxConcurrentClientCalls == 0 {
		limits.MaxConcurrentClientCalls = defaultMaxConcurrentClientCalls
	}

	if limits.MaxActiveSessions < 0 {
		return limits, fmt.Errorf("ConcurrencyLimits.MaxActiveSessions must be non-negative")
	}

	if limits.MaxConcurrentClientCalls < 0 {
		return limits, fmt.Errorf("ConcurrencyLimits.MaxConcurrentClientCalls must be non-negative")
	}

	return limits, nil
}

func cloneClientCapabilities(capabilities acp.ClientCapabilities) acp.ClientCapabilities {
	capabilities.PositionEncodings = append([]acp.PositionEncodingKind(nil), capabilities.PositionEncodings...)

	return capabilities
}

func mapFromRaw(raw json.RawMessage) map[string]any {
	var out map[string]any
	if len(raw) > 0 {
		_ = json.Unmarshal(raw, &out)
	}

	return out
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if value != "" {
			return value
		}
	}

	return ""
}
