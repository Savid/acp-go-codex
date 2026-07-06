package codexacp

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
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
	if seedErr := store.Replace(ctx, SessionKey{SessionID: string(resp.SessionId)}, []SessionStoreReplacement{{
		Key:     SessionKey{SessionID: string(resp.SessionId)},
		Entries: []SessionStoreEntry{SessionStoreEntry(`{"type":"event_msg","payload":{"type":"agent_message","message":"hi"}}`)},
	}}); seedErr != nil {
		t.Fatalf("seed store: %v", seedErr)
	}

	if _, delErr := agent.UnstableDeleteSession(ctx, acp.UnstableDeleteSessionRequest{SessionId: resp.SessionId}); delErr != nil {
		t.Fatalf("DeleteSession returned error: %v", delErr)
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
	if seedErr := store.Replace(ctx, SessionKey{SessionID: string(resp.SessionId)}, []SessionStoreReplacement{{
		Key:     SessionKey{SessionID: string(resp.SessionId)},
		Entries: []SessionStoreEntry{SessionStoreEntry(`{"type":"event_msg","payload":{"type":"agent_message","message":"hi"}}`)},
	}}); seedErr != nil {
		t.Fatalf("seed store: %v", seedErr)
	}

	if _, delErr := agent.UnstableDeleteSession(ctx, DeleteSessionRequest(resp.SessionId)); !errors.Is(delErr, cleanupErr) {
		t.Fatalf("DeleteSession error = %v, want cleanup error", delErr)
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

func TestAgentLifecycleErrorBranches(t *testing.T) {
	ctx := context.Background()

	closed := NewAgent()
	if err := closed.Close(); err != nil {
		t.Fatalf("Close returned error: %v", err)
	}
	if _, err := closed.NewSession(ctx, NewSessionRequest("/tmp/project")); err == nil {
		t.Fatal("closed NewSession succeeded")
	}
	if _, err := closed.ResumeSession(ctx, ResumeSessionRequest("s", "/tmp/project")); err == nil {
		t.Fatal("closed ResumeSession succeeded")
	}
	if _, err := closed.LoadSession(ctx, LoadSessionRequest("s", "/tmp/project")); err == nil {
		t.Fatal("closed LoadSession succeeded")
	}
	if _, err := closed.ListSessions(ctx, acp.ListSessionsRequest{}); err == nil {
		t.Fatal("closed ListSessions succeeded")
	}

	client := newSpyCodexClient()
	agent := NewAgent(withClientFactory(func(context.Context, codex.Options) (codex.Client, error) { return client, nil }))
	if _, err := agent.NewSession(ctx, NewSessionRequest("relative")); err == nil {
		t.Fatal("NewSession accepted relative cwd")
	}
	if _, err := agent.NewSession(ctx, NewSessionRequest("/tmp/project", WithSessionMeta(map[string]any{codexMetaKey: "bad"}))); err == nil {
		t.Fatal("NewSession accepted invalid meta")
	}
	if _, err := agent.NewSession(ctx, NewSessionRequest("/tmp/project", WithSessionMCPServers(acp.McpServer{Sse: &acp.McpServerSseInline{Name: "sse"}}))); err == nil {
		t.Fatal("NewSession accepted unsupported MCP server")
	}

	factoryErr := errors.New("factory failed")
	factoryAgent := NewAgent(withClientFactory(func(context.Context, codex.Options) (codex.Client, error) { return nil, factoryErr }))
	if _, err := factoryAgent.NewSession(ctx, NewSessionRequest("/tmp/project")); !errors.Is(err, factoryErr) {
		t.Fatalf("NewSession factory err=%v", err)
	}

	startErr := errors.New("start failed")
	startClient := &errorCodexClient{spyCodexClient: newSpyCodexClient(), startErr: startErr}
	startAgent := NewAgent(withClientFactory(func(context.Context, codex.Options) (codex.Client, error) { return startClient, nil }))
	if _, err := startAgent.NewSession(ctx, NewSessionRequest("/tmp/project")); !errors.Is(err, startErr) {
		t.Fatalf("NewSession start err=%v", err)
	}
	if !startClient.closed {
		t.Fatal("NewSession start error did not close client")
	}

	limitAgent := NewAgent(
		WithConcurrencyLimits(ConcurrencyLimits{MaxActiveSessions: 1, MaxConcurrentPrompts: 1, MaxConcurrentClientCalls: 1}),
		withClientFactory(func(context.Context, codex.Options) (codex.Client, error) { return newSpyCodexClient(), nil }),
	)
	if _, err := limitAgent.NewSession(ctx, NewSessionRequest("/tmp/project")); err != nil {
		t.Fatalf("first limited NewSession returned error: %v", err)
	}
	if _, err := limitAgent.NewSession(ctx, NewSessionRequest("/tmp/other")); err == nil {
		t.Fatal("NewSession ignored active session limit")
	}
}

func TestAgentSessionOperationErrorBranches(t *testing.T) {
	ctx := context.Background()
	client := newSpyCodexClient()
	agent := NewAgent(withClientFactory(func(context.Context, codex.Options) (codex.Client, error) { return client, nil }))
	resp, err := agent.NewSession(ctx, NewSessionRequest("/tmp/project"))
	if err != nil {
		t.Fatalf("NewSession returned error: %v", err)
	}

	if _, promptErr := agent.Prompt(ctx, TextPromptRequest("missing", "hello")); promptErr == nil {
		t.Fatal("Prompt missing session succeeded")
	}
	if cancelErr := agent.Cancel(ctx, acp.CancelNotification{SessionId: "missing"}); cancelErr == nil {
		t.Fatal("Cancel missing session succeeded")
	}
	if _, closeMissingErr := agent.CloseSession(ctx, acp.CloseSessionRequest{SessionId: "missing"}); closeMissingErr == nil {
		t.Fatal("CloseSession missing session succeeded")
	}
	if _, deleteMissingErr := agent.UnstableDeleteSession(ctx, acp.UnstableDeleteSessionRequest{}); deleteMissingErr == nil {
		t.Fatal("DeleteSession missing id succeeded")
	}
	deleteStoreErr := errors.New("store delete failed")
	deleteStoreAgent := NewAgent(WithSessionStore(&configurableStore{deleteErr: deleteStoreErr}))
	if _, storeDelErr := deleteStoreAgent.UnstableDeleteSession(ctx, acp.UnstableDeleteSessionRequest{SessionId: "session-1"}); !errors.Is(storeDelErr, deleteStoreErr) {
		t.Fatalf("DeleteSession store error = %v", storeDelErr)
	}
	deleteCloseErr := errors.New("delete close failed")
	deleteCloseClient := &errorCodexClient{spyCodexClient: newSpyCodexClient(), closeErr: deleteCloseErr}
	deleteCloseAgent := NewAgent(
		WithSessionStore(NewInMemorySessionStore()),
		withClientFactory(func(context.Context, codex.Options) (codex.Client, error) { return deleteCloseClient, nil }),
	)
	deleteCloseResp, err := deleteCloseAgent.NewSession(ctx, NewSessionRequest("/tmp/project"))
	if err != nil {
		t.Fatalf("delete close NewSession returned error: %v", err)
	}
	if _, closeDelErr := deleteCloseAgent.UnstableDeleteSession(ctx, DeleteSessionRequest(deleteCloseResp.SessionId)); !errors.Is(closeDelErr, deleteCloseErr) {
		t.Fatalf("DeleteSession close error = %v", closeDelErr)
	}

	cancelErr := codex.ErrThreadNotFound
	cancelClient := &cancelErrorClient{spyCodexClient: newSpyCodexClient(), cancelErr: cancelErr}
	cancelAgent := NewAgent(withClientFactory(func(context.Context, codex.Options) (codex.Client, error) { return cancelClient, nil }))
	cancelResp, err := cancelAgent.NewSession(ctx, NewSessionRequest("/tmp/project"))
	if err != nil {
		t.Fatalf("cancel NewSession returned error: %v", err)
	}
	if cancelCodexErr := cancelAgent.Cancel(ctx, acp.CancelNotification{SessionId: cancelResp.SessionId}); cancelCodexErr == nil {
		t.Fatal("Cancel ignored Codex error")
	}

	fatalClient := &runEventsClient{spyCodexClient: newSpyCodexClient(), runErr: codex.ErrConnectionClosed}
	fatalAgent := NewAgent(withClientFactory(func(context.Context, codex.Options) (codex.Client, error) { return fatalClient, nil }))
	fatalResp, err := fatalAgent.NewSession(ctx, NewSessionRequest("/tmp/project"))
	if err != nil {
		t.Fatalf("fatal NewSession returned error: %v", err)
	}
	if _, fatalPromptErr := fatalAgent.Prompt(ctx, TextPromptRequest(fatalResp.SessionId, "hello")); fatalPromptErr == nil {
		t.Fatal("Prompt fatal process error succeeded")
	}
	if _, sessionErr := fatalAgent.session(fatalResp.SessionId); sessionErr == nil {
		t.Fatal("fatal Prompt did not remove session")
	}

	closeErr := errors.New("close failed")
	closeClient := &errorCodexClient{spyCodexClient: newSpyCodexClient(), closeErr: closeErr}
	closeAgent := NewAgent(withClientFactory(func(context.Context, codex.Options) (codex.Client, error) { return closeClient, nil }))
	closeResp, err := closeAgent.NewSession(ctx, NewSessionRequest("/tmp/project"))
	if err != nil {
		t.Fatalf("close NewSession returned error: %v", err)
	}
	if _, err := closeAgent.CloseSession(ctx, acp.CloseSessionRequest{SessionId: closeResp.SessionId}); !errors.Is(err, closeErr) {
		t.Fatalf("CloseSession err=%v", err)
	}
	if _, err := closeAgent.session(closeResp.SessionId); err == nil {
		t.Fatal("CloseSession did not remove session")
	}

	if session := agent.removeSession(resp.SessionId); session == nil {
		t.Fatal("removeSession returned nil")
	}
}

func TestListAndNativeSessionBranches(t *testing.T) {
	ctx := context.Background()
	store := &configurableStore{summaries: []SessionSummary{
		{SessionID: "active", Cwd: "/tmp/project", UpdatedAtUnixMilli: 100, Title: "Active"},
		{SessionID: "stored", Cwd: "/tmp/project", UpdatedAtUnixMilli: 200, Title: "Stored", Meta: map[string]any{"origin": "test"}},
		{SessionID: "other", Cwd: "/tmp/other", UpdatedAtUnixMilli: 300},
	}}
	client := &errorCodexClient{spyCodexClient: newSpyCodexClient(), listThreads: []codex.Thread{
		{},
		{ID: "active-thread", SessionID: "active", Cwd: "/tmp/project", Title: "Active native"},
		{ID: "skip-thread", SessionID: "skip-session", Cwd: "/tmp/project", Title: "Skip active thread"},
		{ID: "deleted-thread", SessionID: "deleted", Cwd: "/tmp/project", Title: "Deleted"},
		{ID: "native", Cwd: "", UpdatedAt: "2026-01-01T00:00:00Z"},
		{ID: "native-model", SessionID: "native-model-session", Cwd: "/tmp/project", Title: "Native model", Model: "gpt"},
	}}
	agent := NewAgent(
		WithSessionStore(store),
		withClientFactory(func(context.Context, codex.Options) (codex.Client, error) { return client, nil }),
	)
	active := newSession(agent, "active", "/tmp/project", nil, codex.Thread{ID: "active-thread", Title: "Active"}, client, sessionMeta{})
	if err := agent.storeStartedSession(active); err != nil {
		t.Fatalf("store active: %v", err)
	}
	activeOther := newSession(agent, "active-other", "/tmp/other", nil, codex.Thread{ID: "active-other-thread", Title: "Other"}, client, sessionMeta{})
	if err := agent.storeStartedSession(activeOther); err != nil {
		t.Fatalf("store other active: %v", err)
	}
	agent.deleted["deleted"] = struct{}{}
	cwd := "/tmp/project"
	list, err := agent.ListSessions(ctx, acp.ListSessionsRequest{Cwd: &cwd})
	if err != nil {
		t.Fatalf("ListSessions returned error: %v", err)
	}
	if len(list.Sessions) != 2 {
		t.Fatalf("ListSessions explicit store sessions = %#v", list.Sessions)
	}

	nativeAgent := NewAgent(withClientFactory(func(context.Context, codex.Options) (codex.Client, error) { return client, nil }))
	nativeAgent.deleted["deleted"] = struct{}{}
	native, err := nativeAgent.listCodexThreads(ctx, acp.ListSessionsRequest{Cwd: &cwd}, map[acp.SessionId]struct{}{"active": {}}, map[string]struct{}{"active-thread": {}, "skip-thread": {}})
	if err != nil {
		t.Fatalf("listCodexThreads returned error: %v", err)
	}
	if len(native) != 2 {
		t.Fatalf("native sessions = %#v", native)
	}
	if _, noCwdErr := nativeAgent.listCodexThreads(ctx, acp.ListSessionsRequest{}, nil, nil); noCwdErr != nil {
		t.Fatalf("listCodexThreads without cwd returned error: %v", noCwdErr)
	}
	badCwd := "relative"
	if _, badCwdErr := nativeAgent.ListSessions(ctx, acp.ListSessionsRequest{Cwd: &badCwd}); badCwdErr == nil {
		t.Fatal("ListSessions accepted relative cwd")
	}
	if _, badCwdThreadsErr := nativeAgent.listCodexThreads(ctx, acp.ListSessionsRequest{Cwd: &badCwd}, nil, nil); badCwdThreadsErr == nil {
		t.Fatal("listCodexThreads accepted relative cwd")
	}
	badCursor := "%%%"
	if _, badCursorErr := nativeAgent.ListSessions(ctx, acp.ListSessionsRequest{Cursor: &badCursor}); badCursorErr == nil {
		t.Fatal("ListSessions accepted invalid cursor")
	}
	factoryErrNative := NewAgent(withClientFactory(func(context.Context, codex.Options) (codex.Client, error) {
		return nil, errors.New("factory failed")
	}))
	if _, factoryErr := factoryErrNative.listCodexThreads(ctx, acp.ListSessionsRequest{}, nil, nil); factoryErr == nil {
		t.Fatal("listCodexThreads ignored factory error")
	}
	listErrAgent := NewAgent(withClientFactory(func(context.Context, codex.Options) (codex.Client, error) {
		return &errorCodexClient{spyCodexClient: newSpyCodexClient(), listErr: errors.New("list failed")}, nil
	}))
	if _, listErr := listErrAgent.ListSessions(ctx, acp.ListSessionsRequest{}); listErr == nil {
		t.Fatal("ListSessions ignored native list error")
	}
	storeErrAgent := NewAgent(WithSessionStore(&configurableStore{listErr: errors.New("store list")}))
	if _, storeListErr := storeErrAgent.ListSessions(ctx, acp.ListSessionsRequest{Cwd: &cwd}); storeListErr == nil {
		t.Fatal("ListSessions ignored store list error")
	}

	sessions := make([]acp.SessionInfo, listSessionsPageSize+1)
	for i := range sessions {
		sessions[i].SessionId = acp.SessionId(fmt.Sprintf("s-%02d", i))
	}
	page, next, err := paginateSessionInfos(sessions, nil)
	if err != nil || len(page) != listSessionsPageSize || next == nil {
		t.Fatalf("paginate first page len=%d next=%v err=%v", len(page), next, err)
	}
	if decoded, err := decodeListCursor(next); err != nil || decoded != listSessionsPageSize {
		t.Fatalf("decode cursor = %d err=%v", decoded, err)
	}
	if _, _, err := paginateSessionInfos(sessions, &badCursor); err == nil {
		t.Fatal("paginate accepted invalid cursor")
	}
	past := encodeListCursor(len(sessions) + 1)
	if _, _, err := paginateSessionInfos(sessions, &past); err == nil {
		t.Fatal("paginate accepted past-end cursor")
	}
	negative := base64.RawURLEncoding.EncodeToString([]byte("-1"))
	if _, err := decodeListCursor(&negative); err == nil {
		t.Fatal("decodeListCursor accepted negative cursor")
	}
}

func TestResumeLoadAndMaterializedBranches(t *testing.T) {
	ctx := context.Background()
	store := &configurableStore{}
	agent := NewAgent(WithSessionStore(store), withClientFactory(func(context.Context, codex.Options) (codex.Client, error) {
		return newSpyCodexClient(), nil
	}))
	if _, err := agent.ResumeSession(ctx, ResumeSessionRequest("s", "relative")); err == nil {
		t.Fatal("ResumeSession accepted relative cwd")
	}
	if _, err := agent.LoadSession(ctx, LoadSessionRequest("s", "relative")); err == nil {
		t.Fatal("LoadSession accepted relative cwd")
	}
	agent.deleted["deleted"] = struct{}{}
	if _, err := agent.ResumeSession(ctx, ResumeSessionRequest("deleted", "/tmp/project")); err == nil {
		t.Fatal("ResumeSession deleted id succeeded")
	}
	if _, err := agent.LoadSession(ctx, LoadSessionRequest("deleted", "/tmp/project")); err == nil {
		t.Fatal("LoadSession deleted id succeeded")
	}
	if _, err := agent.ResumeSession(ctx, ResumeSessionRequest("s", "/tmp/project", WithSessionMeta(map[string]any{codexMetaKey: "bad"}))); err == nil {
		t.Fatal("ResumeSession accepted invalid meta")
	}
	if _, err := agent.LoadSession(ctx, LoadSessionRequest("s", "/tmp/project", WithSessionMeta(map[string]any{codexMetaKey: "bad"}))); err == nil {
		t.Fatal("LoadSession accepted invalid meta")
	}
	store.loadErr = errors.New("load failed")
	if _, err := agent.ResumeSession(ctx, ResumeSessionRequest("s", "/tmp/project")); err == nil {
		t.Fatal("ResumeSession ignored store load error")
	}
	if _, err := agent.LoadSession(ctx, LoadSessionRequest("s", "/tmp/project")); err == nil {
		t.Fatal("LoadSession ignored store load error")
	}
	store.loadErr = nil
	if _, err := agent.ResumeSession(ctx, ResumeSessionRequest("s", "/tmp/project")); err == nil {
		t.Fatal("ResumeSession explicit empty store succeeded")
	}
	if _, err := agent.LoadSession(ctx, LoadSessionRequest("s", "/tmp/project")); err == nil {
		t.Fatal("LoadSession explicit empty store succeeded")
	}

	resumePrepareAgent := NewAgent(withClientFactory(func(context.Context, codex.Options) (codex.Client, error) { return newSpyCodexClient(), nil }))
	if _, err := resumePrepareAgent.ResumeSession(ctx, ResumeSessionRequest("native", "/tmp/project", WithSessionMCPServers(acp.McpServer{Sse: &acp.McpServerSseInline{Name: "sse"}}))); err == nil {
		t.Fatal("ResumeSession accepted unsupported MCP")
	}
	resumeFactoryAgent := NewAgent(withClientFactory(func(context.Context, codex.Options) (codex.Client, error) {
		return nil, errors.New("factory failed")
	}))
	if _, err := resumeFactoryAgent.ResumeSession(ctx, ResumeSessionRequest("native", "/tmp/project")); err == nil {
		t.Fatal("ResumeSession ignored factory error")
	}
	resumeThreadAgent := NewAgent(withClientFactory(func(context.Context, codex.Options) (codex.Client, error) {
		return &errorCodexClient{spyCodexClient: newSpyCodexClient(), resumeErr: codex.ErrThreadNotFound}, nil
	}))
	if _, err := resumeThreadAgent.ResumeSession(ctx, ResumeSessionRequest("native", "/tmp/project")); err == nil {
		t.Fatal("ResumeSession ignored native resume error")
	}
	resumeLimitAgent := NewAgent(
		WithConcurrencyLimits(ConcurrencyLimits{MaxActiveSessions: 1}),
		withClientFactory(func(context.Context, codex.Options) (codex.Client, error) { return newSpyCodexClient(), nil }),
	)
	occupied := newSession(resumeLimitAgent, "occupied", "/tmp/project", nil, codex.Thread{ID: "occupied"}, newSpyCodexClient(), sessionMeta{})
	if err := resumeLimitAgent.storeStartedSession(occupied); err != nil {
		t.Fatalf("store occupied: %v", err)
	}
	if _, err := resumeLimitAgent.ResumeSession(ctx, ResumeSessionRequest("native", "/tmp/project")); err == nil {
		t.Fatal("ResumeSession ignored store backpressure")
	}
}

func TestResumeLoadActiveSessionBranches(t *testing.T) {
	ctx := context.Background()

	activeClient := newSpyCodexClient()
	activeAgent := NewAgent(withClientFactory(func(context.Context, codex.Options) (codex.Client, error) { return activeClient, nil }))
	activeID := acp.SessionId("active")
	activeStart := codexSessionStart{Cwd: "/tmp/project", ResumeID: string(activeID)}
	activeSession := newSession(activeAgent, activeID, "/tmp/project", nil, codex.Thread{ID: string(activeID), SessionID: string(activeID)}, activeClient, sessionMeta{})
	activeSession.fingerprint = codexSessionStartFingerprint(activeStart)
	if err := activeAgent.storeStartedSession(activeSession); err != nil {
		t.Fatalf("store active session: %v", err)
	}
	if _, err := activeAgent.ResumeSession(ctx, ResumeSessionRequest(activeID, "/tmp/project")); err != nil {
		t.Fatalf("ResumeSession active returned error: %v", err)
	}
	if _, err := activeAgent.LoadSession(ctx, LoadSessionRequest(activeID, "/tmp/project")); err != nil {
		t.Fatalf("LoadSession active returned error: %v", err)
	}

	activeLoadErrAgent := NewAgent(WithSessionStore(&configurableStore{loadErr: errors.New("active load failed")}))
	activeLoadSession := newSession(activeLoadErrAgent, activeID, "/tmp/project", nil, codex.Thread{ID: string(activeID), SessionID: string(activeID)}, activeClient, sessionMeta{})
	activeLoadSession.fingerprint = codexSessionStartFingerprint(activeStart)
	if err := activeLoadErrAgent.storeStartedSession(activeLoadSession); err != nil {
		t.Fatalf("store active load err session: %v", err)
	}
	if _, err := activeLoadErrAgent.LoadSession(ctx, LoadSessionRequest(activeID, "/tmp/project")); err == nil {
		t.Fatal("LoadSession active ignored store load error")
	}

	activeReplayErrStore := &configurableStore{entries: []SessionStoreEntry{SessionStoreEntry(`{"type":"event_msg","payload":{"type":"agent_message","message":"hi"}}`)}}
	activeReplayErrAgent := NewAgent(WithSessionStore(activeReplayErrStore))
	activeReplayErrAgent.setAgentClient(&errorAgentClient{recordingAgentClient: newRecordingAgentClient(), updateErr: errors.New("update failed")})
	activeReplayErrSession := newSession(activeReplayErrAgent, activeID, "/tmp/project", nil, codex.Thread{ID: string(activeID), SessionID: string(activeID)}, activeClient, sessionMeta{})
	activeReplayErrSession.fingerprint = codexSessionStartFingerprint(activeStart)
	if err := activeReplayErrAgent.storeStartedSession(activeReplayErrSession); err != nil {
		t.Fatalf("store active replay err session: %v", err)
	}
	if _, err := activeReplayErrAgent.LoadSession(ctx, LoadSessionRequest(activeID, "/tmp/project")); err == nil {
		t.Fatal("LoadSession active ignored rollout replay error")
	}

	activeHistoryErrAgent := NewAgent(withClientFactory(func(context.Context, codex.Options) (codex.Client, error) { return activeClient, nil }))
	activeHistoryErrSession := newSession(activeHistoryErrAgent, activeID, "/tmp/project", nil, codex.Thread{ID: string(activeID), SessionID: string(activeID)}, readErrorClient{Client: activeClient}, sessionMeta{})
	activeHistoryErrSession.fingerprint = codexSessionStartFingerprint(activeStart)
	if err := activeHistoryErrAgent.storeStartedSession(activeHistoryErrSession); err != nil {
		t.Fatalf("store active history err session: %v", err)
	}
	if _, err := activeHistoryErrAgent.LoadSession(ctx, LoadSessionRequest(activeID, "/tmp/project")); err == nil {
		t.Fatal("LoadSession active ignored thread history error")
	}

	loadResumeErrAgent := NewAgent(withClientFactory(func(context.Context, codex.Options) (codex.Client, error) {
		return nil, errors.New("load resume failed")
	}))
	if _, err := loadResumeErrAgent.LoadSession(ctx, LoadSessionRequest("native", "/tmp/project")); err == nil {
		t.Fatal("LoadSession ignored ResumeSession error")
	}
	loadReplayErrAgent := NewAgent(withClientFactory(func(context.Context, codex.Options) (codex.Client, error) {
		return &errorCodexClient{spyCodexClient: newSpyCodexClient(), readErr: errors.New("read failed")}, nil
	}))
	if _, err := loadReplayErrAgent.LoadSession(ctx, LoadSessionRequest("thread-1", "/tmp/project")); err == nil {
		t.Fatal("LoadSession ignored post-resume replay error")
	}
}

func TestResumeLoadMaterializedSessionBranches(t *testing.T) {
	ctx := context.Background()
	store := &configurableStore{}
	agent := NewAgent(WithSessionStore(store), withClientFactory(func(context.Context, codex.Options) (codex.Client, error) {
		return newSpyCodexClient(), nil
	}))

	materializedEntries := []SessionStoreEntry{SessionStoreEntry(`{"type":"response_item","payload":{"type":"message","role":"assistant","content":[{"type":"output_text","text":"agent"}]}}`)}
	store.entries = materializedEntries
	resumeResp, err := agent.ResumeSession(ctx, ResumeSessionRequest("stored", "/tmp/project"))
	if err != nil {
		t.Fatalf("ResumeSession materialized returned error: %v", err)
	}
	if resumeResp.Meta == nil {
		t.Fatal("ResumeSession materialized returned nil meta")
	}

	loadAgent := NewAgent(WithSessionStore(&configurableStore{entries: materializedEntries}), withClientFactory(func(context.Context, codex.Options) (codex.Client, error) {
		return newSpyCodexClient(), nil
	}))
	if _, err := loadAgent.LoadSession(ctx, LoadSessionRequest("stored", "/tmp/project")); err != nil {
		t.Fatalf("LoadSession materialized returned error: %v", err)
	}

	resumeErrAgent := NewAgent(WithSessionStore(&configurableStore{entries: materializedEntries}), withClientFactory(func(context.Context, codex.Options) (codex.Client, error) {
		return &errorCodexClient{spyCodexClient: newSpyCodexClient(), resumeErr: codex.ErrThreadNotFound}, nil
	}))
	if _, err := resumeErrAgent.ResumeSession(ctx, ResumeSessionRequest("stored", "/tmp/project")); err == nil {
		t.Fatal("ResumeSession materialized ignored resume error")
	}
	if _, err := resumeErrAgent.LoadSession(ctx, LoadSessionRequest("stored", "/tmp/project")); err == nil {
		t.Fatal("LoadSession materialized ignored resume error")
	}

	closedMaterialized := NewAgent()
	if err := closedMaterialized.Close(); err != nil {
		t.Fatalf("Close returned error: %v", err)
	}
	if _, err := closedMaterialized.resumeMaterializedSession(ctx, ResumeSessionRequest("stored", "/tmp/project"), materializedEntries); err == nil {
		t.Fatal("resumeMaterializedSession on closed agent succeeded")
	}
	if _, err := closedMaterialized.loadMaterializedSession(ctx, LoadSessionRequest("stored", "/tmp/project"), materializedEntries); err == nil {
		t.Fatal("loadMaterializedSession on closed agent succeeded")
	}
	if _, err := agent.resumeMaterializedSession(ctx, ResumeSessionRequest("bad-meta", "/tmp/project", WithSessionMeta(map[string]any{codexMetaKey: "bad"})), materializedEntries); err == nil {
		t.Fatal("resumeMaterializedSession accepted invalid meta")
	}
	if _, err := agent.loadMaterializedSession(ctx, LoadSessionRequest("bad-meta", "/tmp/project", WithSessionMeta(map[string]any{codexMetaKey: "bad"})), materializedEntries); err == nil {
		t.Fatal("loadMaterializedSession accepted invalid meta")
	}
	origCreateRollout := createMaterializedRolloutTemp
	t.Cleanup(func() { createMaterializedRolloutTemp = origCreateRollout })
	createMaterializedRolloutTemp = func() (materializedRolloutFile, error) {
		return nil, errors.New("create rollout failed")
	}
	if _, err := agent.resumeMaterializedSession(ctx, ResumeSessionRequest("bad-rollout", "/tmp/project"), materializedEntries); err == nil {
		t.Fatal("resumeMaterializedSession ignored materialize error")
	}
	if _, err := agent.loadMaterializedSession(ctx, LoadSessionRequest("bad-rollout", "/tmp/project"), materializedEntries); err == nil {
		t.Fatal("loadMaterializedSession ignored materialize error")
	}
	createMaterializedRolloutTemp = origCreateRollout
	if _, err := agent.resumeMaterializedSession(ctx, ResumeSessionRequest("bad-mcp", "/tmp/project", WithSessionMCPServers(acp.McpServer{Sse: &acp.McpServerSseInline{Name: "sse"}})), materializedEntries); err == nil {
		t.Fatal("resumeMaterializedSession accepted unsupported MCP")
	}
	if _, err := agent.loadMaterializedSession(ctx, LoadSessionRequest("bad-mcp", "/tmp/project", WithSessionMCPServers(acp.McpServer{Sse: &acp.McpServerSseInline{Name: "sse"}})), materializedEntries); err == nil {
		t.Fatal("loadMaterializedSession accepted unsupported MCP")
	}
	materializedFactoryAgent := NewAgent(withClientFactory(func(context.Context, codex.Options) (codex.Client, error) {
		return nil, errors.New("materialized factory failed")
	}))
	if _, err := materializedFactoryAgent.resumeMaterializedSession(ctx, ResumeSessionRequest("stored", "/tmp/project"), materializedEntries); err == nil {
		t.Fatal("resumeMaterializedSession ignored factory error")
	}
	if _, err := materializedFactoryAgent.loadMaterializedSession(ctx, LoadSessionRequest("stored", "/tmp/project"), materializedEntries); err == nil {
		t.Fatal("loadMaterializedSession ignored factory error")
	}
	materializedLimitAgent := NewAgent(
		WithConcurrencyLimits(ConcurrencyLimits{MaxActiveSessions: 1}),
		withClientFactory(func(context.Context, codex.Options) (codex.Client, error) { return newSpyCodexClient(), nil }),
	)
	limitSession := newSession(materializedLimitAgent, "limit", "/tmp/project", nil, codex.Thread{ID: "limit"}, newSpyCodexClient(), sessionMeta{})
	if err := materializedLimitAgent.storeStartedSession(limitSession); err != nil {
		t.Fatalf("store materialized limit session: %v", err)
	}
	if _, err := materializedLimitAgent.resumeMaterializedSession(ctx, ResumeSessionRequest("stored", "/tmp/project"), materializedEntries); err == nil {
		t.Fatal("resumeMaterializedSession ignored store backpressure")
	}
	if _, err := materializedLimitAgent.loadMaterializedSession(ctx, LoadSessionRequest("stored", "/tmp/project"), materializedEntries); err == nil {
		t.Fatal("loadMaterializedSession ignored store backpressure")
	}
	if _, err := agent.loadMaterializedSession(ctx, LoadSessionRequest("bad-replay", "/tmp/project"), []SessionStoreEntry{SessionStoreEntry(`not-json`)}); err == nil {
		t.Fatal("loadMaterializedSession ignored replay error")
	}
}

func TestForkSessionErrorBranches(t *testing.T) {
	ctx := context.Background()
	agent := NewAgent(withClientFactory(func(context.Context, codex.Options) (codex.Client, error) { return newSpyCodexClient(), nil }))
	if _, err := agent.forkSession(ctx, ForkSessionRequest("missing", "/tmp/project")); err == nil {
		t.Fatal("forkSession accepted missing parent")
	}
	parentResp, err := agent.NewSession(ctx, NewSessionRequest("/tmp/project"))
	if err != nil {
		t.Fatalf("NewSession parent returned error: %v", err)
	}
	if _, err := agent.forkSession(ctx, ForkSessionRequest(parentResp.SessionId, "relative")); err == nil {
		t.Fatal("forkSession accepted relative cwd")
	}
	if _, err := agent.forkSession(ctx, ForkSessionRequest(parentResp.SessionId, "/tmp/project", WithSessionMeta(map[string]any{codexMetaKey: "bad"}))); err == nil {
		t.Fatal("forkSession accepted invalid meta")
	}
	if _, err := agent.forkSession(ctx, ForkSessionRequest(parentResp.SessionId, "/tmp/project", WithSessionMCPServers(acp.McpServer{Sse: &acp.McpServerSseInline{Name: "sse"}}))); err == nil {
		t.Fatal("forkSession accepted unsupported MCP")
	}

	parentThread := codex.Thread{ID: "parent-thread", SessionID: "parent"}
	factoryErrAgent := NewAgent(withClientFactory(func(context.Context, codex.Options) (codex.Client, error) {
		return nil, errors.New("fork factory failed")
	}))
	factoryParent := newSession(factoryErrAgent, "parent", "/tmp/project", nil, parentThread, newSpyCodexClient(), sessionMeta{})
	if err := factoryErrAgent.storeStartedSession(factoryParent); err != nil {
		t.Fatalf("store factory parent: %v", err)
	}
	if _, err := factoryErrAgent.forkSession(ctx, ForkSessionRequest("parent", "/tmp/project")); err == nil {
		t.Fatal("forkSession ignored factory error")
	}

	forkErrAgent := NewAgent(withClientFactory(func(context.Context, codex.Options) (codex.Client, error) {
		return &errorCodexClient{spyCodexClient: newSpyCodexClient(), forkErr: codex.ErrThreadNotFound}, nil
	}))
	forkParent := newSession(forkErrAgent, "parent", "/tmp/project", nil, parentThread, newSpyCodexClient(), sessionMeta{})
	if err := forkErrAgent.storeStartedSession(forkParent); err != nil {
		t.Fatalf("store fork parent: %v", err)
	}
	if _, err := forkErrAgent.forkSession(ctx, ForkSessionRequest("parent", "/tmp/project")); err == nil {
		t.Fatal("forkSession ignored fork error")
	}

	limitAgent := NewAgent(
		WithConcurrencyLimits(ConcurrencyLimits{MaxActiveSessions: 1}),
		withClientFactory(func(context.Context, codex.Options) (codex.Client, error) { return newSpyCodexClient(), nil }),
	)
	limitParent := newSession(limitAgent, "parent", "/tmp/project", nil, parentThread, newSpyCodexClient(), sessionMeta{})
	if err := limitAgent.storeStartedSession(limitParent); err != nil {
		t.Fatalf("store limit parent: %v", err)
	}
	if _, err := limitAgent.forkSession(ctx, ForkSessionRequest("parent", "/tmp/project")); err == nil {
		t.Fatal("forkSession ignored store backpressure")
	}
}

func TestSessionHelperBranches(t *testing.T) {
	agent := NewAgent()
	start := codexSessionStart{
		Cwd: "/tmp/project",
		McpServers: []acp.McpServer{
			HTTPMCPServer("z", "https://z.example", nil),
			StdioMCPServer("a", "cmd", nil, nil),
		},
		Meta: sessionMeta{Env: map[string]string{"A": "B"}, ApprovalPolicy: map[string]any{"mode": "ask"}},
	}
	if got := codexSessionStartFingerprint(start); got == "" {
		t.Fatal("empty session start fingerprint")
	}
	if agent.activeSessionForStart("missing", start) != nil {
		t.Fatal("activeSessionForStart found missing session")
	}
	if got := jsonFingerprint(make(chan int)); !strings.HasPrefix(got, "marshal-error:") {
		t.Fatalf("jsonFingerprint error = %q", got)
	}
	if got := mcpServerName(acp.McpServer{Http: &acp.McpServerHttpInline{Name: "http"}}); got != "http" {
		t.Fatalf("HTTP mcpServerName = %q", got)
	}
	if got := mcpServerName(acp.McpServer{Acp: &acp.McpServerAcpInline{Name: "acp"}}); got != "acp" {
		t.Fatalf("ACP mcpServerName = %q", got)
	}
	if got := mcpServerName(acp.McpServer{}); got != "" {
		t.Fatalf("empty mcpServerName = %q", got)
	}
	var sessions []acp.SessionInfo
	seen := map[acp.SessionId]struct{}{}
	addSessionInfo(&sessions, seen, acp.SessionInfo{})
	addSessionInfo(&sessions, seen, acp.SessionInfo{SessionId: "s"})
	addSessionInfo(&sessions, seen, acp.SessionInfo{SessionId: "s"})
	if len(sessions) != 1 {
		t.Fatalf("addSessionInfo sessions = %#v", sessions)
	}
	if ctx, cancel := (&Agent{}).sessionStoreContext(context.Background()); ctx == nil || cancel == nil {
		t.Fatal("sessionStoreContext returned nil values")
	} else {
		cancel()
	}
}

func TestDeleteRetryAndConfigBranches(t *testing.T) {
	ctx := context.Background()
	deleteClient := &errorCodexClient{
		spyCodexClient: newSpyCodexClient(),
		listThreads: []codex.Thread{
			{ID: "", SessionID: "ignored"},
			{ID: "known", SessionID: "session"},
			{ID: "missing", SessionID: "session"},
		},
	}
	deleteClient.deleteErr = codex.ErrThreadNotFound
	agent := NewAgent(withClientFactory(func(context.Context, codex.Options) (codex.Client, error) { return deleteClient, nil }))
	if err := agent.deleteNativeCodexSession(ctx, "session", "known"); err != nil {
		t.Fatalf("delete native with not-found returned error: %v", err)
	}
	deleteClient.deleteErr = errors.New("delete failed")
	if err := agent.deleteNativeCodexSession(ctx, "session", "known"); err == nil {
		t.Fatal("delete native ignored delete error")
	}
	listErrAgent := NewAgent(withClientFactory(func(context.Context, codex.Options) (codex.Client, error) {
		return &errorCodexClient{spyCodexClient: newSpyCodexClient(), listErr: errors.New("list failed")}, nil
	}))
	if err := listErrAgent.deleteNativeCodexSession(ctx, "session", ""); err == nil {
		t.Fatal("delete native ignored list error")
	}
	factoryErrAgent := NewAgent(withClientFactory(func(context.Context, codex.Options) (codex.Client, error) {
		return nil, errors.New("factory failed")
	}))
	if err := factoryErrAgent.deleteNativeCodexSession(ctx, "session", ""); err == nil {
		t.Fatal("delete native ignored factory error")
	}
	factoryErrAgent.deleted["session"] = struct{}{}
	factoryErrAgent.retryDeletedNativeCodexSessions(ctx)
	factoryErrAgent.retryDeleteNativeCodexSession(ctx, "session", "")

	configClient := newSpyCodexClient()
	configAgent := NewAgent(withClientFactory(func(context.Context, codex.Options) (codex.Client, error) { return configClient, nil }))
	resp, err := configAgent.NewSession(ctx, NewSessionRequest("/tmp/project", WithSessionCodexOptions(CodexOptions{ServiceTier: "flex", Personality: "friendly"})))
	if err != nil {
		t.Fatalf("config NewSession returned error: %v", err)
	}
	for _, tc := range []struct {
		id    acp.SessionConfigId
		value acp.SessionConfigValueId
	}{
		{id: configModel, value: "gpt-other"},
		{id: configServiceTier, value: "priority"},
		{id: configPersonality, value: "pragmatic"},
	} {
		if _, err := configAgent.SetSessionConfigOption(ctx, SetConfigOptionRequest(resp.SessionId, tc.id, tc.value)); err != nil {
			t.Fatalf("SetSessionConfigOption %s returned error: %v", tc.id, err)
		}
	}
	if _, err := configAgent.SetSessionConfigOption(ctx, SetConfigOptionRequest(resp.SessionId, configEffort, "bad")); err == nil {
		t.Fatal("SetSessionConfigOption accepted bad effort")
	}
	if _, err := configAgent.SetSessionConfigOption(ctx, SetConfigOptionRequest(resp.SessionId, configPersonality, "bad")); err == nil {
		t.Fatal("SetSessionConfigOption accepted bad personality")
	}
	if _, err := configAgent.SetSessionConfigOption(ctx, SetConfigOptionRequest(resp.SessionId, "unknown", "x")); err == nil {
		t.Fatal("SetSessionConfigOption accepted unknown config")
	}
	if _, err := configAgent.SetSessionConfigOption(ctx, acp.SetSessionConfigOptionRequest{Boolean: &acp.SetSessionConfigOptionBoolean{SessionId: resp.SessionId, ConfigId: "flag", Value: true}}); err == nil {
		t.Fatal("SetSessionConfigOption accepted boolean")
	}
	if _, err := configAgent.SetSessionConfigOption(ctx, acp.SetSessionConfigOptionRequest{}); err == nil {
		t.Fatal("SetSessionConfigOption accepted empty request")
	}
	if _, err := configAgent.SetSessionMode(ctx, acp.SetSessionModeRequest{}); err == nil {
		t.Fatal("SetSessionMode succeeded")
	}
	if got := modelList(ctx, nil); got != nil {
		t.Fatalf("modelList nil = %#v", got)
	}
	if got := modelList(ctx, &errorCodexClient{spyCodexClient: newSpyCodexClient(), modelErr: errors.New("models")}); got != nil {
		t.Fatalf("modelList error = %#v", got)
	}
	if got := clientAccountMeta(ctx, nil); got != nil {
		t.Fatalf("clientAccountMeta nil = %#v", got)
	}
	if got := clientAccountMeta(ctx, &errorCodexClient{spyCodexClient: newSpyCodexClient(), accountErr: errors.New("account")}); got != nil {
		t.Fatalf("clientAccountMeta error = %#v", got)
	}
	if err := codexThreadACPError(nil, nil, nil); err != nil {
		t.Fatalf("nil codexThreadACPError = %v", err)
	}
	if !fatalCodexProcessError(codex.ErrConnectionClosed) {
		t.Fatal("fatalCodexProcessError missed connection closed")
	}
	if data := codexThreadErrorData("s", "thread"); data["threadId"] != "thread" {
		t.Fatalf("thread error data = %#v", data)
	}
	if data := codexThreadErrorData("s", ""); data["threadId"] != nil {
		t.Fatalf("empty thread error data = %#v", data)
	}
	if err := lifecycleMetaError(acp.NewInvalidParams(map[string]any{"x": "y"})); err == nil {
		t.Fatal("lifecycleMetaError returned nil")
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

type configurableStore struct {
	entries   []SessionStoreEntry
	summaries []SessionSummary
	loadErr   error
	listErr   error
	deleteErr error
}

func (s *configurableStore) Append(context.Context, SessionKey, []SessionStoreEntry) error {
	return nil
}

func (s *configurableStore) Load(context.Context, SessionKey) ([]SessionStoreEntry, error) {
	if s.loadErr != nil {
		return nil, s.loadErr
	}

	return cloneStoreEntries(s.entries), nil
}

func (s *configurableStore) Replace(context.Context, SessionKey, []SessionStoreReplacement) error {
	return nil
}

func (s *configurableStore) Delete(context.Context, SessionKey) error {
	return s.deleteErr
}

func (s *configurableStore) ListSessions(context.Context) ([]SessionSummary, error) {
	if s.listErr != nil {
		return nil, s.listErr
	}

	return append([]SessionSummary(nil), s.summaries...), nil
}

func (s *configurableStore) ListSubkeys(context.Context, SessionKey) ([]string, error) {
	return nil, nil
}

type cancelErrorClient struct {
	*spyCodexClient
	cancelErr error
}

func (c *cancelErrorClient) CancelTurn(context.Context, string, string) error {
	return c.cancelErr
}
