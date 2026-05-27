package codexacp

import (
	"context"
	"errors"
	"os"
	"strings"
	"testing"

	"github.com/coder/acp-go-sdk"
	"github.com/savid/acp-go-codex/internal/codex"
)

func TestAgentHelperBranches(t *testing.T) {
	if selectPositionEncoding([]acp.PositionEncodingKind{acp.PositionEncodingKindUtf16}) != acp.PositionEncodingKindUtf16 {
		t.Fatal("utf-16 position encoding was not selected")
	}
	if selectPositionEncoding(nil) != acp.PositionEncodingKindUtf16 {
		t.Fatal("default position encoding changed")
	}

	agent := NewAgent()
	agent.closed = true
	if err := agent.storeStartedSession(&Session{id: "s"}); err == nil {
		t.Fatal("storeStartedSession on closed agent succeeded")
	}

	client := newSpyCodexClient()
	var gotOptions codex.Options
	agent = NewAgent(withClientFactory(func(_ context.Context, options codex.Options) (codex.Client, error) {
		gotOptions = options
		return client, nil
	}))
	_, err := agent.newClient(context.Background(), []acp.McpServer{{
		Stdio: &acp.McpServerStdio{Name: "echo", Command: "echo", Args: []string{"hi"}},
	}})
	if err != nil {
		t.Fatalf("newClient returned error: %v", err)
	}
	if len(gotOptions.ExtraArgs) == 0 || gotOptions.RequestHandler == nil {
		t.Fatalf("newClient options = %#v", gotOptions)
	}
	_, err = agent.newClient(context.Background(), []acp.McpServer{{Sse: &acp.McpServerSseInline{Name: "sse", Url: "http://example.com"}}})
	if err == nil {
		t.Fatal("newClient accepted SSE MCP")
	}
}

func TestLifecycleErrorAndCloseBranches(t *testing.T) {
	ctx := context.Background()
	closed := NewAgent()
	if err := closed.Close(); err != nil {
		t.Fatalf("Close returned error: %v", err)
	}
	if _, err := closed.NewSession(ctx, NewSessionRequest("/tmp/project")); err == nil {
		t.Fatal("NewSession on closed agent succeeded")
	}
	if _, err := closed.ResumeSession(ctx, ResumeSessionRequest("thread", "/tmp/project")); err == nil {
		t.Fatal("ResumeSession on closed agent succeeded")
	}

	agent := NewAgent()
	if _, err := agent.NewSession(ctx, NewSessionRequest("relative")); err == nil {
		t.Fatal("NewSession accepted relative cwd")
	}
	if _, err := agent.NewSession(ctx, NewSessionRequest("/tmp/project", WithSessionOutputSchema(map[string]any{}))); err == nil {
		t.Fatal("NewSession accepted empty output schema")
	}
	if _, err := agent.NewSession(ctx, NewSessionRequest("/tmp/project", WithSessionMeta(map[string]any{codexMetaKey: map[string]any{"options": map[string]any{"effort": "bad"}}}))); err == nil {
		t.Fatal("NewSession accepted bad meta")
	}
	if _, err := agent.NewSession(ctx, NewSessionRequest("/tmp/project", WithSessionMCPServers(acp.McpServer{Acp: &acp.McpServerAcpInline{Id: "acp", Name: "ACP"}}))); err == nil {
		t.Fatal("NewSession accepted ACP MCP without MCP client")
	}
	if _, err := agent.ResumeSession(ctx, ResumeSessionRequest("thread", "relative")); err == nil {
		t.Fatal("ResumeSession accepted relative cwd")
	}
	if _, err := agent.ResumeSession(ctx, ResumeSessionRequest("thread", "/tmp/project", WithSessionMeta(map[string]any{codexMetaKey: map[string]any{"options": map[string]any{"effort": "bad"}}}))); err == nil {
		t.Fatal("ResumeSession accepted bad meta")
	}
	if _, err := agent.LoadSession(ctx, LoadSessionRequest("thread", "relative")); err == nil {
		t.Fatal("LoadSession accepted relative cwd")
	}
	if _, err := agent.UnstableForkSession(ctx, ForkSessionRequest("thread", "relative")); err == nil {
		t.Fatal("ForkSession accepted relative cwd")
	}

	agent = NewAgent(withClientFactory(func(context.Context, codex.Options) (codex.Client, error) {
		return nil, errors.New("factory failed")
	}))
	if _, err := agent.NewSession(ctx, NewSessionRequest("/tmp/project")); err == nil {
		t.Fatal("NewSession with factory failure succeeded")
	}
	agent.setAgentClient(&recordingMCPAgentClient{recordingAgentClient: newRecordingAgentClient()})
	if _, err := agent.NewSession(ctx, NewSessionRequest("/tmp/project", WithSessionMCPServers(acp.McpServer{Acp: &acp.McpServerAcpInline{Id: "acp", Name: "ACP"}}))); err == nil {
		t.Fatal("NewSession with MCP bridge and factory failure succeeded")
	}

	startClient := &errorCodexClient{spyCodexClient: newSpyCodexClient(), startErr: errors.New("not logged in")}
	agent = NewAgent(withClientFactory(func(context.Context, codex.Options) (codex.Client, error) { return startClient, nil }))
	if _, err := agent.NewSession(ctx, NewSessionRequest("/tmp/project")); err == nil {
		t.Fatal("NewSession with start error succeeded")
	} else if reqErr, ok := err.(*acp.RequestError); !ok || reqErr.Message != "Authentication required" {
		t.Fatalf("start auth error = %v", err)
	}
	agent.setAgentClient(&recordingMCPAgentClient{recordingAgentClient: newRecordingAgentClient()})
	if _, err := agent.NewSession(ctx, NewSessionRequest("/tmp/project", WithSessionMCPServers(acp.McpServer{Acp: &acp.McpServerAcpInline{Id: "acp", Name: "ACP"}}))); err == nil {
		t.Fatal("NewSession with MCP bridge and start error succeeded")
	}
	storeCloseAgent := NewAgent()
	storeCloseClient := &closingAccountClient{spyCodexClient: newSpyCodexClient(), agent: storeCloseAgent}
	storeCloseAgent.options.clientFactory = func(context.Context, codex.Options) (codex.Client, error) { return storeCloseClient, nil }
	if _, err := storeCloseAgent.NewSession(ctx, NewSessionRequest("/tmp/project")); err == nil {
		t.Fatal("NewSession with store close race succeeded")
	}

	resumeClient := &errorCodexClient{spyCodexClient: newSpyCodexClient(), resumeErr: errors.New("resume failed")}
	agent = NewAgent(withClientFactory(func(context.Context, codex.Options) (codex.Client, error) { return resumeClient, nil }))
	if _, err := agent.ResumeSession(ctx, ResumeSessionRequest("thread", "/tmp/project")); err == nil {
		t.Fatal("ResumeSession with provider error succeeded")
	}
	agent.setAgentClient(&recordingMCPAgentClient{recordingAgentClient: newRecordingAgentClient()})
	if _, err := agent.ResumeSession(ctx, ResumeSessionRequest("thread", "/tmp/project", WithSessionMCPServers(acp.McpServer{Acp: &acp.McpServerAcpInline{Id: "acp", Name: "ACP"}}))); err == nil {
		t.Fatal("ResumeSession with MCP bridge and provider error succeeded")
	}

	forkClient := &errorCodexClient{spyCodexClient: newSpyCodexClient(), forkErr: errors.New("fork failed")}
	agent = NewAgent(withClientFactory(func(context.Context, codex.Options) (codex.Client, error) { return forkClient, nil }))
	resp, err := agent.NewSession(ctx, NewSessionRequest("/tmp/project"))
	if err != nil {
		t.Fatalf("NewSession for fork error returned: %v", err)
	}
	if _, err := agent.UnstableForkSession(ctx, ForkSessionRequest(resp.SessionId, "/tmp/project")); err == nil {
		t.Fatal("ForkSession with provider error succeeded")
	}

	closeClient := &errorCodexClient{spyCodexClient: newSpyCodexClient(), closeErr: errors.New("close failed")}
	path, err := materializeRollout([]SessionStoreEntry{SessionStoreEntry(`{"type":"event_msg","payload":{"type":"agent_message","message":"hi"}}`)})
	if err != nil {
		t.Fatalf("materializeRollout returned error: %v", err)
	}
	session := &Session{agent: NewAgent(), id: "s", client: closeClient, codexThreadID: "thread", materializedPath: path}
	if err := session.Close(ctx); err == nil || !strings.Contains(err.Error(), "close failed") {
		t.Fatalf("session close error = %v", err)
	}
	if err := removeMaterializedRollout(path); err != nil {
		t.Fatalf("cleanup materialized rollout: %v", err)
	}

	path, err = materializeRollout([]SessionStoreEntry{SessionStoreEntry(`{"type":"event_msg","payload":{"type":"agent_message","message":"hi"}}`)})
	if err != nil {
		t.Fatalf("materializeRollout returned error: %v", err)
	}
	session = &Session{agent: NewAgent(), id: "s", materializedPath: path}
	if err := session.Close(ctx); err != nil {
		t.Fatalf("session close without client returned error: %v", err)
	}
	if err := removeMaterializedRollout(path); err != nil {
		t.Fatalf("remove missing materialized rollout returned error: %v", err)
	}

	if path, err := materializeRollout(nil); err != nil || path != "" {
		t.Fatalf("empty materialize path=%q err=%v", path, err)
	}
}

func TestSessionLifecycleConfigModelAndAccountEdges(t *testing.T) {
	ctx := context.Background()
	client := &errorCodexClient{spyCodexClient: newSpyCodexClient(), modelErr: errors.New("models failed"), accountErr: errors.New("account failed")}
	agent := NewAgent(withClientFactory(func(context.Context, codex.Options) (codex.Client, error) { return client, nil }))
	resp, err := agent.NewSession(ctx, NewSessionRequest("/tmp/project", WithSessionAdditionalDirectories("/tmp/extra")))
	if err != nil {
		t.Fatalf("NewSession returned error: %v", err)
	}
	if account := resp.Meta[codexMetaKey].(map[string]any)[codexAccountMetaKey]; account != nil {
		t.Fatalf("account meta should be absent on account/read error: %#v", account)
	}

	if prompt, err := agent.Prompt(ctx, TextPromptRequest("missing", "hi")); err == nil || prompt.StopReason != "" {
		t.Fatal("Prompt missing session succeeded")
	}
	if _, err := agent.CloseSession(ctx, acp.CloseSessionRequest{SessionId: "missing"}); err != nil {
		t.Fatalf("CloseSession missing returned error: %v", err)
	}
	existing, err := agent.LoadSession(ctx, LoadSessionRequest(resp.SessionId, "/tmp/project"))
	if err != nil {
		t.Fatalf("LoadSession existing returned error: %v", err)
	}
	if existing.Meta[codexMetaKey] == nil {
		t.Fatalf("existing load meta = %#v", existing.Meta)
	}

	if _, err := agent.SetSessionConfigOption(ctx, acp.SetSessionConfigOptionRequest{ValueId: &acp.SetSessionConfigOptionValueId{SessionId: "missing", ConfigId: configModel, Value: "gpt"}}); err == nil {
		t.Fatal("SetSessionConfigOption missing session succeeded")
	}
	if _, err := agent.SetSessionConfigOption(ctx, acp.SetSessionConfigOptionRequest{ValueId: &acp.SetSessionConfigOptionValueId{SessionId: resp.SessionId, ConfigId: configEffort, Value: "bad"}}); err == nil {
		t.Fatal("SetSessionConfigOption bad effort succeeded")
	}
	if _, err := agent.SetSessionConfigOption(ctx, acp.SetSessionConfigOptionRequest{ValueId: &acp.SetSessionConfigOptionValueId{SessionId: resp.SessionId, ConfigId: configPersonality, Value: "bad"}}); err == nil {
		t.Fatal("SetSessionConfigOption bad personality succeeded")
	}
	if _, err := agent.SetSessionConfigOption(ctx, acp.SetSessionConfigOptionRequest{ValueId: &acp.SetSessionConfigOptionValueId{SessionId: resp.SessionId, ConfigId: "missing", Value: "x"}}); err == nil {
		t.Fatal("SetSessionConfigOption unknown config succeeded")
	}
	if _, err := agent.SetSessionMode(ctx, acp.SetSessionModeRequest{SessionId: "missing", ModeId: modeDefault}); err == nil {
		t.Fatal("SetSessionMode missing session succeeded")
	}
	if _, err := agent.UnstableSetSessionModel(ctx, acp.UnstableSetSessionModelRequest{SessionId: "missing", ModelId: "gpt"}); err == nil {
		t.Fatal("SetSessionModel missing session succeeded")
	}
	if got := modelList(ctx, nil); got != nil {
		t.Fatalf("modelList nil = %#v", got)
	}
	if got := modelState("", []codex.Model{{ID: ""}, {ID: "a", Name: "A"}, {ID: "a", Name: "dup"}}); got.CurrentModelId != "default" || len(got.AvailableModels) != 1 {
		t.Fatalf("modelState = %#v", got)
	}
	if got := unstableModelState("", nil); got.CurrentModelId != "default" || len(got.AvailableModels) != 1 {
		t.Fatalf("unstableModelState = %#v", got)
	}
	if clientAccountMeta(ctx, nil) != nil {
		t.Fatal("clientAccountMeta nil client returned data")
	}

	errConn := &errorAgentClient{recordingAgentClient: newRecordingAgentClient(), updateErr: errors.New("update failed")}
	agent.setAgentClient(errConn)
	if _, err := agent.SetSessionConfigOption(ctx, acp.SetSessionConfigOptionRequest{ValueId: &acp.SetSessionConfigOptionValueId{SessionId: resp.SessionId, ConfigId: configServiceTier, Value: "flex"}}); err == nil {
		t.Fatal("SetSessionConfigOption with update failure succeeded")
	}
	if _, err := agent.SetSessionMode(ctx, acp.SetSessionModeRequest{SessionId: resp.SessionId, ModeId: modePlan}); err == nil {
		t.Fatal("SetSessionMode with update failure succeeded")
	}
	if _, err := agent.UnstableSetSessionModel(ctx, acp.UnstableSetSessionModelRequest{SessionId: resp.SessionId, ModelId: "gpt"}); err == nil {
		t.Fatal("SetSessionModel with update failure succeeded")
	}
}

func TestSessionListLoadAndForkErrorBranches(t *testing.T) {
	ctx := context.Background()
	client := &errorCodexClient{
		spyCodexClient: newSpyCodexClient(),
		listThreads: []codex.Thread{
			{},
			{ID: "thread-1", SessionID: "thread-1"},
			{ID: "active-thread", SessionID: "active-session"},
			{ID: "new-thread", Model: "gpt", Cwd: "/tmp/project"},
		},
	}
	agent := NewAgent(withClientFactory(func(context.Context, codex.Options) (codex.Client, error) { return client, nil }))
	active := newSession(agent, "active-session", "/tmp/project", []string{"/tmp/extra"}, codex.Thread{ID: "active-thread", Model: "gpt"}, client, sessionMeta{})
	if err := agent.storeStartedSession(active); err != nil {
		t.Fatalf("store active session: %v", err)
	}
	cwd := "/tmp/project"
	list, err := agent.ListSessions(ctx, ListSessionsRequest(WithListSessionsCwd(cwd), WithListSessionsAdditionalDirectories("/tmp/other")))
	if err != nil {
		t.Fatalf("ListSessions filtered returned error: %v", err)
	}
	if len(list.Sessions) != 3 {
		t.Fatalf("filtered list = %#v", list.Sessions)
	}
	badCwd := "relative"
	if _, err := agent.ListSessions(ctx, ListSessionsRequest(WithListSessionsCwd(badCwd))); err == nil {
		t.Fatal("ListSessions accepted relative cwd")
	}
	listErrClient := &errorCodexClient{spyCodexClient: newSpyCodexClient(), listErr: errors.New("list failed")}
	agent = NewAgent(withClientFactory(func(context.Context, codex.Options) (codex.Client, error) { return listErrClient, nil }))
	if _, err := agent.ListSessions(ctx, acp.ListSessionsRequest{}); err == nil {
		t.Fatal("ListSessions with provider list error succeeded")
	}
	agent = NewAgent(withClientFactory(func(context.Context, codex.Options) (codex.Client, error) { return nil, errors.New("factory failed") }))
	if _, err := agent.ListSessions(ctx, acp.ListSessionsRequest{}); err == nil {
		t.Fatal("ListSessions with factory error succeeded")
	}

	storeErr := errorSessionStore{loadErr: errors.New("load failed"), listErr: errors.New("list failed")}
	agent = NewAgent(WithSessionStore(storeErr), withClientFactory(func(context.Context, codex.Options) (codex.Client, error) { return newSpyCodexClient(), nil }))
	if _, err := agent.LoadSession(ctx, LoadSessionRequest("s", "/tmp/project")); err == nil {
		t.Fatal("LoadSession with store load error succeeded")
	}
	if _, err := agent.listStoredSessions(ctx, "relative", nil); err == nil {
		t.Fatal("listStoredSessions accepted relative cwd")
	}
	if _, err := agent.listStoredSessions(ctx, "/tmp/project", nil); err == nil {
		t.Fatal("listStoredSessions with list error succeeded")
	}
	if _, err := agent.loadStoredSession(ctx, "s", "relative"); err == nil {
		t.Fatal("loadStoredSession accepted relative cwd")
	}

	closed := NewAgent()
	_ = closed.Close()
	if _, err := closed.loadMaterializedSession(ctx, LoadSessionRequest("s", "/tmp/project"), []SessionStoreEntry{SessionStoreEntry(`{"type":"event_msg"}`)}); err == nil {
		t.Fatal("loadMaterializedSession on closed agent succeeded")
	}
	agent = NewAgent()
	if _, err := agent.loadMaterializedSession(ctx, LoadSessionRequest("s", "/tmp/project", WithSessionOutputSchema(map[string]any{})), []SessionStoreEntry{SessionStoreEntry(`{"type":"event_msg"}`)}); err == nil {
		t.Fatal("loadMaterializedSession accepted bad meta")
	}
	origTMP := os.Getenv("TMPDIR")
	t.Setenv("TMPDIR", "/path/that/does/not/exist")
	if _, err := agent.loadMaterializedSession(ctx, LoadSessionRequest("s", "/tmp/project"), []SessionStoreEntry{SessionStoreEntry(`{"type":"event_msg"}`)}); err == nil {
		t.Fatal("loadMaterializedSession accepted materialize failure")
	}
	t.Setenv("TMPDIR", origTMP)
	agent = NewAgent(withClientFactory(func(context.Context, codex.Options) (codex.Client, error) { return nil, errors.New("factory failed") }))
	if _, err := agent.loadMaterializedSession(ctx, LoadSessionRequest("s", "/tmp/project"), []SessionStoreEntry{SessionStoreEntry(`{"type":"event_msg"}`)}); err == nil {
		t.Fatal("loadMaterializedSession accepted factory failure")
	}
	resumeErrClient := &errorCodexClient{spyCodexClient: newSpyCodexClient(), resumeErr: errors.New("not logged in")}
	agent = NewAgent(withClientFactory(func(context.Context, codex.Options) (codex.Client, error) { return resumeErrClient, nil }))
	if _, err := agent.loadMaterializedSession(ctx, LoadSessionRequest("s", "/tmp/project"), []SessionStoreEntry{SessionStoreEntry(`{"type":"event_msg"}`)}); err == nil {
		t.Fatal("loadMaterializedSession accepted resume failure")
	}
	agent = NewAgent(withClientFactory(func(context.Context, codex.Options) (codex.Client, error) { return newSpyCodexClient(), nil }))
	if _, err := agent.loadMaterializedSession(ctx, LoadSessionRequest("s", "/tmp/project"), []SessionStoreEntry{SessionStoreEntry(`{"payload":{}}`)}); err == nil {
		t.Fatal("loadMaterializedSession accepted replay failure")
	}

	agent = NewAgent()
	if _, err := agent.UnstableForkSession(ctx, ForkSessionRequest("missing", "/tmp/project")); err == nil {
		t.Fatal("Fork missing session succeeded")
	}
	client = &errorCodexClient{spyCodexClient: newSpyCodexClient()}
	agent = NewAgent(withClientFactory(func(context.Context, codex.Options) (codex.Client, error) { return client, nil }))
	resp, err := agent.NewSession(ctx, NewSessionRequest("/tmp/project"))
	if err != nil {
		t.Fatalf("NewSession for fork branch returned error: %v", err)
	}
	if _, err := agent.UnstableForkSession(ctx, ForkSessionRequest(resp.SessionId, "/tmp/project", WithSessionMeta(map[string]any{codexMetaKey: map[string]any{"options": map[string]any{"outputSchema": map[string]any{}}}}))); err == nil {
		t.Fatal("Fork accepted bad meta")
	}
	if _, err := agent.UnstableForkSession(ctx, acp.UnstableForkSessionRequest{SessionId: resp.SessionId, Cwd: "/tmp/project", McpServers: []acp.UnstableMcpServer{{Sse: &acp.UnstableMcpServerSse{Name: "sse", Url: "http://example.com"}}}}); err == nil {
		t.Fatal("Fork accepted SSE MCP")
	}
	agent = NewAgent(withClientFactory(func(context.Context, codex.Options) (codex.Client, error) { return nil, errors.New("factory failed") }))
	agent.sessions[resp.SessionId] = active
	if _, err := agent.UnstableForkSession(ctx, ForkSessionRequest(resp.SessionId, "/tmp/project")); err == nil {
		t.Fatal("Fork accepted factory failure")
	}
}

func TestListSessionsStoreAndTokenErrors(t *testing.T) {
	ctx := context.Background()
	cwd := t.TempDir()
	client := &errorCodexClient{
		spyCodexClient: newSpyCodexClient(),
		listThreads: []codex.Thread{
			{},
			{ID: "active-thread", SessionID: "different-session"},
			{ID: "other-thread", SessionID: "active-session"},
			{ID: "stored-thread", SessionID: "stored-session", Model: "gpt-test"},
		},
	}
	agent := NewAgent(withClientFactory(func(context.Context, codex.Options) (codex.Client, error) { return client, nil }))
	active := newSession(agent, "active-session", cwd, nil, codex.Thread{ID: "active-thread"}, client, sessionMeta{})
	if err := agent.storeStartedSession(active); err != nil {
		t.Fatalf("store active session: %v", err)
	}
	list, err := agent.ListSessions(ctx, ListSessionsRequest(WithListSessionsCwd(cwd)))
	if err != nil {
		t.Fatalf("ListSessions returned error: %v", err)
	}
	if len(list.Sessions) != 2 {
		t.Fatalf("ListSessions did not skip active duplicates: %#v", list.Sessions)
	}

	if stored, err := NewAgent(WithSessionStore(noListStore{})).listStoredSessions(ctx, cwd, nil); err != nil || stored != nil {
		t.Fatalf("non-lister stored sessions = %#v err=%v", stored, err)
	}
	store := NewInMemorySessionStore()
	projectKey, err := projectKeyForDirectory(cwd)
	if err != nil {
		t.Fatalf("project key: %v", err)
	}
	if err := store.Append(ctx, SessionKey{ProjectKey: projectKey, SessionID: "stored"}, []SessionStoreEntry{SessionStoreEntry(`{"type":"a"}`)}); err != nil {
		t.Fatalf("append stored session: %v", err)
	}
	storedAgent := NewAgent(WithSessionStore(store))
	stored, err := storedAgent.listStoredSessions(ctx, cwd, map[acp.SessionId]struct{}{"stored": {}})
	if err != nil || len(stored) != 0 {
		t.Fatalf("active stored sessions = %#v err=%v", stored, err)
	}
	storedAgent.options.SessionStoreLoadTimeout = 0
	storeCtx, cancel := storedAgent.sessionStoreContext(ctx)
	defer cancel()
	if _, ok := storeCtx.Deadline(); !ok {
		t.Fatal("default session store timeout did not set a deadline")
	}

	if _, err := agent.SetSessionConfigOption(ctx, acp.SetSessionConfigOptionRequest{Boolean: &acp.SetSessionConfigOptionBoolean{SessionId: active.id, ConfigId: "bool", Value: true}}); err == nil {
		t.Fatal("boolean config mutation succeeded")
	}

	resumeStoreCloseAgent := NewAgent()
	resumeStoreCloseClient := &closingAccountClient{spyCodexClient: newSpyCodexClient(), agent: resumeStoreCloseAgent}
	resumeStoreCloseAgent.options.clientFactory = func(context.Context, codex.Options) (codex.Client, error) {
		return resumeStoreCloseClient, nil
	}
	if _, err := resumeStoreCloseAgent.ResumeSession(ctx, ResumeSessionRequest("thread", cwd)); err == nil {
		t.Fatal("ResumeSession with store close race succeeded")
	}
	if _, err := NewAgent().ResumeSession(ctx, ResumeSessionRequest("thread", cwd, WithSessionMCPServers(acp.McpServer{Acp: &acp.McpServerAcpInline{Id: "acp", Name: "ACP"}}))); err == nil {
		t.Fatal("ResumeSession accepted ACP MCP without MCP client")
	}
	resumeBridgeAgent := NewAgent(withClientFactory(func(context.Context, codex.Options) (codex.Client, error) {
		return nil, errors.New("factory failed")
	}))
	resumeBridgeAgent.setAgentClient(&recordingMCPAgentClient{recordingAgentClient: newRecordingAgentClient()})
	if _, err := resumeBridgeAgent.ResumeSession(ctx, ResumeSessionRequest("thread", cwd, WithSessionMCPServers(acp.McpServer{Acp: &acp.McpServerAcpInline{Id: "acp", Name: "ACP"}}))); err == nil {
		t.Fatal("ResumeSession with MCP bridge accepted factory failure")
	}
	loadResumeAgent := NewAgent(withClientFactory(func(context.Context, codex.Options) (codex.Client, error) {
		return &errorCodexClient{spyCodexClient: newSpyCodexClient(), resumeErr: errors.New("resume failed")}, nil
	}))
	if _, err := loadResumeAgent.LoadSession(ctx, LoadSessionRequest("thread", cwd)); err == nil {
		t.Fatal("LoadSession fallback accepted resume failure")
	}
	readFailAgent := NewAgent(withClientFactory(func(context.Context, codex.Options) (codex.Client, error) {
		return readErrorClient{Client: newSpyCodexClient()}, nil
	}))
	if _, err := readFailAgent.LoadSession(ctx, LoadSessionRequest("thread", cwd)); err == nil {
		t.Fatal("LoadSession fallback ignored replay read error")
	}

	loadAgent := NewAgent()
	if _, err := loadAgent.loadMaterializedSession(ctx, LoadSessionRequest("s", cwd, WithSessionMCPServers(acp.McpServer{Acp: &acp.McpServerAcpInline{Id: "acp", Name: "ACP"}})), []SessionStoreEntry{SessionStoreEntry(`{"type":"event_msg","payload":{"type":"agent_message","message":"hi"}}`)}); err == nil {
		t.Fatal("loadMaterializedSession accepted ACP MCP without MCP client")
	}
	loadAgent = NewAgent(withClientFactory(func(context.Context, codex.Options) (codex.Client, error) {
		return nil, errors.New("factory failed")
	}))
	loadAgent.setAgentClient(&recordingMCPAgentClient{recordingAgentClient: newRecordingAgentClient()})
	if _, err := loadAgent.loadMaterializedSession(ctx, LoadSessionRequest("s", cwd, WithSessionMCPServers(acp.McpServer{Acp: &acp.McpServerAcpInline{Id: "acp", Name: "ACP"}})), []SessionStoreEntry{SessionStoreEntry(`{"type":"event_msg","payload":{"type":"agent_message","message":"hi"}}`)}); err == nil {
		t.Fatal("loadMaterializedSession with MCP bridge accepted factory failure")
	}
	loadStoreCloseAgent := NewAgent()
	loadStoreCloseClient := &closingAccountClient{spyCodexClient: newSpyCodexClient(), agent: loadStoreCloseAgent}
	loadStoreCloseAgent.options.clientFactory = func(context.Context, codex.Options) (codex.Client, error) {
		return loadStoreCloseClient, nil
	}
	if _, err := loadStoreCloseAgent.loadMaterializedSession(ctx, LoadSessionRequest("s", cwd), []SessionStoreEntry{SessionStoreEntry(`{"type":"event_msg","payload":{"type":"agent_message","message":"hi"}}`)}); err == nil {
		t.Fatal("loadMaterializedSession with store close race succeeded")
	}

	forkAgent := NewAgent(withClientFactory(func(context.Context, codex.Options) (codex.Client, error) {
		return &errorCodexClient{spyCodexClient: newSpyCodexClient(), forkErr: errors.New("fork failed")}, nil
	}))
	forkAgent.setAgentClient(&recordingMCPAgentClient{recordingAgentClient: newRecordingAgentClient()})
	parent := newSession(forkAgent, "parent", cwd, nil, codex.Thread{ID: "parent-thread"}, newSpyCodexClient(), sessionMeta{})
	if err := forkAgent.storeStartedSession(parent); err != nil {
		t.Fatalf("store parent: %v", err)
	}
	if _, err := forkAgent.UnstableForkSession(ctx, ForkSessionRequest("parent", "relative")); err == nil {
		t.Fatal("fork accepted relative cwd after parent lookup")
	}
	forkNoMCPAgent := NewAgent()
	if err := forkNoMCPAgent.storeStartedSession(newSession(forkNoMCPAgent, "parent", cwd, nil, codex.Thread{ID: "parent-thread"}, newSpyCodexClient(), sessionMeta{})); err != nil {
		t.Fatalf("store no-MCP parent: %v", err)
	}
	if _, err := forkNoMCPAgent.UnstableForkSession(ctx, ForkSessionRequest("parent", cwd, WithSessionMCPServers(acp.McpServer{Acp: &acp.McpServerAcpInline{Id: "acp", Name: "ACP"}}))); err == nil {
		t.Fatal("fork accepted ACP MCP without MCP client")
	}
	if _, err := forkAgent.UnstableForkSession(ctx, ForkSessionRequest("parent", cwd, WithSessionMCPServers(acp.McpServer{Acp: &acp.McpServerAcpInline{Id: "acp", Name: "ACP"}}))); err == nil {
		t.Fatal("fork with MCP bridge accepted provider failure")
	}
	forkBridgeAgent := NewAgent(withClientFactory(func(context.Context, codex.Options) (codex.Client, error) {
		return nil, errors.New("factory failed")
	}))
	forkBridgeAgent.setAgentClient(&recordingMCPAgentClient{recordingAgentClient: newRecordingAgentClient()})
	if err := forkBridgeAgent.storeStartedSession(newSession(forkBridgeAgent, "parent", cwd, nil, codex.Thread{ID: "parent-thread"}, newSpyCodexClient(), sessionMeta{})); err != nil {
		t.Fatalf("store bridge parent: %v", err)
	}
	if _, err := forkBridgeAgent.UnstableForkSession(ctx, ForkSessionRequest("parent", cwd, WithSessionMCPServers(acp.McpServer{Acp: &acp.McpServerAcpInline{Id: "acp", Name: "ACP"}}))); err == nil {
		t.Fatal("fork with MCP bridge accepted factory failure")
	}
	forkStoreCloseAgent := NewAgent()
	forkStoreCloseClient := &closingAccountClient{spyCodexClient: newSpyCodexClient(), agent: forkStoreCloseAgent}
	forkStoreCloseAgent.options.clientFactory = func(context.Context, codex.Options) (codex.Client, error) { return forkStoreCloseClient, nil }
	if err := forkStoreCloseAgent.storeStartedSession(newSession(forkStoreCloseAgent, "parent", cwd, nil, codex.Thread{ID: "parent-thread"}, newSpyCodexClient(), sessionMeta{})); err != nil {
		t.Fatalf("store close parent: %v", err)
	}
	if _, err := forkStoreCloseAgent.UnstableForkSession(ctx, ForkSessionRequest("parent", cwd)); err == nil {
		t.Fatal("fork with store close race succeeded")
	}
}

func TestSessionResumeForkCancelSettersAndClose(t *testing.T) {
	client := newSpyCodexClient()
	agent := NewAgent(withClientFactory(func(context.Context, codex.Options) (codex.Client, error) { return client, nil }))
	agent.setAgentClient(newRecordingAgentClient())
	ctx := context.Background()

	resume, err := agent.ResumeSession(ctx, ResumeSessionRequest("thread-1", "/tmp/project"))
	if err != nil {
		t.Fatalf("ResumeSession returned error: %v", err)
	}
	if resume.Models == nil || resume.ConfigOptions == nil {
		t.Fatalf("resume response = %#v", resume)
	}
	if err := agent.Cancel(ctx, acp.CancelNotification{SessionId: "thread-1"}); err != nil {
		t.Fatalf("Cancel returned error: %v", err)
	}
	if _, err := agent.SetSessionMode(ctx, acp.SetSessionModeRequest{SessionId: "thread-1", ModeId: modePlan}); err != nil {
		t.Fatalf("SetSessionMode returned error: %v", err)
	}
	if _, err := agent.UnstableSetSessionModel(ctx, acp.UnstableSetSessionModelRequest{SessionId: "thread-1", ModelId: "gpt-other"}); err != nil {
		t.Fatalf("UnstableSetSessionModel returned error: %v", err)
	}
	fork, err := agent.UnstableForkSession(ctx, ForkSessionRequest("thread-1", "/tmp/project"))
	if err != nil {
		t.Fatalf("UnstableForkSession returned error: %v", err)
	}
	if fork.SessionId == "" || fork.ConfigOptions == nil || fork.Models == nil {
		t.Fatalf("fork response = %#v", fork)
	}
	if _, err := agent.SetSessionConfigOption(ctx, acp.SetSessionConfigOptionRequest{}); err == nil {
		t.Fatal("empty config option succeeded")
	}
	if _, err := agent.SetSessionMode(ctx, acp.SetSessionModeRequest{SessionId: "thread-1", ModeId: "bad"}); err == nil {
		t.Fatal("bad mode succeeded")
	}
	if _, err := agent.CloseSession(ctx, acp.CloseSessionRequest{SessionId: "thread-1"}); err != nil {
		t.Fatalf("CloseSession returned error: %v", err)
	}
	if err := agent.Close(); err != nil {
		t.Fatalf("Agent Close returned error: %v", err)
	}
	if err := agent.ensureOpen(); err == nil {
		t.Fatal("closed agent was still open")
	}
}
