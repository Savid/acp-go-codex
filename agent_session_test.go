package codexacp

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/coder/acp-go-sdk"
	"github.com/savid/acp-go-codex/internal/codex"
	"github.com/stretchr/testify/require"
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

func requireUnknownSession(t *testing.T, err error) {
	t.Helper()
	reqErr, ok := err.(*acp.RequestError)
	if !ok {
		t.Fatalf("error = %T %v, want ACP request error", err, err)
	}
	if reqErr.Code != -32602 {
		t.Fatalf("request error = %#v, want code=-32602", reqErr)
	}
	data, ok := reqErr.Data.(map[string]any)
	if !ok {
		t.Fatalf("request error data = %#v, want map", reqErr.Data)
	}
	if data["error"] != "unknown session" || data["field"] != "sessionId" {
		t.Fatalf("request error data = %#v, want {error:unknown session, field:sessionId}", data)
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
		requireUnknownSession(t, err)
	}
	if _, err := agent.LoadSession(ctx, LoadSessionRequest(resp.SessionId, "/tmp/project")); err == nil {
		t.Fatal("LoadSession after delete succeeded")
	} else {
		requireUnknownSession(t, err)
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
		requireUnknownSession(t, err)
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
		requireUnknownSession(t, err)
	}
	if !containsString(nativeClient.deletedThreadSnapshot(), "thread-native") {
		t.Fatalf("LoadSession did not retry native cleanup: %#v", nativeClient.deletedThreadSnapshot())
	}

	if _, err := agent.ResumeSession(ctx, ResumeSessionRequest(sessionID, "/tmp/project")); err == nil {
		t.Fatal("ResumeSession returned nil error")
	} else {
		requireUnknownSession(t, err)
	}
}

func TestDeleteRetainedRuntimeThreadReleasesOwnership(t *testing.T) {
	ctx := context.Background()
	store := NewInMemorySessionStore()
	client := newSpyCodexClient()
	agent := NewAgent(
		WithSessionStore(store),
		withClientFactory(func(context.Context, codex.Options) (codex.Client, error) { return client, nil }),
	)

	created, err := agent.NewSession(ctx, NewSessionRequest("/tmp/project"))
	require.NoError(t, err)
	active := agent.activeSession(created.SessionId)
	require.NotNil(t, active)
	threadID := active.codexThreadID
	_, err = agent.CloseSession(ctx, acp.CloseSessionRequest{SessionId: created.SessionId})
	require.NoError(t, err)
	require.NotNil(t, agent.retainedThreads[created.SessionId])

	_, err = agent.UnstableDeleteSession(ctx, acp.UnstableDeleteSessionRequest{SessionId: created.SessionId})
	require.NoError(t, err)
	require.Nil(t, agent.retainedThreads[created.SessionId])
	require.Contains(t, client.deletedThreadSnapshot(), threadID)
}

func TestRetainedDeleteAndRetryCleanupErrors(t *testing.T) {
	ctx := context.Background()
	entries := []SessionStoreEntry{SessionStoreEntry(`{"type":"session_meta","payload":{"id":"thread"}}`)}
	oldRemove := removeMaterializedRolloutFile
	t.Cleanup(func() { removeMaterializedRolloutFile = oldRemove })

	fixture := func(id acp.SessionId) (*Agent, *retainedRuntimeThread) {
		client := newSpyCodexClient()
		agent := NewAgent(
			WithSessionStore(NewInMemorySessionStore()),
			withClientFactory(func(context.Context, codex.Options) (codex.Client, error) { return client, nil }),
		)
		agent.runtimeClient = client
		agent.runtimeEpoch = 4
		path, err := materializeRollout(t.TempDir(), entries)
		require.NoError(t, err)
		retained := &retainedRuntimeThread{
			sessionID:        id,
			threadID:         "thread",
			path:             path,
			client:           client,
			epoch:            4,
			materializedPath: path,
		}
		agent.retainedThreads[id] = retained

		return agent, retained
	}

	agent, retained := fixture("delete")
	removeMaterializedRolloutFile = func(string) error { return errors.New("retained delete cleanup failed") }
	_, err := agent.UnstableDeleteSession(ctx, acp.UnstableDeleteSessionRequest{SessionId: retained.sessionID})
	require.ErrorContains(t, err, "retained delete cleanup failed")
	require.True(t, retained.nativeEnded)
	require.Same(t, retained, agent.retainedThreads[retained.sessionID])
	removeMaterializedRolloutFile = oldRemove
	require.NoError(t, agent.releaseRetainedRuntimeThreads(retained.client, retained.epoch))

	agent, retained = fixture("retry")
	removeMaterializedRolloutFile = func(string) error { return errors.New("retained retry cleanup failed") }
	agent.retryDeleteNativeCodexSession(ctx, retained.sessionID, "")
	require.True(t, retained.nativeEnded)
	require.Same(t, retained, agent.retainedThreads[retained.sessionID])
	removeMaterializedRolloutFile = oldRemove
	require.NoError(t, agent.releaseRetainedRuntimeThreads(retained.client, retained.epoch))
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

	t.Run("foreign module path ignored", func(t *testing.T) {
		meta := map[string]any{"github.com/savid/acp-go-codex": map[string]any{"anything": true}}
		if _, err := agent.NewSession(ctx, NewSessionRequest("/tmp/project", WithSessionMeta(meta))); err != nil {
			t.Fatalf("NewSession rejected foreign module-path _meta namespace instead of ignoring it: %v", err)
		}
	})
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
	if startClient.closed {
		t.Fatal("NewSession start error closed the Agent-owned shared runtime")
	}

	limitAgent := NewAgent(
		WithConcurrencyLimits(ConcurrencyLimits{MaxActiveSessions: 1, MaxConcurrentClientCalls: 1}),
		withClientFactory(func(context.Context, codex.Options) (codex.Client, error) { return newSpyCodexClient(), nil }),
	)
	if _, err := limitAgent.NewSession(ctx, NewSessionRequest("/tmp/project")); err != nil {
		t.Fatalf("first limited NewSession returned error: %v", err)
	}
	if _, err := limitAgent.NewSession(ctx, NewSessionRequest("/tmp/other")); err == nil {
		t.Fatal("NewSession ignored active session limit")
	}
}

func TestUnknownSessionUniformInvalidParams(t *testing.T) {
	ctx := context.Background()
	agent := NewAgent(
		WithSessionStore(NewInMemorySessionStore()),
		withClientFactory(func(context.Context, codex.Options) (codex.Client, error) {
			return newSpyCodexClient(), nil
		}),
	)

	_, promptErr := agent.Prompt(ctx, TextPromptRequest("missing", "test-turn", "hello"))
	requireUnknownSession(t, promptErr)
	requireUnknownSession(t, agent.Cancel(ctx, acp.CancelNotification{SessionId: "missing"}))
	_, configErr := agent.SetSessionConfigOption(ctx, SetConfigOptionRequest("missing", configModel, "gpt"))
	requireUnknownSession(t, configErr)
	_, closeErr := agent.CloseSession(ctx, acp.CloseSessionRequest{SessionId: "missing"})
	requireUnknownSession(t, closeErr)
	_, resumeErr := agent.ResumeSession(ctx, ResumeSessionRequest("missing", "/tmp/project"))
	requireUnknownSession(t, resumeErr)
	_, loadErr := agent.LoadSession(ctx, LoadSessionRequest("missing", "/tmp/project"))
	requireUnknownSession(t, loadErr)
}

func TestAgentSessionOperationErrorBranches(t *testing.T) {
	ctx := context.Background()
	client := newSpyCodexClient()
	agent := NewAgent(withClientFactory(func(context.Context, codex.Options) (codex.Client, error) { return client, nil }))
	resp, err := agent.NewSession(ctx, NewSessionRequest("/tmp/project"))
	if err != nil {
		t.Fatalf("NewSession returned error: %v", err)
	}

	if _, promptErr := agent.Prompt(ctx, TextPromptRequest("missing", "test-turn", "hello")); promptErr == nil {
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
	if _, closeDelErr := deleteCloseAgent.UnstableDeleteSession(ctx, DeleteSessionRequest(deleteCloseResp.SessionId)); closeDelErr != nil {
		t.Fatalf("DeleteSession released thread with error = %v", closeDelErr)
	}
	if closeErr := deleteCloseAgent.Close(); !errors.Is(closeErr, deleteCloseErr) {
		t.Fatalf("Agent close error = %v", closeErr)
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
	if _, fatalPromptErr := fatalAgent.Prompt(ctx, TextPromptRequest(fatalResp.SessionId, "test-turn", "hello")); !isTurnFailure(fatalPromptErr, codex.CauseTransport) {
		t.Fatalf("Prompt fatal process error = %v, want codex_turn_failed transport", fatalPromptErr)
	}
	if _, sessionErr := fatalAgent.session(fatalResp.SessionId); sessionErr != nil {
		t.Fatalf("fatal Prompt must leave session addressable: %v", sessionErr)
	}

	closeErr := errors.New("close failed")
	closeClient := &errorCodexClient{spyCodexClient: newSpyCodexClient(), closeErr: closeErr}
	closeAgent := NewAgent(withClientFactory(func(context.Context, codex.Options) (codex.Client, error) { return closeClient, nil }))
	closeResp, err := closeAgent.NewSession(ctx, NewSessionRequest("/tmp/project"))
	if err != nil {
		t.Fatalf("close NewSession returned error: %v", err)
	}
	if _, err := closeAgent.CloseSession(ctx, acp.CloseSessionRequest{SessionId: closeResp.SessionId}); err != nil {
		t.Fatalf("CloseSession logical release err=%v", err)
	}
	if _, err := closeAgent.session(closeResp.SessionId); err == nil {
		t.Fatal("CloseSession did not remove session")
	}
	if err := closeAgent.Close(); !errors.Is(err, closeErr) {
		t.Fatalf("Agent runtime close err=%v", err)
	}

	if session := agent.removeSession(resp.SessionId); session == nil {
		t.Fatal("removeSession returned nil")
	}
}

func TestListSessionsUsesActiveAndStoreAuthority(t *testing.T) {
	ctx := context.Background()
	store := &configurableStore{summaries: []SessionSummary{
		{SessionID: "active", Cwd: "/tmp/project", UpdatedAtUnixMilli: 100, Title: "Active"},
		{SessionID: "stored", Cwd: "/tmp/project", UpdatedAtUnixMilli: 200, Title: "Stored", Meta: map[string]any{"origin": "test"}},
		{SessionID: "other", Cwd: "/tmp/other", UpdatedAtUnixMilli: 300},
	}}
	client := &errorCodexClient{spyCodexClient: newSpyCodexClient(), listErr: errors.New("native list must not be lifecycle authority")}
	agent := NewAgent(
		WithSessionStore(store),
		withClientFactory(func(context.Context, codex.Options) (codex.Client, error) { return client, nil }),
	)
	active := newSession(agent, "active", "/tmp/project", nil, codex.Thread{ID: "active-thread", Title: "Active"}, client, sessionMeta{}, nil)
	if err := agent.storeStartedSession(active); err != nil {
		t.Fatalf("store active: %v", err)
	}
	activeOther := newSession(agent, "active-other", "/tmp/other", nil, codex.Thread{ID: "active-other-thread", Title: "Other"}, client, sessionMeta{}, nil)
	if err := agent.storeStartedSession(activeOther); err != nil {
		t.Fatalf("store other active: %v", err)
	}
	cwd := "/tmp/project"
	list, err := agent.ListSessions(ctx, acp.ListSessionsRequest{Cwd: &cwd})
	if err != nil {
		t.Fatalf("ListSessions returned error: %v", err)
	}
	if len(list.Sessions) != 2 {
		t.Fatalf("ListSessions active/store sessions = %#v", list.Sessions)
	}

	residualClient := &errorCodexClient{spyCodexClient: newSpyCodexClient(), listThreads: []codex.Thread{{
		ID: "residual-thread", SessionID: "residual", Cwd: "/tmp/project", Title: "Residual native thread",
	}}}
	nativeAgent := NewAgent(withClientFactory(func(context.Context, codex.Options) (codex.Client, error) { return residualClient, nil }))
	native, err := nativeAgent.ListSessions(ctx, acp.ListSessionsRequest{Cwd: &cwd})
	if err != nil {
		t.Fatalf("ListSessions with residual native thread returned error: %v", err)
	}
	if len(native.Sessions) != 0 {
		t.Fatalf("ListSessions adopted residual native threads: %#v", native.Sessions)
	}
	badCwd := "relative"
	if _, badCwdErr := nativeAgent.ListSessions(ctx, acp.ListSessionsRequest{Cwd: &badCwd}); badCwdErr == nil {
		t.Fatal("ListSessions accepted relative cwd")
	}
	badCursor := "%%%"
	if _, badCursorErr := nativeAgent.ListSessions(ctx, acp.ListSessionsRequest{Cursor: &badCursor}); badCursorErr == nil {
		t.Fatal("ListSessions accepted invalid cursor")
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

	resumeClient := &errorCodexClient{spyCodexClient: newSpyCodexClient(), listThreads: []codex.Thread{{ID: "native", SessionID: "native"}}}
	resumeAgent := NewAgent(withClientFactory(func(context.Context, codex.Options) (codex.Client, error) { return resumeClient, nil }))
	if resumeAgent.options.SessionStore == nil {
		t.Fatal("NewAgent did not install the default in-memory session store")
	}
	if _, err := resumeAgent.ResumeSession(ctx, ResumeSessionRequest("native", "/tmp/project")); err == nil {
		t.Fatal("ResumeSession adopted a residual native thread absent from the default store")
	}
	if resumeAgent.activeSession("native") != nil {
		t.Fatal("ResumeSession installed a residual native thread")
	}
	resumeClient.mu.Lock()
	resumeReq := resumeClient.resume
	resumeClient.mu.Unlock()
	if resumeReq.ThreadID != "" {
		t.Fatalf("ResumeSession called native resume for an unknown store session: %#v", resumeReq)
	}

	loadClient := &errorCodexClient{spyCodexClient: newSpyCodexClient(), listThreads: []codex.Thread{{ID: "native-load", SessionID: "native-load"}}}
	loadAgent := NewAgent(withClientFactory(func(context.Context, codex.Options) (codex.Client, error) { return loadClient, nil }))
	if _, err := loadAgent.LoadSession(ctx, LoadSessionRequest("native-load", "/tmp/project")); err == nil {
		t.Fatal("LoadSession adopted native history absent from the default store")
	}
	if loadAgent.activeSession("native-load") != nil {
		t.Fatal("LoadSession installed a residual native thread")
	}
	loadClient.mu.Lock()
	loadResumeReq := loadClient.resume
	loadClient.mu.Unlock()
	if loadResumeReq.ThreadID != "" {
		t.Fatalf("LoadSession called native resume for an unknown store session: %#v", loadResumeReq)
	}
}

func TestResumeLoadActiveSessionBranches(t *testing.T) {
	ctx := context.Background()

	activeClient := newSpyCodexClient()
	activeAgent := NewAgent(withClientFactory(func(context.Context, codex.Options) (codex.Client, error) { return activeClient, nil }))
	activeID := acp.SessionId("active")
	activeStart := codexSessionStart{Cwd: "/tmp/project", ResumeID: string(activeID)}
	activeSession := newSession(activeAgent, activeID, "/tmp/project", nil, codex.Thread{ID: string(activeID), SessionID: string(activeID)}, activeClient, sessionMeta{}, nil)
	activeSession.fingerprint = codexSessionStartFingerprint(activeStart)
	if err := activeAgent.storeStartedSession(activeSession); err != nil {
		t.Fatalf("store active session: %v", err)
	}
	if _, err := activeAgent.ResumeSession(ctx, ResumeSessionRequest(activeID, "/tmp/project")); err != nil {
		t.Fatalf("ResumeSession active returned error: %v", err)
	}
	if _, err := activeAgent.ResumeSession(ctx, ResumeSessionRequest(activeID, "/tmp/project", WithSessionAdditionalDirectories("/tmp/different"))); err == nil {
		t.Fatal("ResumeSession changed active binding without store rows succeeded")
	}
	if activeAgent.activeSession(activeID) != activeSession {
		t.Fatal("ResumeSession changed active binding removed the usable session")
	}
	if _, err := activeAgent.LoadSession(ctx, LoadSessionRequest(activeID, "/tmp/project")); err == nil {
		t.Fatal("LoadSession active session without store rows succeeded")
	}
	if _, err := activeAgent.LoadSession(ctx, LoadSessionRequest(activeID, "/tmp/project", WithSessionAdditionalDirectories("/tmp/different"))); err == nil {
		t.Fatal("LoadSession changed active binding without store rows succeeded")
	}
	if activeAgent.activeSession(activeID) != activeSession {
		t.Fatal("LoadSession changed active binding removed the usable session")
	}
	if deleted := activeClient.deletedThreadSnapshot(); len(deleted) != 0 {
		t.Fatalf("store-missing active lifecycle deleted native threads: %#v", deleted)
	}
	defaultStore, ok := activeAgent.options.SessionStore.(*InMemorySessionStore)
	if !ok {
		t.Fatalf("default session store = %T, want *InMemorySessionStore", activeAgent.options.SessionStore)
	}
	if err := defaultStore.Replace(ctx, SessionKey{SessionID: string(activeID)}, []SessionStoreReplacement{{
		Key: SessionKey{SessionID: string(activeID)},
		Entries: []SessionStoreEntry{SessionStoreEntry(
			`{"type":"event_msg","payload":{"type":"agent_message","message":"stored history"}}`,
		)},
	}}); err != nil {
		t.Fatalf("seed active store: %v", err)
	}
	if _, err := activeAgent.LoadSession(ctx, LoadSessionRequest(activeID, "/tmp/project")); err != nil {
		t.Fatalf("LoadSession active with store rows returned error: %v", err)
	}

	activeLoadErrAgent := NewAgent(WithSessionStore(&configurableStore{loadErr: errors.New("active load failed")}))
	activeLoadSession := newSession(activeLoadErrAgent, activeID, "/tmp/project", nil, codex.Thread{ID: string(activeID), SessionID: string(activeID)}, activeClient, sessionMeta{}, nil)
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
	activeReplayErrSession := newSession(activeReplayErrAgent, activeID, "/tmp/project", nil, codex.Thread{ID: string(activeID), SessionID: string(activeID)}, activeClient, sessionMeta{}, nil)
	activeReplayErrSession.fingerprint = codexSessionStartFingerprint(activeStart)
	if err := activeReplayErrAgent.storeStartedSession(activeReplayErrSession); err != nil {
		t.Fatalf("store active replay err session: %v", err)
	}
	if _, err := activeReplayErrAgent.LoadSession(ctx, LoadSessionRequest(activeID, "/tmp/project")); err == nil {
		t.Fatal("LoadSession active ignored rollout replay error")
	}
}

func TestMCPToolApprovalModeChangeForcesActiveNativeRebind(t *testing.T) {
	for _, lifecycle := range []struct {
		name string
		run  func(context.Context, *Agent, acp.SessionId, ...SessionRequestOption) error
	}{
		{
			name: "resume",
			run: func(ctx context.Context, agent *Agent, id acp.SessionId, opts ...SessionRequestOption) error {
				_, err := agent.ResumeSession(ctx, ResumeSessionRequest(id, "/tmp/project", opts...))

				return err
			},
		},
		{
			name: "load",
			run: func(ctx context.Context, agent *Agent, id acp.SessionId, opts ...SessionRequestOption) error {
				_, err := agent.LoadSession(ctx, LoadSessionRequest(id, "/tmp/project", opts...))

				return err
			},
		},
	} {
		t.Run(lifecycle.name, func(t *testing.T) {
			ctx := context.Background()
			store := NewInMemorySessionStore()
			client := newRuntimeRecordingClient()
			agent := NewAgent(
				WithSessionStore(store),
				withClientFactory(func(context.Context, codex.Options) (codex.Client, error) { return client, nil }),
			)
			t.Cleanup(func() { require.NoError(t, agent.Close()) })
			server := HTTPMCPServer("marker", "https://marker.example.test/mcp", map[string]string{
				"Authorization": "Bearer marker",
			})

			created, err := agent.NewSession(ctx, NewSessionRequest(
				"/tmp/project",
				WithSessionMCPServers(server),
				WithSessionCodexOptions(NewCodexOptions(WithCodexMCPToolApprovalMode("auto"))),
			))
			require.NoError(t, err)
			require.NoError(t, store.Replace(ctx, SessionKey{SessionID: string(created.SessionId)}, []SessionStoreReplacement{{
				Key: SessionKey{SessionID: string(created.SessionId)},
				Entries: []SessionStoreEntry{SessionStoreEntry(
					`{"type":"session_meta","payload":{"id":"thread-1","cwd":"/tmp/project"}}`,
				)},
			}}))

			client.mu.Lock()
			client.resumes = nil
			client.mu.Unlock()

			err = lifecycle.run(
				ctx,
				agent,
				created.SessionId,
				WithSessionMCPServers(server),
				WithSessionCodexOptions(NewCodexOptions(WithCodexMCPToolApprovalMode("prompt"))),
			)
			require.NoError(t, err)

			client.mu.Lock()
			resumes := append([]codex.ThreadResumeRequest(nil), client.resumes...)
			client.mu.Unlock()
			require.Len(t, resumes, 1, "mode-only lifecycle change skipped thread/resume")
			require.Equal(t, "thread-1", resumes[0].ThreadID)
			servers, ok := resumes[0].Config["mcp_servers"].(map[string]any)
			require.True(t, ok)
			marker, ok := servers["marker"].(map[string]any)
			require.True(t, ok)
			require.Equal(t, "prompt", marker["default_tools_approval_mode"])
			active := agent.activeSession(created.SessionId)
			require.NotNil(t, active)
			active.mu.Lock()
			approvalMode := active.mcpApprovalMode
			active.mu.Unlock()
			require.Equal(t, "prompt", approvalMode)
		})
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
	createMaterializedRolloutTemp = func(string) (materializedRolloutFile, error) {
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
	limitSession := newSession(materializedLimitAgent, "limit", "/tmp/project", nil, codex.Thread{ID: "limit"}, newSpyCodexClient(), sessionMeta{}, nil)
	if err := materializedLimitAgent.storeStartedSession(limitSession); err != nil {
		t.Fatalf("store materialized limit session: %v", err)
	}
	if _, err := materializedLimitAgent.resumeMaterializedSession(ctx, ResumeSessionRequest("stored", "/tmp/project"), materializedEntries); err == nil {
		t.Fatal("resumeMaterializedSession ignored store backpressure")
	}
	if _, err := materializedLimitAgent.loadMaterializedSession(ctx, LoadSessionRequest("stored", "/tmp/project"), materializedEntries); err == nil {
		t.Fatal("loadMaterializedSession ignored store backpressure")
	}
	replayErrorAgent := NewAgent(withClientFactory(func(context.Context, codex.Options) (codex.Client, error) {
		return newSpyCodexClient(), nil
	}))
	replayErrorAgent.setAgentClient(&errorAgentClient{
		recordingAgentClient: newRecordingAgentClient(),
		updateErr:            errors.New("replay update failed"),
	})
	if _, err := replayErrorAgent.loadMaterializedSession(
		ctx,
		LoadSessionRequest("replay-error", "/tmp/project"),
		materializedEntries,
	); err == nil {
		t.Fatal("loadMaterializedSession ignored post-start replay error")
	}
	if _, err := agent.loadMaterializedSession(ctx, LoadSessionRequest("bad-replay", "/tmp/project"), []SessionStoreEntry{SessionStoreEntry(`not-json`)}); err == nil {
		t.Fatal("loadMaterializedSession ignored replay error")
	}
}

type activeRolloutPathClient struct {
	*spyCodexClient

	nativeThreadID string
	nativePath     string
	turnStarted    chan struct{}
	startOnce      sync.Once
	resumeCount    int
}

func (c *activeRolloutPathClient) StartThread(ctx context.Context, req codex.ThreadStartRequest) (codex.Thread, error) {
	thread, err := c.spyCodexClient.StartThread(ctx, req)
	if err != nil {
		return codex.Thread{}, err
	}

	thread.ID = c.nativeThreadID
	thread.SessionID = c.nativeThreadID
	thread.Path = c.nativePath

	return thread, nil
}

func (c *activeRolloutPathClient) ResumeThread(ctx context.Context, req codex.ThreadResumeRequest) (codex.Thread, error) {
	c.mu.Lock()
	defer c.mu.Unlock()

	c.resume = req
	c.resumeCount++
	if req.ThreadID != c.nativeThreadID {
		return codex.Thread{}, fmt.Errorf("wrong active thread: %s", req.ThreadID)
	}
	if req.Path != c.nativePath {
		return codex.Thread{}, fmt.Errorf("cannot resume running thread %s with stale path", req.ThreadID)
	}

	thread := c.thread
	thread.ID = c.nativeThreadID
	thread.SessionID = c.nativeThreadID
	thread.Path = c.nativePath
	thread.Cwd = req.Cwd
	thread.Title = ""
	thread.UpdatedAt = ""

	return thread, nil
}

func (c *activeRolloutPathClient) RunTurn(ctx context.Context, req codex.TurnStartRequest) (<-chan codex.Event, error) {
	c.mu.Lock()
	c.lastTurn = req
	c.mu.Unlock()
	c.startOnce.Do(func() { close(c.turnStarted) })

	events := make(chan codex.Event)
	go func() {
		<-ctx.Done()
		close(events)
	}()

	return events, nil
}

// A steering interrupt followed by session/close removes the wrapper session
// but leaves the native Codex thread owned by the shared app-server. A store
// resume must retain that runtime ownership, use its canonical rollout path,
// and reject store identities belonging to another logical session.
func TestResumeInterruptedActiveThreadUsesOwnedRolloutPath(t *testing.T) {
	ctx := context.Background()
	nativeThreadID := "thread-active"
	nativePath := filepath.Join(t.TempDir(), "native-rollout.jsonl")
	entries := []SessionStoreEntry{
		SessionStoreEntry(`{"type":"session_meta","payload":{"id":"thread-active","cwd":"/tmp/project"}}`),
		SessionStoreEntry(`{"type":"event_msg","payload":{"type":"user_message","message":"before interrupt"}}`),
	}
	var rollout bytes.Buffer
	for _, entry := range entries {
		rollout.Write(entry)
		rollout.WriteByte('\n')
	}
	require.NoError(t, os.WriteFile(nativePath, rollout.Bytes(), 0o600))

	store := NewInMemorySessionStore()
	client := &activeRolloutPathClient{
		spyCodexClient: newSpyCodexClient(),
		nativeThreadID: nativeThreadID,
		nativePath:     nativePath,
		turnStarted:    make(chan struct{}),
	}
	var sessionScratchAdmissions int
	agent := NewAgent(
		WithSessionStore(store),
		WithRuntimeResourceHooks(RuntimeResourceHooks{
			ReserveScratchRoot: func(_ context.Context, kind RuntimeResourceKind) (func(), error) {
				if kind == RuntimeResourceSession {
					sessionScratchAdmissions++

					return nil, errors.New("active thread must not materialize its mirrored rollout")
				}

				return func() {}, nil
			},
		}),
		withClientFactory(func(context.Context, codex.Options) (codex.Client, error) { return client, nil }),
	)
	agent.setAgentClient(newRecordingAgentClient())
	t.Cleanup(func() { require.NoError(t, agent.Close()) })

	created, err := agent.NewSession(ctx, NewSessionRequest("/tmp/project"))
	require.NoError(t, err)
	active := agent.activeSession(created.SessionId)
	require.NotNil(t, active)

	type promptResult struct {
		resp acp.PromptResponse
		err  error
	}
	promptDone := make(chan promptResult, 1)
	go func() {
		resp, promptErr := agent.Prompt(ctx, TextPromptRequest(created.SessionId, "interrupted-turn", "wait"))
		promptDone <- promptResult{resp: resp, err: promptErr}
	}()
	<-client.turnStarted
	active.cancelTurn()
	result := <-promptDone
	require.NoError(t, result.err)
	require.Equal(t, acp.StopReasonCancelled, result.resp.StopReason)

	mirrored, err := store.Load(ctx, SessionKey{SessionID: string(created.SessionId)})
	require.NoError(t, err)
	require.Equal(t, entries, mirrored)
	_, err = agent.CloseSession(ctx, acp.CloseSessionRequest{SessionId: created.SessionId})
	require.NoError(t, err)
	require.Nil(t, agent.activeSession(created.SessionId))

	client.mu.Lock()
	require.Equal(t, []string{nativeThreadID}, client.unsubscribed)
	require.Zero(t, client.resumeCount)
	client.mu.Unlock()

	hijackID := acp.SessionId("other-logical-session")
	require.NoError(t, store.Replace(ctx, SessionKey{SessionID: string(hijackID)}, []SessionStoreReplacement{{
		Key:     SessionKey{SessionID: string(hijackID)},
		Entries: entries,
	}}))
	_, err = agent.ResumeSession(ctx, ResumeSessionRequest(hijackID, "/tmp/project"))
	require.ErrorContains(t, err, "retained by another session")

	missingIdentity := []SessionStoreEntry{SessionStoreEntry(`{"type":"event_msg","payload":{"type":"user_message","message":"missing identity"}}`)}
	require.NoError(t, store.Replace(ctx, SessionKey{SessionID: string(created.SessionId)}, []SessionStoreReplacement{{
		Key:     SessionKey{SessionID: string(created.SessionId)},
		Entries: missingIdentity,
	}}))
	_, err = agent.ResumeSession(ctx, ResumeSessionRequest(created.SessionId, "/tmp/project"))
	require.ErrorContains(t, err, "thread identity is required")

	wrongRetainedIdentity := []SessionStoreEntry{SessionStoreEntry(`{"type":"session_meta","payload":{"id":"thread-other"}}`)}
	require.NoError(t, store.Replace(ctx, SessionKey{SessionID: string(created.SessionId)}, []SessionStoreReplacement{{
		Key:     SessionKey{SessionID: string(created.SessionId)},
		Entries: wrongRetainedIdentity,
	}}))
	_, err = agent.ResumeSession(ctx, ResumeSessionRequest(created.SessionId, "/tmp/project"))
	require.ErrorContains(t, err, "does not match the retained session")
	require.NoError(t, store.Replace(ctx, SessionKey{SessionID: string(created.SessionId)}, []SessionStoreReplacement{{
		Key:     SessionKey{SessionID: string(created.SessionId)},
		Entries: entries,
	}}))

	resumed, err := agent.ResumeSession(ctx, ResumeSessionRequest(
		created.SessionId,
		"/tmp/project",
		WithSessionAdditionalDirectories("/tmp/additional"),
	))
	require.NoError(t, err)
	require.NotNil(t, resumed.Meta)
	resumedActive := agent.activeSession(created.SessionId)
	require.NotNil(t, resumedActive)
	require.NotSame(t, active, resumedActive)
	require.Zero(t, sessionScratchAdmissions)

	client.mu.Lock()
	require.Equal(t, 1, client.resumeCount)
	require.Equal(t, nativeThreadID, client.resume.ThreadID)
	require.Equal(t, nativePath, client.resume.Path)
	require.Equal(t, []string{nativeThreadID}, client.unsubscribed)
	client.mu.Unlock()

	_, err = agent.LoadSession(ctx, LoadSessionRequest(
		created.SessionId,
		"/tmp/project",
		WithSessionAdditionalDirectories("/tmp/load"),
	))
	require.NoError(t, err)
	require.Same(t, resumedActive, agent.activeSession(created.SessionId))
	require.Zero(t, sessionScratchAdmissions)

	client.mu.Lock()
	require.Equal(t, 2, client.resumeCount)
	require.Equal(t, nativePath, client.resume.Path)
	require.Equal(t, []string{nativeThreadID}, client.unsubscribed)
	client.mu.Unlock()

	agent.setAgentClient(&errorAgentClient{
		recordingAgentClient: newRecordingAgentClient(),
		updateErr:            errors.New("replay update failed"),
	})
	_, err = agent.LoadSession(ctx, LoadSessionRequest(
		created.SessionId,
		"/tmp/project",
		WithSessionAdditionalDirectories("/tmp/replay-error"),
	))
	require.ErrorContains(t, err, "replay update failed")
	agent.setAgentClient(newRecordingAgentClient())

	wrongEntries := []SessionStoreEntry{
		SessionStoreEntry(`{"type":"session_meta","payload":{"id":"thread-other","cwd":"/tmp/project"}}`),
	}
	require.NoError(t, store.Replace(ctx, SessionKey{SessionID: string(created.SessionId)}, []SessionStoreReplacement{{
		Key:     SessionKey{SessionID: string(created.SessionId)},
		Entries: wrongEntries,
	}}))
	_, err = agent.LoadSession(ctx, LoadSessionRequest(
		created.SessionId,
		"/tmp/project",
		WithSessionAdditionalDirectories("/tmp/different"),
	))
	require.ErrorContains(t, err, "stored Codex thread does not match the active session")

	client.mu.Lock()
	require.Equal(t, 3, client.resumeCount, "wrong-thread snapshot reached Codex")
	client.mu.Unlock()
}

type activeRebindEdgeClient struct {
	*spyCodexClient
	resume  func(context.Context, codex.ThreadResumeRequest) (codex.Thread, error)
	account func(context.Context) (codex.Account, error)
}

func (c *activeRebindEdgeClient) ResumeThread(ctx context.Context, req codex.ThreadResumeRequest) (codex.Thread, error) {
	return c.resume(ctx, req)
}

func (c *activeRebindEdgeClient) AccountRead(ctx context.Context) (codex.Account, error) {
	if c.account != nil {
		return c.account(ctx)
	}

	return c.spyCodexClient.AccountRead(ctx)
}

func TestActiveStoredRebindFailureBranches(t *testing.T) {
	ctx := context.Background()
	entries := []SessionStoreEntry{SessionStoreEntry(`{"type":"session_meta","payload":{"id":"thread-active"}}`)}
	params := ResumeSessionRequest("session", "/tmp/project")

	bind := func(agent *Agent, client codex.Client) *session {
		active := newSession(agent, "session", "/tmp/project", nil, codex.Thread{
			ID:   "thread-active",
			Path: "/native/rollout.jsonl",
		}, client, sessionMeta{}, nil)
		agent.sessions[active.id] = active
		agent.runtimeClient = client
		agent.runtimeDead = false

		return active
	}

	t.Run("materialized dispatcher keeps active ownership", func(t *testing.T) {
		client := newSpyCodexClient()
		agent := NewAgent()
		bind(agent, client)
		_, err := agent.resumeMaterializedSession(ctx, params, entries)
		require.NoError(t, err)
	})

	t.Run("relaunch failure", func(t *testing.T) {
		agent := NewAgent(withClientFactory(func(context.Context, codex.Options) (codex.Client, error) {
			return nil, errors.New("relaunch failed")
		}))
		active := bind(agent, newSpyCodexClient())
		active.setClientDead(true)
		agent.runtimeDead = true
		_, err := agent.rebindActiveStoredSession(ctx, params, entries, sessionMeta{}, active)
		require.ErrorContains(t, err, "relaunch failed")
	})

	t.Run("ownership unavailable", func(t *testing.T) {
		agent := NewAgent()
		active := bind(agent, nil)
		_, err := agent.rebindActiveStoredSession(ctx, params, entries, sessionMeta{}, active)
		require.ErrorContains(t, err, "active Codex thread ownership is unavailable")
	})

	t.Run("MCP validation", func(t *testing.T) {
		client := newSpyCodexClient()
		agent := NewAgent()
		active := bind(agent, client)
		invalid := ResumeSessionRequest("session", "/tmp/project", WithSessionMCPServers(acp.McpServer{
			Sse: &acp.McpServerSseInline{Name: "removed"},
		}))
		_, err := agent.rebindActiveStoredSession(ctx, invalid, entries, sessionMeta{}, active)
		require.Error(t, err)
	})

	t.Run("native resume failure", func(t *testing.T) {
		client := &errorCodexClient{spyCodexClient: newSpyCodexClient(), resumeErr: errors.New("resume failed")}
		agent := NewAgent()
		active := bind(agent, client)
		_, err := agent.rebindActiveStoredSession(ctx, params, entries, sessionMeta{}, active)
		require.ErrorContains(t, err, "resume failed")
	})

	for _, tc := range []struct {
		name   string
		thread codex.Thread
		want   string
	}{
		{
			name:   "wrong returned thread",
			thread: codex.Thread{ID: "thread-other", Path: "/native/rollout.jsonl"},
			want:   "different native thread",
		},
		{
			name:   "wrong returned path",
			thread: codex.Thread{ID: "thread-active", Path: "/other/rollout.jsonl"},
			want:   "different rollout path",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			client := &activeRebindEdgeClient{
				spyCodexClient: newSpyCodexClient(),
				resume: func(context.Context, codex.ThreadResumeRequest) (codex.Thread, error) {
					return tc.thread, nil
				},
			}
			agent := NewAgent()
			active := bind(agent, client)
			_, err := agent.rebindActiveStoredSession(ctx, params, entries, sessionMeta{}, active)
			require.ErrorContains(t, err, tc.want)
		})
	}

	t.Run("empty returned path keeps ownership", func(t *testing.T) {
		client := &activeRebindEdgeClient{
			spyCodexClient: newSpyCodexClient(),
			resume: func(context.Context, codex.ThreadResumeRequest) (codex.Thread, error) {
				return codex.Thread{ID: "thread-active"}, nil
			},
		}
		agent := NewAgent()
		active := bind(agent, client)
		_, err := agent.rebindActiveStoredSession(ctx, params, entries, sessionMeta{}, active)
		require.NoError(t, err)
		require.Equal(t, "/native/rollout.jsonl", active.rolloutPath)
	})

	t.Run("canary failure", func(t *testing.T) {
		client := &runtimeFailureClient{
			runtimeRecordingClient: newRuntimeRecordingClient(),
			events:                 []codex.Event{{Kind: codex.EventCompleted}},
		}
		agent := NewAgent()
		active := bind(agent, client)
		withMCP := ResumeSessionRequest("session", "/tmp/project", WithSessionMCPServers(
			HTTPMCPServer("marker", "https://example.test/mcp", nil),
		))
		_, err := agent.rebindActiveStoredSession(ctx, withMCP, entries, sessionMeta{}, active)
		require.ErrorContains(t, err, "runtime_ready")
	})

	t.Run("ownership changes before commit", func(t *testing.T) {
		agent := NewAgent()
		client := &activeRebindEdgeClient{spyCodexClient: newSpyCodexClient()}
		client.resume = func(_ context.Context, req codex.ThreadResumeRequest) (codex.Thread, error) {
			return codex.Thread{ID: req.ThreadID, Path: req.Path}, nil
		}
		active := bind(agent, client)
		client.account = func(context.Context) (codex.Account, error) {
			agent.mu.Lock()
			agent.sessions[active.id] = &session{id: active.id}
			agent.mu.Unlock()

			return codex.Account{}, nil
		}
		_, err := agent.rebindActiveStoredSession(ctx, params, entries, sessionMeta{}, active)
		require.ErrorContains(t, err, "ownership changed during resume")
	})
}

func TestRetainedRuntimeResumeFailureBranches(t *testing.T) {
	ctx := context.Background()
	entries := []SessionStoreEntry{SessionStoreEntry(`{"type":"session_meta","payload":{"id":"thread-active"}}`)}
	params := ResumeSessionRequest("session", "/tmp/project")

	fixture := func(client codex.Client) (*Agent, *retainedRuntimeThread) {
		agent := NewAgent()
		agent.runtimeClient = client
		agent.runtimeEpoch = 9
		retained := &retainedRuntimeThread{
			sessionID: "session",
			threadID:  "thread-active",
			path:      "/native/rollout.jsonl",
			client:    client,
			epoch:     9,
			claimed:   true,
		}
		agent.retainedThreads[retained.sessionID] = retained

		return agent, retained
	}

	t.Run("MCP validation", func(t *testing.T) {
		agent, retained := fixture(newSpyCodexClient())
		invalid := ResumeSessionRequest("session", "/tmp/project", WithSessionMCPServers(acp.McpServer{
			Sse: &acp.McpServerSseInline{Name: "removed"},
		}))
		_, _, err := agent.resumeRetainedRuntimeSession(ctx, invalid, sessionMeta{}, retained)
		require.Error(t, err)
	})

	t.Run("native resume failure", func(t *testing.T) {
		client := &errorCodexClient{spyCodexClient: newSpyCodexClient(), resumeErr: errors.New("retained resume failed")}
		agent, retained := fixture(client)
		_, _, err := agent.resumeRetainedRuntimeSession(ctx, params, sessionMeta{}, retained)
		require.ErrorContains(t, err, "retained resume failed")
	})

	for _, tc := range []struct {
		name   string
		thread codex.Thread
		want   string
	}{
		{
			name:   "wrong returned thread",
			thread: codex.Thread{ID: "thread-other", Path: "/native/rollout.jsonl"},
			want:   "different retained native thread",
		},
		{
			name:   "wrong returned path",
			thread: codex.Thread{ID: "thread-active", Path: "/other/rollout.jsonl"},
			want:   "different rollout path",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			client := &activeRebindEdgeClient{
				spyCodexClient: newSpyCodexClient(),
				resume: func(context.Context, codex.ThreadResumeRequest) (codex.Thread, error) {
					return tc.thread, nil
				},
			}
			agent, retained := fixture(client)
			_, _, err := agent.resumeRetainedRuntimeSession(ctx, params, sessionMeta{}, retained)
			require.ErrorContains(t, err, tc.want)
			require.False(t, retained.claimed)
			require.Len(t, client.unsubscribed, 1)

			client.resume = func(_ context.Context, req codex.ThreadResumeRequest) (codex.Thread, error) {
				return codex.Thread{ID: req.ThreadID, Path: req.Path}, nil
			}
			claimed, claimErr := agent.claimRetainedRuntimeThreadForStore("session", "thread-active")
			require.NoError(t, claimErr)
			require.Same(t, retained, claimed)
			_, retried, retryErr := agent.resumeRetainedRuntimeSession(ctx, params, sessionMeta{}, claimed)
			require.NoError(t, retryErr)
			require.NotNil(t, retried)
			require.NoError(t, agent.Close())
		})
	}

	t.Run("empty returned path keeps retained ownership", func(t *testing.T) {
		client := &activeRebindEdgeClient{
			spyCodexClient: newSpyCodexClient(),
			resume: func(_ context.Context, req codex.ThreadResumeRequest) (codex.Thread, error) {
				return codex.Thread{ID: req.ThreadID}, nil
			},
		}
		agent, retained := fixture(client)
		_, session, err := agent.resumeRetainedRuntimeSession(ctx, params, sessionMeta{}, retained)
		require.NoError(t, err)
		require.Equal(t, "/native/rollout.jsonl", session.rolloutPath)
		require.NoError(t, agent.Close())
	})

	t.Run("canary failure", func(t *testing.T) {
		client := &runtimeFailureClient{
			runtimeRecordingClient: newRuntimeRecordingClient(),
			events:                 []codex.Event{{Kind: codex.EventCompleted}},
		}
		agent, retained := fixture(client)
		withMCP := ResumeSessionRequest("session", "/tmp/project", WithSessionMCPServers(
			HTTPMCPServer("marker", "https://example.test/mcp", nil),
		))
		_, _, err := agent.resumeRetainedRuntimeSession(ctx, withMCP, sessionMeta{}, retained)
		require.ErrorContains(t, err, "runtime_ready")
		require.False(t, retained.claimed)
		require.Len(t, client.unsubscribed, 1)
		claimed, claimErr := agent.claimRetainedRuntimeThreadForStore("session", "thread-active")
		require.NoError(t, claimErr)
		_, retried, retryErr := agent.resumeRetainedRuntimeSession(ctx, params, sessionMeta{}, claimed)
		require.NoError(t, retryErr)
		require.NotNil(t, retried)
		require.NoError(t, agent.Close())
	})

	t.Run("store commit failure rolls back and retries", func(t *testing.T) {
		client := &activeRebindEdgeClient{
			spyCodexClient: newSpyCodexClient(),
			resume: func(_ context.Context, req codex.ThreadResumeRequest) (codex.Thread, error) {
				return codex.Thread{ID: req.ThreadID, Path: req.Path}, nil
			},
		}
		agent, retained := fixture(client)
		agent.options.ConcurrencyLimits.MaxActiveSessions = 1
		agent.sessions["occupied"] = &session{id: "occupied"}
		_, _, err := agent.resumeRetainedRuntimeSession(ctx, params, sessionMeta{}, retained)
		require.ErrorContains(t, err, valueBackpressure)
		require.False(t, retained.claimed)
		require.Len(t, client.unsubscribed, 1)

		delete(agent.sessions, "occupied")
		claimed, claimErr := agent.claimRetainedRuntimeThreadForStore("session", "thread-active")
		require.NoError(t, claimErr)
		_, retried, retryErr := agent.resumeRetainedRuntimeSession(ctx, params, sessionMeta{}, claimed)
		require.NoError(t, retryErr)
		require.NotNil(t, retried)
		require.NoError(t, agent.Close())
	})

	t.Run("ownership changes before commit", func(t *testing.T) {
		client := &activeRebindEdgeClient{spyCodexClient: newSpyCodexClient()}
		client.resume = func(_ context.Context, req codex.ThreadResumeRequest) (codex.Thread, error) {
			return codex.Thread{ID: req.ThreadID, Path: req.Path}, nil
		}
		agent, retained := fixture(client)
		client.account = func(context.Context) (codex.Account, error) {
			agent.mu.Lock()
			agent.retainedThreads[retained.sessionID] = &retainedRuntimeThread{sessionID: retained.sessionID}
			agent.mu.Unlock()

			return codex.Account{}, nil
		}
		_, _, err := agent.resumeRetainedRuntimeSession(ctx, params, sessionMeta{}, retained)
		require.ErrorContains(t, err, "ownership changed")
	})

	t.Run("load retained replay error and success", func(t *testing.T) {
		loadEntries := []SessionStoreEntry{
			SessionStoreEntry(`{"type":"session_meta","payload":{"id":"thread-active"}}`),
			SessionStoreEntry(`{"type":"event_msg","payload":{"type":"agent_message","message":"history"}}`),
		}

		client := &activeRebindEdgeClient{
			spyCodexClient: newSpyCodexClient(),
			resume: func(_ context.Context, req codex.ThreadResumeRequest) (codex.Thread, error) {
				return codex.Thread{ID: req.ThreadID, Path: req.Path}, nil
			},
		}
		agent, retained := fixture(client)
		retained.claimed = false
		agent.setAgentClient(&errorAgentClient{
			recordingAgentClient: newRecordingAgentClient(),
			updateErr:            errors.New("retained replay failed"),
		})
		_, err := agent.loadMaterializedSession(ctx, LoadSessionRequest("session", "/tmp/project"), loadEntries)
		require.ErrorContains(t, err, "retained replay failed")

		client = &activeRebindEdgeClient{
			spyCodexClient: newSpyCodexClient(),
			resume: func(_ context.Context, req codex.ThreadResumeRequest) (codex.Thread, error) {
				return codex.Thread{ID: req.ThreadID, Path: req.Path}, nil
			},
		}
		agent, retained = fixture(client)
		retained.claimed = false
		agent.setAgentClient(newRecordingAgentClient())
		_, err = agent.loadMaterializedSession(ctx, LoadSessionRequest("session", "/tmp/project"), loadEntries)
		require.NoError(t, err)
	})

	t.Run("load retained lookup and resume errors", func(t *testing.T) {
		client := newSpyCodexClient()
		agent, retained := fixture(client)
		retained.claimed = false
		wrong := []SessionStoreEntry{SessionStoreEntry(`{"type":"session_meta","payload":{"id":"wrong"}}`)}
		_, err := agent.loadMaterializedSession(ctx, LoadSessionRequest("session", "/tmp/project"), wrong)
		require.ErrorContains(t, err, "does not match")

		agent, retained = fixture(client)
		retained.claimed = false
		_, err = agent.loadMaterializedSession(ctx, LoadSessionRequest(
			"session",
			"/tmp/project",
			WithSessionMCPServers(acp.McpServer{Sse: &acp.McpServerSseInline{Name: "removed"}}),
		), entries)
		require.Error(t, err)
	})
}

type blockingLifecycleCodexClient struct {
	*spyCodexClient

	unsubscribeStarted chan struct{}
	unsubscribeRelease chan struct{}
	unsubscribeOnce    sync.Once
	resumeStarted      chan codex.ThreadResumeRequest
	resumeRelease      chan struct{}
	resumeMu           sync.Mutex
	resumeCalls        int
}

func (c *blockingLifecycleCodexClient) UnsubscribeThread(ctx context.Context, threadID string) error {
	if c.unsubscribeStarted != nil {
		c.unsubscribeOnce.Do(func() { close(c.unsubscribeStarted) })
		select {
		case <-c.unsubscribeRelease:
		case <-ctx.Done():
			return ctx.Err()
		}
	}

	return c.spyCodexClient.UnsubscribeThread(ctx, threadID)
}

func (c *blockingLifecycleCodexClient) ResumeThread(ctx context.Context, req codex.ThreadResumeRequest) (codex.Thread, error) {
	c.resumeMu.Lock()
	c.resumeCalls++
	c.resumeMu.Unlock()

	if c.resumeStarted != nil {
		select {
		case c.resumeStarted <- req:
		default:
		}
		select {
		case <-c.resumeRelease:
		case <-ctx.Done():
			return codex.Thread{}, ctx.Err()
		}
	}

	thread, err := c.spyCodexClient.ResumeThread(ctx, req)
	if thread.Path == "" {
		thread.Path = req.Path
	}

	return thread, err
}

func (c *blockingLifecycleCodexClient) resumeCallCount() int {
	c.resumeMu.Lock()
	defer c.resumeMu.Unlock()

	return c.resumeCalls
}

func TestCloseSessionSerializesRetainedResume(t *testing.T) {
	ctx := context.Background()
	store := &configurableStore{}
	client := &blockingLifecycleCodexClient{
		spyCodexClient:     newSpyCodexClient(),
		unsubscribeStarted: make(chan struct{}),
		unsubscribeRelease: make(chan struct{}),
	}
	client.thread.Path = "/native/rollout.jsonl"
	agent := NewAgent(
		WithSessionStore(store),
		withClientFactory(func(context.Context, codex.Options) (codex.Client, error) { return client, nil }),
	)
	created, err := agent.NewSession(ctx, NewSessionRequest("/tmp/project"))
	require.NoError(t, err)
	store.entries = []SessionStoreEntry{SessionStoreEntry(fmt.Sprintf(
		`{"type":"session_meta","payload":{"id":%q}}`, client.thread.ID,
	))}

	closeResult := make(chan error, 1)
	go func() {
		_, closeErr := agent.CloseSession(ctx, acp.CloseSessionRequest{SessionId: created.SessionId})
		closeResult <- closeErr
	}()
	<-client.unsubscribeStarted

	_, err = agent.ResumeSession(ctx, ResumeSessionRequest(created.SessionId, "/tmp/project"))
	require.ErrorContains(t, err, "session close in progress")
	require.Zero(t, client.resumeCallCount())

	close(client.unsubscribeRelease)
	require.NoError(t, <-closeResult)
	_, err = agent.ResumeSession(ctx, ResumeSessionRequest(created.SessionId, "/tmp/project"))
	require.NoError(t, err)
	require.Equal(t, 1, client.resumeCallCount())
	client.mu.Lock()
	require.Equal(t, "/native/rollout.jsonl", client.resume.Path)
	client.mu.Unlock()
	require.NoError(t, agent.Close())
}

func TestCloseSessionUnsubscribeFailureKeepsOwnership(t *testing.T) {
	ctx := context.Background()
	client := &errorCodexClient{spyCodexClient: newSpyCodexClient(), unsubscribeErr: errors.New("unsubscribe failed")}
	client.thread.Path = "/native/rollout.jsonl"
	agent := NewAgent(withClientFactory(func(context.Context, codex.Options) (codex.Client, error) { return client, nil }))
	created, err := agent.NewSession(ctx, NewSessionRequest("/tmp/project"))
	require.NoError(t, err)
	active := agent.activeSession(created.SessionId)
	materialized, err := materializeRollout(t.TempDir(), []SessionStoreEntry{SessionStoreEntry(`{"type":"session_meta"}`)})
	require.NoError(t, err)
	released := false
	active.materializedPath = materialized
	active.materializedRelease = func() { released = true }

	_, err = agent.CloseSession(ctx, acp.CloseSessionRequest{SessionId: created.SessionId})
	require.ErrorContains(t, err, "unsubscribe failed")
	require.Same(t, active, agent.activeSession(created.SessionId))
	require.Empty(t, agent.retainedThreads)
	require.FileExists(t, materialized)
	require.False(t, released)
	active.mu.Lock()
	require.False(t, active.closing)
	require.Equal(t, materialized, active.materializedPath)
	active.mu.Unlock()

	client.unsubscribeErr = nil
	_, err = agent.CloseSession(ctx, acp.CloseSessionRequest{SessionId: created.SessionId})
	require.NoError(t, err)
	require.Nil(t, agent.activeSession(created.SessionId))
	require.Equal(t, materialized, agent.retainedThreads[created.SessionId].materializedPath)
	require.False(t, released)
	require.NoError(t, agent.Close())
	require.True(t, released)
}

func retainedConcurrencyFixture(store SessionStore, client codex.Client) (*Agent, *retainedRuntimeThread) {
	agent := NewAgent(WithSessionStore(store))
	agent.runtimeClient = client
	agent.runtimeEpoch = 11
	retained := &retainedRuntimeThread{
		sessionID: "session",
		threadID:  "thread-active",
		path:      "/native/rollout.jsonl",
		client:    client,
		epoch:     11,
	}
	agent.retainedThreads[retained.sessionID] = retained

	return agent, retained
}

func TestRetainedRuntimeClaimSerializesResumeAndDelete(t *testing.T) {
	ctx := context.Background()
	entries := []SessionStoreEntry{SessionStoreEntry(`{"type":"session_meta","payload":{"id":"thread-active"}}`)}
	params := ResumeSessionRequest("session", "/tmp/project")

	t.Run("double resume and delete lose to claimed resume", func(t *testing.T) {
		store := &configurableStore{entries: entries}
		client := &blockingLifecycleCodexClient{
			spyCodexClient: newSpyCodexClient(),
			resumeStarted:  make(chan codex.ThreadResumeRequest, 1),
			resumeRelease:  make(chan struct{}),
		}
		agent, _ := retainedConcurrencyFixture(store, client)

		firstResult := make(chan error, 1)
		go func() {
			_, resumeErr := agent.ResumeSession(ctx, params)
			firstResult <- resumeErr
		}()
		<-client.resumeStarted

		_, err := agent.ResumeSession(ctx, params)
		require.ErrorContains(t, err, "lifecycle is already in progress")
		_, err = agent.UnstableDeleteSession(ctx, DeleteSessionRequest("session"))
		require.ErrorContains(t, err, "lifecycle is already in progress")
		require.Equal(t, 1, client.resumeCallCount())
		require.Empty(t, client.deletedThreadSnapshot())

		close(client.resumeRelease)
		require.NoError(t, <-firstResult)
		require.Equal(t, 1, client.resumeCallCount())
		require.NoError(t, agent.Close())
	})

	t.Run("resume loses to claimed delete", func(t *testing.T) {
		store := &blockingDeleteSessionStore{
			configurableStore: &configurableStore{entries: entries},
			started:           make(chan struct{}),
			release:           make(chan struct{}),
		}
		client := &blockingLifecycleCodexClient{spyCodexClient: newSpyCodexClient()}
		agent, _ := retainedConcurrencyFixture(store, client)

		deleteResult := make(chan error, 1)
		go func() {
			_, deleteErr := agent.UnstableDeleteSession(ctx, DeleteSessionRequest("session"))
			deleteResult <- deleteErr
		}()
		<-store.started

		_, err := agent.ResumeSession(ctx, params)
		require.ErrorContains(t, err, "lifecycle is already in progress")
		require.Zero(t, client.resumeCallCount())

		close(store.release)
		require.NoError(t, <-deleteResult)
		deletedThreads := client.deletedThreadSnapshot()
		require.NotEmpty(t, deletedThreads)
		require.Contains(t, deletedThreads, "thread-active")
		require.NoError(t, agent.Close())
	})
}

func TestSessionLifecycleGuardBranches(t *testing.T) {
	ctx := context.Background()
	agent := NewAgent()
	client := newSpyCodexClient()
	active := newSession(agent, "session", "/tmp/project", nil, codex.Thread{ID: "thread"}, client, sessionMeta{}, nil)
	agent.sessions[active.id] = active

	require.NoError(t, agent.validateSessionLifecycle(active.id, active))
	agent.closed = true
	require.ErrorContains(t, agent.validateSessionLifecycle(active.id, active), "agent is closed")
	_, err := agent.acquireSessionLifecycle(active.id)
	require.ErrorContains(t, err, "agent is closed")
	_, err = agent.beginSessionClose(active.id)
	require.ErrorContains(t, err, "agent is closed")
	agent.closed = false

	active.closing = true
	_, err = agent.session(active.id)
	require.ErrorContains(t, err, "session close in progress")
	_, err = agent.acquireSessionLifecycle(active.id)
	require.ErrorContains(t, err, "session close in progress")
	_, err = agent.beginSessionClose(active.id)
	require.ErrorContains(t, err, "session close in progress")
	require.ErrorContains(t, agent.validateSessionLifecycle(active.id, active), "session close in progress")
	_, err = agent.LoadSession(ctx, LoadSessionRequest(active.id, "/tmp/project"))
	require.ErrorContains(t, err, "session close in progress")
	active.closing = false

	delete(agent.sessions, active.id)
	require.ErrorContains(t, agent.validateSessionLifecycle(active.id, active), "unknown session")
	agent.abortSessionClose(active.id, active)
	agent.sessions[active.id] = active

	materialized, err := materializeRollout(t.TempDir(), []SessionStoreEntry{SessionStoreEntry(`{"type":"session_meta"}`)})
	require.NoError(t, err)
	released := false
	active.materializedPath = materialized
	active.materializedRelease = func() { released = true }
	_, err = agent.CloseSession(ctx, acp.CloseSessionRequest{SessionId: active.id})
	require.NoError(t, err)
	require.True(t, released)
	require.NoFileExists(t, materialized)
}

func TestAcquireSessionLifecycleRevalidatesAfterWaiting(t *testing.T) {
	agent := NewAgent()
	active := newSession(agent, "session", "/tmp/project", nil, codex.Thread{ID: "thread"}, newSpyCodexClient(), sessionMeta{}, nil)
	agent.sessions[active.id] = active

	// Hold both locks that follow the initial Agent lookup. Observing Agent.mu
	// held proves the acquisition goroutine passed that lookup and is waiting
	// on session.mu; holding lifecycle keeps it from revalidating too early.
	active.lifecycle.Lock()
	active.mu.Lock()

	result := make(chan error, 1)
	go func() {
		_, err := agent.acquireSessionLifecycle(active.id)
		result <- err
	}()

	require.Eventually(t, func() bool {
		if agent.mu.TryLock() {
			agent.mu.Unlock()

			return false
		}

		return true
	}, time.Second, time.Millisecond)

	active.mu.Unlock()
	require.Eventually(t, func() bool {
		if !agent.mu.TryLock() {
			return false
		}

		delete(agent.sessions, active.id)
		agent.mu.Unlock()

		return true
	}, time.Second, time.Millisecond)

	active.lifecycle.Unlock()
	require.ErrorContains(t, <-result, "unknown session")
}

func TestRetainedResumeRollbackUnsubscribeFailureStaysClaimed(t *testing.T) {
	ctx := context.Background()
	client := &errorCodexClient{
		spyCodexClient: newSpyCodexClient(),
		unsubscribeErr: errors.New("rollback unsubscribe failed"),
	}
	client.thread.Path = "/wrong/rollout.jsonl"
	agent := NewAgent()
	agent.runtimeClient = client
	agent.runtimeEpoch = 4
	retained := &retainedRuntimeThread{
		sessionID: "session",
		threadID:  "thread-active",
		path:      "/native/rollout.jsonl",
		client:    client,
		epoch:     4,
		claimed:   true,
	}
	agent.retainedThreads[retained.sessionID] = retained

	_, _, err := agent.resumeRetainedRuntimeSession(
		ctx,
		ResumeSessionRequest(retained.sessionID, "/tmp/project"),
		sessionMeta{},
		retained,
	)
	require.ErrorContains(t, err, "different rollout path")
	require.ErrorContains(t, err, "rollback unsubscribe failed")
	require.True(t, retained.claimed)
	require.Same(t, retained, agent.retainedThreads[retained.sessionID])

	retained.claimed = false
	client.unsubscribeErr = nil
	require.NoError(t, agent.Close())
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
	factoryParent := newSession(factoryErrAgent, "parent", "/tmp/project", nil, parentThread, newSpyCodexClient(), sessionMeta{}, nil)
	if err := factoryErrAgent.storeStartedSession(factoryParent); err != nil {
		t.Fatalf("store factory parent: %v", err)
	}
	if _, err := factoryErrAgent.forkSession(ctx, ForkSessionRequest("parent", "/tmp/project")); err == nil {
		t.Fatal("forkSession ignored factory error")
	}

	forkErrAgent := NewAgent(withClientFactory(func(context.Context, codex.Options) (codex.Client, error) {
		return &errorCodexClient{spyCodexClient: newSpyCodexClient(), forkErr: codex.ErrThreadNotFound}, nil
	}))
	forkParent := newSession(forkErrAgent, "parent", "/tmp/project", nil, parentThread, newSpyCodexClient(), sessionMeta{}, nil)
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
	limitParent := newSession(limitAgent, "parent", "/tmp/project", nil, parentThread, newSpyCodexClient(), sessionMeta{}, nil)
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
		Meta: sessionMeta{ApprovalPolicy: map[string]any{"mode": "ask"}},
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
	if got := mcpServerName(acp.McpServer{Sse: &acp.McpServerSseInline{Name: "sse"}}); got != "sse" {
		t.Fatalf("SSE mcpServerName = %q", got)
	}
	if got := mcpServerName(acp.McpServer{Stdio: &acp.McpServerStdio{Name: "stdio"}}); got != "stdio" {
		t.Fatalf("stdio mcpServerName = %q", got)
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
	if err := codexThreadACPError(nil, nil); err != nil {
		t.Fatalf("nil codexThreadACPError = %v", err)
	}
	if err := lifecycleMetaError(acp.NewInvalidParams(map[string]any{"x": "y"})); err == nil {
		t.Fatal("lifecycleMetaError returned nil")
	}
	if got := NewAgent().codexConfig(); got != nil {
		t.Fatalf("codexConfig with no overrides = %#v, want nil", got)
	}
	overrideAgent := NewAgent(WithCodexConfigOverrides(map[string]any{"model_provider": "litellm"}))
	config := overrideAgent.codexConfig()
	if config["model_provider"] != "litellm" {
		t.Fatalf("codexConfig = %#v", config)
	}
	config["model_provider"] = "mutated"
	if overrideAgent.options.Config["model_provider"] != "litellm" {
		t.Fatalf("codexConfig did not return independent clone: %#v", overrideAgent.options.Config)
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

type blockingDeleteSessionStore struct {
	*configurableStore
	started chan struct{}
	release chan struct{}
	once    sync.Once
}

func (s *blockingDeleteSessionStore) Delete(ctx context.Context, _ SessionKey) error {
	s.once.Do(func() { close(s.started) })
	select {
	case <-s.release:
		return s.deleteErr
	case <-ctx.Done():
		return ctx.Err()
	}
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

func TestListSessionsListsStoreBackedSessionsWithoutCwd(t *testing.T) {
	agent := NewAgent(WithSessionStore(&configurableStore{summaries: []SessionSummary{
		{SessionID: "stored-a", Cwd: "/tmp/project-a", UpdatedAtUnixMilli: 2},
		{SessionID: "stored-b", Cwd: "", UpdatedAtUnixMilli: 1},
	}}))

	resp, err := agent.ListSessions(context.Background(), acp.ListSessionsRequest{})
	if err != nil {
		t.Fatalf("ListSessions returned error: %v", err)
	}
	if len(resp.Sessions) != 2 || resp.Sessions[0].SessionId != "stored-a" || resp.Sessions[1].SessionId != "stored-b" {
		t.Fatalf("store-backed sessions without cwd = %#v", resp.Sessions)
	}

	cwd := "/tmp/project-a"
	filtered, err := agent.ListSessions(context.Background(), acp.ListSessionsRequest{Cwd: &cwd})
	if err != nil {
		t.Fatalf("ListSessions with cwd returned error: %v", err)
	}
	if len(filtered.Sessions) != 2 {
		t.Fatalf("cwd filter must retain empty-cwd summaries, got %#v", filtered.Sessions)
	}

	other := "/tmp/other"
	otherFiltered, err := agent.ListSessions(context.Background(), acp.ListSessionsRequest{Cwd: &other})
	if err != nil {
		t.Fatalf("ListSessions with other cwd returned error: %v", err)
	}
	if len(otherFiltered.Sessions) != 1 || otherFiltered.Sessions[0].SessionId != "stored-b" {
		t.Fatalf("cwd filter result = %#v", otherFiltered.Sessions)
	}
}

func TestCodexThreadACPErrorBranches(t *testing.T) {
	if err := codexThreadACPError(errors.New("unauthorized"), nil); err == nil {
		t.Fatal("auth error returned nil")
	} else {
		var reqErr *acp.RequestError
		if !errors.As(err, &reqErr) || reqErr.Code != -32000 {
			t.Fatalf("auth error mapped to %v, want -32000", err)
		}
	}

	if err := codexThreadACPError(errors.Join(codex.ErrThreadNotFound, errors.New("gone")), nil); err == nil {
		t.Fatal("thread-not-found returned nil")
	} else {
		var reqErr *acp.RequestError
		if !errors.As(err, &reqErr) || reqErr.Code != -32602 {
			t.Fatalf("thread-not-found mapped to %v, want -32602", err)
		}
	}

	passthrough := errors.New("some other error")
	if err := codexThreadACPError(passthrough, nil); !errors.Is(err, passthrough) {
		t.Fatalf("passthrough error = %v", err)
	}
}
