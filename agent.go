package codexacp

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"slices"
	"sync"
	"time"

	"github.com/coder/acp-go-sdk"
	"github.com/savid/acp-go-codex/internal/codex"
	"github.com/savid/acp-go-codex/internal/observer"
)

const (
	listSessionsPageSize            = 50
	defaultMaxActiveSessions        = 32
	defaultMaxConcurrentPrompts     = 1
	defaultMaxConcurrentClientCalls = 16
	closeTimeout                    = 5 * time.Second

	jsonFieldError     = "error"
	jsonFieldMessage   = "message"
	jsonFieldCwd       = "cwd"
	jsonFieldEntries   = "entries"
	jsonFieldIndex     = "index"
	jsonFieldSessionID = "sessionId"
	validationRequired = "required"

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
	options    Options
	log        *slog.Logger
	observe    *observer.Observer
	optionsErr error

	mu          sync.Mutex
	closed      bool
	conn        agentClient
	sessions    map[acp.SessionId]*session
	deleted     map[acp.SessionId]struct{}
	clientCalls chan struct{}
	authTokens  *ChatGPTAuthTokens

	clientCapabilities acp.ClientCapabilities
	positionEncoding   acp.PositionEncodingKind
}

type codexClientEventSink struct {
	agent *Agent

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
	limits, optionsErr := normalizeConcurrencyLimits(options.ConcurrencyLimits)
	options.ConcurrencyLimits = limits

	log := options.Logger
	if log == nil {
		log = slog.Default()
	}

	return &Agent{
		options:    options,
		log:        log,
		optionsErr: optionsErr,
		observe: observer.New(observer.Config{
			MeterProvider:  options.MeterProvider,
			Propagator:     options.TextMapPropagator,
			TracerProvider: options.TracerProvider,
			Version:        options.AgentVersion,
		}),
		sessions:    make(map[acp.SessionId]*session),
		deleted:     make(map[acp.SessionId]struct{}),
		clientCalls: make(chan struct{}, limits.MaxConcurrentClientCalls),
	}
}

func (a *Agent) setAgentClient(conn agentClient) {
	a.mu.Lock()
	defer a.mu.Unlock()

	a.conn = conn
}

// Serve runs an ACP agent over the provided streams.
func Serve(ctx context.Context, input io.Reader, output io.Writer, opts ...Option) error {
	agent := NewAgent(opts...)
	defer func() {
		if err := agent.Close(); err != nil {
			agent.log.DebugContext(context.Background(), "close Codex ACP agent failed", slog.String("error", err.Error()))
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
	a.observe.AddActiveSession(context.Background(), -int64(len(sessions)))

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

// Initialize implements ACP initialize.
func (a *Agent) Initialize(_ context.Context, params acp.InitializeRequest) (acp.InitializeResponse, error) {
	if a.optionsErr != nil {
		return acp.InitializeResponse{}, acp.NewInvalidParams(map[string]any{jsonFieldError: a.optionsErr.Error()})
	}
	title := a.options.AgentTitle
	positionEncoding := selectPositionEncoding(params.ClientCapabilities.PositionEncodings)
	a.mu.Lock()
	a.clientCapabilities = cloneClientCapabilities(params.ClientCapabilities)
	a.positionEncoding = positionEncoding
	a.mu.Unlock()

	codexMeta := map[string]any{
		"fork": map[string]any{
			"unstable": true,
			"method":   ForkSessionMethod,
			"request":  "acp.UnstableForkSessionRequest JSON payload only",
			"response": "acp.UnstableForkSessionResponse JSON payload only",
		},
		"elicitation": map[string]any{
			"unstable": true,
			"scope":    "session",
			"tracks":   "in-progress ACP elicitation RFD",
		},
		rawEventCapabilityKey: map[string]any{
			"method":         RawEventMethod,
			"enabledBy":      rawEventEnabledByPath,
			"maxBytes":       rawEventMaxBytes,
			"defaultEnabled": false,
		},
		"sessionStore": map[string]any{
			"format": SessionStoreFormat,
			"key":    []string{"sessionId", "subpath"},
		},
		structuredOutputCapabilityKey: map[string]any{
			"config": "_meta.codex.options.outputSchema",
			"result": "_meta.codex.structuredOutput",
			"schema": "json_schema",
		},
	}

	return acp.InitializeResponse{
		ProtocolVersion: acp.ProtocolVersionNumber,
		AgentInfo: &acp.Implementation{
			Name:    a.options.AgentName,
			Title:   &title,
			Version: a.options.AgentVersion,
		},
		AuthMethods: a.authMethods(params),
		AgentCapabilities: acp.AgentCapabilities{
			Meta: map[string]any{
				codexMetaKey: codexMeta,
			},
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

func (a *Agent) newClient(ctx context.Context, mcpServers []acp.McpServer, envOverlay map[string]string, mcpToolApprovalMode string) (codex.Client, error) {
	factory := a.options.clientFactory
	if factory == nil {
		factory = func(ctx context.Context, options codex.Options) (codex.Client, error) {
			return codex.NewAppServerClient(ctx, options)
		}
	}

	extraArgs, mcpEnv, err := a.mcpServerConfigArgs(mcpServers, mcpToolApprovalMode)
	if err != nil {
		return nil, err
	}
	otelConfig, err := a.codexOTELConfig(envOverlay)
	if err != nil {
		return nil, err
	}
	extraArgs = append(append([]string(nil), otelConfig.ExtraArgs...), extraArgs...)

	a.observe.RecordCodexProcessStart(ctx)
	eventSink := &codexClientEventSink{agent: a}
	env := cloneStringMap(a.options.Env)
	if env == nil && (len(envOverlay) > 0 || len(mcpEnv) > 0) {
		env = map[string]string{}
	}
	for key, value := range envOverlay {
		env[key] = value
	}
	for key, value := range mcpEnv {
		env[key] = value
	}
	client, err := factory(ctx, codex.Options{
		CLIPath:      a.options.ExecutablePath,
		CodexHome:    a.options.Home,
		DefaultModel: a.options.DefaultModel,
		Env:          a.observe.InjectTraceEnv(ctx, env),
		Config:       a.codexConfig(),
		ExtraArgs:    extraArgs,
		Logger:       a.log,
		EventHandler: eventSink.Handle,
		RequestHandler: func(ctx context.Context, req codex.ServerRequest) (any, error) {
			return a.handleCodexServerRequest(ctx, req)
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

func (a *Agent) codexConfig() map[string]any {
	return nil
}

func (s *codexClientEventSink) Handle(_ context.Context, event codex.Event) {
	switch event.Kind {
	case codex.EventAccountUpdated:
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

	for _, event := range pending {
		s.agent.applyCodexClientEvent(context.Background(), client, event)
	}
}

func (a *Agent) applyCodexClientEvent(ctx context.Context, client codex.Client, event codex.Event) {
	if event.Kind == codex.EventAccountUpdated {
		a.updateAccountForClient(client, event.ThreadID, event.Account)
	}
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

	return nil
}

func (a *Agent) acquireClientCall(ctx context.Context) (func(), error) {
	if a.clientCalls == nil {
		return func() {}, nil
	}
	select {
	case a.clientCalls <- struct{}{}:
		return func() { <-a.clientCalls }, nil
	default:
		return nil, acp.NewInvalidRequest(map[string]any{jsonFieldError: "backpressure", "limit": "client_calls"})
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
		return nil, newResourceNotFound(map[string]any{jsonFieldSessionID: id})
	}

	return session, nil
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
	previous := a.sessions[session.id]
	if previous == nil && len(a.sessions) >= a.options.ConcurrencyLimits.MaxActiveSessions {
		a.mu.Unlock()
		if err := session.Close(context.Background()); err != nil {
			a.log.DebugContext(context.Background(), "close backpressured Codex session failed", slog.String(jsonFieldError, err.Error()))
		}
		return acp.NewInvalidRequest(map[string]any{jsonFieldError: "backpressure", "limit": "active_sessions"})
	}
	a.sessions[session.id] = session
	a.mu.Unlock()

	if previous != nil {
		if err := previous.Close(context.Background()); err != nil {
			a.log.WarnContext(context.Background(), "close replaced Codex session failed", slog.String(jsonFieldError, err.Error()))
		}
		return nil
	}

	a.observe.AddActiveSession(context.Background(), 1)

	return nil
}

func newAgentClosedError() *acp.RequestError {
	return acp.NewInternalError(map[string]any{jsonFieldError: "agent is closed"})
}

func (a *Agent) removeSession(id acp.SessionId) *session {
	a.mu.Lock()
	defer a.mu.Unlock()

	session := a.sessions[id]
	delete(a.sessions, id)

	return session
}

func (a *Agent) removeSessionIf(id acp.SessionId, session *session) bool {
	a.mu.Lock()
	defer a.mu.Unlock()

	if a.sessions[id] != session {
		return false
	}
	delete(a.sessions, id)

	return true
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

func (a *Agent) emitUpdate(ctx context.Context, sessionID acp.SessionId, update acp.SessionUpdate) error {
	conn := a.connection()
	if conn == nil {
		return nil
	}

	return conn.SessionUpdate(ctx, acp.SessionNotification{SessionId: sessionID, Update: update})
}

func (a *Agent) sessionStore() SessionStore {
	if a.options.SessionStore != nil {
		return a.options.SessionStore
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.options.SessionStore == nil {
		a.options.SessionStore = NewInMemorySessionStore()
	}

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
	if limits.MaxConcurrentPrompts == 0 {
		limits.MaxConcurrentPrompts = defaultMaxConcurrentPrompts
	}
	if limits.MaxConcurrentClientCalls == 0 {
		limits.MaxConcurrentClientCalls = defaultMaxConcurrentClientCalls
	}
	if limits.MaxActiveSessions < 0 {
		return limits, fmt.Errorf("ConcurrencyLimits.MaxActiveSessions must be non-negative")
	}
	if limits.MaxConcurrentPrompts < 0 {
		return limits, fmt.Errorf("ConcurrencyLimits.MaxConcurrentPrompts must be non-negative")
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
