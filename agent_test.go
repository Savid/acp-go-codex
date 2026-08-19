package codexacp

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/coder/acp-go-sdk"
	"github.com/savid/acp-go-codex/internal/codex"
	"github.com/stretchr/testify/require"
	"go.uber.org/goleak"
)

// requireOptionsInternalError pins the construction-time option verdict to
// internal error, not invalid params: the caller's params were valid and the
// agent the embedding host built is what is broken. The data object carries
// exactly one key because there is no wire field to name.
func requireOptionsInternalError(t *testing.T, err error) {
	t.Helper()

	var requestErr *acp.RequestError

	require.ErrorAs(t, err, &requestErr)
	require.Equal(t, -32603, requestErr.Code)
	require.Equal(t, "Internal error", requestErr.Message)

	data := asType[map[string]any](t, requestErr.Data)
	require.Len(t, data, 1)
	require.NotEmpty(t, asType[string](t, data[jsonFieldError]))
}

// asType performs a checked type assertion, failing the test if v is not a T.
func asType[T any](t *testing.T, v any) T {
	t.Helper()

	typed, ok := v.(T)
	if !ok {
		t.Fatalf("unexpected type %T, want %T", v, typed)
	}

	return typed
}

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
	if rawMeta, rawOK := meta[rawEventCapabilityKey].(map[string]any); !rawOK || rawMeta[jsonFieldMethod] != RawEventMethod {
		t.Fatalf("missing raw event capability: %#v", meta)
	}
	elicitationMeta, ok := meta["elicitation"].(map[string]any)
	if !ok || elicitationMeta["unstable"] != true || elicitationMeta["tracks"] != "ACP v1 elicitation" {
		t.Fatalf("unexpected elicitation capability: %#v", meta["elicitation"])
	}
	outputSchemaMeta, ok := meta[structuredOutputCapabilityKey].(map[string]any)
	if !ok || outputSchemaMeta[jsonFieldResult] != "_meta.codex.structuredOutput" {
		t.Fatalf("missing output schema capability: %#v", meta)
	}
	if _, ok := meta["goals"]; ok {
		t.Fatalf("goals capability advertised: %#v", meta)
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
		Meta:      inboundRouteMeta("turn-1"),
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
		t.Fatalf("listed session absent from default store after logical release = %#v, want none", listResp.Sessions)
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
	agentConn := newLocalAgentConnection(agent, a2cW, c2aR)
	agent.setAgentClient(agentConn)

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
		Meta:      inboundRouteMeta("turn-1"),
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
	if _, err := clientConn.Prompt(ctx, TextPromptRequest(newResp.SessionId, "test-turn", "hello")); err != nil {
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
	case <-time.After(5 * time.Second):
		t.Fatal("Serve did not return after cancellation")
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
	mu           sync.Mutex
	updates      []acp.SessionNotification
	elicitations []acp.UnstableCreateElicitationRequest
	extensions   []extensionNotification
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

func (c *recordingClient) Elicitations() []acp.UnstableCreateElicitationRequest {
	c.mu.Lock()
	defer c.mu.Unlock()

	return append([]acp.UnstableCreateElicitationRequest(nil), c.elicitations...)
}

func (c *recordingClient) Extensions() []extensionNotification {
	c.mu.Lock()
	defer c.mu.Unlock()

	return append([]extensionNotification(nil), c.extensions...)
}

func (c *recordingClient) UnstableCreateElicitation(_ context.Context, request acp.UnstableCreateElicitationRequest) (acp.UnstableCreateElicitationResponse, error) {
	c.mu.Lock()
	defer c.mu.Unlock()

	c.elicitations = append(c.elicitations, request)
	resp := acp.NewUnstableCreateElicitationResponseAccept()
	resp.Accept.Content = map[string]any{"ok": true}

	return resp, nil
}

func (*recordingClient) RequestPermission(context.Context, acp.RequestPermissionRequest) (acp.RequestPermissionResponse, error) {
	return acp.RequestPermissionResponse{Outcome: acp.NewRequestPermissionOutcomeCancelled()}, nil
}

func (c *recordingClient) HandleExtensionMethod(_ context.Context, method string, params json.RawMessage) (any, error) {
	c.mu.Lock()
	defer c.mu.Unlock()

	c.extensions = append(c.extensions, extensionNotification{method: method, params: append(json.RawMessage(nil), params...)})

	return map[string]any{"ok": true}, nil
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

func (a *Agent) sessionMust(id acp.SessionId) *session {
	session, err := a.session(id)
	if err != nil {
		panic(err)
	}

	return session
}

func TestCodexClientEventSinkUpdatesMatchingSessions(t *testing.T) {
	agent := NewAgent()
	client := newSpyCodexClient()
	otherClient := newSpyCodexClient()
	matching := newSession(agent, "matching", "/tmp/project", nil, codex.Thread{ID: "thread-1"}, client, sessionMeta{}, nil)
	sameClientOtherThread := newSession(agent, "other-thread", "/tmp/project", nil, codex.Thread{ID: "thread-2"}, client, sessionMeta{}, nil)
	other := newSession(agent, "other-client", "/tmp/project", nil, codex.Thread{ID: "thread-1"}, otherClient, sessionMeta{}, nil)
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
	agent.applyCodexClientEvent(context.Background(), client, codex.Event{Kind: codex.EventRaw})
}

func TestAgentCoreBranchEdges(t *testing.T) {
	ctx := context.Background()

	invalidLimits := []ConcurrencyLimits{
		{MaxActiveSessions: -1},
		{MaxConcurrentClientCalls: -1},
	}
	for _, limits := range invalidLimits {
		agent := NewAgent(WithConcurrencyLimits(limits))

		_, err := agent.Initialize(ctx, acp.InitializeRequest{})
		requireOptionsInternalError(t, err)

		// Every entry point answers the verdict, not just the handshake: an
		// embedded host can open a session without ever calling initialize.
		_, err = agent.NewSession(ctx, acp.NewSessionRequest{Cwd: t.TempDir()})
		requireOptionsInternalError(t, err)
	}
	for _, key := range []string{
		"mcp_servers",
		"mcp_servers.tools",
		"mcp_servers . tools",
		`"mcp_servers".tools`,
		`"mcp\u005fservers".tools`,
		`'mcp_servers'.tools`,
	} {
		agent := NewAgent(WithCodexConfigOverrides(map[string]any{key: "forbidden"}))

		_, err := agent.Initialize(ctx, acp.InitializeRequest{})
		requireOptionsInternalError(t, err)
	}
	for key, want := range map[string]string{
		"":                   "",
		" model.provider ":   "model",
		"plain":              "plain",
		`"model_provider".x`: "model_provider",
		`'model_provider'.x`: "model_provider",
		`'unterminated`:      `'unterminated`,
		`"unterminated`:      `"unterminated`,
		`"bad\q".provider`:   `"bad\q".provider`,
		`"escaped\\\"key".x`: `escaped\"key`,
	} {
		if got := codexConfigRootKey(key); got != want {
			t.Fatalf("codexConfigRootKey(%q) = %q, want %q", key, got, want)
		}
	}
	if got := selectPositionEncoding([]acp.PositionEncodingKind{acp.PositionEncodingKindUtf16}); got != acp.PositionEncodingKindUtf16 {
		t.Fatalf("selectPositionEncoding = %q", got)
	}
	if got := selectPositionEncoding([]acp.PositionEncodingKind{"bad", acp.PositionEncodingKindUtf32}); got != acp.PositionEncodingKindUtf16 {
		t.Fatalf("selectPositionEncoding never selects utf32 = %q", got)
	}
	if got := selectPositionEncoding(nil); got != acp.PositionEncodingKindUtf16 {
		t.Fatalf("selectPositionEncoding default = %q", got)
	}

	var gotOptions codex.Options
	envAgent := NewAgent(withClientFactory(func(_ context.Context, options codex.Options) (codex.Client, error) {
		gotOptions = options

		return newSpyCodexClient(), nil
	}))
	if _, err := envAgent.launchRuntimeClient(ctx, 1, "", minSupportedCodexVersion); err != nil {
		t.Fatalf("newClient with env overlays returned error: %v", err)
	}
	if len(gotOptions.Env) != 0 {
		t.Fatalf("newClient env = %#v", gotOptions.Env)
	}

	nilCalls := NewAgent()
	nilCalls.clientCalls = nil
	release, err := nilCalls.acquireClientCall(ctx)
	if err != nil {
		t.Fatalf("acquireClientCall nil channel returned error: %v", err)
	}
	release()

	closedForSession := NewAgent()
	if err := closedForSession.Close(); err != nil {
		t.Fatalf("Close returned error: %v", err)
	}
	if _, err := closedForSession.session("missing"); err == nil {
		t.Fatal("session on closed agent succeeded")
	}

	closedForStore := NewAgent()
	if err := closedForStore.Close(); err != nil {
		t.Fatalf("Close returned error: %v", err)
	}
	closedSession := newSession(closedForStore, "closed", "/tmp/project", nil, codex.Thread{ID: "closed"}, &errorCodexClient{spyCodexClient: newSpyCodexClient(), closeErr: errors.New("close failed")}, sessionMeta{}, nil)
	if err := closedForStore.storeStartedSession(closedSession); err == nil {
		t.Fatal("storeStartedSession on closed agent succeeded")
	}

	limited := NewAgent(WithConcurrencyLimits(ConcurrencyLimits{MaxActiveSessions: 1}))
	first := newSession(limited, "first", "/tmp/project", nil, codex.Thread{ID: "first"}, newSpyCodexClient(), sessionMeta{}, nil)
	if err := limited.storeStartedSession(first); err != nil {
		t.Fatalf("store first session: %v", err)
	}
	// Backpressure refuses registration and leaves the candidate untouched. The
	// caller owns the wrapper until registration succeeds and already closes it
	// on every error, so containing it here as well would run one session's
	// containment boundary twice over one native thread.
	backpressured := newSpyCodexClient()
	second := newSession(limited, "second", "/tmp/project", nil, codex.Thread{ID: "second"}, backpressured, sessionMeta{}, nil)
	if err := limited.storeStartedSession(second); err == nil {
		t.Fatal("storeStartedSession ignored active session limit")
	}
	if contained := backpressured.unsubscribedSnapshot(); len(contained) != 0 {
		t.Fatalf("backpressure refusal contained a candidate its caller owns: %#v", contained)
	}

	replacing := NewAgent()
	old := newSession(replacing, "same", "/tmp/project", nil, codex.Thread{ID: "old"}, &errorCodexClient{spyCodexClient: newSpyCodexClient(), closeErr: errors.New("close failed")}, sessionMeta{}, nil)
	if err := replacing.storeStartedSession(old); err != nil {
		t.Fatalf("store old session: %v", err)
	}
	newer := newSession(replacing, "same", "/tmp/project", nil, codex.Thread{ID: "new"}, newSpyCodexClient(), sessionMeta{}, nil)
	if err := replacing.storeStartedSession(newer); err != nil {
		t.Fatalf("replace session: %v", err)
	}
}

// TestSameIdInstallRefusesOverAnUnsettledSession pins what a same-id install
// does with the instance it replaces. That instance's close is a whole boundary
// — containment, the durable commit it still owes, and the material that commit
// is read back from — so a close that does not complete leaves all three owed by
// a wrapper the install would leave unreferenced. The install is refused and the
// replaced session keeps its id, so the next attempt runs the boundary again.
func TestSameIdInstallRefusesOverAnUnsettledSession(t *testing.T) {
	agent := NewAgent()

	unsettledDir := t.TempDir()
	child := filepath.Join(unsettledDir, "child")
	require.NoError(t, os.WriteFile(child, []byte("x"), 0o600))

	unsettled := newSession(agent, "same", "/tmp/project", nil, codex.Thread{ID: "old"}, newSpyCodexClient(), sessionMeta{}, nil)
	unsettled.materializedPath = unsettledDir
	require.NoError(t, agent.storeStartedSession(unsettled))

	refused := newSession(agent, "same", "/tmp/project", nil, codex.Thread{ID: "new"}, newSpyCodexClient(), sessionMeta{}, nil)
	require.Error(t, agent.storeStartedSession(refused))
	require.Same(t, unsettled, agent.activeSession("same"),
		"an incomplete boundary keeps its id, and everything it still owes with it")

	// The material can be released now, so the same install runs the boundary
	// again and this time it completes.
	require.NoError(t, os.Remove(child))
	require.NoError(t, agent.storeStartedSession(refused))
	require.Same(t, refused, agent.activeSession("same"))
	require.NoError(t, agent.Close())
}

type spyCodexClient struct {
	mu sync.Mutex

	thread codex.Thread
	closed bool

	start          codex.ThreadStartRequest
	resume         codex.ThreadResumeRequest
	fork           codex.ThreadForkRequest
	lastTurn       codex.TurnStartRequest
	steer          codex.TurnSteerRequest
	compact        codex.ThreadCompactRequest
	review         codex.ReviewStartRequest
	turns          codex.ThreadTurnsListRequest
	loggedOut      bool
	login          codex.ChatGPTAuthTokens
	unsubscribed   []string
	deletedThreads []string

	rateLimitsSupported bool
	rateLimits          codex.RateLimitSnapshot
	rateLimitsErr       error
	rateLimitsReads     int
}

func (c *spyCodexClient) ListBackgroundTerminals(
	context.Context,
	codex.BackgroundTerminalListRequest,
) (codex.BackgroundTerminalListResponse, error) {
	return codex.BackgroundTerminalListResponse{}, nil
}

func (c *spyCodexClient) TerminateBackgroundTerminal(
	context.Context,
	codex.BackgroundTerminalTerminateRequest,
) (bool, error) {
	return false, nil
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
	c.mu.Lock()
	defer c.mu.Unlock()
	c.fork = req
	thread := c.thread
	thread.ID = "fork-thread"
	thread.Cwd = req.Cwd

	return thread, nil
}

func (c *spyCodexClient) ListThreads(context.Context, codex.ThreadListRequest) ([]codex.Thread, error) {
	return []codex.Thread{c.thread}, nil
}

func (c *spyCodexClient) ReadThread(context.Context, codex.ThreadReadRequest) (codex.ThreadHistory, error) {
	return codex.ThreadHistory{Thread: c.thread, Items: []map[string]any{{"type": valueAgentMessageCamel, "text": "history"}}}, nil
}

func (c *spyCodexClient) ListTurns(_ context.Context, req codex.ThreadTurnsListRequest) (codex.ThreadTurnsListResponse, error) {
	c.mu.Lock()
	c.turns = req
	c.mu.Unlock()

	return codex.ThreadTurnsListResponse{Turns: []map[string]any{{"id": "turn-1"}}}, nil
}

func (c *spyCodexClient) RunTurn(ctx context.Context, req codex.TurnStartRequest) (codex.Turn, error) {
	c.mu.Lock()
	c.lastTurn = req
	c.mu.Unlock()
	out := make(chan codex.Event, 2)
	go func() {
		defer close(out)
		out <- codex.Event{Kind: codex.EventAgentMessageDelta, ThreadID: req.ThreadID, TurnID: "turn-1", Text: `{"ok":true}`}
		out <- codex.Event{Kind: codex.EventCompleted, ThreadID: req.ThreadID, TurnID: "turn-1", StopReason: codex.StopReasonEndTurn, Usage: codex.Usage{InputTokens: 1, OutputTokens: 2}}
	}()

	return codex.Turn{ID: "turn-1", Events: out}, nil
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

func (c *spyCodexClient) MCPServerStatusList(context.Context, string) (codex.MCPServerStatusListResponse, error) {
	return codex.MCPServerStatusListResponse{Servers: []codex.MCPServerStatus{{Name: "mcp", Status: "ready"}}}, nil
}

func (c *spyCodexClient) UnsubscribeThread(_ context.Context, threadID string) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.unsubscribed = append(c.unsubscribed, threadID)

	return nil
}

func (c *spyCodexClient) DeleteThread(_ context.Context, req codex.ThreadDeleteRequest) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.deletedThreads = append(c.deletedThreads, req.ThreadID)

	return nil
}

func (c *spyCodexClient) deletedThreadSnapshot() []string {
	c.mu.Lock()
	defer c.mu.Unlock()

	return append([]string(nil), c.deletedThreads...)
}

func (c *spyCodexClient) unsubscribedSnapshot() []string {
	c.mu.Lock()
	defer c.mu.Unlock()

	return append([]string(nil), c.unsubscribed...)
}

func (c *spyCodexClient) ModelList(context.Context) ([]codex.Model, error) {
	return []codex.Model{{ID: "gpt-initial", Name: "GPT Initial"}, {ID: "gpt-other", Name: "GPT Other"}}, nil
}

func (c *spyCodexClient) AccountRead(context.Context) (codex.Account, error) {
	return codex.Account{ID: "acct", Email: "user@example.com", PlanType: "plus", Raw: map[string]any{"accessToken": "secret"}}, nil
}

func (c *spyCodexClient) RateLimitsSupported() bool {
	c.mu.Lock()
	defer c.mu.Unlock()

	return c.rateLimitsSupported
}

func (c *spyCodexClient) ReadRateLimits(context.Context) (codex.RateLimitSnapshot, error) {
	c.mu.Lock()
	defer c.mu.Unlock()

	c.rateLimitsReads++
	if c.rateLimitsErr != nil {
		return codex.RateLimitSnapshot{}, c.rateLimitsErr
	}

	return c.rateLimits, nil
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

	updates      []acp.SessionNotification
	extensions   []extensionNotification
	permissions  []acp.RequestPermissionRequest
	elicitations []acp.UnstableCreateElicitationRequest
	scopes       []elicitationScope
	permission   acp.PermissionOptionId
	elicitation  acp.UnstableCreateElicitationResponse
}

type extensionNotification struct {
	method string
	params any
}

func newRecordingAgentClient() *recordingAgentClient {
	return &recordingAgentClient{done: make(chan struct{}), permission: "accept"}
}

func (c *recordingAgentClient) Done() <-chan struct{} { return c.done }

func (c *recordingAgentClient) UnstableCreateElicitation(ctx context.Context, request acp.UnstableCreateElicitationRequest) (acp.UnstableCreateElicitationResponse, error) {
	return c.CreateElicitation(ctx, request, elicitationScope{})
}

func (c *recordingAgentClient) CreateElicitation(_ context.Context, request acp.UnstableCreateElicitationRequest, scope elicitationScope) (acp.UnstableCreateElicitationResponse, error) {
	c.elicitations = append(c.elicitations, request)
	c.scopes = append(c.scopes, scope)
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

func (c *recordingAgentClient) RequestPermission(_ context.Context, request acp.RequestPermissionRequest) (acp.RequestPermissionResponse, error) {
	c.permissions = append(c.permissions, request)

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
		errCh <- Serve(
			serveCtx,
			c2aR,
			a2cW,
			WithScratchDir(t.TempDir()),
			withClientFactory(func(context.Context, codex.Options) (codex.Client, error) {
				return &errorCodexClient{
					spyCodexClient: newSpyCodexClient(),
					closeErr:       errors.Join(errors.New("close failed"), codex.ErrProcessContainmentIncomplete),
				}, nil
			}),
		)
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
	serveErr := <-errCh
	if !errors.Is(serveErr, ErrProcessContainmentIncomplete) {
		t.Fatalf("Serve close-error returned %v, want ErrProcessContainmentIncomplete", serveErr)
	}
	if !errors.Is(ErrProcessContainmentIncomplete, codex.ErrProcessContainmentIncomplete) {
		t.Fatalf("public process-tree error does not preserve internal identity")
	}
	_ = c2aR.Close()
	_ = a2cW.Close()

	agent := NewAgent()
	agent.options.clientFactory = nil
	agent.options.ExecutablePath = filepath.Join(t.TempDir(), "missing-codex")
	if _, err := agent.launchRuntimeClient(context.Background(), 1, "", minSupportedCodexVersion); err == nil {
		t.Fatal("newClient with nil factory and no Codex CLI succeeded")
	}
	var gotOptions codex.Options
	requestAgent := NewAgent(withClientFactory(func(_ context.Context, options codex.Options) (codex.Client, error) {
		gotOptions = options

		return newSpyCodexClient(), nil
	}))
	if _, err := requestAgent.launchRuntimeClient(context.Background(), 1, "", minSupportedCodexVersion); err != nil {
		t.Fatalf("newClient for request handler returned error: %v", err)
	}
	if _, err := gotOptions.RequestHandler(context.Background(), codex.ServerRequest{Method: "missing"}); err == nil {
		t.Fatal("Codex request handler accepted missing method")
	}
}

func TestServeJoinsIncompleteRuntimeLaunchBeforeReturning(t *testing.T) {
	serveCtx, serveCancel := context.WithCancel(context.Background())
	c2aR, c2aW := io.Pipe()
	a2cR, a2cW := io.Pipe()
	t.Cleanup(func() {
		serveCancel()
		_ = c2aR.Close()
		_ = c2aW.Close()
		_ = a2cR.Close()
		_ = a2cW.Close()
	})

	factoryStarted := make(chan struct{})
	releaseFactory := make(chan struct{})
	scratchDir := t.TempDir()
	serveErr := make(chan error, 1)
	go func() {
		serveErr <- Serve(
			serveCtx,
			c2aR,
			a2cW,
			WithScratchDir(scratchDir),
			withClientFactory(func(context.Context, codex.Options) (codex.Client, error) {
				close(factoryStarted)
				<-releaseFactory

				return &errorCodexClient{
					spyCodexClient: newSpyCodexClient(),
					closeErr:       codex.ErrProcessContainmentIncomplete,
				}, nil
			}),
		)
	}()

	clientConn := acp.NewClientSideConnection(&recordingClient{}, c2aW, a2cR)
	if _, err := clientConn.Initialize(context.Background(), acp.InitializeRequest{}); err != nil {
		t.Fatal(err)
	}
	sessionCtx, cancelSession := context.WithCancel(context.Background())
	sessionErr := make(chan error, 1)
	go func() {
		_, err := clientConn.NewSession(sessionCtx, acp.NewSessionRequest{Cwd: "/tmp/project", McpServers: []acp.McpServer{}})
		sessionErr <- err
	}()
	<-factoryStarted

	serveCancel()
	select {
	case err := <-serveErr:
		t.Fatalf("Serve returned before the admitted runtime launch cleaned up: %v", err)
	case <-time.After(25 * time.Millisecond):
	}

	close(releaseFactory)
	select {
	case err := <-serveErr:
		if !errors.Is(err, ErrProcessContainmentIncomplete) {
			t.Fatalf("Serve error = %v, want ErrProcessContainmentIncomplete", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Serve did not join the incomplete runtime launch")
	}
	cancelSession()
	select {
	case <-sessionErr:
	case <-time.After(time.Second):
		t.Fatal("session request did not settle after Serve returned")
	}
}

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
	turnsErr         error
	collaborationErr error
	mcpStatusErr     error
	loginErr         error
	logoutErr        error
	deleteErr        error
	unsubscribeErr   error
	listThreads      []codex.Thread
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

func (c *errorCodexClient) MCPServerStatusList(ctx context.Context, threadID string) (codex.MCPServerStatusListResponse, error) {
	if c.mcpStatusErr != nil {
		return codex.MCPServerStatusListResponse{}, c.mcpStatusErr
	}

	return c.spyCodexClient.MCPServerStatusList(ctx, threadID)
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

func (c *errorCodexClient) UnsubscribeThread(ctx context.Context, threadID string) error {
	if c.unsubscribeErr != nil {
		return c.unsubscribeErr
	}

	return c.spyCodexClient.UnsubscribeThread(ctx, threadID)
}

func (c *errorCodexClient) DeleteThread(ctx context.Context, req codex.ThreadDeleteRequest) error {
	if c.deleteErr != nil {
		return c.deleteErr
	}

	return c.spyCodexClient.DeleteThread(ctx, req)
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

func (c *serverRequestErrorClient) CreateElicitation(context.Context, acp.UnstableCreateElicitationRequest, elicitationScope) (acp.UnstableCreateElicitationResponse, error) {
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

	return acp.RequestPermissionResponse{Outcome: acp.NewRequestPermissionOutcomeSelected("accept")}, nil
}

func TestMain(m *testing.M) {
	fakeMode := decodeFakeCodexMode(os.Getenv(fakeCodexModeEnv))

	// When the process-death test relaunches this binary as the codex CLI, act
	// as a fake app-server instead of running the suite.
	if len(os.Args) > 1 {
		switch os.Args[1] {
		case "--version":
			fmt.Println("codex-cli 0.144.1")
			os.Exit(0)
		case "app-server":
			if fakeMode.Mode == fakeCodexCancelTreeMode {
				runCancelTreeFakeCodexAppServer(fakeMode)
			} else {
				runFakeCodexAppServer()
			}
			os.Exit(0)
		case fakeCodexDelayedChildArg:
			runFakeCodexDelayedChild(fakeMode)
			os.Exit(0)
		}
	}

	// The Secret Service client keeps one library-owned session-bus connection
	// for the process lifetime and exposes no way to close it, so the
	// credential-residence matrix would otherwise report it as a leak.
	goleak.VerifyTestMain(m,
		goleak.IgnoreAnyFunction("github.com/godbus/dbus/v5.(*Conn).inWorker"),
		goleak.IgnoreAnyFunction("github.com/godbus/dbus/v5.(*Conn).outWorker"),
		goleak.IgnoreAnyFunction("github.com/godbus/dbus/v5.newConn.func1"),
	)
}

// fakeCodexStderrTail is emitted on the fake app-server's stderr so a mid-turn
// process death surfaces a real diagnostic tail rather than a bare EOF.
const fakeCodexStderrTail = "codex app-server: fatal: killed (out of memory)"

const (
	fakeCodexModeEnv              = "ACP_GO_CODEX_FAKE_MODE"
	fakeCodexCancelTreeMode       = "cancel-tree"
	fakeCodexDelayedChildArg      = "fake-delayed-child"
	fakeCodexBlockingPrompt       = "START_DELAYED_CHILD"
	fakeCodexReplacementPrompt    = "REPLACEMENT_TURN"
	fakeCodexReplacementReply     = "REPLACEMENT_OK"
	fakeCodexThreadID             = "thread-cancel-tree"
	fakeCodexBlockingTurnID       = "turn-blocking"
	fakeCodexReplacementTurnID    = "turn-replacement"
	fakeCodexBackgroundProcessID  = "background-target-process"
	fakeCodexStalePeerThreadID    = "thread-stale-peer"
	fakeCodexLateAbortRolloutRow  = `{"type":"event_msg","payload":{"type":"turn_aborted","turn_id":"turn-blocking"}}`
	fakeCodexChildSentinelDelay   = 750 * time.Millisecond
	fakeCodexChildObservationWait = 1250 * time.Millisecond
)

type fakeCodexMode struct {
	Mode           string `json:"mode"`
	ChildStarted   string `json:"childStarted"`
	CancelReturned string `json:"cancelReturned"`
	ChildSentinel  string `json:"childSentinel"`
	RolloutPath    string `json:"rolloutPath"`
}

func decodeFakeCodexMode(raw string) fakeCodexMode {
	var mode fakeCodexMode
	_ = json.Unmarshal([]byte(raw), &mode)

	return mode
}

func fakeCodexModeEnvMap(mode fakeCodexMode) map[string]string {
	raw, err := json.Marshal(mode)
	if err != nil {
		panic(err)
	}

	return map[string]string{fakeCodexModeEnv: string(raw)}
}

// runFakeCodexAppServer speaks just enough of the codex app-server JSON-RPC
// protocol to complete the launch handshake, then dies mid-turn on turn/start so
// the transport observes a real process exit (exit status 1 + stderr tail).
func runFakeCodexAppServer() {
	fmt.Fprintln(os.Stderr, fakeCodexStderrTail)

	writeReply := func(id any, result map[string]any) {
		payload, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": id, jsonFieldResult: result})
		payload = append(payload, '\n')
		_, _ = os.Stdout.Write(payload)
	}

	decoder := json.NewDecoder(os.Stdin)
	for {
		var msg map[string]any
		if err := decoder.Decode(&msg); err != nil {
			return
		}

		id, hasID := msg["id"]
		if !hasID {
			continue
		}

		if method, _ := msg[jsonFieldMethod].(string); method == "turn/start" {
			writeReply(id, map[string]any{"turn": map[string]any{"id": "turn-1"}})
			os.Exit(1)
		}

		writeReply(id, map[string]any{})
	}
}

// runCancelTreeFakeCodexAppServer deliberately implements the native failure
// observed in production: turn/interrupt acknowledges cancellation but leaves
// a command descendant running. The fake exposes that descendant through the
// same thread-scoped background-terminal API as Codex 0.144.4.
func runCancelTreeFakeCodexAppServer(mode fakeCodexMode) {
	rolloutPath := mode.RolloutPath

	writeMessage := func(message map[string]any) {
		payload, _ := json.Marshal(message)
		payload = append(payload, '\n')
		_, _ = os.Stdout.Write(payload)
	}
	writeReply := func(id any, result map[string]any) {
		writeMessage(map[string]any{"jsonrpc": "2.0", "id": id, jsonFieldResult: result})
	}
	writeError := func(id any, message string) {
		writeMessage(map[string]any{
			"jsonrpc": "2.0",
			"id":      id,
			jsonFieldError: map[string]any{
				"code": -32602, "message": message,
			},
		})
	}
	writeNotification := func(method string, params map[string]any) {
		writeMessage(map[string]any{"jsonrpc": "2.0", jsonFieldMethod: method, "params": params})
	}

	var backgroundChild *exec.Cmd
	defer func() {
		if backgroundChild != nil && backgroundChild.Process != nil {
			_ = backgroundChild.Process.Kill()
			_ = backgroundChild.Wait()
		}
	}()

	decoder := json.NewDecoder(os.Stdin)
	for {
		var msg map[string]any
		if err := decoder.Decode(&msg); err != nil {
			return
		}

		id, hasID := msg["id"]
		if !hasID {
			continue
		}

		method, _ := msg[jsonFieldMethod].(string)
		switch method {
		case "thread/start":
			if rolloutPath != "" {
				if _, err := os.Stat(rolloutPath); errors.Is(err, os.ErrNotExist) {
					_ = appendFakeCodexRolloutRow(
						rolloutPath,
						`{"type":"session_meta","payload":{"id":"`+fakeCodexThreadID+`"}}`,
					)
				}
			}
			writeReply(id, map[string]any{"thread": map[string]any{"id": fakeCodexThreadID, "path": rolloutPath}})
		case "thread/resume":
			params, _ := msg["params"].(map[string]any)
			threadID, _ := params["threadId"].(string)
			if threadID == fakeCodexStalePeerThreadID {
				writeError(id, "no rollout found for thread id "+threadID)

				continue
			}
			path, _ := params["path"].(string)
			writeReply(id, map[string]any{"thread": map[string]any{"id": fakeCodexThreadID, "path": path}})
		case "turn/start":
			rawParams, _ := json.Marshal(msg["params"])
			if strings.Contains(string(rawParams), fakeCodexBlockingPrompt) {
				backgroundChild = newFakeCodexDelayedChildCommand()
				backgroundChild.Env = os.Environ()
				if err := backgroundChild.Start(); err != nil {
					os.Exit(2)
				}

				writeReply(id, map[string]any{"turn": map[string]any{"id": fakeCodexBlockingTurnID}})
				writeNotification("item/started", map[string]any{
					"threadId": fakeCodexThreadID,
					"turnId":   fakeCodexBlockingTurnID,
					"item": map[string]any{
						"id": fakeCodexBlockingTurnID + "-command", "type": "commandExecution", "command": "delayed child",
					},
				})

				continue
			}

			writeReply(id, map[string]any{"turn": map[string]any{"id": fakeCodexReplacementTurnID}})
			writeNotification("item/agentMessage/delta", map[string]any{
				"threadId": fakeCodexThreadID, "turnId": fakeCodexReplacementTurnID, "delta": fakeCodexReplacementReply,
			})
			writeNotification("turn/completed", map[string]any{
				"threadId": fakeCodexThreadID,
				"turn":     map[string]any{"id": fakeCodexReplacementTurnID, "status": "completed"},
			})
		case "turn/interrupt":
			// Intentionally acknowledge without touching the delayed child, and
			// without stalling: a prompt settles behind the containment
			// boundary, so an interrupt that waits on the cancelled prompt is a
			// fixture that waits on itself.
			writeReply(id, map[string]any{})
		case "thread/backgroundTerminals/list":
			params, _ := msg["params"].(map[string]any)
			threadID, _ := params["threadId"].(string)
			data := []any{}
			if threadID == fakeCodexThreadID && backgroundChild != nil && backgroundChild.Process != nil {
				data = append(data, map[string]any{
					"itemId":    fakeCodexBlockingTurnID + "-command",
					"processId": fakeCodexBackgroundProcessID,
					"osPid":     backgroundChild.Process.Pid,
				})
			}
			writeReply(id, map[string]any{"data": data, "nextCursor": nil})
		case "thread/backgroundTerminals/terminate":
			params, _ := msg["params"].(map[string]any)
			threadID, _ := params["threadId"].(string)
			processID, _ := params["processId"].(string)
			terminated := false
			if threadID == fakeCodexThreadID && processID == fakeCodexBackgroundProcessID && backgroundChild != nil {
				if backgroundChild.Process != nil {
					_ = backgroundChild.Process.Kill()
				}
				_ = backgroundChild.Wait()
				backgroundChild = nil
				terminated = true
			}
			// Native cleanup finishes the rollout as the contained descendant
			// dies. Publishing the abort row here rather than at interrupt is
			// what makes the durable copy a proof: a mirror that read before
			// containment cannot hold this row.
			if terminated && rolloutPath != "" {
				_ = appendFakeCodexRolloutRow(rolloutPath, fakeCodexLateAbortRolloutRow)
			}
			writeReply(id, map[string]any{"terminated": terminated})
		default:
			writeReply(id, map[string]any{})
		}
	}
}

func appendFakeCodexRolloutRow(path string, row string) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}

	file, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600) // #nosec G304 -- test-only temp path.
	if err != nil {
		return err
	}
	defer file.Close()

	// The fake native process appends to this rollout as the isolated identity,
	// so a file the test creates has to belong to that identity too. Only a
	// privileged runner can hand it over; an unprivileged one holds a single
	// identity and never launches the fake process as a second one.
	uid, gid := testIsolationIdentity()
	if os.Geteuid() == 0 && (uid != uint32(os.Getuid()) || gid != uint32(os.Getgid())) {
		if err = file.Chown(int(uid), int(gid)); err != nil {
			return err
		}
	}

	_, err = fmt.Fprintln(file, row)

	return err
}

func runFakeCodexDelayedChild(mode fakeCodexMode) {
	ignoreFakeCodexChildTerminationSignals()

	started := mode.ChildStarted
	cancelReturned := mode.CancelReturned
	sentinel := mode.ChildSentinel
	if started == "" || cancelReturned == "" || sentinel == "" {
		os.Exit(3)
	}

	identity, err := json.Marshal(currentFakeCodexChildIdentity())
	if err != nil {
		os.Exit(4)
	}
	if err := os.WriteFile(started, identity, 0o600); err != nil {
		os.Exit(4)
	}

	deadline := time.Now().Add(fakeCodexChildSentinelDelay)
	for time.Now().Before(deadline) {
		if _, err := os.Stat(cancelReturned); err == nil {
			_ = os.WriteFile(sentinel, []byte("survived cancellation return"), 0o600)

			return
		}

		time.Sleep(5 * time.Millisecond)
	}

	_ = os.WriteFile(sentinel, []byte("delayed side effect"), 0o600)
}

type appendErrorStore struct{}

func (appendErrorStore) Append(context.Context, SessionKey, []SessionStoreEntry) error {
	return errors.New("append failed")
}
func (appendErrorStore) Load(context.Context, SessionKey) ([]SessionStoreEntry, error) {
	return nil, nil
}
func (appendErrorStore) Replace(context.Context, SessionKey, []SessionStoreReplacement) error {
	return nil
}
func (appendErrorStore) Delete(context.Context, SessionKey) error                  { return nil }
func (appendErrorStore) ListSessions(context.Context) ([]SessionSummary, error)    { return nil, nil }
func (appendErrorStore) ListSubkeys(context.Context, SessionKey) ([]string, error) { return nil, nil }

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

func (c *acceptElicitationClient) UnstableCreateElicitation(ctx context.Context, request acp.UnstableCreateElicitationRequest) (acp.UnstableCreateElicitationResponse, error) {
	return c.CreateElicitation(ctx, request, elicitationScope{})
}

func (c *acceptElicitationClient) CreateElicitation(_ context.Context, request acp.UnstableCreateElicitationRequest, scope elicitationScope) (acp.UnstableCreateElicitationResponse, error) {
	c.elicitations = append(c.elicitations, request)
	c.scopes = append(c.scopes, scope)
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

func (c *runEventsClient) RunTurn(context.Context, codex.TurnStartRequest) (codex.Turn, error) {
	if c.runErr != nil {
		return codex.Turn{}, c.runErr
	}
	out := make(chan codex.Event, len(c.events))
	go func() {
		defer close(out)
		for _, event := range c.events {
			out <- event
		}
	}()

	turnID := "turn"
	if len(c.events) > 0 && c.events[0].TurnID != "" {
		turnID = c.events[0].TurnID
	}

	return codex.Turn{ID: turnID, Events: out}, nil
}

func (c *runEventsClient) CancelTurn(context.Context, string, string) error { return nil }
func (c *runEventsClient) UnsubscribeThread(context.Context, string) error  { return nil }
func (c *runEventsClient) DeleteThread(context.Context, codex.ThreadDeleteRequest) error {
	return nil
}
func (c *runEventsClient) Close(context.Context) error { return nil }

type openRunEventsClient struct {
	*spyCodexClient
	events []codex.Event
}

func (c *openRunEventsClient) RunTurn(ctx context.Context, _ codex.TurnStartRequest) (codex.Turn, error) {
	out := make(chan codex.Event, len(c.events))
	go func() {
		defer close(out)
		for _, event := range c.events {
			out <- event
		}
		<-ctx.Done()
	}()

	return codex.Turn{ID: "turn", Events: out}, nil
}

func (c *openRunEventsClient) CancelTurn(context.Context, string, string) error { return nil }
func (c *openRunEventsClient) UnsubscribeThread(context.Context, string) error  { return nil }
func (c *openRunEventsClient) DeleteThread(context.Context, codex.ThreadDeleteRequest) error {
	return nil
}
func (c *openRunEventsClient) Close(context.Context) error { return nil }

type cancelDuringRunClient struct {
	*spyCodexClient
	session *session
}

func (c *cancelDuringRunClient) RunTurn(context.Context, codex.TurnStartRequest) (codex.Turn, error) {
	out := make(chan codex.Event, 1)
	go func() {
		defer close(out)
		c.session.cancelTurn()
		out <- codex.Event{Kind: codex.EventError, ThreadID: "thread", TurnID: "turn", Err: errors.New("canceled")}
	}()

	return codex.Turn{ID: "turn", Events: out}, nil
}

func (c *cancelDuringRunClient) CancelTurn(context.Context, string, string) error { return nil }
func (c *cancelDuringRunClient) UnsubscribeThread(context.Context, string) error  { return nil }
func (c *cancelDuringRunClient) DeleteThread(context.Context, codex.ThreadDeleteRequest) error {
	return nil
}
func (c *cancelDuringRunClient) Close(context.Context) error { return nil }

func TestServeReturnsImmediatelyOnCanceledContext(t *testing.T) {
	err := Serve(canceledContext(), strings.NewReader(""), io.Discard)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("Serve with canceled context = %v, want context.Canceled", err)
	}
}

func TestExplicitProcessIsolationPreservesPolicy(t *testing.T) {
	base := map[string]string{"CANARY": "base"}
	policy := &ProcessIsolation{
		UID: 12, GID: 34, BaseEnvironment: base,
		StandaloneOwnerID: "deployment-1", StandaloneStateRoot: "/var/lib/codex",
	}

	converted := codexProcessIsolation(policy)
	base["CANARY"] = "mutated"

	if converted.UID != 12 || converted.GID != 34 || converted.BaseEnvironment["CANARY"] != "base" ||
		converted.StandaloneOwnerID != "deployment-1" || converted.StandaloneStateRoot != "/var/lib/codex" {
		t.Fatalf("converted isolation = %#v", converted)
	}
	if codexProcessIsolation(nil) != nil {
		t.Fatal("nil isolation did not remain nil")
	}
}

// TestInitializeRejectsGlobalShellEnvironmentPolicyOverrides proves the whole
// shell_environment_policy root is reserved to the thread that owns the session
// environment. A `-c` override applies to every thread of the app-server at
// once, so reserving only `.set` would leave the sibling keys open, and every
// TOML spelling of the root has to be recognised for the reservation to mean
// anything.
func TestInitializeRejectsGlobalShellEnvironmentPolicyOverrides(t *testing.T) {
	ctx := context.Background()

	for _, key := range []string{
		"shell_environment_policy",
		"shell_environment_policy.set.FOO",
		"shell_environment_policy.inherit",
		"shell_environment_policy.exclude",
		"shell_environment_policy.ignore_default_excludes",
		"shell_environment_policy . set . FOO",
		`"shell_environment_policy".set.FOO`,
		`"shell\u005fenvironment\u005fpolicy".set.FOO`,
		`'shell_environment_policy'.set.FOO`,
	} {
		agent := NewAgent(WithCodexConfigOverrides(map[string]any{key: "bar"}))

		_, err := agent.Initialize(ctx, acp.InitializeRequest{})

		requireOptionsInternalError(t, err)

		reqErr, ok := err.(*acp.RequestError)
		if !ok {
			t.Fatalf("Initialize accepted process-global shell environment override %q: %T %v", key, err, err)
		}
		if !strings.Contains(fmt.Sprint(reqErr.Data), "thread-owned shell environment") {
			t.Fatalf("shell environment override %q data = %#v", key, reqErr.Data)
		}
	}

	// A neighbouring root that only shares the prefix stays available.
	agent := NewAgent(WithCodexConfigOverrides(map[string]any{"shell_environment_policy_extra": "ok"}))
	if _, err := agent.Initialize(ctx, acp.InitializeRequest{}); err != nil {
		t.Fatalf("Initialize rejected an unreserved config root: %v", err)
	}
}
