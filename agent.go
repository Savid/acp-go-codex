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
	jsonFieldCode       = "code"
	jsonFieldData       = "data"
	jsonFieldMessage    = "message"
	jsonFieldCwd        = "cwd"
	jsonFieldEntries    = "entries"
	jsonFieldIndex      = "index"
	jsonFieldSessionID  = "sessionId"
	jsonFieldField      = "field"
	validationRequired  = "required"
	validationDuplicate = "duplicate"
	errValueUnsupported = "unsupported"

	jsonFieldSource          = "source"
	jsonFieldSequence        = "sequence"
	jsonFieldEvent           = "event"
	jsonFieldScope           = "scope"
	jsonFieldName            = "name"
	jsonFieldServer          = "server"
	jsonFieldPrompt          = "prompt"
	jsonFieldText            = "text"
	jsonFieldUnstable        = "unstable"
	jsonFieldType            = "type"
	jsonFieldURL             = "url"
	jsonFieldMode            = "mode"
	jsonFieldTitle           = "title"
	jsonFieldMeta            = "_meta"
	jsonFieldAction          = "action"
	jsonFieldReason          = "reason"
	jsonFieldPath            = "path"
	jsonFieldConfigID        = "configId"
	jsonFieldAccessToken     = "accessToken"
	jsonFieldNetworkAccess   = "networkAccess"
	jsonFieldRequestedSchema = "requestedSchema"
	jsonFieldResult          = "result"

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

	// limitSessionPrompt names the per-session prompt serialization limit a
	// concurrent second prompt is refused under. The token is family-fixed.
	limitSessionPrompt = "session_prompt"

	jsonFieldLimit        = "limit"
	jsonFieldValue        = "value"
	jsonFieldContent      = "content"
	jsonFieldCause        = "cause"
	jsonFieldStatusCode   = "statusCode"
	jsonFieldProviderCode = "providerCode"

	valueTurnFailed      = "codex_turn_failed"
	valueInternalFailure = "codex_internal_failure"

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

	mu                    sync.Mutex
	closed                bool
	closeDone             chan struct{}
	closeErr              error
	conn                  agentClient
	sessions              map[acp.SessionId]*session
	deleted               map[acp.SessionId]struct{}
	deleting              map[acp.SessionId]int
	clientCalls           chan struct{}
	lifecycleCalls        chan struct{}
	authTokens            *ChatGPTAuthTokens
	runtimeClient         codex.Client
	runtimeEpoch          uint64
	runtimeDead           bool
	runtimeStarting       chan struct{}
	runtimeStartCancel    context.CancelFunc
	runtimeClosing        chan struct{}
	runtimeNativeRelease  func() error
	runtimeScratchRoot    string
	runtimeScratchRelease func()
	runtimeCleanupErr     error
	retainedThreads       map[acp.SessionId]*retainedRuntimeThread
	retiredResidences     []retiredNativeResidence
	retiredResidenceBytes int64

	clientCapabilities acp.ClientCapabilities
	positionEncoding   acp.PositionEncodingKind
	// lifecycle is the answer this connection settled on at initialize. An
	// absent answer makes every envelope, prompt correlation, and action
	// correlation illegal on the connection.
	lifecycle lifecycle.Negotiated
}

type codexClientEventSink struct {
	agent *Agent
	epoch uint64

	mu      sync.Mutex
	client  codex.Client
	pending []codex.Event
	failure error
}

const startupClientEventLimit = 1024

var (
	_ acp.Agent                  = (*Agent)(nil)
	_ acp.AgentLoader            = (*Agent)(nil)
	_ acp.ExtensionMethodHandler = (*Agent)(nil)
)

// NewAgent creates an ACP agent for Codex.
func NewAgent(opts ...Option) *Agent {
	options := applyOptions(opts)

	if options.SessionStore == nil {
		options.SessionStore = NewInMemorySessionStore()
	}

	limits, optionsErr := normalizeConcurrencyLimits(options.ConcurrencyLimits)
	optionsErr = errors.Join(optionsErr, validateCodexConfigOverrides(options.Config))
	optionsErr = errors.Join(optionsErr, validateHostAuthority(options.HostAuthority))
	optionsErr = errors.Join(optionsErr, validateRuntimeEnvironment(options.Env))
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
	agent := &Agent{
		options:         options,
		log:             log,
		optionsErr:      optionsErr,
		observe:         observe,
		sessions:        make(map[acp.SessionId]*session),
		deleted:         make(map[acp.SessionId]struct{}),
		deleting:        make(map[acp.SessionId]int),
		clientCalls:     make(chan struct{}, clientCallLimit),
		lifecycleCalls:  make(chan struct{}, 1),
		retainedThreads: make(map[acp.SessionId]*retainedRuntimeThread),
	}

	if optionsErr == nil {
		agent.providerAuth = newProviderAuth(agent)
	}

	return agent
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
			agent.log.DebugContext(context.Background(), "close Codex ACP agent failed")

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

	sessions := make([]*session, 0, len(a.sessions))
	for _, session := range a.sessions {
		sessions = append(sessions, session)
	}

	conn := a.conn
	a.sessions = make(map[acp.SessionId]*session)
	a.closed = true
	a.conn = nil
	a.mu.Unlock()

	closeCtx := context.Background()

	// The ladder's fourth rung, for every session at once: no completer may
	// outlive the process that armed it, and cancelling the flows after the
	// native interrupts below would leave each one armed against a tree already
	// being torn down.
	if a.providerAuth != nil {
		a.providerAuth.closeAll()
	}

	results := make(chan error, len(sessions))
	for _, ownedSession := range sessions {
		go func(target *session) {
			var sessionErr error

			defer func() {
				if recovered := recover(); recovered != nil {
					handleAgentGoroutinePanic(closeCtx, a.log, "Codex session close", nil, recovered)

					sessionErr = errors.New("codex session close panicked")
				}

				results <- sessionErr
			}()

			sessionErr = target.Close(closeCtx)
		}(ownedSession)
	}

	runtimeResult := make(chan error, 1)

	go func() {
		var runtimeErr error

		defer func() {
			if recovered := recover(); recovered != nil {
				handleAgentGoroutinePanic(closeCtx, a.log, "Codex runtime close", nil, recovered)

				runtimeErr = errors.New("codex runtime close panicked")
			}

			runtimeResult <- runtimeErr
		}()

		runtimeErr = a.closeSharedRuntime(closeCtx)
	}()

	// Closing the owned ACP transport is the cancellation path for stalled
	// writers and host requests. Every worker is still joined below; interrupt
	// never substitutes for joining it.
	if interrupter, ok := conn.(transportInterrupter); ok {
		_ = interrupter.InterruptTransport()
	}

	var err error

	for range sessions {
		err = errors.Join(err, <-results)
	}

	err = errors.Join(err, <-runtimeResult)

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
// the embedding host built. The wire classification is closed; the original
// error remains available to the embedding caller through optionsErr.
func (a *Agent) optionsError() error {
	if a.optionsErr == nil {
		return nil
	}

	wireErr := acp.NewInternalError(map[string]any{jsonFieldError: valueInternalFailure})
	if errors.Is(a.optionsErr, ErrHostAuthorityUnavailable) {
		return errors.Join(a.optionsErr, wireErr)
	}

	return wireErr
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
			jsonFieldUnstable: true,
			jsonFieldMethod:   ForkSessionMethod,
			"request":         "acp.UnstableForkSessionRequest JSON payload only",
			"response":        "acp.UnstableForkSessionResponse JSON payload only",
		},
		"steer": map[string]any{
			jsonFieldUnstable: true,
			jsonFieldMethod:   SteerTurnMethod,
			"request":         "acp.PromptRequest JSON payload with exact turn route",
		},
		"elicitation": map[string]any{
			jsonFieldUnstable: true,
			jsonFieldScope:    valueSession,
			"tracks":          "ACP v1 elicitation",
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

func (a *Agent) launchRuntimeClient(ctx context.Context, epoch uint64, scratchRoot string, nativeVersion string) (codex.Client, error) {
	env := a.staticRuntimeEnv()
	home := a.resolvedCodexHomeForEnv(env)

	factory := a.options.clientFactory
	if factory == nil {
		factory = func(ctx context.Context, options codex.Options) (codex.Client, error) {
			return codex.NewAppServerClient(ctx, options)
		}
	}

	configurationStarted := time.Now()

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
		Scratch:             scratchRoot,
		ScratchParent:       filepath.Dir(scratchRoot),
		NativeVersion:       nativeVersion,
		DefaultModel:        a.options.DefaultModel,
		Env:                 a.observe.InjectTraceEnv(ctx, env),
		ImplicitEnvironment: cloneStringMap(a.options.implicitEnvironment),
		HostAuthority:       adaptHostAuthority(a.options.HostAuthority),
		Config:              a.codexConfig(),
		ExtraArgs:           extraArgs,
		Logger:              a.log,
		EventHandler:        eventSink.Handle,
		RequestHandler: func(ctx context.Context, req codex.ServerRequest) (any, error) {
			return a.handleCodexServerRequestForEpoch(ctx, epoch, req)
		},
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

	if sinkErr := eventSink.SetClient(client); sinkErr != nil {
		closeCtx, cancelClose := context.WithTimeout(context.WithoutCancel(ctx), closeTimeout)
		closeErr := client.Close(closeCtx)

		cancelClose()

		return nil, errors.Join(sinkErr, closeErr)
	}

	if tokens, ok := a.externalAuthTokens(); ok {
		if err := client.LoginWithChatGPTTokens(ctx, toCodexAuthTokens(tokens)); err != nil {
			_ = client.Close(context.Background())

			return nil, err
		}
	}

	return client, nil
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
	case codex.EventAccountUpdated, codex.EventLoginCompleted, codex.EventError:
	default:
		return
	}

	s.mu.Lock()

	client := s.client
	if client == nil {
		if s.failure != nil {
			s.mu.Unlock()

			return
		}

		if len(s.pending) == startupClientEventLimit {
			s.pending = nil
			s.failure = fmt.Errorf("%w: startup client event router", codex.ErrTurnEventOverflow)
			s.mu.Unlock()

			return
		}

		s.pending = append(s.pending, event)
		s.mu.Unlock()

		return
	}
	s.mu.Unlock()

	s.agent.applyCodexClientEvent(context.Background(), client, event)
}

func (s *codexClientEventSink) SetClient(client codex.Client) error {
	s.mu.Lock()

	failure := s.failure
	if failure != nil {
		s.pending = nil
		s.mu.Unlock()

		return errors.Join(codex.ErrConnectionClosed, failure)
	}

	s.client = client
	pending := append([]codex.Event(nil), s.pending...)
	s.pending = nil
	s.mu.Unlock()

	for i := range pending {
		s.agent.applyCodexClientEvent(context.Background(), client, pending[i])
	}

	return nil
}

func (a *Agent) applyCodexClientEvent(ctx context.Context, client codex.Client, event codex.Event) {
	switch event.Kind {
	case codex.EventAccountUpdated:
		a.updateAccountForClient(client, event.ThreadID, event.Account)
	case codex.EventLoginCompleted:
		if a.providerAuth != nil {
			a.providerAuth.loginCompleted(ctx, event.Login)
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

func (a *Agent) acquireLifecycleCall(ctx context.Context) (func(), error) {
	if a.lifecycleCalls == nil {
		return func() {}, nil
	}

	select {
	case a.lifecycleCalls <- struct{}{}:
		return func() { <-a.lifecycleCalls }, nil
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

func (a *Agent) session(id acp.SessionId) (*session, error) {
	a.mu.Lock()
	defer a.mu.Unlock()

	if a.closed {
		return nil, newAgentClosedError()
	}

	// A committed tombstone answers before the active map is even consulted. The
	// agent keeps owning a wrapper whose teardown has not finished, so the map
	// can still hold one; the id is nonetheless wire-indistinguishable from one
	// that never existed, on this method and every other session-scoped request.
	if a.deleteCommittedLocked(id) {
		return nil, newUnknownSession()
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

	// Only a delete still in flight is answered here, and it is answered with a
	// retriable conflict because it may yet fail. A committed tombstone is left
	// to the deleted-id check further in, which also retries the native cleanup
	// a previous delete may have failed to finish.
	if a.deletePendingLocked(id) {
		a.mu.Unlock()

		return nil, newSessionDeleteInProgress()
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

	if session.lifecycleEstablishmentPending() {
		session.sessionOps.RUnlock()

		return nil, acp.NewInvalidRequest(map[string]any{
			jsonFieldError: "Codex session establishment response is still outstanding",
			jsonFieldLimit: limitSessionPrompt,
		})
	}

	return session.sessionOps.RUnlock, nil
}

func (a *Agent) validateSessionLifecycle(id acp.SessionId, session *session) error {
	a.mu.Lock()
	closed := a.closed
	current := a.sessions[id]
	deleting := a.deletePendingLocked(id)
	deleted := a.deleteCommittedLocked(id)

	session.mu.Lock()
	closing := session.closing
	session.mu.Unlock()
	a.mu.Unlock()

	switch {
	case closed:
		return newAgentClosedError()
	case closing:
		return newSessionCloseInProgress()
	case deleted:
		return newUnknownSession()

	// A delete that has not yet committed its tombstone may still fail, leaving
	// the id perfectly loadable, so it earns a retriable conflict rather than the
	// permanent unknown-session verdict a host would take as final.
	case deleting:
		return newSessionDeleteInProgress()
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

// deleteCommittedLocked reports that a delete of this id reached its durable
// tombstone. Only a committed tombstone proves the id is gone; a delete that is
// still running may still fail, in which case the id was never deleted at all.
// Agent.mu is held by the caller.
func (a *Agent) deleteCommittedLocked(id acp.SessionId) bool {
	_, ok := a.deleted[id]

	return ok
}

// deleteFencedLocked reports that an id is barred from coming back at all: a
// committed tombstone bars it forever, and a running delete bars it for the
// duration. Callers that answer a host distinguish the two with
// deleteCommittedLocked, because only the tombstone earns a terminal verdict.
// Agent.mu is held by the caller.
func (a *Agent) deleteFencedLocked(id acp.SessionId) bool {
	return a.deleteCommittedLocked(id) || a.deletePendingLocked(id)
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

// storeStartedSession decides whether a freshly started native thread may become
// reachable under its id. It only decides: a refusal leaves the candidate
// untouched for its caller to close, because the caller owns the wrapper until
// registration succeeds and every caller already closes on error. Closing here
// too would run one session's containment boundary twice over one native
// thread, and the second sweep — against a thread the first one already
// unsubscribed — can fail and escalate into the generation fence, retiring the
// shared app-server for the sake of a thread that was already contained.
// storeRetainedRuntimeSession refuses the same way, so both registration paths
// have one shape.
func (a *Agent) storeStartedSession(session *session) error {
	if a.negotiatedLifecycle().Present() {
		if err := session.attachNativeEvents(); err != nil {
			return err
		}
	}

	a.mu.Lock()

	switch {
	case a.closed:
		a.mu.Unlock()

		return newAgentClosedError()

	// The admission check at the head of load and resume happens before the
	// native resume it blocks in, so the fence is re-read here, where the
	// wrapper would actually become reachable. A resume that raced a delete
	// hands back a native thread nobody may address, so it is refused with the
	// verdict the fence has actually reached: a committed tombstone makes the id
	// permanently unknown, while a delete still in flight has decided nothing
	// yet and only conflicts.
	case a.deleteFencedLocked(session.id):
		committed := a.deleteCommittedLocked(session.id)
		a.mu.Unlock()

		if committed {
			return newUnknownSession()
		}

		return newSessionDeleteInProgress()
	}

	previous := a.sessions[session.id]
	if previous == nil && len(a.sessions) >= a.options.ConcurrencyLimits.MaxActiveSessions {
		a.mu.Unlock()

		return acp.NewInvalidRequest(map[string]any{jsonFieldError: valueBackpressure, jsonFieldLimit: "active_sessions"})
	}

	a.sessions[session.id] = session
	a.mu.Unlock()

	a.readmitProviderAuth(session.id)

	if previous != nil {
		if err := previous.Close(context.Background()); err != nil {
			// The replaced session's close is a whole boundary, and one that does
			// not complete still owes every rung behind the failure: the prefix a
			// settlement captured and could not place, the materialized rollout that
			// prefix is read back from, and the scratch reservation holding it.
			// Installing over it would drop all three with a wrapper nothing
			// references any more, so the id goes back to the session that still
			// owes them and the install is refused; the caller closes its candidate,
			// and the next load runs the boundary again.
			a.restoreReplacedSession(session, previous)

			return err
		}

		return nil
	}

	a.observe.AddActiveSession(context.Background(), 1)

	return nil
}

// restoreReplacedSession gives a replaced session its id back when its own close
// boundary did not complete. The agent goes on owning it, so Agent.Close still
// sweeps it and a later load still finds the state it holds.
func (a *Agent) restoreReplacedSession(candidate *session, previous *session) {
	a.mu.Lock()
	defer a.mu.Unlock()

	if a.sessions[candidate.id] == candidate {
		a.sessions[candidate.id] = previous
	}
}

// closeSessionProviderAuth cancels every pending provider-auth flow the
// addressed session owns: armed completers are disarmed and each record
// terminalizes as cancelled/session_closed. It is the shutdown ladder's fourth
// rung, and it runs identically on session close, session/delete, and
// Agent.Close — always before the native interrupt, so no flow is abandoned to
// a process that is already being torn down.
func (a *Agent) closeSessionProviderAuth(id acp.SessionId) {
	if a.providerAuth == nil {
		return
	}

	a.providerAuth.closeSession(id)
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

// newSessionDeleteInProgress refuses a lifecycle request that raced a delete
// which has not yet committed its tombstone. The refusal is a conflict rather
// than the unknown-session verdict because a delete can still fail, and a host
// told an id is unknown is entitled to treat that as permanent.
func newSessionDeleteInProgress() *acp.RequestError {
	return acp.NewInvalidRequest(map[string]any{jsonFieldError: "session delete lifecycle is already in progress"})
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

	// Close is a session-scoped request method, so a committed tombstone answers
	// it as unknown rather than handing back the wrapper the delete's own
	// teardown still owns.
	if a.deleteCommittedLocked(id) {
		return nil, newUnknownSession()
	}

	session := a.sessions[id]
	if session == nil {
		return nil, newUnknownSession()
	}

	session.mu.Lock()
	defer session.mu.Unlock()

	if session.closing {
		if session.closeContained && (session.closeCommitPending || session.closeRemovalPending) {
			session.closeCommitPending = false
			session.closeRemovalPending = false

			return session, nil
		}

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

// abortSessionClose gives an incomplete boundary its session back. The close
// swept the provider-auth flows before it began, so a session it then re-admits
// is answered by the lifecycle surface and refused as unknown by every auth leg
// until some later load happened to readmit it; the sweep is undone here with
// the admission that caused it. The flows the sweep cancelled stay cancelled —
// they belonged to a session the host asked to close, and the retry is what
// decides that session's fate, not the legs it had in flight.
func (a *Agent) abortSessionClose(id acp.SessionId, session *session) {
	if !a.clearSessionClosing(id, session) {
		return
	}

	a.readmitProviderAuth(id)
}

// clearSessionClosing reopens admission for a session the agent still holds, and
// reports whether it did. The broker's own lock is taken outside this one.
func (a *Agent) clearSessionClosing(id acp.SessionId, session *session) bool {
	a.mu.Lock()
	defer a.mu.Unlock()

	if a.sessions[id] != session {
		return false
	}

	session.mu.Lock()
	session.closing = false
	session.closeContained = false
	session.closeCommitPending = false
	session.closeCommitDone = false
	session.closeRemovalPending = false
	session.mu.Unlock()

	return true
}

// finishSessionCloseRetainingThread publishes native-thread ownership only
// after unsubscribe succeeded. Materialized rollout cleanup moves with that
// ownership so the canonical path remains valid until rebind or runtime end.
func (a *Agent) finishSessionCloseRetainingThread(id acp.SessionId, session *session) (bool, bool, error) {
	a.mu.Lock()
	defer a.mu.Unlock()

	if a.sessions[id] != session {
		return false, false, nil
	}

	session.mu.Lock()
	defer session.mu.Unlock()

	if a.runtimeClient != session.client || a.runtimeDead || session.clientDead || session.codexThreadID == "" {
		delete(a.sessions, id)

		return true, false, nil
	}

	if a.retainedThreads == nil {
		a.retainedThreads = make(map[acp.SessionId]*retainedRuntimeThread)
	}

	if a.retainedThreads[id] == nil && len(a.retainedThreads) == retainedRuntimeThreadLimit {
		return false, false, acp.NewInvalidRequest(map[string]any{
			jsonFieldError: "Codex retained thread registry is full",
			jsonFieldLimit: "retained_threads",
		})
	}

	delete(a.sessions, id)

	a.retainedThreads[id] = &retainedRuntimeThread{
		sessionID:           id,
		threadID:            session.codexThreadID,
		path:                session.rolloutPath,
		client:              session.client,
		epoch:               a.runtimeEpoch,
		materializedPath:    session.materializedPath,
		materializedRelease: session.materializedRelease,
		materializedBytes:   session.materializedBytes,
	}
	session.materializedPath = ""
	session.materializedRelease = nil
	session.materializedBytes = 0
	session.materializedEpoch = 0

	return true, true, nil
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
