package codexacp

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	"github.com/coder/acp-go-sdk"
	"github.com/savid/acp-go-codex/internal/codex"
)

func requireRequestError(t *testing.T, err error, code int, message string) {
	t.Helper()
	reqErr, ok := err.(*acp.RequestError)
	if !ok {
		t.Fatalf("error = %T %v, want ACP request error", err, err)
	}
	if reqErr.Code != code || reqErr.Message != message {
		t.Fatalf("request error = %#v, want code=%d message=%q", reqErr, code, message)
	}
}

func TestForkIsExtensionOnly(t *testing.T) {
	ctx := context.Background()
	client := newSpyCodexClient()
	agent := NewAgent(withClientFactory(func(context.Context, codex.Options) (codex.Client, error) {
		return client, nil
	}))

	parent, err := agent.NewSession(ctx, NewSessionRequest("/tmp/project"))
	if err != nil {
		t.Fatalf("NewSession returned error: %v", err)
	}
	if _, ok := any(agent).(interface {
		UnstableForkSession(context.Context, acp.UnstableForkSessionRequest) (acp.UnstableForkSessionResponse, error)
	}); ok {
		t.Fatal("Agent exposes UnstableForkSession")
	}

	raw, err := json.Marshal(ForkSessionRequest(parent.SessionId, "/tmp/project"))
	if err != nil {
		t.Fatalf("marshal fork request: %v", err)
	}
	resp, err := agent.HandleExtensionMethod(ctx, ForkSessionMethod, raw)
	if err != nil {
		t.Fatalf("HandleExtensionMethod fork returned error: %v", err)
	}
	fork, ok := resp.(acp.UnstableForkSessionResponse)
	if !ok || fork.SessionId == "" {
		t.Fatalf("fork response = %#v", resp)
	}

	if _, err := agent.HandleExtensionMethod(ctx, acp.AgentMethodSessionFork, raw); err == nil {
		t.Fatal("stable session/fork extension route succeeded")
	} else {
		requireRequestError(t, err, -32601, "Method not found")
	}
}

func TestLocalDispatcherDoesNotRouteDeletedSessionMethods(t *testing.T) {
	agent := NewAgent()
	conn := &localAgentConnection{agent: agent}
	conn.initialized.Store(true)
	ctx := context.Background()

	raw, err := json.Marshal(ForkSessionRequest("parent", "/tmp/project"))
	if err != nil {
		t.Fatalf("marshal fork request: %v", err)
	}
	if _, reqErr := conn.handle(ctx, acp.AgentMethodSessionFork, raw); reqErr == nil || reqErr.Code != -32601 {
		t.Fatalf("session/fork request error = %#v, want method-not-found", reqErr)
	}

	modeRaw, err := json.Marshal(acp.SetSessionModeRequest{SessionId: "s", ModeId: modePlan})
	if err != nil {
		t.Fatalf("marshal mode request: %v", err)
	}
	if _, reqErr := conn.handle(ctx, acp.AgentMethodSessionSetMode, modeRaw); reqErr == nil || reqErr.Code != -32601 {
		t.Fatalf("session/set_mode request error = %#v, want method-not-found", reqErr)
	}
}

func TestDeleteSessionTombstonesStoreAndBlocksLoadResume(t *testing.T) {
	ctx := context.Background()
	store := NewInMemorySessionStore()
	client := newSpyCodexClient()
	agent := NewAgent(
		WithSessionStore(store),
		withClientFactory(func(context.Context, codex.Options) (codex.Client, error) { return client, nil }),
	)

	resp, err := agent.NewSession(ctx, NewSessionRequest("/tmp/project"))
	if err != nil {
		t.Fatalf("NewSession returned error: %v", err)
	}
	if err := store.Replace(ctx, SessionKey{SessionID: string(resp.SessionId)}, []SessionStoreReplacement{{
		Key:     SessionKey{SessionID: string(resp.SessionId)},
		Entries: []SessionStoreEntry{SessionStoreEntry(`{"type":"event_msg","payload":{"type":"agent_message","message":"hi"}}`)},
	}}); err != nil {
		t.Fatalf("seed store: %v", err)
	}

	if _, err := agent.UnstableDeleteSession(ctx, acp.UnstableDeleteSessionRequest{SessionId: resp.SessionId}); err != nil {
		t.Fatalf("DeleteSession returned error: %v", err)
	}
	if !containsString(client.deletedThreadSnapshot(), "thread-1") {
		t.Fatalf("native thread was not deleted: %#v", client.deletedThreadSnapshot())
	}
	entries, err := store.Load(ctx, SessionKey{SessionID: string(resp.SessionId)})
	if err != nil {
		t.Fatalf("Load tombstoned store returned error: %v", err)
	}
	if len(entries) != 0 {
		t.Fatalf("tombstoned entries = %#v", entries)
	}
	if _, err := agent.ResumeSession(ctx, ResumeSessionRequest(resp.SessionId, "/tmp/project")); err == nil {
		t.Fatal("ResumeSession after delete succeeded")
	} else {
		requireRequestError(t, err, -32002, "Resource not found")
	}
	if _, err := agent.LoadSession(ctx, LoadSessionRequest(resp.SessionId, "/tmp/project")); err == nil {
		t.Fatal("LoadSession after delete succeeded")
	} else {
		requireRequestError(t, err, -32002, "Resource not found")
	}
}

func TestDeleteSessionSurfacesNativeCleanupErrorAfterTombstone(t *testing.T) {
	ctx := context.Background()
	store := NewInMemorySessionStore()
	cleanupErr := errors.New("delete native failed")
	client := &errorCodexClient{spyCodexClient: newSpyCodexClient(), deleteErr: cleanupErr}
	agent := NewAgent(
		WithSessionStore(store),
		withClientFactory(func(context.Context, codex.Options) (codex.Client, error) { return client, nil }),
	)

	resp, err := agent.NewSession(ctx, NewSessionRequest("/tmp/project"))
	if err != nil {
		t.Fatalf("NewSession returned error: %v", err)
	}
	if err := store.Replace(ctx, SessionKey{SessionID: string(resp.SessionId)}, []SessionStoreReplacement{{
		Key:     SessionKey{SessionID: string(resp.SessionId)},
		Entries: []SessionStoreEntry{SessionStoreEntry(`{"type":"event_msg","payload":{"type":"agent_message","message":"hi"}}`)},
	}}); err != nil {
		t.Fatalf("seed store: %v", err)
	}

	if _, err := agent.UnstableDeleteSession(ctx, DeleteSessionRequest(resp.SessionId)); !errors.Is(err, cleanupErr) {
		t.Fatalf("DeleteSession error = %v, want cleanup error", err)
	}
	entries, err := store.Load(ctx, SessionKey{SessionID: string(resp.SessionId)})
	if err != nil {
		t.Fatalf("Load tombstoned store returned error: %v", err)
	}
	if len(entries) != 0 {
		t.Fatalf("tombstoned entries = %#v", entries)
	}
	if _, err := agent.ResumeSession(ctx, ResumeSessionRequest(resp.SessionId, "/tmp/project")); err == nil {
		t.Fatal("ResumeSession after failed cleanup succeeded")
	} else {
		requireRequestError(t, err, -32002, "Resource not found")
	}
}

func TestDeleteSessionTombstoneSurvivesRestartAndRetriesNativeCleanup(t *testing.T) {
	ctx := context.Background()
	store := NewInMemorySessionStore()
	sessionID := acp.SessionId("11111111-1111-4111-8111-111111111111")
	if err := store.Delete(ctx, SessionKey{SessionID: string(sessionID)}); err != nil {
		t.Fatalf("seed tombstone: %v", err)
	}

	nativeClient := &errorCodexClient{
		spyCodexClient: newSpyCodexClient(),
		listThreads: []codex.Thread{{
			ID:        "thread-native",
			SessionID: string(sessionID),
			Cwd:       "/tmp/project",
			Title:     "Native",
		}},
	}
	agent := NewAgent(
		WithSessionStore(store),
		withClientFactory(func(context.Context, codex.Options) (codex.Client, error) { return nativeClient, nil }),
	)

	listResp, err := agent.ListSessions(ctx, ListSessionsRequest(WithListSessionsCwd("/tmp/project")))
	if err != nil {
		t.Fatalf("ListSessions returned error: %v", err)
	}
	if len(listResp.Sessions) != 0 {
		t.Fatalf("ListSessions returned tombstoned native sessions: %#v", listResp.Sessions)
	}
	if len(nativeClient.deletedThreadSnapshot()) != 0 {
		t.Fatalf("ListSessions should not need native cleanup without an in-process deleted set: %#v", nativeClient.deletedThreadSnapshot())
	}

	if _, err := agent.LoadSession(ctx, LoadSessionRequest(sessionID, "/tmp/project")); err == nil {
		t.Fatal("LoadSession returned nil error")
	} else {
		requireRequestError(t, err, -32002, "Resource not found")
	}
	if !containsString(nativeClient.deletedThreadSnapshot(), "thread-native") {
		t.Fatalf("LoadSession did not retry native cleanup: %#v", nativeClient.deletedThreadSnapshot())
	}

	if _, err := agent.ResumeSession(ctx, ResumeSessionRequest(sessionID, "/tmp/project")); err == nil {
		t.Fatal("ResumeSession returned nil error")
	} else {
		requireRequestError(t, err, -32002, "Resource not found")
	}
}

func containsString(values []string, want string) bool {
	for _, value := range values {
		if value == want {
			return true
		}
	}
	return false
}

func TestLifecycleMetaRejectsDeletedNamespaces(t *testing.T) {
	ctx := context.Background()
	agent := NewAgent(withClientFactory(func(context.Context, codex.Options) (codex.Client, error) {
		return newSpyCodexClient(), nil
	}))

	cases := []struct {
		name  string
		meta  map[string]any
		field string
	}{
		{
			name:  "package path",
			meta:  map[string]any{"github.com/savid/acp-go-codex": map[string]any{}},
			field: "_meta.github.com/savid/acp-go-codex",
		},
		{
			name:  "mode option",
			meta:  map[string]any{codexMetaKey: map[string]any{"options": map[string]any{"mode": "plan"}}},
			field: "_meta.codex.options.mode",
		},
		{
			name:  "goals",
			meta:  map[string]any{codexMetaKey: map[string]any{"goal": map[string]any{"objective": "ship"}}},
			field: "_meta.codex.goal",
		},
		{
			name:  "sdk message",
			meta:  map[string]any{codexMetaKey: map[string]any{"emitRawSDKMessages": true}},
			field: "_meta.codex.emitRawSDKMessages",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := agent.NewSession(ctx, NewSessionRequest("/tmp/project", WithSessionMeta(tc.meta))); err == nil {
				t.Fatal("NewSession accepted deleted metadata")
			} else {
				requireUnsupportedFieldError(t, err, tc.field)
			}
		})
	}
}

func requireUnsupportedFieldError(t *testing.T, err error, field string) {
	t.Helper()
	var reqErr *acp.RequestError
	if !errors.As(err, &reqErr) {
		t.Fatalf("error = %T %v, want ACP request error", err, err)
	}
	if reqErr.Code != -32602 || reqErr.Message != "Invalid params" {
		t.Fatalf("request error = %#v, want invalid params", reqErr)
	}
	want := map[string]any{"error": "unsupported", "field": field}
	if !mapsEqual(reqErr.Data, want) {
		t.Fatalf("request error data = %#v, want %#v", reqErr.Data, want)
	}
}

func mapsEqual(got any, want map[string]any) bool {
	gotMap, ok := got.(map[string]any)
	if !ok {
		return false
	}
	if len(gotMap) != len(want) {
		return false
	}
	for key, value := range want {
		if gotMap[key] != value {
			return false
		}
	}
	return true
}
