package codexacp

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"slices"
	"sync"

	"github.com/coder/acp-go-sdk"
	"github.com/savid/acp-go-codex/internal/codex"
	"github.com/savid/acp-go-codex/internal/observer"
)

const (
	jsonFieldError = "error"

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
	options Options
	log     *slog.Logger
	observe *observer.Observer

	mu       sync.Mutex
	closed   bool
	conn     agentClient
	sessions map[acp.SessionId]*Session
	imports  map[string]*sessionImport

	importStore    *InMemorySessionStore
	mcpConnections map[acp.UnstableMcpConnectionId]*mcpBridgeConn
	authTokens     *codex.ChatGPTAuthTokens

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

	log := options.Logger
	if log == nil {
		log = slog.Default()
	}

	return &Agent{
		options: options,
		log:     log,
		observe: observer.New(observer.Config{
			MeterProvider:  options.MeterProvider,
			Propagator:     options.TextMapPropagator,
			TracerProvider: options.TracerProvider,
			Version:        options.AgentVersion,
		}),
		sessions:       make(map[acp.SessionId]*Session),
		imports:        make(map[string]*sessionImport),
		importStore:    NewInMemorySessionStore(),
		mcpConnections: make(map[acp.UnstableMcpConnectionId]*mcpBridgeConn),
	}
}

// SetAgentConnection installs the ACP connection used to send client
// notifications. The SDK does not call this automatically, so Serve wires it.
func (a *Agent) SetAgentConnection(conn *acp.AgentSideConnection) {
	a.setAgentClient(conn)
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
	sessions := make([]*Session, 0, len(a.sessions))
	for _, session := range a.sessions {
		sessions = append(sessions, session)
	}
	a.sessions = make(map[acp.SessionId]*Session)
	a.closed = true
	a.conn = nil
	a.mu.Unlock()

	var err error
	for _, session := range sessions {
		err = errors.Join(err, session.Close(context.Background()))
	}
	a.observe.AddActiveSession(context.Background(), -int64(len(sessions)))

	return err
}

func (a *Agent) setExternalAuthTokens(tokens codex.ChatGPTAuthTokens) {
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

func (a *Agent) externalAuthTokens() (codex.ChatGPTAuthTokens, bool) {
	a.mu.Lock()
	defer a.mu.Unlock()

	if a.authTokens == nil {
		return codex.ChatGPTAuthTokens{}, false
	}

	return *a.authTokens, true
}

// Initialize implements ACP initialize.
func (a *Agent) Initialize(_ context.Context, params acp.InitializeRequest) (acp.InitializeResponse, error) {
	title := a.options.AgentTitle
	positionEncoding := selectPositionEncoding(params.ClientCapabilities.PositionEncodings)
	a.mu.Lock()
	a.clientCapabilities = cloneClientCapabilities(params.ClientCapabilities)
	a.positionEncoding = positionEncoding
	a.mu.Unlock()

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
				codexMetaKey: map[string]any{
					"provider":           "codex",
					"preferredTransport": "app-server",
					rawSDKMessagesCapabilityKey: map[string]any{
						capabilityScopeKey:         capabilityScopeSession,
						rawSDKMessagesMethodKey:    rawCodexSDKMessageMethod,
						rawSDKMessagesEnabledByKey: rawSDKMessagesEnabledByPath,
					},
					outputSchemaCapabilityKey: map[string]any{
						capabilityScopeKey: "session",
						"types":            []string{"json_schema"},
						"config":           outputSchemaConfigPath,
						"result":           outputSchemaResultPath,
						"rawEvents":        rawCodexSDKMessageMethod,
					},
					"sessionImport": map[string]any{
						capabilityScopeKey: "session",
						"format":           codexSessionImportFormatJSON,
						"methods": map[string]string{
							"import":       codexSessionImportMethod,
							"importChunk":  codexSessionImportChunkMethod,
							"commitImport": codexSessionCommitImportMethod,
							"abortImport":  codexSessionAbortImportMethod,
						},
					},
				},
			},
			LoadSession: true,
			Auth:        a.authCapabilities(),
			McpCapabilities: acp.McpCapabilities{
				Acp:  true,
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
				Fork:                  &acp.SessionForkCapabilities{},
				List:                  &acp.SessionListCapabilities{},
				Resume:                &acp.SessionResumeCapabilities{},
			},
		},
	}, nil
}

func (a *Agent) newClient(ctx context.Context, mcpServers []acp.McpServer) (codex.Client, error) {
	factory := a.options.clientFactory
	if factory == nil {
		factory = func(ctx context.Context, options codex.Options) (codex.Client, error) {
			return codex.NewAppServerClient(ctx, options)
		}
	}

	extraArgs, err := a.mcpServerConfigArgs(mcpServers)
	if err != nil {
		return nil, err
	}

	a.observe.RecordCodexProcessStart(ctx)
	eventSink := &codexClientEventSink{agent: a}
	client, err := factory(ctx, codex.Options{
		CLIPath:      a.options.CodexPath,
		CodexHome:    a.options.CodexHome,
		DefaultModel: a.options.DefaultModel,
		Env:          a.observe.InjectTraceEnv(ctx, a.options.Env),
		ExtraArgs:    extraArgs,
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
		if err := client.LoginWithChatGPTTokens(ctx, tokens); err != nil {
			_ = client.Close(context.Background())
			return nil, err
		}
	}

	return client, nil
}

func (s *codexClientEventSink) Handle(_ context.Context, event codex.Event) {
	if event.Kind != codex.EventAccountUpdated {
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

	s.agent.updateAccountForClient(client, event.ThreadID, event.Account)
}

func (s *codexClientEventSink) SetClient(client codex.Client) {
	s.mu.Lock()
	s.client = client
	pending := append([]codex.Event(nil), s.pending...)
	s.pending = nil
	s.mu.Unlock()

	for _, event := range pending {
		s.agent.updateAccountForClient(client, event.ThreadID, event.Account)
	}
}

func (a *Agent) updateAccountForClient(client codex.Client, threadID string, account codex.Account) {
	meta := redactedAccountMeta(account)
	if len(meta) == 0 {
		return
	}
	a.mu.Lock()
	sessions := make([]*Session, 0, len(a.sessions))
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
		return acp.NewInvalidRequest(map[string]any{jsonFieldError: "agent is closed"})
	}

	return nil
}

func (a *Agent) session(id acp.SessionId) (*Session, error) {
	a.mu.Lock()
	defer a.mu.Unlock()

	session, ok := a.sessions[id]
	if !ok {
		return nil, acp.NewInvalidParams(map[string]any{"sessionId": id, jsonFieldError: "session not found"})
	}

	return session, nil
}

func (a *Agent) storeStartedSession(session *Session) error {
	a.mu.Lock()
	if a.closed {
		a.mu.Unlock()
		return acp.NewInvalidRequest(map[string]any{jsonFieldError: "agent is closed"})
	}
	_, existed := a.sessions[session.id]
	a.sessions[session.id] = session
	a.mu.Unlock()

	if !existed {
		a.observe.AddActiveSession(context.Background(), 1)
	}

	return nil
}

func (a *Agent) removeSession(id acp.SessionId) *Session {
	a.mu.Lock()
	defer a.mu.Unlock()

	session := a.sessions[id]
	delete(a.sessions, id)

	return session
}

func (a *Agent) connection() agentClient {
	a.mu.Lock()
	defer a.mu.Unlock()

	return a.conn
}

func (a *Agent) emitUpdate(ctx context.Context, sessionID acp.SessionId, update acp.SessionUpdate) error {
	conn := a.connection()
	if conn == nil {
		return nil
	}

	return conn.SessionUpdate(ctx, acp.SessionNotification{SessionId: sessionID, Update: update})
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
