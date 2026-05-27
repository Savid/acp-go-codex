package codexacp

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/coder/acp-go-sdk"
	"github.com/savid/acp-go-codex/internal/codex"
	"go.uber.org/goleak"
)

func TestInitializeAdvertisesCodexCapabilities(t *testing.T) {
	agent := NewAgent(
		WithAgentName("codex-test"),
		WithAgentTitle("Codex Test"),
		WithAgentVersion("v1.2.3"),
	)

	resp, err := agent.Initialize(context.Background(), acp.InitializeRequest{
		ClientCapabilities: acp.ClientCapabilities{
			PositionEncodings: []acp.PositionEncodingKind{acp.PositionEncodingKindUtf8},
		},
	})
	if err != nil {
		t.Fatalf("Initialize returned error: %v", err)
	}

	if resp.ProtocolVersion != acp.ProtocolVersionNumber {
		t.Fatalf("protocol version = %v, want %v", resp.ProtocolVersion, acp.ProtocolVersionNumber)
	}
	if resp.AgentInfo == nil || resp.AgentInfo.Name != "codex-test" || resp.AgentInfo.Version != "v1.2.3" {
		t.Fatalf("unexpected agent info: %#v", resp.AgentInfo)
	}
	if resp.AgentCapabilities.SessionCapabilities.Close == nil {
		t.Fatal("session close capability was not advertised")
	}
	if resp.AgentCapabilities.SessionCapabilities.List == nil {
		t.Fatal("session list capability was not advertised")
	}
	if resp.AgentCapabilities.PositionEncoding == nil || *resp.AgentCapabilities.PositionEncoding != acp.PositionEncodingKindUtf8 {
		t.Fatalf("position encoding = %v, want utf-8", resp.AgentCapabilities.PositionEncoding)
	}

	meta, ok := resp.AgentCapabilities.Meta[codexMetaKey].(map[string]any)
	if !ok {
		t.Fatalf("missing Codex meta: %#v", resp.AgentCapabilities.Meta)
	}
	if meta["provider"] != "codex" || meta["preferredTransport"] != "app-server" {
		t.Fatalf("unexpected Codex meta: %#v", meta)
	}
	if meta[rawSDKMessagesCapabilityKey] == nil {
		t.Fatalf("missing raw SDK message capability: %#v", meta)
	}
}

func TestPlaceholderSessionLifecycle(t *testing.T) {
	agent := newPlaceholderAgent(WithDefaultModel("gpt-5.5"))
	ctx := context.Background()

	newResp, err := agent.NewSession(ctx, acp.NewSessionRequest{
		Cwd:                   "/tmp/project",
		AdditionalDirectories: []string{"/tmp/other"},
		McpServers:            []acp.McpServer{},
	})
	if err != nil {
		t.Fatalf("NewSession returned error: %v", err)
	}
	if newResp.SessionId == "" {
		t.Fatal("NewSession returned empty session id")
	}

	messageID, err := newSessionID()
	if err != nil {
		t.Fatalf("newSessionID returned error: %v", err)
	}
	promptResp, err := agent.Prompt(ctx, acp.PromptRequest{
		SessionId: newResp.SessionId,
		MessageId: &messageID,
		Prompt:    []acp.ContentBlock{acp.TextBlock("hello")},
	})
	if err != nil {
		t.Fatalf("Prompt returned error: %v", err)
	}
	if promptResp.StopReason != acp.StopReasonEndTurn {
		t.Fatalf("stop reason = %v, want %v", promptResp.StopReason, acp.StopReasonEndTurn)
	}
	if promptResp.UserMessageId == nil || *promptResp.UserMessageId != messageID {
		t.Fatalf("user message id = %v, want %q", promptResp.UserMessageId, messageID)
	}

	listResp, err := agent.ListSessions(ctx, acp.ListSessionsRequest{})
	if err != nil {
		t.Fatalf("ListSessions returned error: %v", err)
	}
	if len(listResp.Sessions) != 1 {
		t.Fatalf("listed sessions = %d, want 1", len(listResp.Sessions))
	}
	if listResp.Sessions[0].SessionId != newResp.SessionId {
		t.Fatalf("listed session id = %q, want %q", listResp.Sessions[0].SessionId, newResp.SessionId)
	}

	if _, closeErr := agent.CloseSession(ctx, acp.CloseSessionRequest{SessionId: newResp.SessionId}); closeErr != nil {
		t.Fatalf("CloseSession returned error: %v", closeErr)
	}

	listResp, err = agent.ListSessions(ctx, acp.ListSessionsRequest{})
	if err != nil {
		t.Fatalf("ListSessions after close returned error: %v", err)
	}
	if len(listResp.Sessions) != 0 {
		t.Fatalf("listed sessions after close = %d, want 0", len(listResp.Sessions))
	}
}

func TestACPConnectionStreamsPlaceholderUpdates(t *testing.T) {
	ctx := context.Background()
	c2aR, c2aW := io.Pipe()
	a2cR, a2cW := io.Pipe()
	t.Cleanup(func() {
		_ = c2aR.Close()
		_ = c2aW.Close()
		_ = a2cR.Close()
		_ = a2cW.Close()
	})

	client := &recordingClient{}
	clientConn := acp.NewClientSideConnection(client, c2aW, a2cR)

	agent := newPlaceholderAgent()
	agentConn := acp.NewAgentSideConnection(agent, a2cW, c2aR)
	agent.SetAgentConnection(agentConn)

	if _, err := clientConn.Initialize(ctx, acp.InitializeRequest{}); err != nil {
		t.Fatalf("Initialize returned error: %v", err)
	}

	newResp, err := clientConn.NewSession(ctx, acp.NewSessionRequest{
		Cwd:        "/tmp/project",
		McpServers: []acp.McpServer{},
	})
	if err != nil {
		t.Fatalf("NewSession returned error: %v", err)
	}

	promptResp, err := clientConn.Prompt(ctx, acp.PromptRequest{
		SessionId: newResp.SessionId,
		Prompt:    []acp.ContentBlock{acp.TextBlock("hello")},
	})
	if err != nil {
		t.Fatalf("Prompt returned error: %v", err)
	}
	if promptResp.StopReason != acp.StopReasonEndTurn {
		t.Fatalf("stop reason = %v, want %v", promptResp.StopReason, acp.StopReasonEndTurn)
	}

	updates := client.Updates()
	if len(updates) != 3 {
		t.Fatalf("streamed updates = %d, want 3", len(updates))
	}
	if updates[0].Update.Plan == nil {
		t.Fatalf("first update = %#v, want plan", updates[0].Update)
	}
	if updates[1].Update.AgentThoughtChunk == nil {
		t.Fatalf("second update = %#v, want thought", updates[1].Update)
	}
	if updates[2].Update.AgentMessageChunk == nil {
		t.Fatalf("third update = %#v, want agent message", updates[2].Update)
	}
}

func TestServeLocalConnectionStreamsPlaceholderUpdates(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	c2aR, c2aW := io.Pipe()
	a2cR, a2cW := io.Pipe()
	t.Cleanup(func() {
		_ = c2aR.Close()
		_ = c2aW.Close()
		_ = a2cR.Close()
		_ = a2cW.Close()
	})

	errCh := make(chan error, 1)
	go func() {
		errCh <- Serve(ctx, c2aR, a2cW, withClientFactory(func(_ context.Context, options codex.Options) (codex.Client, error) {
			return codex.NewPlaceholderClient(options), nil
		}))
	}()

	client := &recordingClient{}
	clientConn := acp.NewClientSideConnection(client, c2aW, a2cR)
	if _, err := clientConn.Initialize(ctx, acp.InitializeRequest{}); err != nil {
		t.Fatalf("Initialize returned error: %v", err)
	}
	newResp, err := clientConn.NewSession(ctx, acp.NewSessionRequest{Cwd: "/tmp/project", McpServers: []acp.McpServer{}})
	if err != nil {
		t.Fatalf("NewSession returned error: %v", err)
	}
	if _, err := clientConn.Prompt(ctx, TextPromptRequest(newResp.SessionId, "hello")); err != nil {
		t.Fatalf("Prompt returned error: %v", err)
	}
	if _, err := clientConn.CloseSession(ctx, acp.CloseSessionRequest{SessionId: newResp.SessionId}); err != nil {
		t.Fatalf("CloseSession returned error: %v", err)
	}
	cancel()
	_ = c2aW.Close()
	_ = a2cR.Close()
	select {
	case err := <-errCh:
		if err != nil && err != context.Canceled {
			t.Fatalf("Serve returned error: %v", err)
		}
	case <-ctx.Done():
	}
	if len(client.Updates()) == 0 {
		t.Fatal("expected streamed updates")
	}
}

func newPlaceholderAgent(opts ...Option) *Agent {
	all := append([]Option{
		withClientFactory(func(_ context.Context, options codex.Options) (codex.Client, error) {
			return codex.NewPlaceholderClient(options), nil
		}),
	}, opts...)

	return NewAgent(all...)
}

type recordingClient struct {
	mu      sync.Mutex
	updates []acp.SessionNotification
}

func (c *recordingClient) Updates() []acp.SessionNotification {
	c.mu.Lock()
	defer c.mu.Unlock()

	return append([]acp.SessionNotification(nil), c.updates...)
}

func (c *recordingClient) SessionUpdate(_ context.Context, notification acp.SessionNotification) error {
	c.mu.Lock()
	defer c.mu.Unlock()

	c.updates = append(c.updates, notification)

	return nil
}

func (*recordingClient) RequestPermission(context.Context, acp.RequestPermissionRequest) (acp.RequestPermissionResponse, error) {
	return acp.RequestPermissionResponse{Outcome: acp.NewRequestPermissionOutcomeCancelled()}, nil
}

func (*recordingClient) ReadTextFile(context.Context, acp.ReadTextFileRequest) (acp.ReadTextFileResponse, error) {
	return acp.ReadTextFileResponse{}, acp.NewMethodNotFound(acp.ClientMethodFsReadTextFile)
}

func (*recordingClient) WriteTextFile(context.Context, acp.WriteTextFileRequest) (acp.WriteTextFileResponse, error) {
	return acp.WriteTextFileResponse{}, acp.NewMethodNotFound(acp.ClientMethodFsWriteTextFile)
}

func (*recordingClient) CreateTerminal(context.Context, acp.CreateTerminalRequest) (acp.CreateTerminalResponse, error) {
	return acp.CreateTerminalResponse{}, acp.NewMethodNotFound(acp.ClientMethodTerminalCreate)
}

func (*recordingClient) KillTerminal(context.Context, acp.KillTerminalRequest) (acp.KillTerminalResponse, error) {
	return acp.KillTerminalResponse{}, acp.NewMethodNotFound(acp.ClientMethodTerminalKill)
}

func (*recordingClient) TerminalOutput(context.Context, acp.TerminalOutputRequest) (acp.TerminalOutputResponse, error) {
	return acp.TerminalOutputResponse{}, acp.NewMethodNotFound(acp.ClientMethodTerminalOutput)
}

func (*recordingClient) ReleaseTerminal(context.Context, acp.ReleaseTerminalRequest) (acp.ReleaseTerminalResponse, error) {
	return acp.ReleaseTerminalResponse{}, acp.NewMethodNotFound(acp.ClientMethodTerminalRelease)
}

func (*recordingClient) WaitForTerminalExit(context.Context, acp.WaitForTerminalExitRequest) (acp.WaitForTerminalExitResponse, error) {
	return acp.WaitForTerminalExitResponse{}, acp.NewMethodNotFound(acp.ClientMethodTerminalWaitForExit)
}

func (a *Agent) sessionMust(id acp.SessionId) *Session {
	session, err := a.session(id)
	if err != nil {
		panic(err)
	}
	return session
}

func quoteJSON(value string) string {
	raw, _ := json.Marshal(value)
	return string(raw)
}

func TestCodexClientEventSinkUpdatesMatchingSessions(t *testing.T) {
	agent := NewAgent()
	client := newSpyCodexClient()
	otherClient := newSpyCodexClient()
	matching := newSession(agent, "matching", "/tmp/project", nil, codex.Thread{ID: "thread-1"}, client, sessionMeta{})
	sameClientOtherThread := newSession(agent, "other-thread", "/tmp/project", nil, codex.Thread{ID: "thread-2"}, client, sessionMeta{})
	other := newSession(agent, "other-client", "/tmp/project", nil, codex.Thread{ID: "thread-1"}, otherClient, sessionMeta{})
	if err := agent.storeStartedSession(matching); err != nil {
		t.Fatalf("store matching session: %v", err)
	}
	if err := agent.storeStartedSession(sameClientOtherThread); err != nil {
		t.Fatalf("store other-thread session: %v", err)
	}
	if err := agent.storeStartedSession(other); err != nil {
		t.Fatalf("store other-client session: %v", err)
	}

	sink := &codexClientEventSink{agent: agent}
	sink.Handle(context.Background(), codex.Event{Kind: codex.EventRaw})
	sink.Handle(context.Background(), codex.Event{
		Kind:     codex.EventAccountUpdated,
		ThreadID: "thread-1",
		Account:  codex.Account{ID: "acct", Email: "user@example.com", PlanType: "plus"},
	})
	sink.SetClient(client)
	if matching.accountMeta["id"] != "acct" {
		t.Fatalf("matching account meta = %#v", matching.accountMeta)
	}
	if len(sameClientOtherThread.accountMeta) != 0 || len(other.accountMeta) != 0 {
		t.Fatalf("non-matching sessions updated: other-thread=%#v other=%#v", sameClientOtherThread.accountMeta, other.accountMeta)
	}

	sink.Handle(context.Background(), codex.Event{
		Kind:     codex.EventAccountUpdated,
		ThreadID: "thread-2",
		Account:  codex.Account{ID: "acct-2"},
	})
	if sameClientOtherThread.accountMeta["id"] != "acct-2" {
		t.Fatalf("direct account update = %#v", sameClientOtherThread.accountMeta)
	}
	agent.updateAccountForClient(client, "", codex.Account{})
}

type spyCodexClient struct {
	mu sync.Mutex

	thread codex.Thread
	closed bool

	start     codex.ThreadStartRequest
	resume    codex.ThreadResumeRequest
	lastTurn  codex.TurnStartRequest
	steer     codex.TurnSteerRequest
	compact   codex.ThreadCompactRequest
	review    codex.ReviewStartRequest
	turns     codex.ThreadTurnsListRequest
	loggedOut bool
	login     codex.ChatGPTAuthTokens
}

func newSpyCodexClient() *spyCodexClient {
	return &spyCodexClient{
		thread: codex.Thread{
			ID:        "thread-1",
			SessionID: "thread-1",
			Cwd:       "/tmp/project",
			Model:     "gpt-initial",
			Provider:  "openai",
			Title:     "Thread",
			UpdatedAt: time.Unix(1, 0).UTC().Format(time.RFC3339),
		},
	}
}

func (c *spyCodexClient) StartThread(ctx context.Context, req codex.ThreadStartRequest) (codex.Thread, error) {
	if err := ctx.Err(); err != nil {
		return codex.Thread{}, err
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	c.start = req
	thread := c.thread
	thread.Cwd = req.Cwd
	thread.Model = firstNonEmpty(req.Model, thread.Model)
	return thread, nil
}

func (c *spyCodexClient) ResumeThread(ctx context.Context, req codex.ThreadResumeRequest) (codex.Thread, error) {
	if err := ctx.Err(); err != nil {
		return codex.Thread{}, err
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	c.resume = req
	thread := c.thread
	thread.ID = firstNonEmpty(req.ThreadID, thread.ID)
	thread.Cwd = req.Cwd
	return thread, nil
}

func (c *spyCodexClient) ForkThread(ctx context.Context, req codex.ThreadForkRequest) (codex.Thread, error) {
	if err := ctx.Err(); err != nil {
		return codex.Thread{}, err
	}
	thread := c.thread
	thread.ID = "fork-thread"
	thread.Cwd = req.Cwd
	return thread, nil
}

func (c *spyCodexClient) ListThreads(context.Context, codex.ThreadListRequest) ([]codex.Thread, error) {
	return []codex.Thread{c.thread}, nil
}

func (c *spyCodexClient) ReadThread(context.Context, codex.ThreadReadRequest) (codex.ThreadHistory, error) {
	return codex.ThreadHistory{Thread: c.thread, Items: []map[string]any{{"type": "agentMessage", "text": "history"}}}, nil
}

func (c *spyCodexClient) ListTurns(_ context.Context, req codex.ThreadTurnsListRequest) (codex.ThreadTurnsListResponse, error) {
	c.mu.Lock()
	c.turns = req
	c.mu.Unlock()
	return codex.ThreadTurnsListResponse{Turns: []map[string]any{{"id": "turn-1"}}}, nil
}

func (c *spyCodexClient) RunTurn(ctx context.Context, req codex.TurnStartRequest) (<-chan codex.Event, error) {
	c.mu.Lock()
	c.lastTurn = req
	c.mu.Unlock()
	out := make(chan codex.Event, 2)
	go func() {
		defer close(out)
		out <- codex.Event{Kind: codex.EventAgentMessageDelta, ThreadID: req.ThreadID, TurnID: "turn-1", Text: `{"ok":true}`}
		out <- codex.Event{Kind: codex.EventCompleted, ThreadID: req.ThreadID, TurnID: "turn-1", StopReason: codex.StopReasonEndTurn, Usage: codex.Usage{InputTokens: 1, OutputTokens: 2}}
	}()
	return out, nil
}

func (c *spyCodexClient) SteerTurn(_ context.Context, req codex.TurnSteerRequest) error {
	c.mu.Lock()
	c.steer = req
	c.mu.Unlock()
	return nil
}

func (c *spyCodexClient) CancelTurn(context.Context, string, string) error { return nil }

func (c *spyCodexClient) CompactThread(_ context.Context, req codex.ThreadCompactRequest) (map[string]any, error) {
	c.mu.Lock()
	c.compact = req
	c.mu.Unlock()
	return map[string]any{"status": "ok"}, nil
}

func (c *spyCodexClient) StartReview(_ context.Context, req codex.ReviewStartRequest) (map[string]any, error) {
	c.mu.Lock()
	c.review = req
	c.mu.Unlock()
	return map[string]any{"status": "reviewing"}, nil
}

func (c *spyCodexClient) CollaborationModeList(context.Context) (codex.CollaborationModeListResponse, error) {
	return codex.CollaborationModeListResponse{Modes: []codex.CollaborationMode{{ID: "default"}, {ID: "plan"}}}, nil
}

func (c *spyCodexClient) MCPServerStatusList(context.Context) (codex.MCPServerStatusListResponse, error) {
	return codex.MCPServerStatusListResponse{Servers: []codex.MCPServerStatus{{Name: "mcp", Status: "ready"}}}, nil
}

func (c *spyCodexClient) UnsubscribeThread(context.Context, string) error { return nil }

func (c *spyCodexClient) ModelList(context.Context) ([]codex.Model, error) {
	return []codex.Model{{ID: "gpt-initial", Name: "GPT Initial"}, {ID: "gpt-other", Name: "GPT Other"}}, nil
}

func (c *spyCodexClient) AccountRead(context.Context) (codex.Account, error) {
	return codex.Account{ID: "acct", Email: "user@example.com", PlanType: "plus", Raw: map[string]any{"accessToken": "secret"}}, nil
}

func (c *spyCodexClient) LoginWithChatGPTTokens(_ context.Context, tokens codex.ChatGPTAuthTokens) error {
	c.login = tokens
	return nil
}

func (c *spyCodexClient) Logout(context.Context) error {
	c.loggedOut = true
	return nil
}

func (c *spyCodexClient) Close(context.Context) error {
	c.closed = true
	return nil
}

type recordingAgentClient struct {
	done chan struct{}

	updates     []acp.SessionNotification
	extensions  []extensionNotification
	permission  acp.PermissionOptionId
	elicitation acp.UnstableCreateElicitationResponse
}

type extensionNotification struct {
	method string
	params any
}

func newRecordingAgentClient() *recordingAgentClient {
	return &recordingAgentClient{done: make(chan struct{}), permission: permissionAccept}
}

func (c *recordingAgentClient) Done() <-chan struct{} { return c.done }

func (c *recordingAgentClient) UnstableCreateElicitation(context.Context, acp.UnstableCreateElicitationRequest) (acp.UnstableCreateElicitationResponse, error) {
	if c.elicitation.Accept != nil || c.elicitation.Decline != nil || c.elicitation.Cancel != nil {
		return c.elicitation, nil
	}
	return acp.NewUnstableCreateElicitationResponseCancel(), nil
}

func (c *recordingAgentClient) UnstableConnectMcp(context.Context, acp.UnstableConnectMcpRequest) (acp.UnstableConnectMcpResponse, error) {
	return acp.UnstableConnectMcpResponse{ConnectionId: "mcp-1"}, nil
}

func (c *recordingAgentClient) UnstableDisconnectMcp(context.Context, acp.UnstableDisconnectMcpRequest) (acp.UnstableDisconnectMcpResponse, error) {
	return acp.UnstableDisconnectMcpResponse{}, nil
}

func (c *recordingAgentClient) RequestPermission(context.Context, acp.RequestPermissionRequest) (acp.RequestPermissionResponse, error) {
	return acp.RequestPermissionResponse{Outcome: acp.NewRequestPermissionOutcomeSelected(c.permission)}, nil
}

func (c *recordingAgentClient) SessionUpdate(_ context.Context, notification acp.SessionNotification) error {
	c.updates = append(c.updates, notification)
	return nil
}

func (c *recordingAgentClient) NotifyExtension(_ context.Context, method string, params any) error {
	c.extensions = append(c.extensions, extensionNotification{method: method, params: params})
	return nil
}

func TestAgentServeAndNewClientEdges(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := Serve(ctx, strings.NewReader(""), io.Discard); err == nil {
		t.Fatal("Serve with canceled context succeeded")
	}
	if err := Serve(context.Background(), strings.NewReader(""), io.Discard); err != nil {
		t.Fatalf("Serve EOF returned error: %v", err)
	}
	serveCtx, serveCancel := context.WithCancel(context.Background())
	c2aR, c2aW := io.Pipe()
	a2cR, a2cW := io.Pipe()
	errCh := make(chan error, 1)
	go func() {
		errCh <- Serve(serveCtx, c2aR, a2cW, withClientFactory(func(context.Context, codex.Options) (codex.Client, error) {
			return &errorCodexClient{spyCodexClient: newSpyCodexClient(), closeErr: errors.New("close failed")}, nil
		}))
	}()
	clientConn := acp.NewClientSideConnection(&recordingClient{}, c2aW, a2cR)
	if _, err := clientConn.Initialize(context.Background(), acp.InitializeRequest{}); err != nil {
		t.Fatalf("serve close-error initialize: %v", err)
	}
	if _, err := clientConn.NewSession(context.Background(), acp.NewSessionRequest{Cwd: "/tmp/project", McpServers: []acp.McpServer{}}); err != nil {
		t.Fatalf("serve close-error session: %v", err)
	}
	serveCancel()
	_ = c2aW.Close()
	_ = a2cR.Close()
	if err := <-errCh; err != nil && !errors.Is(err, context.Canceled) {
		t.Fatalf("Serve close-error returned %v", err)
	}
	_ = c2aR.Close()
	_ = a2cW.Close()

	agent := NewAgent()
	agent.options.clientFactory = nil
	agent.options.CodexPath = filepath.Join(t.TempDir(), "missing-codex")
	if _, err := agent.newClient(context.Background(), nil); err == nil {
		t.Fatal("newClient with nil factory and no Codex CLI succeeded")
	}
	var gotOptions codex.Options
	requestAgent := NewAgent(withClientFactory(func(_ context.Context, options codex.Options) (codex.Client, error) {
		gotOptions = options
		return newSpyCodexClient(), nil
	}))
	if _, err := requestAgent.newClient(context.Background(), nil); err != nil {
		t.Fatalf("newClient for request handler returned error: %v", err)
	}
	if _, err := gotOptions.RequestHandler(context.Background(), codex.ServerRequest{Method: "missing"}); err == nil {
		t.Fatal("Codex request handler accepted missing method")
	}
}

func rawPtr(raw json.RawMessage) *json.RawMessage { return &raw }

type errorWriter struct{}

func (errorWriter) Write([]byte) (int, error) { return 0, errors.New("write failed") }

func canceledContext() context.Context {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	return ctx
}

type stringer string

func (s stringer) String() string { return string(s) }

type errorCodexClient struct {
	*spyCodexClient

	startErr         error
	resumeErr        error
	forkErr          error
	closeErr         error
	modelErr         error
	accountErr       error
	listErr          error
	steerErr         error
	compactErr       error
	reviewErr        error
	readErr          error
	turnsErr         error
	collaborationErr error
	mcpStatusErr     error
	loginErr         error
	logoutErr        error
	listThreads      []codex.Thread
}

type closingAccountClient struct {
	*spyCodexClient
	agent *Agent
}

func (c *closingAccountClient) AccountRead(context.Context) (codex.Account, error) {
	_ = c.agent.Close()
	return codex.Account{ID: "closed"}, nil
}

func (c *errorCodexClient) StartThread(ctx context.Context, req codex.ThreadStartRequest) (codex.Thread, error) {
	if c.startErr != nil {
		return codex.Thread{}, c.startErr
	}
	return c.spyCodexClient.StartThread(ctx, req)
}

func (c *errorCodexClient) ResumeThread(ctx context.Context, req codex.ThreadResumeRequest) (codex.Thread, error) {
	if c.resumeErr != nil {
		return codex.Thread{}, c.resumeErr
	}
	return c.spyCodexClient.ResumeThread(ctx, req)
}

func (c *errorCodexClient) ForkThread(ctx context.Context, req codex.ThreadForkRequest) (codex.Thread, error) {
	if c.forkErr != nil {
		return codex.Thread{}, c.forkErr
	}
	return c.spyCodexClient.ForkThread(ctx, req)
}

func (c *errorCodexClient) ListThreads(ctx context.Context, req codex.ThreadListRequest) ([]codex.Thread, error) {
	if c.listErr != nil {
		return nil, c.listErr
	}
	if c.listThreads != nil {
		return append([]codex.Thread(nil), c.listThreads...), nil
	}
	return c.spyCodexClient.ListThreads(ctx, req)
}

func (c *errorCodexClient) ReadThread(ctx context.Context, req codex.ThreadReadRequest) (codex.ThreadHistory, error) {
	if c.readErr != nil {
		return codex.ThreadHistory{}, c.readErr
	}
	return c.spyCodexClient.ReadThread(ctx, req)
}

func (c *errorCodexClient) ListTurns(ctx context.Context, req codex.ThreadTurnsListRequest) (codex.ThreadTurnsListResponse, error) {
	if c.turnsErr != nil {
		return codex.ThreadTurnsListResponse{}, c.turnsErr
	}
	return c.spyCodexClient.ListTurns(ctx, req)
}

func (c *errorCodexClient) SteerTurn(ctx context.Context, req codex.TurnSteerRequest) error {
	if c.steerErr != nil {
		return c.steerErr
	}
	return c.spyCodexClient.SteerTurn(ctx, req)
}

func (c *errorCodexClient) CompactThread(ctx context.Context, req codex.ThreadCompactRequest) (map[string]any, error) {
	if c.compactErr != nil {
		return nil, c.compactErr
	}
	return c.spyCodexClient.CompactThread(ctx, req)
}

func (c *errorCodexClient) StartReview(ctx context.Context, req codex.ReviewStartRequest) (map[string]any, error) {
	if c.reviewErr != nil {
		return nil, c.reviewErr
	}
	return c.spyCodexClient.StartReview(ctx, req)
}

func (c *errorCodexClient) CollaborationModeList(ctx context.Context) (codex.CollaborationModeListResponse, error) {
	if c.collaborationErr != nil {
		return codex.CollaborationModeListResponse{}, c.collaborationErr
	}
	return c.spyCodexClient.CollaborationModeList(ctx)
}

func (c *errorCodexClient) MCPServerStatusList(ctx context.Context) (codex.MCPServerStatusListResponse, error) {
	if c.mcpStatusErr != nil {
		return codex.MCPServerStatusListResponse{}, c.mcpStatusErr
	}
	return c.spyCodexClient.MCPServerStatusList(ctx)
}

func (c *errorCodexClient) ModelList(ctx context.Context) ([]codex.Model, error) {
	if c.modelErr != nil {
		return nil, c.modelErr
	}
	return c.spyCodexClient.ModelList(ctx)
}

func (c *errorCodexClient) AccountRead(ctx context.Context) (codex.Account, error) {
	if c.accountErr != nil {
		return codex.Account{}, c.accountErr
	}
	return c.spyCodexClient.AccountRead(ctx)
}

func (c *errorCodexClient) LoginWithChatGPTTokens(ctx context.Context, tokens codex.ChatGPTAuthTokens) error {
	if c.loginErr != nil {
		return c.loginErr
	}
	return c.spyCodexClient.LoginWithChatGPTTokens(ctx, tokens)
}

func (c *errorCodexClient) Logout(ctx context.Context) error {
	if c.logoutErr != nil {
		return c.logoutErr
	}
	return c.spyCodexClient.Logout(ctx)
}

func (c *errorCodexClient) Close(ctx context.Context) error {
	if c.closeErr != nil {
		return c.closeErr
	}
	return c.spyCodexClient.Close(ctx)
}

type errorAgentClient struct {
	*recordingAgentClient
	updateErr error
}

func (c *errorAgentClient) SessionUpdate(ctx context.Context, notification acp.SessionNotification) error {
	if c.updateErr != nil {
		return c.updateErr
	}
	return c.recordingAgentClient.SessionUpdate(ctx, notification)
}

type serverRequestErrorClient struct {
	*recordingAgentClient
	permissionErr  error
	elicitationErr error
}

func (c *serverRequestErrorClient) RequestPermission(context.Context, acp.RequestPermissionRequest) (acp.RequestPermissionResponse, error) {
	return acp.RequestPermissionResponse{}, c.permissionErr
}

func (c *serverRequestErrorClient) UnstableCreateElicitation(context.Context, acp.UnstableCreateElicitationRequest) (acp.UnstableCreateElicitationResponse, error) {
	return acp.UnstableCreateElicitationResponse{}, c.elicitationErr
}

type blockingPermissionAgentClient struct {
	*recordingAgentClient
	started chan struct{}
	release chan struct{}
}

func (c *blockingPermissionAgentClient) RequestPermission(context.Context, acp.RequestPermissionRequest) (acp.RequestPermissionResponse, error) {
	close(c.started)
	<-c.release
	return acp.RequestPermissionResponse{Outcome: acp.NewRequestPermissionOutcomeSelected(permissionAccept)}, nil
}

type connectErrorMCPAgentClient struct {
	*recordingMCPAgentClient
}

func (c *connectErrorMCPAgentClient) UnstableConnectMcp(context.Context, acp.UnstableConnectMcpRequest) (acp.UnstableConnectMcpResponse, error) {
	return acp.UnstableConnectMcpResponse{}, errors.New("connect failed")
}

type readErrorClient struct {
	codex.Client
}

func (c readErrorClient) ReadThread(context.Context, codex.ThreadReadRequest) (codex.ThreadHistory, error) {
	return codex.ThreadHistory{}, errors.New("read failed")
}

type errorSessionStore struct {
	loadErr error
	listErr error
}

func (s errorSessionStore) Append(context.Context, SessionKey, []SessionStoreEntry) error {
	return nil
}

func (s errorSessionStore) Load(context.Context, SessionKey) ([]SessionStoreEntry, error) {
	if s.loadErr != nil {
		return nil, s.loadErr
	}
	return nil, nil
}

func (s errorSessionStore) ListSessions(context.Context, string) ([]SessionSummary, error) {
	if s.listErr != nil {
		return nil, s.listErr
	}
	return nil, nil
}

func bufioReadLine(conn net.Conn) (string, error) {
	buf := make([]byte, 0, 128)
	tmp := make([]byte, 1)
	for {
		n, err := conn.Read(tmp)
		if n > 0 {
			buf = append(buf, tmp[0])
			if tmp[0] == '\n' {
				return string(buf), nil
			}
		}
		if err != nil {
			return string(buf), err
		}
	}
}

func TestMain(m *testing.M) {
	goleak.VerifyTestMain(m)
}

type noListStore struct{}

func (noListStore) Append(context.Context, SessionKey, []SessionStoreEntry) error { return nil }
func (noListStore) Load(context.Context, SessionKey) ([]SessionStoreEntry, error) { return nil, nil }

type appendErrorStore struct{}

func (appendErrorStore) Append(context.Context, SessionKey, []SessionStoreEntry) error {
	return errors.New("append failed")
}
func (appendErrorStore) Load(context.Context, SessionKey) ([]SessionStoreEntry, error) {
	return nil, nil
}

type loadErrorStore struct{}

func (loadErrorStore) Append(context.Context, SessionKey, []SessionStoreEntry) error { return nil }
func (loadErrorStore) Load(context.Context, SessionKey) ([]SessionStoreEntry, error) {
	return nil, errors.New("load failed")
}

type existingNoReplaceStore struct{}

func (existingNoReplaceStore) Append(context.Context, SessionKey, []SessionStoreEntry) error {
	return nil
}
func (existingNoReplaceStore) Load(context.Context, SessionKey) ([]SessionStoreEntry, error) {
	return []SessionStoreEntry{SessionStoreEntry(`{"type":"old"}`)}, nil
}

type replaceErrorStore struct{}

func (replaceErrorStore) Append(context.Context, SessionKey, []SessionStoreEntry) error { return nil }
func (replaceErrorStore) Load(context.Context, SessionKey) ([]SessionStoreEntry, error) {
	return []SessionStoreEntry{SessionStoreEntry(`{"type":"old"}`)}, nil
}
func (replaceErrorStore) ReplaceSession(context.Context, SessionKey, []SessionStoreReplacement) error {
	return errors.New("replace failed")
}

type extensionErrorClient struct {
	*recordingAgentClient
}

func (c *extensionErrorClient) NotifyExtension(context.Context, string, any) error {
	return errors.New("extension failed")
}

type nilPermissionClient struct {
	*recordingAgentClient
}

func (c *nilPermissionClient) RequestPermission(context.Context, acp.RequestPermissionRequest) (acp.RequestPermissionResponse, error) {
	return acp.RequestPermissionResponse{}, nil
}

type acceptElicitationClient struct {
	*recordingAgentClient
}

func (c *acceptElicitationClient) UnstableCreateElicitation(context.Context, acp.UnstableCreateElicitationRequest) (acp.UnstableCreateElicitationResponse, error) {
	resp := acp.NewUnstableCreateElicitationResponseAccept()
	resp.Accept.Content = map[string]any{"name": "Ada"}
	resp.Accept.Meta = map[string]any{"ok": true}
	return resp, nil
}

type runEventsClient struct {
	*spyCodexClient
	events []codex.Event
	runErr error
}

func (c *runEventsClient) RunTurn(context.Context, codex.TurnStartRequest) (<-chan codex.Event, error) {
	if c.runErr != nil {
		return nil, c.runErr
	}
	out := make(chan codex.Event, len(c.events))
	go func() {
		defer close(out)
		for _, event := range c.events {
			out <- event
		}
	}()
	return out, nil
}

func (c *runEventsClient) CancelTurn(context.Context, string, string) error { return nil }
func (c *runEventsClient) UnsubscribeThread(context.Context, string) error  { return nil }
func (c *runEventsClient) Close(context.Context) error                      { return nil }

type cancelErrorClient struct {
	*spyCodexClient
}

func (c *cancelErrorClient) CancelTurn(context.Context, string, string) error {
	return errors.New("cancel failed")
}

type cancelDuringRunClient struct {
	*spyCodexClient
	session *Session
}

func (c *cancelDuringRunClient) RunTurn(context.Context, codex.TurnStartRequest) (<-chan codex.Event, error) {
	out := make(chan codex.Event, 1)
	go func() {
		defer close(out)
		c.session.cancelTurn()
		out <- codex.Event{Kind: codex.EventError, ThreadID: "thread", TurnID: "turn", Err: errors.New("canceled")}
	}()
	return out, nil
}

func (c *cancelDuringRunClient) CancelTurn(context.Context, string, string) error { return nil }
func (c *cancelDuringRunClient) UnsubscribeThread(context.Context, string) error  { return nil }
func (c *cancelDuringRunClient) Close(context.Context) error                      { return nil }

type messageErrorMCPClient struct {
	*recordingMCPAgentClient
}

func (c *messageErrorMCPClient) UnstableMessageMcp(context.Context, acp.UnstableMessageMcpRequest) (acp.UnstableMessageMcpResponse, error) {
	return nil, acp.NewInvalidParams(map[string]any{"mcp": "failed"})
}

type errorListener struct{}

func (errorListener) Accept() (net.Conn, error) { return nil, errors.New("accept failed") }
func (errorListener) Close() error              { return nil }
func (errorListener) Addr() net.Addr            { return dummyAddr("tcp") }

type dummyAddr string

func (a dummyAddr) Network() string { return string(a) }
func (a dummyAddr) String() string  { return string(a) }

type noopConn struct{}

func (noopConn) Read([]byte) (int, error)         { return 0, io.EOF }
func (noopConn) Write([]byte) (int, error)        { return 0, io.ErrClosedPipe }
func (noopConn) Close() error                     { return nil }
func (noopConn) LocalAddr() net.Addr              { return dummyAddr("local") }
func (noopConn) RemoteAddr() net.Addr             { return dummyAddr("remote") }
func (noopConn) SetDeadline(time.Time) error      { return nil }
func (noopConn) SetReadDeadline(time.Time) error  { return nil }
func (noopConn) SetWriteDeadline(time.Time) error { return nil }

var _ fmt.Stringer = dummyAddr("")
