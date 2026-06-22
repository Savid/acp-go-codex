package codexacp

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/coder/acp-go-sdk"
	"github.com/savid/acp-go-codex/internal/codex"
)

func TestExtensionAuthAndConfigAdditionalBranches(t *testing.T) {
	ctx := context.Background()
	client := &errorCodexClient{spyCodexClient: newSpyCodexClient()}
	agent := NewAgent(withClientFactory(func(context.Context, codex.Options) (codex.Client, error) { return client, nil }))
	agent.setAgentClient(newRecordingAgentClient())
	resp, err := agent.NewSession(ctx, NewSessionRequest("/tmp/project"))
	if err != nil {
		t.Fatalf("NewSession returned error: %v", err)
	}
	session := agent.sessionMust(resp.SessionId)
	session.setTurnID("turn-1")

	if _, err := agent.HandleExtensionMethod(ctx, "_missing", json.RawMessage(`{}`)); err == nil {
		t.Fatal("unknown extension succeeded")
	}
	if _, err := agent.HandleExtensionMethod(ctx, codexSessionImportChunkMethod, json.RawMessage(`{"sessionId":"s","cwd":"/tmp/project","entries":[{"type":"event_msg"}]}`)); err != nil {
		t.Fatalf("chunk extension returned error: %v", err)
	}
	if _, err := agent.HandleExtensionMethod(ctx, codexSessionAbortImportMethod, json.RawMessage(`{"importId":"missing"}`)); err != nil {
		t.Fatalf("abort extension returned error: %v", err)
	}
	if _, err := agent.HandleExtensionMethod(ctx, codexSessionCommitImportMethod, json.RawMessage(`{"importId":"missing"}`)); err == nil {
		t.Fatal("commit missing import succeeded")
	}
	if _, _, err := agent.extensionSession(codexSessionExtensionParams{SessionID: "missing"}); err == nil {
		t.Fatal("extensionSession missing session succeeded")
	}
	if got, threadID, err := agent.extensionSession(codexSessionExtensionParams{ThreadID: session.codexThreadID}); err != nil || got != session || threadID != session.codexThreadID {
		t.Fatalf("extensionSession by thread got=%#v thread=%q err=%v", got, threadID, err)
	}
	if agent.sessionByCodexThread(" ") != nil {
		t.Fatal("blank Codex thread lookup returned session")
	}

	invalidJSON := json.RawMessage(`{bad}`)
	for name, call := range map[string]func() (any, error){
		"steer": func() (any, error) { return agent.steerCodexTurn(ctx, invalidJSON) },
		"compact": func() (any, error) {
			return agent.compactCodexThread(ctx, invalidJSON)
		},
		"review": func() (any, error) {
			return agent.startCodexReview(ctx, invalidJSON)
		},
		"read": func() (any, error) {
			return agent.readCodexThread(ctx, invalidJSON)
		},
		"turns": func() (any, error) {
			return agent.listCodexThreadTurns(ctx, invalidJSON)
		},
		"collaboration": func() (any, error) {
			return agent.listCodexCollaborationModes(ctx, invalidJSON)
		},
		"mcpStatus": func() (any, error) {
			return agent.listCodexMCPServerStatus(ctx, invalidJSON)
		},
	} {
		if _, err := call(); err == nil {
			t.Fatalf("%s accepted invalid JSON", name)
		}
	}

	emptyTurnSession := newSession(agent, "empty-turn", "/tmp/project", nil, codex.Thread{ID: "empty-thread"}, client, sessionMeta{})
	if err := agent.storeStartedSession(emptyTurnSession); err != nil {
		t.Fatalf("store empty-turn session: %v", err)
	}
	if _, err := agent.steerCodexTurn(ctx, json.RawMessage(`{"sessionId":"empty-turn","input":[{"type":"text","text":"hi"}]}`)); err == nil {
		t.Fatal("steer without expected turn succeeded")
	}
	if _, err := agent.steerCodexTurn(ctx, json.RawMessage(`{"sessionId":"`+string(resp.SessionId)+`","expectedTurnId":"turn-1","prompt":[{"type":"audio","audio":{"data":"x","mimeType":"audio/wav"}}]}`)); err == nil {
		t.Fatal("steer accepted unsupported prompt content")
	}
	if _, err := agent.steerCodexTurn(ctx, json.RawMessage(`{"sessionId":"`+string(resp.SessionId)+`","expectedTurnId":"turn-1"}`)); err == nil {
		t.Fatal("steer without input succeeded")
	}
	client.steerErr = errors.New("steer failed")
	if _, err := agent.steerCodexTurn(ctx, json.RawMessage(`{"sessionId":"`+string(resp.SessionId)+`","expectedTurnId":"turn-1","input":[{"type":"text","text":"hi"}]}`)); err == nil {
		t.Fatal("steer provider error succeeded")
	}
	client.steerErr = nil
	client.compactErr = errors.New("compact failed")
	if _, err := agent.compactCodexThread(ctx, json.RawMessage(`{"threadId":"`+session.codexThreadID+`"}`)); err == nil {
		t.Fatal("compact provider error succeeded")
	}
	client.compactErr = nil
	client.reviewErr = errors.New("review failed")
	if _, err := agent.startCodexReview(ctx, json.RawMessage(`{"sessionId":"`+string(resp.SessionId)+`"}`)); err == nil {
		t.Fatal("review provider error succeeded")
	}
	client.reviewErr = nil
	client.readErr = errors.New("read failed")
	if _, err := agent.readCodexThread(ctx, json.RawMessage(`{"sessionId":"`+string(resp.SessionId)+`"}`)); err == nil {
		t.Fatal("read provider error succeeded")
	}
	client.readErr = nil
	client.turnsErr = errors.New("turns failed")
	if _, err := agent.listCodexThreadTurns(ctx, json.RawMessage(`{"sessionId":"`+string(resp.SessionId)+`"}`)); err == nil {
		t.Fatal("turns provider error succeeded")
	}
	client.turnsErr = nil
	client.collaborationErr = errors.New("collaboration failed")
	if _, err := agent.listCodexCollaborationModes(ctx, json.RawMessage(`{"sessionId":"`+string(resp.SessionId)+`"}`)); err == nil {
		t.Fatal("collaboration provider error succeeded")
	}
	client.collaborationErr = nil
	client.mcpStatusErr = errors.New("mcp status failed")
	if _, err := agent.listCodexMCPServerStatus(ctx, json.RawMessage(`{"sessionId":"`+string(resp.SessionId)+`"}`)); err == nil {
		t.Fatal("mcp status provider error succeeded")
	}
	client.mcpStatusErr = nil

	if values := modelConfigValues("", []codex.Model{{}, {ID: "a", Name: "A"}, {ID: "a", Name: "dup"}}); len(values) != 1 {
		t.Fatalf("model values = %#v", values)
	}
	if opts := codexConfigOptions("gpt", "", "", "", "", nil); len(opts) != 2 || opts[1].Select.CurrentValue != acp.SessionConfigValueId(modeDefault) {
		t.Fatalf("config options = %#v", opts)
	}
	if _, err := agent.SetSessionConfigOption(ctx, acp.SetSessionConfigOptionRequest{ValueId: &acp.SetSessionConfigOptionValueId{SessionId: resp.SessionId, ConfigId: configModel, Value: "gpt-other"}}); err != nil {
		t.Fatalf("SetSessionConfigOption model returned error: %v", err)
	}
	if _, err := agent.SetSessionConfigOption(ctx, acp.SetSessionConfigOptionRequest{ValueId: &acp.SetSessionConfigOptionValueId{SessionId: resp.SessionId, ConfigId: configPersonality, Value: "friendly"}}); err != nil {
		t.Fatalf("SetSessionConfigOption personality returned error: %v", err)
	}
	held := session.turnQueue()
	held <- struct{}{}
	canceled := canceledContext()
	if _, err := agent.SetSessionConfigOption(canceled, acp.SetSessionConfigOptionRequest{ValueId: &acp.SetSessionConfigOptionValueId{SessionId: resp.SessionId, ConfigId: configModel, Value: "blocked"}}); err == nil {
		t.Fatal("SetSessionConfigOption with canceled acquire succeeded")
	}
	<-held

	if _, err := agent.Authenticate(ctx, acp.AuthenticateRequest{MethodId: "wrong"}); err == nil {
		t.Fatal("Authenticate accepted wrong method")
	}
	if _, err := agent.Authenticate(ctx, acp.AuthenticateRequest{MethodId: authMethodChatGPTAuthTokens}); err == nil {
		t.Fatal("Authenticate accepted missing token metadata")
	}
	tokenMeta := map[string]any{codexMetaKey: map[string]any{"auth": map[string]any{authChatGPTAuthTokensMetaPath: map[string]any{"accessToken": "a", "expiresAt": float64(7)}}}}
	authAgent := NewAgent(withClientFactory(func(context.Context, codex.Options) (codex.Client, error) { return nil, errors.New("factory failed") }))
	if _, err := authAgent.Authenticate(ctx, acp.AuthenticateRequest{MethodId: authMethodChatGPTAuthTokens, Meta: tokenMeta}); err == nil {
		t.Fatal("Authenticate accepted factory failure")
	}
	client.loginErr = errors.New("login failed")
	if _, err := agent.Authenticate(ctx, acp.AuthenticateRequest{MethodId: authMethodChatGPTAuthTokens, Meta: tokenMeta}); err == nil {
		t.Fatal("Authenticate accepted login failure")
	}
	client.loginErr = nil

	logoutAgent := NewAgent(
		WithAllowAccountLogout(true),
		withClientFactory(func(context.Context, codex.Options) (codex.Client, error) {
			return nil, errors.New("logout factory failed")
		}),
	)
	logoutAgent.sessions["s"] = &Session{agent: logoutAgent, id: "s", client: &errorCodexClient{spyCodexClient: newSpyCodexClient(), closeErr: errors.New("close failed")}}
	if _, err := logoutAgent.Logout(ctx, acp.LogoutRequest{}); err == nil || !strings.Contains(err.Error(), "close failed") || !strings.Contains(err.Error(), "logout factory failed") {
		t.Fatalf("logout close/factory error = %v", err)
	}
	client.logoutErr = errors.New("logout failed")
	logoutAgent = NewAgent(
		WithAllowAccountLogout(true),
		withClientFactory(func(context.Context, codex.Options) (codex.Client, error) { return client, nil }),
	)
	if _, err := logoutAgent.Logout(ctx, acp.LogoutRequest{}); err == nil || !strings.Contains(err.Error(), "logout failed") {
		t.Fatalf("logout provider error = %v", err)
	}
}

func TestExtensionErrorBranches(t *testing.T) {
	client := newSpyCodexClient()
	agent := NewAgent(withClientFactory(func(context.Context, codex.Options) (codex.Client, error) { return client, nil }))
	ctx := context.Background()
	resp, err := agent.NewSession(ctx, NewSessionRequest("/tmp/project"))
	if err != nil {
		t.Fatalf("NewSession returned error: %v", err)
	}
	session := agent.sessionMust(resp.SessionId)

	if got, _, err := agent.extensionSession(codexSessionExtensionParams{ThreadID: session.codexThreadID}); err != nil || got != session {
		t.Fatalf("extensionSession by thread got=%#v err=%v", got, err)
	}
	if _, _, err := agent.extensionSession(codexSessionExtensionParams{}); err == nil {
		t.Fatal("extensionSession without ids succeeded")
	}

	badJSONMethods := []string{
		codexTurnSteerMethod,
		codexThreadCompactMethod,
		codexReviewStartMethod,
		codexThreadReadMethod,
		codexThreadTurnsListMethod,
		codexCollaborationListMethod,
		codexMCPServerStatusListMethod,
	}
	for _, method := range badJSONMethods {
		if _, err := agent.HandleExtensionMethod(ctx, method, json.RawMessage(`{bad}`)); err == nil {
			t.Fatalf("%s accepted bad JSON", method)
		}
	}
	if _, err := agent.HandleExtensionMethod(ctx, codexTurnSteerMethod, json.RawMessage(`{"sessionId":"`+string(resp.SessionId)+`"}`)); err == nil {
		t.Fatal("steer without active turn succeeded")
	}
	session.setTurnID("turn-1")
	if _, err := agent.HandleExtensionMethod(ctx, codexTurnSteerMethod, json.RawMessage(`{"sessionId":"`+string(resp.SessionId)+`"}`)); err == nil {
		t.Fatal("steer without input succeeded")
	}
	if _, err := agent.HandleExtensionMethod(ctx, codexTurnSteerMethod, json.RawMessage(`{"sessionId":"`+string(resp.SessionId)+`","prompt":[{"type":"audio"}]}`)); err == nil {
		t.Fatal("steer accepted unsupported prompt")
	}
	review, err := agent.HandleExtensionMethod(ctx, codexReviewStartMethod, json.RawMessage(`{"sessionId":"`+string(resp.SessionId)+`"}`))
	if err != nil {
		t.Fatalf("review default target returned error: %v", err)
	}
	if review.(map[string]any)["status"] != "reviewing" {
		t.Fatalf("review response = %#v", review)
	}
}

func TestExtensionMethodsUseNativeCodexClient(t *testing.T) {
	client := newSpyCodexClient()
	agent := NewAgent(withClientFactory(func(context.Context, codex.Options) (codex.Client, error) { return client, nil }))

	ctx := context.Background()
	resp, err := agent.NewSession(ctx, NewSessionRequest("/tmp/project"))
	if err != nil {
		t.Fatalf("NewSession returned error: %v", err)
	}
	session, err := agent.session(resp.SessionId)
	if err != nil {
		t.Fatalf("session lookup returned error: %v", err)
	}
	session.setTurnID("turn-1")

	params := json.RawMessage(`{"sessionId":"` + string(resp.SessionId) + `","prompt":[{"type":"text","text":"more"}]}`)
	result, err := agent.HandleExtensionMethod(ctx, codexTurnSteerMethod, params)
	if err != nil {
		t.Fatalf("steer extension returned error: %v", err)
	}
	resultMap, ok := result.(map[string]any)
	if !ok || resultMap["turnId"] != "turn-1" {
		t.Fatalf("steer result = %#v", result)
	}

	if _, err := agent.HandleExtensionMethod(ctx, codexThreadCompactMethod, json.RawMessage(`{"sessionId":"`+string(resp.SessionId)+`"}`)); err != nil {
		t.Fatalf("compact extension returned error: %v", err)
	}
	if _, err := agent.HandleExtensionMethod(ctx, codexReviewStartMethod, json.RawMessage(`{"sessionId":"`+string(resp.SessionId)+`","target":{"type":"custom","instructions":"check"}}`)); err != nil {
		t.Fatalf("review extension returned error: %v", err)
	}
	if _, err := agent.HandleExtensionMethod(ctx, codexThreadReadMethod, json.RawMessage(`{"sessionId":"`+string(resp.SessionId)+`"}`)); err != nil {
		t.Fatalf("read extension returned error: %v", err)
	}
	if _, err := agent.HandleExtensionMethod(ctx, codexThreadTurnsListMethod, json.RawMessage(`{"sessionId":"`+string(resp.SessionId)+`","limit":1}`)); err != nil {
		t.Fatalf("turns extension returned error: %v", err)
	}
	if _, err := agent.HandleExtensionMethod(ctx, codexCollaborationListMethod, json.RawMessage(`{"sessionId":"`+string(resp.SessionId)+`"}`)); err != nil {
		t.Fatalf("collaboration extension returned error: %v", err)
	}
	if _, err := agent.HandleExtensionMethod(ctx, codexMCPServerStatusListMethod, json.RawMessage(`{"sessionId":"`+string(resp.SessionId)+`"}`)); err != nil {
		t.Fatalf("mcp status extension returned error: %v", err)
	}

	client.mu.Lock()
	defer client.mu.Unlock()
	if client.steer.ExpectedTurnID != "turn-1" || len(client.steer.Input) != 1 {
		t.Fatalf("steer request = %#v", client.steer)
	}
	if client.compact.ThreadID == "" || client.review.Target["type"] != "custom" || client.turns.Limit != 1 {
		t.Fatalf("native calls not recorded: compact=%#v review=%#v turns=%#v", client.compact, client.review, client.turns)
	}
}

func TestExtensionProtocolErrorPaths(t *testing.T) {
	ctx := context.Background()
	client := newSpyCodexClient()
	agent := NewAgent(withClientFactory(func(context.Context, codex.Options) (codex.Client, error) { return client, nil }))
	resp, err := agent.NewSession(ctx, NewSessionRequest("/tmp/project"))
	if err != nil {
		t.Fatalf("NewSession returned error: %v", err)
	}
	session := agent.sessionMust(resp.SessionId)
	session.setTurnID("turn-1")
	for method, params := range map[string]json.RawMessage{
		codexTurnSteerMethod:           json.RawMessage(`{"sessionId":"missing","expectedTurnId":"turn","input":[{"type":"text","text":"x"}]}`),
		codexThreadCompactMethod:       json.RawMessage(`{"sessionId":"missing"}`),
		codexReviewStartMethod:         json.RawMessage(`{"sessionId":"missing"}`),
		codexThreadReadMethod:          json.RawMessage(`{"sessionId":"missing"}`),
		codexThreadTurnsListMethod:     json.RawMessage(`{"sessionId":"missing"}`),
		codexCollaborationListMethod:   json.RawMessage(`{"sessionId":"missing"}`),
		codexMCPServerStatusListMethod: json.RawMessage(`{"sessionId":"missing"}`),
	} {
		if _, err := agent.HandleExtensionMethod(ctx, method, params); err == nil {
			t.Fatalf("%s accepted missing session", method)
		}
	}
}
