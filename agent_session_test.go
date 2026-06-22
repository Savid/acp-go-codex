package codexacp

import (
	"context"
	"encoding/base64"
	"errors"
	"os"
	"strconv"
	"strings"
	"testing"

	"github.com/coder/acp-go-sdk"
	"github.com/savid/acp-go-codex/internal/codex"
)

func requireResourceNotFound(t *testing.T, err error, sessionID acp.SessionId) {
	t.Helper()
	reqErr, ok := err.(*acp.RequestError)
	if !ok {
		t.Fatalf("error = %T %v, want ACP request error", err, err)
	}
	if reqErr.Code != -32002 || reqErr.Message != "Resource not found" {
		t.Fatalf("request error = %#v, want resource not found", reqErr)
	}
	data, ok := reqErr.Data.(map[string]any)
	if !ok {
		t.Fatalf("resource data = %T %#v, want map", reqErr.Data, reqErr.Data)
	}
	if data[jsonFieldSessionID] != sessionID {
		t.Fatalf("resource session id = %#v, want %q", data[jsonFieldSessionID], sessionID)
	}
}

func requireAgentClosed(t *testing.T, err error) {
	t.Helper()
	reqErr, ok := err.(*acp.RequestError)
	if !ok {
		t.Fatalf("error = %T %v, want ACP request error", err, err)
	}
	if reqErr.Code != -32603 || reqErr.Message != "Internal error" {
		t.Fatalf("request error = %#v, want internal error", reqErr)
	}
	data, ok := reqErr.Data.(map[string]any)
	if !ok || data[jsonFieldError] != "agent is closed" {
		t.Fatalf("agent closed data = %T %#v", reqErr.Data, reqErr.Data)
	}
}

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
	if err := agent.storeStartedSession(&Session{id: "s", client: &errorCodexClient{spyCodexClient: newSpyCodexClient(), closeErr: errors.New("close failed")}}); err == nil {
		t.Fatal("storeStartedSession on closed agent with close error succeeded")
	}

	agent = NewAgent()
	if err := agent.storeStartedSession(&Session{id: "s", client: &errorCodexClient{spyCodexClient: newSpyCodexClient(), closeErr: errors.New("close failed")}}); err != nil {
		t.Fatalf("store previous session: %v", err)
	}
	if err := agent.storeStartedSession(&Session{id: "s", client: newSpyCodexClient()}); err != nil {
		t.Fatalf("replace previous session: %v", err)
	}

	client := newSpyCodexClient()
	var gotOptions codex.Options
	agent = NewAgent(withClientFactory(func(_ context.Context, options codex.Options) (codex.Client, error) {
		gotOptions = options
		return client, nil
	}))
	_, err := agent.newClient(context.Background(), []acp.McpServer{{
		Stdio: &acp.McpServerStdio{Name: "echo", Command: "echo", Args: []string{"hi"}},
	}}, nil)
	if err != nil {
		t.Fatalf("newClient returned error: %v", err)
	}
	if len(gotOptions.ExtraArgs) == 0 || gotOptions.RequestHandler == nil {
		t.Fatalf("newClient options = %#v", gotOptions)
	}
	_, err = agent.newClient(context.Background(), []acp.McpServer{{
		Http: &acp.McpServerHttpInline{Name: "http", Url: "https://example.com", Headers: []acp.HttpHeader{{Name: "Authorization", Value: "Bearer token"}}},
	}}, nil)
	if err != nil {
		t.Fatalf("newClient with HTTP MCP returned error: %v", err)
	}
	if gotOptions.Env["CODEX_MCP_HEADER_HTTP_AUTHORIZATION"] != "Bearer token" {
		t.Fatalf("newClient MCP header env = %#v", gotOptions.Env)
	}
	_, err = agent.newClient(context.Background(), []acp.McpServer{{Sse: &acp.McpServerSseInline{Name: "sse", Url: "http://example.com"}}}, nil)
	if err == nil {
		t.Fatal("newClient accepted SSE MCP")
	}

	if codexThreadACPError(nil, nil, nil) != nil {
		t.Fatal("nil Codex thread error mapped to non-nil")
	}
	plainErr := errors.New("plain")
	if codexThreadACPError(plainErr, nil, nil) != plainErr {
		t.Fatal("plain Codex thread error was not preserved")
	}
	data := codexThreadErrorData("s", "")
	if len(data) != 1 || data[jsonFieldSessionID] != acp.SessionId("s") {
		t.Fatalf("thread error data without thread = %#v", data)
	}
}

func TestNativeThreadNotFoundMapsResourceNotFound(t *testing.T) {
	ctx := context.Background()

	resumeAgent := NewAgent(withClientFactory(func(context.Context, codex.Options) (codex.Client, error) {
		return &errorCodexClient{spyCodexClient: newSpyCodexClient(), resumeErr: codex.ErrThreadNotFound}, nil
	}))
	if _, err := resumeAgent.ResumeSession(ctx, ResumeSessionRequest("missing-session", "/tmp/project")); err == nil {
		t.Fatal("ResumeSession with missing native thread succeeded")
	} else {
		requireResourceNotFound(t, err, "missing-session")
	}

	loadAgent := NewAgent(withClientFactory(func(context.Context, codex.Options) (codex.Client, error) {
		return &errorCodexClient{spyCodexClient: newSpyCodexClient(), resumeErr: codex.ErrThreadNotFound}, nil
	}))
	entries := []SessionStoreEntry{SessionStoreEntry(`{"type":"event_msg","payload":{"type":"agent_message","message":"hi"}}`)}
	if _, err := loadAgent.loadMaterializedSession(ctx, LoadSessionRequest("stored-session", "/tmp/project"), entries); err == nil {
		t.Fatal("loadMaterializedSession with missing native thread succeeded")
	} else {
		requireResourceNotFound(t, err, "stored-session")
	}

	forkClient := &errorCodexClient{spyCodexClient: newSpyCodexClient(), forkErr: codex.ErrThreadNotFound}
	forkAgent := NewAgent(withClientFactory(func(context.Context, codex.Options) (codex.Client, error) { return forkClient, nil }))
	parent := newSession(forkAgent, "parent-session", "/tmp/project", nil, codex.Thread{ID: "parent-thread"}, newSpyCodexClient(), sessionMeta{})
	if err := forkAgent.storeStartedSession(parent); err != nil {
		t.Fatalf("store parent session: %v", err)
	}
	if _, err := forkAgent.UnstableForkSession(ctx, ForkSessionRequest("parent-session", "/tmp/project")); err == nil {
		t.Fatal("ForkSession with missing native thread succeeded")
	} else {
		requireResourceNotFound(t, err, "parent-session")
	}

	promptSession := &Session{agent: NewAgent(), id: "prompt-session", cwd: "/tmp/project", codexThreadID: "prompt-thread", client: &runEventsClient{runErr: codex.ErrThreadNotFound}}
	if _, err := promptSession.Prompt(ctx, TextPromptRequest("prompt-session", "hi")); err == nil {
		t.Fatal("Prompt with missing native thread succeeded")
	} else {
		requireResourceNotFound(t, err, "prompt-session")
	}

	replaySession := &Session{agent: NewAgent(), id: "replay-session", codexThreadID: "replay-thread", client: &errorCodexClient{spyCodexClient: newSpyCodexClient(), readErr: codex.ErrThreadNotFound}}
	if err := replaySession.replayThreadHistory(ctx); err == nil {
		t.Fatal("replayThreadHistory with missing native thread succeeded")
	} else {
		requireResourceNotFound(t, err, "replay-session")
	}

	extensionClient := &errorCodexClient{spyCodexClient: newSpyCodexClient()}
	extensionAgent := NewAgent(withClientFactory(func(context.Context, codex.Options) (codex.Client, error) { return extensionClient, nil }))
	resp, err := extensionAgent.NewSession(ctx, NewSessionRequest("/tmp/project"))
	if err != nil {
		t.Fatalf("NewSession for extension test returned error: %v", err)
	}
	extensionClient.readErr = codex.ErrThreadNotFound
	if _, err := extensionAgent.readCodexThread(ctx, mustJSONRaw(map[string]any{jsonFieldSessionID: resp.SessionId})); err == nil {
		t.Fatal("read extension with missing native thread succeeded")
	} else {
		requireResourceNotFound(t, err, resp.SessionId)
	}

	goalClient := &errorCodexClient{spyCodexClient: newSpyCodexClient()}
	goalAgent := NewAgent(WithCodexGoals(true), withClientFactory(func(context.Context, codex.Options) (codex.Client, error) { return goalClient, nil }))
	goalResp, err := goalAgent.NewSession(ctx, NewSessionRequest("/tmp/project"))
	if err != nil {
		t.Fatalf("NewSession for goal test returned error: %v", err)
	}
	goalClient.goalErr = codex.ErrThreadNotFound
	if _, err := goalAgent.setCodexGoal(ctx, mustJSONRaw(map[string]any{jsonFieldSessionID: goalResp.SessionId, codexGoalMetaKey: map[string]any{goalFieldObjective: "prove 1 + 1 = 2"}})); err == nil {
		t.Fatal("goal extension with missing native thread succeeded")
	} else {
		requireResourceNotFound(t, err, goalResp.SessionId)
	}
}

func TestSessionLifecycleRuntimeOptions(t *testing.T) {
	ctx := context.Background()
	client := newSpyCodexClient()
	var gotOptions codex.Options
	agent := NewAgent(
		WithEnv(map[string]string{"BASE": "base", "OVERRIDE": "base"}),
		withClientFactory(func(_ context.Context, options codex.Options) (codex.Client, error) {
			gotOptions = options
			return client, nil
		}),
	)

	resp, err := agent.NewSession(ctx, NewSessionRequest("/tmp/project", WithSessionCodexOptions(NewCodexOptions(
		WithCodexEnv(map[string]string{"OVERRIDE": "request", "REQUEST": "request"}),
		WithCodexApprovalPolicy("never"),
		WithCodexSandboxPolicy("workspace-write"),
	))))
	if err != nil {
		t.Fatalf("NewSession returned error: %v", err)
	}
	if gotOptions.Env["BASE"] != "base" || gotOptions.Env["OVERRIDE"] != "request" || gotOptions.Env["REQUEST"] != "request" {
		t.Fatalf("Codex process env = %#v", gotOptions.Env)
	}
	if client.start.ApprovalPolicy != "never" {
		t.Fatalf("thread approval policy = %#v", client.start.ApprovalPolicy)
	}
	if client.start.Sandbox != "workspace-write" {
		t.Fatalf("thread sandbox = %#v", client.start.Sandbox)
	}

	if _, err := agent.Prompt(ctx, TextPromptRequest(resp.SessionId, "hi")); err != nil {
		t.Fatalf("Prompt returned error: %v", err)
	}
	if client.lastTurn.ApprovalPolicy != "never" {
		t.Fatalf("turn approval policy = %#v", client.lastTurn.ApprovalPolicy)
	}
	if client.lastTurn.SandboxPolicy.(map[string]any)["type"] != "workspaceWrite" {
		t.Fatalf("turn sandbox = %#v", client.lastTurn.SandboxPolicy)
	}
	if client.lastTurn.SandboxPolicy.(map[string]any)["networkAccess"] != false {
		t.Fatalf("turn sandbox missing network policy: %#v", client.lastTurn.SandboxPolicy)
	}
	if client.lastTurn.SandboxPolicy.(map[string]any)["writableRoots"] == nil {
		t.Fatalf("turn sandbox writableRoots encoded nil: %#v", client.lastTurn.SandboxPolicy)
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
	} else {
		requireAgentClosed(t, err)
	}
	if _, err := closed.ResumeSession(ctx, ResumeSessionRequest("thread", "/tmp/project")); err == nil {
		t.Fatal("ResumeSession on closed agent succeeded")
	} else {
		requireAgentClosed(t, err)
	}
	closedCalls := map[string]func() error{
		"prompt": func() error {
			_, err := closed.Prompt(ctx, TextPromptRequest("thread", "hi"))
			return err
		},
		"cancel": func() error {
			return closed.Cancel(ctx, acp.CancelNotification{SessionId: "thread"})
		},
		"closeSession": func() error {
			_, err := closed.CloseSession(ctx, acp.CloseSessionRequest{SessionId: "thread"})
			return err
		},
		"list": func() error {
			_, err := closed.ListSessions(ctx, ListSessionsRequest())
			return err
		},
		"load": func() error {
			_, err := closed.LoadSession(ctx, LoadSessionRequest("thread", "/tmp/project"))
			return err
		},
		"fork": func() error {
			_, err := closed.UnstableForkSession(ctx, ForkSessionRequest("thread", "/tmp/project"))
			return err
		},
		"setConfig": func() error {
			_, err := closed.SetSessionConfigOption(ctx, acp.SetSessionConfigOptionRequest{
				ValueId: &acp.SetSessionConfigOptionValueId{SessionId: "thread", ConfigId: configMode, Value: acp.SessionConfigValueId(modePlan)},
			})
			return err
		},
		"authenticate": func() error {
			_, err := closed.Authenticate(ctx, acp.AuthenticateRequest{})
			return err
		},
		"logout": func() error {
			_, err := closed.Logout(ctx, acp.LogoutRequest{})
			return err
		},
		"extension": func() error {
			_, err := closed.HandleExtensionMethod(ctx, codexSessionImportMethod, mustJSONRaw(map[string]any{}))
			return err
		},
	}
	for name, call := range closedCalls {
		t.Run("closed_"+name, func(t *testing.T) {
			requireAgentClosed(t, call())
		})
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
	if _, err := agent.LoadSession(ctx, LoadSessionRequest("thread", "/tmp/project", WithSessionMeta(map[string]any{codexMetaKey: map[string]any{"options": map[string]any{"effort": "bad"}}}))); err == nil {
		t.Fatal("LoadSession accepted bad meta")
	}
	if _, err := agent.LoadSession(ctx, LoadSessionRequest("thread", "/tmp/project", WithSessionGoal(CodexGoal{Status: CodexGoalStatusActive}))); err == nil {
		t.Fatal("LoadSession accepted bad goal meta")
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
	goalStartClient := &errorCodexClient{spyCodexClient: newSpyCodexClient(), goalErr: errors.New("goal failed")}
	agent = NewAgent(WithCodexGoals(true), withClientFactory(func(context.Context, codex.Options) (codex.Client, error) { return goalStartClient, nil }))
	if _, err := agent.NewSession(ctx, NewSessionRequest("/tmp/project", WithSessionGoal(CodexGoal{Objective: "ship"}))); err == nil {
		t.Fatal("NewSession with initial goal error succeeded")
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
	goalResumeClient := &errorCodexClient{spyCodexClient: newSpyCodexClient(), goalErr: errors.New("goal failed")}
	agent = NewAgent(WithCodexGoals(true), withClientFactory(func(context.Context, codex.Options) (codex.Client, error) { return goalResumeClient, nil }))
	if _, err := agent.ResumeSession(ctx, ResumeSessionRequest("thread", "/tmp/project", WithSessionGoal(CodexGoal{Objective: "ship"}))); err == nil {
		t.Fatal("ResumeSession with initial goal error succeeded")
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
	} else {
		requireResourceNotFound(t, err, "missing")
	}
	if _, err := agent.CloseSession(ctx, acp.CloseSessionRequest{SessionId: "missing"}); err == nil {
		t.Fatal("CloseSession missing succeeded")
	} else {
		requireResourceNotFound(t, err, "missing")
	}
	existing, err := agent.LoadSession(ctx, LoadSessionRequest(resp.SessionId, "/tmp/project"))
	if err != nil {
		t.Fatalf("LoadSession existing returned error: %v", err)
	}
	if existing.Meta[codexMetaKey] == nil {
		t.Fatalf("existing load meta = %#v", existing.Meta)
	}
	client.goalErr = errors.New("goal failed")
	if _, err := agent.LoadSession(ctx, LoadSessionRequest(resp.SessionId, "/tmp/project", WithSessionGoal(CodexGoal{Objective: "ship"}))); err == nil {
		t.Fatal("LoadSession existing with goal error succeeded")
	}
	client.goalErr = nil

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
	if err := agent.Cancel(ctx, acp.CancelNotification{SessionId: "missing"}); err == nil {
		t.Fatal("Cancel missing session succeeded")
	}
	if got := modelList(ctx, nil); got != nil {
		t.Fatalf("modelList nil = %#v", got)
	}
	if clientAccountMeta(ctx, nil) != nil {
		t.Fatal("clientAccountMeta nil client returned data")
	}

	errConn := &errorAgentClient{recordingAgentClient: newRecordingAgentClient(), updateErr: errors.New("update failed")}
	agent.setAgentClient(errConn)
	if _, err := agent.SetSessionConfigOption(ctx, acp.SetSessionConfigOptionRequest{ValueId: &acp.SetSessionConfigOptionValueId{SessionId: resp.SessionId, ConfigId: configServiceTier, Value: "flex"}}); err == nil {
		t.Fatal("SetSessionConfigOption with update failure succeeded")
	}
}

func TestActiveLoadReplayAndResumeReplacement(t *testing.T) {
	ctx := context.Background()
	cwd := t.TempDir()
	conn := newRecordingAgentClient()
	store := NewInMemorySessionStore()
	firstClient := newSpyCodexClient()
	secondClient := newSpyCodexClient()
	clients := []codex.Client{firstClient, secondClient}
	var starts int
	agent := NewAgent(
		WithSessionStore(store),
		withClientFactory(func(context.Context, codex.Options) (codex.Client, error) {
			client := clients[starts]
			starts++
			return client, nil
		}),
	)
	agent.setAgentClient(conn)

	newResp, err := agent.NewSession(ctx, NewSessionRequest(cwd))
	if err != nil {
		t.Fatalf("NewSession returned error: %v", err)
	}
	if _, err := agent.LoadSession(ctx, LoadSessionRequest(newResp.SessionId, cwd)); err != nil {
		t.Fatalf("active LoadSession returned error: %v", err)
	}
	if starts != 1 {
		t.Fatalf("active load started %d clients, want 1", starts)
	}
	if len(conn.updates) == 0 || conn.updates[len(conn.updates)-1].Update.AgentMessageChunk == nil {
		t.Fatalf("active load did not replay thread history: %#v", conn.updates)
	}
	projectKey, err := projectKeyForDirectory(cwd)
	if err != nil {
		t.Fatalf("project key: %v", err)
	}
	if err := store.Append(ctx, SessionKey{ProjectKey: projectKey, SessionID: string(newResp.SessionId)}, []SessionStoreEntry{SessionStoreEntry(`{"type":"event_msg","payload":{"type":"user_message","message":"from store"}}`)}); err != nil {
		t.Fatalf("append store history: %v", err)
	}
	if _, err := agent.LoadSession(ctx, LoadSessionRequest(newResp.SessionId, cwd)); err != nil {
		t.Fatalf("active stored LoadSession returned error: %v", err)
	}
	if conn.updates[len(conn.updates)-1].Update.UserMessageChunk == nil {
		t.Fatalf("active stored load did not replay rollout: %#v", conn.updates)
	}

	if _, err := agent.ResumeSession(ctx, ResumeSessionRequest(newResp.SessionId, cwd)); err != nil {
		t.Fatalf("active ResumeSession returned error: %v", err)
	}
	if starts != 1 {
		t.Fatalf("active resume started %d clients, want 1", starts)
	}

	if _, err := agent.ResumeSession(ctx, ResumeSessionRequest(newResp.SessionId, t.TempDir())); err != nil {
		t.Fatalf("replacement ResumeSession returned error: %v", err)
	}
	if starts != 2 {
		t.Fatalf("replacement resume started %d clients, want 2", starts)
	}
	if !firstClient.closed {
		t.Fatal("replacement resume did not close the previous Codex client")
	}

	goalClient := &errorCodexClient{spyCodexClient: newSpyCodexClient(), goalErr: errors.New("goal failed")}
	goalAgent := NewAgent(WithCodexGoals(true), withClientFactory(func(context.Context, codex.Options) (codex.Client, error) {
		return goalClient, nil
	}))
	goalResp, err := goalAgent.NewSession(ctx, NewSessionRequest(cwd))
	if err != nil {
		t.Fatalf("goal NewSession returned error: %v", err)
	}
	if _, err := goalAgent.ResumeSession(ctx, ResumeSessionRequest(goalResp.SessionId, cwd, WithSessionGoal(CodexGoal{Objective: "ship"}))); err == nil {
		t.Fatal("active ResumeSession ignored goal error")
	}
}

func TestPromptFatalCodexProcessRemovesSession(t *testing.T) {
	ctx := context.Background()
	client := &runEventsClient{
		spyCodexClient: newSpyCodexClient(),
		events:         []codex.Event{{Kind: codex.EventError, Err: codex.ErrConnectionClosed}},
	}
	agent := NewAgent(withClientFactory(func(context.Context, codex.Options) (codex.Client, error) { return client, nil }))
	resp, err := agent.NewSession(ctx, NewSessionRequest("/tmp/project"))
	if err != nil {
		t.Fatalf("NewSession returned error: %v", err)
	}
	if _, err := agent.Prompt(ctx, TextPromptRequest(resp.SessionId, "hi")); err == nil {
		t.Fatal("Prompt with fatal Codex process error succeeded")
	} else if reqErr, ok := err.(*acp.RequestError); !ok || reqErr.Code != -32603 {
		t.Fatalf("fatal process error = %T %v, want ACP internal error", err, err)
	}
	if _, err := agent.Prompt(ctx, TextPromptRequest(resp.SessionId, "again")); err == nil {
		t.Fatal("fatal Codex process did not remove the session")
	}
}

func TestActiveLoadReplayErrors(t *testing.T) {
	ctx := context.Background()
	cwd := t.TempDir()

	loadErrAgent := NewAgent(
		WithSessionStore(errorSessionStore{loadErr: errors.New("load failed")}),
		withClientFactory(func(context.Context, codex.Options) (codex.Client, error) { return newSpyCodexClient(), nil }),
	)
	loadErrResp, err := loadErrAgent.NewSession(ctx, NewSessionRequest(cwd))
	if err != nil {
		t.Fatalf("loadErr NewSession returned error: %v", err)
	}
	if _, err := loadErrAgent.LoadSession(ctx, LoadSessionRequest(loadErrResp.SessionId, cwd)); err == nil {
		t.Fatal("active LoadSession ignored store load error")
	}

	store := NewInMemorySessionStore()
	badReplayAgent := NewAgent(
		WithSessionStore(store),
		withClientFactory(func(context.Context, codex.Options) (codex.Client, error) { return newSpyCodexClient(), nil }),
	)
	badReplayResp, err := badReplayAgent.NewSession(ctx, NewSessionRequest(cwd))
	if err != nil {
		t.Fatalf("badReplay NewSession returned error: %v", err)
	}
	projectKey, err := projectKeyForDirectory(cwd)
	if err != nil {
		t.Fatalf("project key: %v", err)
	}
	if err := store.Append(ctx, SessionKey{ProjectKey: projectKey, SessionID: string(badReplayResp.SessionId)}, []SessionStoreEntry{SessionStoreEntry(`{}`)}); err != nil {
		t.Fatalf("append bad store row: %v", err)
	}
	if _, err := badReplayAgent.LoadSession(ctx, LoadSessionRequest(badReplayResp.SessionId, cwd)); err == nil {
		t.Fatal("active LoadSession ignored rollout replay error")
	}

	readErrAgent := NewAgent(withClientFactory(func(context.Context, codex.Options) (codex.Client, error) {
		return readErrorClient{Client: newSpyCodexClient()}, nil
	}))
	readErrResp, err := readErrAgent.NewSession(ctx, NewSessionRequest(cwd))
	if err != nil {
		t.Fatalf("readErr NewSession returned error: %v", err)
	}
	if _, err := readErrAgent.LoadSession(ctx, LoadSessionRequest(readErrResp.SessionId, cwd)); err == nil {
		t.Fatal("active LoadSession ignored thread history replay error")
	}
}

func TestSessionFingerprintAndListHelperBranches(t *testing.T) {
	httpName := mcpServerName(acp.McpServer{Http: &acp.McpServerHttpInline{Name: "http"}})
	sseName := mcpServerName(acp.McpServer{Sse: &acp.McpServerSseInline{Name: "sse"}})
	acpName := mcpServerName(acp.McpServer{Acp: &acp.McpServerAcpInline{Name: "acp"}})
	stdioName := mcpServerName(acp.McpServer{Stdio: &acp.McpServerStdio{Name: "stdio"}})
	if httpName != "http" || sseName != "sse" || acpName != "acp" || stdioName != "stdio" || mcpServerName(acp.McpServer{}) != "" {
		t.Fatal("mcpServerName did not map all server kinds")
	}

	left := codexSessionStartFingerprint(codexSessionStart{
		Cwd:        "/repo",
		McpServers: []acp.McpServer{{Stdio: &acp.McpServerStdio{Name: "b"}}, {Stdio: &acp.McpServerStdio{Name: "a"}}},
		Meta: sessionMeta{
			Model:           "gpt",
			ReasoningEffort: "high",
			ServiceTier:     "flex",
			Personality:     "pragmatic",
			Env:             map[string]string{"A": "B"},
			ApprovalPolicy:  "never",
			SandboxPolicy:   "workspace-write",
			OutputSchema:    map[string]any{"type": "object"},
			RawMessages:     rawMessageConfig{All: true},
		},
		ResumeID:         "ignored",
		ForkParentID:     "ignored",
		MaterializedPath: "ignored",
	})
	right := codexSessionStartFingerprint(codexSessionStart{
		Cwd:        "/repo",
		McpServers: []acp.McpServer{{Stdio: &acp.McpServerStdio{Name: "a"}}, {Stdio: &acp.McpServerStdio{Name: "b"}}},
		Meta:       sessionMeta{Model: "gpt", ReasoningEffort: "high", ServiceTier: "flex", Personality: "pragmatic", Env: map[string]string{"A": "B"}, ApprovalPolicy: "never", SandboxPolicy: "workspace-write", OutputSchema: map[string]any{"type": "object"}, RawMessages: rawMessageConfig{All: true}},
	})
	if left != right {
		t.Fatalf("fingerprints differ after MCP sorting:\n%s\n%s", left, right)
	}
	if !strings.HasPrefix(jsonFingerprint(func() {}), "marshal-error:") {
		t.Fatal("jsonFingerprint did not report marshal errors")
	}

	agent := NewAgent()
	session := &Session{id: "s", fingerprint: left}
	agent.sessions["s"] = session
	if got := agent.activeSessionForStart("missing", codexSessionStart{}); got != nil {
		t.Fatalf("activeSessionForStart missing = %#v", got)
	}
	if got := agent.activeSessionForStart("s", codexSessionStart{Cwd: "/other"}); got != nil {
		t.Fatalf("activeSessionForStart mismatch = %#v", got)
	}
	if agent.removeSessionIf("s", &Session{id: "other"}) {
		t.Fatal("removeSessionIf removed a different session")
	}

	var infos []acp.SessionInfo
	seen := map[acp.SessionId]struct{}{"seen": {}}
	addSessionInfo(&infos, seen, acp.SessionInfo{})
	addSessionInfo(&infos, seen, acp.SessionInfo{SessionId: "seen"})
	addSessionInfo(&infos, seen, acp.SessionInfo{SessionId: "new"})
	if len(infos) != 1 || infos[0].SessionId != "new" {
		t.Fatalf("addSessionInfo result = %#v", infos)
	}

	negative := base64.RawURLEncoding.EncodeToString([]byte("-1"))
	if _, err := decodeListCursor(&negative); err == nil {
		t.Fatal("decodeListCursor accepted negative offset")
	}
	invalidBase64 := "!!!"
	if _, err := decodeListCursor(&invalidBase64); err == nil {
		t.Fatal("decodeListCursor accepted invalid base64")
	}
}

func TestSessionListLoadAndForkErrorBranches(t *testing.T) {
	ctx := context.Background()
	client := &errorCodexClient{spyCodexClient: newSpyCodexClient(), listThreads: []codex.Thread{}}
	agent := NewAgent(withClientFactory(func(context.Context, codex.Options) (codex.Client, error) { return client, nil }))
	active := newSession(agent, "active-session", "/tmp/project", []string{"/tmp/extra"}, codex.Thread{ID: "active-thread", Model: "gpt"}, client, sessionMeta{})
	if err := agent.storeStartedSession(active); err != nil {
		t.Fatalf("store active session: %v", err)
	}
	cwd := "/tmp/project"
	list, err := agent.ListSessions(ctx, ListSessionsRequest(WithListSessionsCwd(cwd)))
	if err != nil {
		t.Fatalf("ListSessions returned error: %v", err)
	}
	if len(list.Sessions) != 1 || list.Sessions[0].SessionId != "active-session" {
		t.Fatalf("cwd list = %#v", list.Sessions)
	}
	otherCwd := t.TempDir()
	list, err = agent.ListSessions(ctx, ListSessionsRequest(WithListSessionsCwd(otherCwd)))
	if err != nil {
		t.Fatalf("ListSessions cwd mismatch returned error: %v", err)
	}
	if len(list.Sessions) != 0 {
		t.Fatalf("cwd-mismatch list = %#v", list.Sessions)
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
	if _, err := agent.ResumeSession(ctx, ResumeSessionRequest("s", "/tmp/project")); err == nil {
		t.Fatal("ResumeSession with store load error succeeded")
	}
	if _, err := agent.ListSessions(ctx, ListSessionsRequest(WithListSessionsCwd("/tmp/project"))); err == nil {
		t.Fatal("ListSessions with store list error succeeded")
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
	loadGoalClient := &errorCodexClient{spyCodexClient: newSpyCodexClient(), goalErr: errors.New("goal failed")}
	agent = NewAgent(WithCodexGoals(true), withClientFactory(func(context.Context, codex.Options) (codex.Client, error) { return loadGoalClient, nil }))
	if _, err := agent.loadMaterializedSession(ctx, LoadSessionRequest("s", "/tmp/project", WithSessionGoal(CodexGoal{Objective: "ship"})), []SessionStoreEntry{SessionStoreEntry(`{"type":"event_msg","payload":{"type":"agent_message","message":"hi"}}`)}); err == nil {
		t.Fatal("loadMaterializedSession with goal restore error succeeded")
	}

	resumeEntries := []SessionStoreEntry{SessionStoreEntry(`{"type":"event_msg","payload":{"type":"agent_message","message":"hi"}}`)}
	if _, err := closed.resumeMaterializedSession(ctx, ResumeSessionRequest("s", "/tmp/project"), resumeEntries); err == nil {
		t.Fatal("resumeMaterializedSession on closed agent succeeded")
	}
	agent = NewAgent()
	if _, err := agent.resumeMaterializedSession(ctx, ResumeSessionRequest("s", "/tmp/project", WithSessionOutputSchema(map[string]any{})), resumeEntries); err == nil {
		t.Fatal("resumeMaterializedSession accepted bad meta")
	}
	t.Setenv("TMPDIR", "/path/that/does/not/exist")
	if _, err := agent.resumeMaterializedSession(ctx, ResumeSessionRequest("s", "/tmp/project"), resumeEntries); err == nil {
		t.Fatal("resumeMaterializedSession accepted materialize failure")
	}
	t.Setenv("TMPDIR", origTMP)
	if _, err := NewAgent().resumeMaterializedSession(ctx, ResumeSessionRequest("s", "/tmp/project", WithSessionMCPServers(acp.McpServer{Acp: &acp.McpServerAcpInline{Id: "acp", Name: "ACP"}})), resumeEntries); err == nil {
		t.Fatal("resumeMaterializedSession accepted ACP MCP without MCP client")
	}
	agent = NewAgent(withClientFactory(func(context.Context, codex.Options) (codex.Client, error) { return nil, errors.New("factory failed") }))
	if _, err := agent.resumeMaterializedSession(ctx, ResumeSessionRequest("s", "/tmp/project"), resumeEntries); err == nil {
		t.Fatal("resumeMaterializedSession accepted factory failure")
	}
	agent.setAgentClient(&recordingMCPAgentClient{recordingAgentClient: newRecordingAgentClient()})
	if _, err := agent.resumeMaterializedSession(ctx, ResumeSessionRequest("s", "/tmp/project", WithSessionMCPServers(acp.McpServer{Acp: &acp.McpServerAcpInline{Id: "acp", Name: "ACP"}})), resumeEntries); err == nil {
		t.Fatal("resumeMaterializedSession with MCP bridge accepted factory failure")
	}
	resumeErrClient = &errorCodexClient{spyCodexClient: newSpyCodexClient(), resumeErr: errors.New("resume failed")}
	agent = NewAgent(withClientFactory(func(context.Context, codex.Options) (codex.Client, error) { return resumeErrClient, nil }))
	if _, err := agent.resumeMaterializedSession(ctx, ResumeSessionRequest("s", "/tmp/project"), resumeEntries); err == nil {
		t.Fatal("resumeMaterializedSession accepted resume failure")
	}
	agent.setAgentClient(&recordingMCPAgentClient{recordingAgentClient: newRecordingAgentClient()})
	if _, err := agent.resumeMaterializedSession(ctx, ResumeSessionRequest("s", "/tmp/project", WithSessionMCPServers(acp.McpServer{Acp: &acp.McpServerAcpInline{Id: "acp", Name: "ACP"}})), resumeEntries); err == nil {
		t.Fatal("resumeMaterializedSession with MCP bridge accepted resume failure")
	}
	resumeMaterializedStoreCloseAgent := NewAgent()
	resumeMaterializedStoreCloseClient := &closingAccountClient{spyCodexClient: newSpyCodexClient(), agent: resumeMaterializedStoreCloseAgent}
	resumeMaterializedStoreCloseAgent.options.clientFactory = func(context.Context, codex.Options) (codex.Client, error) {
		return resumeMaterializedStoreCloseClient, nil
	}
	if _, err := resumeMaterializedStoreCloseAgent.resumeMaterializedSession(ctx, ResumeSessionRequest("s", "/tmp/project"), resumeEntries); err == nil {
		t.Fatal("resumeMaterializedSession with store close race succeeded")
	}
	resumeGoalClient := &errorCodexClient{spyCodexClient: newSpyCodexClient(), goalErr: errors.New("goal failed")}
	agent = NewAgent(WithCodexGoals(true), withClientFactory(func(context.Context, codex.Options) (codex.Client, error) { return resumeGoalClient, nil }))
	if _, err := agent.resumeMaterializedSession(ctx, ResumeSessionRequest("s", "/tmp/project", WithSessionGoal(CodexGoal{Objective: "ship"})), resumeEntries); err == nil {
		t.Fatal("resumeMaterializedSession with goal restore error succeeded")
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
	goalForkClient := &errorCodexClient{spyCodexClient: newSpyCodexClient(), goalErr: errors.New("goal failed")}
	agent = NewAgent(WithCodexGoals(true), withClientFactory(func(context.Context, codex.Options) (codex.Client, error) { return goalForkClient, nil }))
	goalParent := newSession(agent, "goal-parent", "/tmp/project", nil, codex.Thread{ID: "goal-parent-thread"}, goalForkClient, sessionMeta{})
	if err := agent.storeStartedSession(goalParent); err != nil {
		t.Fatalf("store goal parent: %v", err)
	}
	if _, err := agent.UnstableForkSession(ctx, ForkSessionRequest("goal-parent", "/tmp/project", WithSessionGoal(CodexGoal{Objective: "ship"}))); err == nil {
		t.Fatal("Fork with initial goal error succeeded")
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
	if summary := storedAgent.storedGoalSummary(ctx, projectKey, "stored"); summary != nil {
		t.Fatalf("missing goal sidecar summary = %#v", summary)
	}
	if err := store.Replace(ctx, SessionKey{ProjectKey: projectKey, SessionID: "stored", Subpath: sessionStoreGoalSubpath}, []SessionStoreEntry{SessionStoreEntry(`bad`)}); err != nil {
		t.Fatalf("replace bad sidecar: %v", err)
	}
	if summary := storedAgent.storedGoalSummary(ctx, projectKey, "stored"); summary != nil {
		t.Fatalf("bad goal sidecar summary = %#v", summary)
	}
	if err := store.Replace(ctx, SessionKey{ProjectKey: projectKey, SessionID: "stored", Subpath: sessionStoreGoalSubpath}, []SessionStoreEntry{SessionStoreEntry(`{"goal":null}`)}); err != nil {
		t.Fatalf("replace clear sidecar: %v", err)
	}
	if summary := storedAgent.storedGoalSummary(ctx, projectKey, "stored"); summary != nil {
		t.Fatalf("clear goal sidecar summary = %#v", summary)
	}
	if err := store.Replace(ctx, SessionKey{ProjectKey: projectKey, SessionID: "stored", Subpath: sessionStoreGoalSubpath}, []SessionStoreEntry{SessionStoreEntry(`{"goal":{"objective":"Stored goal","status":"active"}}`)}); err != nil {
		t.Fatalf("replace valid sidecar: %v", err)
	}
	if summary := storedAgent.storedGoalSummary(ctx, projectKey, "stored").(map[string]any); summary[goalFieldObjective] != "Stored goal" {
		t.Fatalf("valid goal sidecar summary = %#v", summary)
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

func TestListSessionsPaginationAndCursorErrors(t *testing.T) {
	ctx := context.Background()
	cwd := t.TempDir()
	additional := t.TempDir()
	agent := NewAgent(withClientFactory(func(context.Context, codex.Options) (codex.Client, error) {
		return &errorCodexClient{spyCodexClient: newSpyCodexClient(), listThreads: []codex.Thread{}}, nil
	}))
	for i := 0; i < listSessionsPageSize+1; i++ {
		id := acp.SessionId("session-" + strconv.Itoa(i))
		session := newSession(agent, id, cwd, []string{additional}, codex.Thread{ID: "thread-" + strconv.Itoa(i)}, newSpyCodexClient(), sessionMeta{})
		if err := agent.storeStartedSession(session); err != nil {
			t.Fatalf("store session %d: %v", i, err)
		}
	}

	first, err := agent.ListSessions(ctx, ListSessionsRequest(WithListSessionsCwd(cwd)))
	if err != nil {
		t.Fatalf("first ListSessions returned error: %v", err)
	}
	if len(first.Sessions) != listSessionsPageSize || first.NextCursor == nil {
		t.Fatalf("first page len=%d cursor=%v", len(first.Sessions), first.NextCursor)
	}
	second, err := agent.ListSessions(ctx, ListSessionsRequest(WithListSessionsCwd(cwd), WithListSessionsCursor(*first.NextCursor)))
	if err != nil {
		t.Fatalf("second ListSessions returned error: %v", err)
	}
	if len(second.Sessions) != 1 || second.NextCursor != nil {
		t.Fatalf("second page len=%d cursor=%v", len(second.Sessions), second.NextCursor)
	}
	if _, err := agent.ListSessions(ctx, ListSessionsRequest(WithListSessionsCwd(cwd), WithListSessionsCursor("bad"))); err == nil {
		t.Fatal("ListSessions accepted invalid cursor")
	}
	pastEnd := encodeListCursor(listSessionsPageSize + 2)
	if _, err := agent.ListSessions(ctx, ListSessionsRequest(WithListSessionsCwd(cwd), WithListSessionsCursor(pastEnd))); err == nil {
		t.Fatal("ListSessions accepted cursor past end")
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
	if resume.ConfigOptions == nil {
		t.Fatalf("resume response = %#v", resume)
	}
	requireNoTopLevelConfigState(t, resume)
	if err := agent.Cancel(ctx, acp.CancelNotification{SessionId: "thread-1"}); err != nil {
		t.Fatalf("Cancel returned error: %v", err)
	}
	if _, err := agent.SetSessionConfigOption(ctx, acp.SetSessionConfigOptionRequest{
		ValueId: &acp.SetSessionConfigOptionValueId{
			SessionId: "thread-1",
			ConfigId:  configMode,
			Value:     acp.SessionConfigValueId(modePlan),
		},
	}); err != nil {
		t.Fatalf("SetSessionConfigOption mode returned error: %v", err)
	}
	if _, err := agent.SetSessionConfigOption(ctx, acp.SetSessionConfigOptionRequest{
		ValueId: &acp.SetSessionConfigOptionValueId{
			SessionId: "thread-1",
			ConfigId:  configModel,
			Value:     "gpt-other",
		},
	}); err != nil {
		t.Fatalf("SetSessionConfigOption model returned error: %v", err)
	}
	fork, err := agent.UnstableForkSession(ctx, ForkSessionRequest("thread-1", "/tmp/project"))
	if err != nil {
		t.Fatalf("UnstableForkSession returned error: %v", err)
	}
	if fork.SessionId == "" || fork.ConfigOptions == nil {
		t.Fatalf("fork response = %#v", fork)
	}
	requireNoTopLevelConfigState(t, fork)
	if _, err := agent.SetSessionConfigOption(ctx, acp.SetSessionConfigOptionRequest{}); err == nil {
		t.Fatal("empty config option succeeded")
	}
	if _, err := agent.SetSessionConfigOption(ctx, acp.SetSessionConfigOptionRequest{
		ValueId: &acp.SetSessionConfigOptionValueId{
			SessionId: "thread-1",
			ConfigId:  configMode,
			Value:     "bad",
		},
	}); err == nil {
		t.Fatal("bad mode config succeeded")
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
