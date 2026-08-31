package codexacp

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"reflect"
	"sync"
	"testing"
	"time"

	"github.com/coder/acp-go-sdk"
	"github.com/savid/acp-go-codex/internal/codex"
	"github.com/savid/acp-go-codex/internal/lifecycle"
	"github.com/stretchr/testify/require"
)

type gatedHostActionWriter struct {
	requestStarted chan json.RawMessage
	releaseRequest chan struct{}
	once           sync.Once
}

func (w *gatedHostActionWriter) InterruptWrite() error { return nil }

func (w *gatedHostActionWriter) Write(payload []byte) (int, error) {
	var message struct {
		Method string          `json:"method"`
		ID     json.RawMessage `json:"id"`
	}
	_ = json.Unmarshal(payload, &message)
	if message.Method == acp.ClientMethodSessionRequestPermission || message.Method == acp.ClientMethodElicitationCreate {
		w.once.Do(func() {
			w.requestStarted <- append(json.RawMessage(nil), message.ID...)
			<-w.releaseRequest
		})
	}

	return len(payload), nil
}

// strictPermissionClient enforces the current publication order: a permission
// request is accepted only after its toolCallId is published and before that
// tool call reaches a terminal state.
type strictPermissionClient struct {
	*recordingAgentClient

	mu          sync.Mutex
	states      map[acp.ToolCallId]acp.ToolCallStatus
	order       []string
	permissions []acp.RequestPermissionRequest
	callbacks   []map[string]any
	turnNonce   string
}

type blockingElicitationAgentClient struct {
	*recordingAgentClient
	entered chan elicitationScope
	release chan struct{}
}

type orderedBarrierPermissionClient struct {
	*strictPermissionClient
	startEntered      chan struct{}
	releaseStart      chan struct{}
	permissionEntered chan struct{}
	releasePermission chan struct{}
	startOnce         sync.Once
	permissionOnce    sync.Once
}

func (c *orderedBarrierPermissionClient) SessionUpdate(
	ctx context.Context,
	notification acp.SessionNotification,
) error {
	if notification.Update.ToolCall != nil {
		c.startOnce.Do(func() { close(c.startEntered) })
		select {
		case <-c.releaseStart:
		case <-ctx.Done():
			return ctx.Err()
		}
	}

	return c.strictPermissionClient.SessionUpdate(ctx, notification)
}

func (c *orderedBarrierPermissionClient) RequestPermission(
	ctx context.Context,
	request acp.RequestPermissionRequest,
) (acp.RequestPermissionResponse, error) {
	response, err := c.strictPermissionClient.RequestPermission(ctx, request)
	if err != nil {
		return response, err
	}

	c.permissionOnce.Do(func() { close(c.permissionEntered) })
	select {
	case <-c.releasePermission:
		return response, nil
	case <-ctx.Done():
		return acp.RequestPermissionResponse{}, ctx.Err()
	}
}

func (c *blockingElicitationAgentClient) CreateElicitation(
	ctx context.Context,
	request acp.UnstableCreateElicitationRequest,
	scope elicitationScope,
) (acp.UnstableCreateElicitationResponse, error) {
	c.elicitations = append(c.elicitations, request)
	c.scopes = append(c.scopes, scope)
	c.entered <- scope

	select {
	case <-c.release:
		return acp.UnstableCreateElicitationResponse{
			Accept: &acp.UnstableCreateElicitationAccept{Action: "accept", Content: map[string]any{"value": "ok"}},
		}, nil
	case <-ctx.Done():
		return acp.UnstableCreateElicitationResponse{}, ctx.Err()
	}
}

func (c *blockingElicitationAgentClient) CreateElicitationRegistered(
	ctx context.Context,
	request acp.UnstableCreateElicitationRequest,
	scope elicitationScope,
	_ string,
	registered func() error,
) (acp.UnstableCreateElicitationResponse, error) {
	if err := registered(); err != nil {
		return acp.UnstableCreateElicitationResponse{}, err
	}

	return c.CreateElicitation(ctx, request, scope)
}

func newStrictPermissionClient(turnNonce string) *strictPermissionClient {
	return &strictPermissionClient{
		recordingAgentClient: newRecordingAgentClient(),
		states:               make(map[acp.ToolCallId]acp.ToolCallStatus),
		turnNonce:            turnNonce,
	}
}

func (c *strictPermissionClient) SessionUpdate(_ context.Context, notification acp.SessionNotification) error {
	c.mu.Lock()
	defer c.mu.Unlock()

	wantMeta := inboundRouteMeta(c.turnNonce)
	if !reflect.DeepEqual(notification.Meta, wantMeta) {
		return fmt.Errorf("session update route = %#v, want %#v", notification.Meta, wantMeta)
	}

	if start := notification.Update.ToolCall; start != nil {
		c.states[start.ToolCallId] = start.Status
		c.order = append(c.order, fmt.Sprintf("start:%s:%s", start.ToolCallId, start.Status))
	}
	if update := notification.Update.ToolCallUpdate; update != nil {
		if _, published := c.states[update.ToolCallId]; !published {
			return fmt.Errorf("tool update %q preceded its start", update.ToolCallId)
		}
		if update.Status != nil {
			c.states[update.ToolCallId] = *update.Status
			c.order = append(c.order, fmt.Sprintf("update:%s:%s", update.ToolCallId, *update.Status))
		}
	}

	return nil
}

func (c *strictPermissionClient) RequestPermission(
	ctx context.Context,
	request acp.RequestPermissionRequest,
) (acp.RequestPermissionResponse, error) {
	c.mu.Lock()
	defer c.mu.Unlock()

	status, published := c.states[request.ToolCall.ToolCallId]
	if !published || status == acp.ToolCallStatusCompleted || status == acp.ToolCallStatusFailed {
		return acp.RequestPermissionResponse{}, fmt.Errorf(
			"permission tool %q is not an already-published nonterminal tool call",
			request.ToolCall.ToolCallId,
		)
	}

	c.permissions = append(c.permissions, request)
	c.callbacks = append(c.callbacks, cloneAnyMap(turnRouteMetaFromContext(ctx)))
	c.order = append(c.order, "permission:"+string(request.ToolCall.ToolCallId))

	return acp.RequestPermissionResponse{Outcome: acp.NewRequestPermissionOutcomeSelected("accept")}, nil
}

func (c *strictPermissionClient) callbackRoutes() []map[string]any {
	c.mu.Lock()
	defer c.mu.Unlock()

	return append([]map[string]any(nil), c.callbacks...)
}

func (c *strictPermissionClient) snapshot() ([]string, []acp.RequestPermissionRequest) {
	c.mu.Lock()
	defer c.mu.Unlock()

	return append([]string(nil), c.order...), append([]acp.RequestPermissionRequest(nil), c.permissions...)
}

func newStrictPermissionSession(t *testing.T) (*Agent, *session, *strictPermissionClient, context.Context) {
	t.Helper()

	ctx := context.Background()
	client := newSpyCodexClient()
	agent := NewAgent(withClientFactory(func(context.Context, codex.Options) (codex.Client, error) {
		return client, nil
	}))
	conn := newStrictPermissionClient("permission-turn")
	agent.setAgentClient(conn)

	response, err := agent.NewSession(ctx, NewSessionRequest(t.TempDir()))
	require.NoError(t, err)
	session := agent.sessionMust(response.SessionId)
	turnCtx := session.beginTurn(ctx, "permission-turn")
	session.setTurnID("native-permission-turn")
	t.Cleanup(session.finishTurn)
	t.Cleanup(session.fenceSession)

	return agent, session, conn, turnCtx
}

func TestStrictPermissionClientRejectsMissingOrIncorrectNotificationRoute(t *testing.T) {
	client := newStrictPermissionClient("turn-exact")

	for name, meta := range map[string]map[string]any{
		"missing": nil,
		"stale":   inboundRouteMeta("turn-stale"),
		"extra": {
			routeMetaKey: map[string]any{
				routeVersionKey:   routeVersion,
				routeTurnNonceKey: "turn-exact",
				"extra":           true,
			},
		},
	} {
		t.Run(name, func(t *testing.T) {
			err := client.SessionUpdate(context.Background(), acp.SessionNotification{Meta: meta})
			require.ErrorContains(t, err, "session update route")
		})
	}

	require.NoError(t, client.SessionUpdate(context.Background(), acp.SessionNotification{
		Meta: inboundRouteMeta("turn-exact"),
	}))
}

func emitNativePermissionToolEvent(
	t *testing.T,
	session *session,
	ctx context.Context,
	event codex.Event,
) {
	t.Helper()

	state := &promptEventState{snapshot: session.snapshot()}
	require.NoError(t, session.emitPromptUpdates(ctx, event, event, state))
}

func TestMCPPermissionCorrelatesLiveShapedApprovalToPublishedExecID(t *testing.T) {
	agent, session, conn, turnCtx := newStrictPermissionSession(t)

	// A concurrent MCP call makes correlation prove server/tool identity rather
	// than accidentally selecting the only active tool.
	emitNativePermissionToolEvent(t, session, turnCtx, codex.Event{
		Kind: codex.EventToolStarted,
		Tool: codex.ToolEvent{
			ID:    "exec-decoy",
			Title: "other search",
			Kind:  toolKindMcpToolCall,
			Raw:   map[string]any{"id": "exec-decoy", "type": toolKindMcpToolCall, "server": "other", "tool": "search"},
		},
	})
	emitNativePermissionToolEvent(t, session, turnCtx, codex.Event{
		Kind: codex.EventToolStarted,
		Tool: codex.ToolEvent{
			ID:    "exec-2f447194-7f1c-4769-8141-57421828893b",
			Title: "wagie execute",
			Kind:  toolKindMcpToolCall,
			Raw: map[string]any{
				"id": "exec-2f447194-7f1c-4769-8141-57421828893b", "type": toolKindMcpToolCall,
				"server": "wagie", "tool": "execute", "arguments": map[string]any{"code": "return api.request({})"},
			},
		},
	})

	result, err := agent.handleCodexServerRequest(turnCtx, codex.ServerRequest{
		ID:     json.RawMessage(`80`),
		Method: codex.RequestMCPElicitation,
		Params: json.RawMessage(`{"threadId":"` + session.codexThreadID + `","turnId":"native-permission-turn","serverName":"wagie","message":"Allow the wagie MCP server to run tool \"execute\"?","tool_params":{"code":"return api.request({})"},"_meta":{"codex_approval_kind":"mcp_tool_call","codex_request_type":"approval","tool_name":"execute","tool_title":"Execute"}}`),
	})
	require.NoError(t, err)
	require.Equal(t, "accept", asType[map[string]any](t, result)["action"])

	order, permissions := conn.snapshot()
	require.Len(t, permissions, 1)
	require.Equal(t, acp.ToolCallId("exec-2f447194-7f1c-4769-8141-57421828893b"), permissions[0].ToolCall.ToolCallId)
	require.Equal(t, acp.ToolCallStatusInProgress, *permissions[0].ToolCall.Status)
	require.Equal(t, []string{
		"start:exec-decoy:in_progress",
		"start:exec-2f447194-7f1c-4769-8141-57421828893b:in_progress",
		"permission:exec-2f447194-7f1c-4769-8141-57421828893b",
	}, order)
}

func TestMCPPermissionRejectsStaleNativeTurnBeforeLedgerMatch(t *testing.T) {
	agent, session, conn, turnCtx := newStrictPermissionSession(t)
	emitNativePermissionToolEvent(t, session, turnCtx, codex.Event{
		Kind: codex.EventToolStarted,
		Tool: codex.ToolEvent{
			ID: "exec-current", Kind: toolKindMcpToolCall,
			Raw: map[string]any{"server": "wagie", "tool": "execute"},
		},
	})

	response, err := agent.handleCodexServerRequest(turnCtx, codex.ServerRequest{
		ID:     json.RawMessage(`84`),
		Method: codex.RequestMCPElicitation,
		Params: json.RawMessage(`{"threadId":"` + session.codexThreadID + `","turnId":"stale-native-turn","serverName":"wagie","message":"Allow tool?","_meta":{"codex_approval_kind":"mcp_tool_call","codex_request_type":"approval","tool_name":"execute","tool_title":"Execute"}}`),
	})
	require.NoError(t, err)
	require.Equal(t, "cancel", asType[map[string]any](t, response)["action"])

	_, permissions := conn.snapshot()
	require.Empty(t, permissions)

	_, requested, err := session.requestPermissionForTool(turnCtx, conn, acp.RequestPermissionRequest{
		ToolCall: acp.ToolCallUpdate{
			ToolCallId: "exec-current",
			RawInput: map[string]any{
				"turnId":     "stale-native-turn",
				"serverName": "wagie",
				"_meta": map[string]any{
					"tool_name": "execute",
				},
			},
		},
	}, permissionToolMCP)
	require.NoError(t, err)
	require.False(t, requested)
	_, permissions = conn.snapshot()
	require.Empty(t, permissions)
}

func TestPermissionPublishesPendingBeforeCallbackAndDedupesNativeStart(t *testing.T) {
	agent, session, conn, turnCtx := newStrictPermissionSession(t)

	result, err := agent.handleCodexServerRequest(context.Background(), codex.ServerRequest{
		ID:     json.RawMessage(`"approval-before-start"`),
		Method: codex.RequestMCPElicitation,
		Params: json.RawMessage(`{"threadId":"` + session.codexThreadID + `","turnId":"native-permission-turn","serverName":"wagie","message":"Allow the wagie MCP server to run tool \"execute\"?","tool_params":{"code":"return 1"},"_meta":{"codex_approval_kind":"mcp_tool_call","codex_request_type":"approval","tool_name":"execute","tool_title":"Execute"}}`),
	})
	require.NoError(t, err)
	require.Equal(t, "accept", asType[map[string]any](t, result)["action"])

	nativeTool := codex.ToolEvent{
		ID:    "exec-arrived-later",
		Title: "wagie execute",
		Kind:  toolKindMcpToolCall,
		Raw:   map[string]any{"id": "exec-arrived-later", "type": toolKindMcpToolCall, "server": "wagie", "tool": "execute"},
	}
	emitNativePermissionToolEvent(t, session, turnCtx, codex.Event{Kind: codex.EventToolStarted, Tool: nativeTool})
	emitNativePermissionToolEvent(t, session, turnCtx, codex.Event{Kind: codex.EventToolCompleted, Tool: nativeTool})

	order, permissions := conn.snapshot()
	require.Len(t, permissions, 1)
	require.Equal(t, acp.ToolCallId("approval-before-start"), permissions[0].ToolCall.ToolCallId)
	require.Equal(t, acp.ToolCallStatusPending, *permissions[0].ToolCall.Status)
	require.Equal(t, []string{
		"start:approval-before-start:pending",
		"permission:approval-before-start",
		"update:approval-before-start:in_progress",
		"update:approval-before-start:completed",
	}, order)
	require.Equal(t, []map[string]any{nil}, conn.callbackRoutes())
}

func TestPermissionBeforeNativeStartRoutesSyntheticPendingForEveryClass(t *testing.T) {
	for _, test := range []struct {
		name   string
		method string
		id     string
		params string
	}{
		{
			name: "command", method: codex.RequestCommandApproval, id: "command-before-start",
			params: `,"approvalId":"command-before-start","command":"printf live"`,
		},
		{
			name: "file", method: codex.RequestFileChangeApproval, id: "file-before-start",
			params: `,"approvalId":"file-before-start","grantRoot":"/repo"`,
		},
		{
			name: "permissions", method: codex.RequestPermissionsApproval, id: "permissions-before-start",
			params: `,"itemId":"permissions-before-start","permissions":{"filesystem":{"write":true}}`,
		},
		{
			name: "mcp", method: codex.RequestMCPElicitation, id: "mcp-before-start",
			params: `,"serverName":"wagie","message":"Allow tool?","_meta":{"codex_approval_kind":"mcp_tool_call","codex_request_type":"approval","tool_name":"execute","tool_title":"Execute"}`,
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			agent, session, conn, _ := newStrictPermissionSession(t)

			response, err := agent.handleCodexServerRequest(context.Background(), codex.ServerRequest{
				ID:     json.RawMessage(`"` + test.id + `"`),
				Method: test.method,
				Params: json.RawMessage(`{"threadId":"` + session.codexThreadID + `","turnId":"native-permission-turn"` + test.params + `}`),
			})
			require.NoError(t, err)
			require.NotNil(t, response)

			order, permissions := conn.snapshot()
			require.Equal(t, []string{
				"start:" + test.id + ":pending",
				"permission:" + test.id,
			}, order)
			require.Len(t, permissions, 1)
			require.Equal(t, acp.ToolCallId(test.id), permissions[0].ToolCall.ToolCallId)
			require.Equal(t, acp.ToolCallStatusPending, *permissions[0].ToolCall.Status)
			require.Equal(t, []map[string]any{nil}, conn.callbackRoutes())
		})
	}
}

func TestCommandFileAndProfilePermissionsUsePublishedNonterminalIDs(t *testing.T) {
	tests := []struct {
		name       string
		method     string
		classKind  string
		published  string
		requestID  string
		params     string
		wantOrder  []string
		wantToolID acp.ToolCallId
	}{
		{
			name: "command correlates approval ID to item ID", method: codex.RequestCommandApproval,
			classKind: toolKindCommandExecution, published: "command-item", requestID: "command-approval",
			params:    `,"approvalId":"command-approval","command":"git status"`,
			wantOrder: []string{"start:command-item:in_progress", "permission:command-item"}, wantToolID: "command-item",
		},
		{
			name: "file change correlates approval ID to item ID", method: codex.RequestFileChangeApproval,
			classKind: toolKindFileChange, published: "file-item", requestID: "file-approval",
			params:    `,"approvalId":"file-approval","grantRoot":"/repo"`,
			wantOrder: []string{"start:file-item:in_progress", "permission:file-item"}, wantToolID: "file-item",
		},
		{
			name: "permission profile is published before callback", method: codex.RequestPermissionsApproval,
			requestID: "profile-request", params: `,"itemId":"profile-request","permissions":{"filesystem":{"write":true}}`,
			wantOrder: []string{"start:profile-request:pending", "permission:profile-request"}, wantToolID: "profile-request",
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			agent, session, conn, turnCtx := newStrictPermissionSession(t)
			if test.published != "" {
				emitNativePermissionToolEvent(t, session, turnCtx, codex.Event{
					Kind: codex.EventToolStarted,
					Tool: codex.ToolEvent{ID: test.published, Title: test.published, Kind: test.classKind, Raw: map[string]any{"id": test.published, "type": test.classKind}},
				})
			}

			_, err := agent.handleCodexServerRequest(turnCtx, codex.ServerRequest{
				ID:     json.RawMessage(`"` + test.requestID + `"`),
				Method: test.method,
				Params: json.RawMessage(`{"threadId":"` + session.codexThreadID + `","turnId":"native-permission-turn"` + test.params + `}`),
			})
			require.NoError(t, err)

			order, permissions := conn.snapshot()
			require.Equal(t, test.wantOrder, order)
			require.Len(t, permissions, 1)
			require.Equal(t, test.wantToolID, permissions[0].ToolCall.ToolCallId)
		})
	}
}

func TestPermissionCorrelationFailureAndHelperBranches(t *testing.T) {
	agent, session, conn, turnCtx := newStrictPermissionSession(t)
	liveMCPParams := map[string]any{"turnId": "native-permission-turn"}

	inactive := newSession(agent, "inactive", "/tmp/project", nil, codex.Thread{ID: "inactive-thread"}, newSpyCodexClient(), sessionMeta{}, nil)
	_, requested, err := inactive.requestPermissionForTool(context.Background(), conn, acp.RequestPermissionRequest{}, permissionToolMCP)
	require.NoError(t, err)
	require.False(t, requested)

	_, requested, err = session.requestPermissionForTool(turnCtx, conn, acp.RequestPermissionRequest{
		ToolCall: acp.ToolCallUpdate{RawInput: "malformed"},
	}, permissionToolMCP)
	require.NoError(t, err)
	require.False(t, requested)

	canceled, cancel := context.WithCancel(turnCtx)
	cancel()
	_, requested, err = session.requestPermissionForTool(canceled, conn, acp.RequestPermissionRequest{
		ToolCall: acp.ToolCallUpdate{RawInput: liveMCPParams},
	}, permissionToolMCP)
	require.NoError(t, err)
	require.False(t, requested)

	session.permissionTools.reset()
	_, requested, err = session.requestPermissionForTool(turnCtx, conn, acp.RequestPermissionRequest{
		ToolCall: acp.ToolCallUpdate{RawInput: liveMCPParams},
	}, permissionToolMCP)
	require.NoError(t, err)
	require.False(t, requested)

	session.permissionTools.reset()
	session.permissionTools.tools = map[acp.ToolCallId]*permissionToolRecord{
		"finished-record": {id: "finished-record", class: permissionToolMCP, terminal: true},
	}
	session.permissionTools.aliases = map[string]acp.ToolCallId{"approval": "finished-record"}
	_, requested, err = session.requestPermissionForTool(turnCtx, conn, acp.RequestPermissionRequest{
		ToolCall: acp.ToolCallUpdate{ToolCallId: "approval", RawInput: liveMCPParams},
	}, permissionToolMCP)
	require.NoError(t, err)
	require.False(t, requested)

	session.permissionTools.reset()
	session.permissionTools.aliases = map[string]acp.ToolCallId{"approval": "missing"}
	_, requested, err = session.requestPermissionForTool(turnCtx, conn, acp.RequestPermissionRequest{
		ToolCall: acp.ToolCallUpdate{ToolCallId: "approval", RawInput: liveMCPParams},
	}, permissionToolMCP)
	require.NoError(t, err)
	require.False(t, requested)

	session.permissionTools.reset()
	session.permissionTools.tools = map[acp.ToolCallId]*permissionToolRecord{
		"one": {id: "one", class: permissionToolMCP},
		"two": {id: "two", class: permissionToolMCP},
	}
	_, requested, err = session.requestPermissionForTool(turnCtx, conn, acp.RequestPermissionRequest{
		ToolCall: acp.ToolCallUpdate{ToolCallId: "unmatched", RawInput: liveMCPParams},
	}, permissionToolMCP)
	require.NoError(t, err)
	require.False(t, requested)

	emitFailure := &errorAgentClient{
		recordingAgentClient: newRecordingAgentClient(),
		updateErr:            errors.New("pending start failed"),
	}
	agent.setAgentClient(emitFailure)
	session.permissionTools.reset()
	_, requested, err = session.requestPermissionForTool(turnCtx, emitFailure, acp.RequestPermissionRequest{
		ToolCall: acp.ToolCallUpdate{ToolCallId: "awaiting-start", RawInput: liveMCPParams},
	}, permissionToolMCP)
	require.ErrorContains(t, err, "pending start failed")
	require.False(t, requested)

	_, err = session.preparePermissionToolEvent(t.Context(), codex.Event{
		Kind: codex.EventToolStarted,
		Tool: codex.ToolEvent{ID: "failed-publication", Kind: toolKindMcpToolCall},
	})
	require.NoError(t, err)

	_, err = session.preparePermissionToolEvent(t.Context(), codex.Event{
		Kind: codex.EventToolCompleted,
		Tool: codex.ToolEvent{ID: "completed-without-start", Kind: toolKindMcpToolCall},
	})
	require.NoError(t, err)
	require.True(t, session.permissionTools.tools["completed-without-start"].terminal)

	score, compatible := permissionFingerprintScore(
		permissionToolFingerprint{title: "same"},
		permissionToolFingerprint{title: "same"},
	)
	require.True(t, compatible)
	require.Equal(t, 1, score)
	_, compatible = permissionFingerprintScore(
		permissionToolFingerprint{server: "left"},
		permissionToolFingerprint{server: "right"},
	)
	require.False(t, compatible)

	merged := mergePermissionFingerprint(permissionToolFingerprint{}, permissionToolFingerprint{
		title: "title", server: "server", tool: "tool",
	})
	require.Equal(t, permissionToolFingerprint{title: "title", server: "server", tool: "tool"}, merged)
}

func TestPermissionServerRequestsFailClosedOutsideTurn(t *testing.T) {
	ctx := context.Background()
	client := newSpyCodexClient()
	agent := NewAgent(withClientFactory(func(context.Context, codex.Options) (codex.Client, error) { return client, nil }))
	conn := newStrictPermissionClient("outside-turn")
	agent.setAgentClient(conn)
	created, err := agent.NewSession(ctx, NewSessionRequest(t.TempDir()))
	require.NoError(t, err)
	session := agent.sessionMust(created.SessionId)
	session.setTurnID("native-outside")
	enableClientElicitation(agent, true, true)

	permissions, err := agent.handleCodexServerRequest(ctx, codex.ServerRequest{
		ID:     json.RawMessage(`"permissions-outside-turn"`),
		Method: codex.RequestPermissionsApproval,
		Params: json.RawMessage(`{"threadId":"` + session.codexThreadID + `","turnId":"native-outside","itemId":"profile","permissions":{"filesystem":{"write":true}}}`),
	})
	require.NoError(t, err)
	require.NotNil(t, permissions)

	mcp, err := agent.handleCodexServerRequest(ctx, codex.ServerRequest{
		ID:     json.RawMessage(`"mcp-outside-turn"`),
		Method: codex.RequestMCPElicitation,
		Params: json.RawMessage(`{"threadId":"` + session.codexThreadID + `","turnId":"native-outside","serverName":"wagie","message":"Allow tool?","_meta":{"codex_approval_kind":"mcp_tool_call","codex_request_type":"approval","tool_name":"execute","tool_title":"execute"}}`),
	})
	require.NoError(t, err)
	require.Equal(t, "cancel", asType[map[string]any](t, mcp)["action"])

	userElicitation, err := agent.handleCodexServerRequest(ctx, codex.ServerRequest{
		ID:     json.RawMessage(`71`),
		Method: codex.RequestMCPElicitation,
		Params: json.RawMessage(`{"threadId":"` + session.codexThreadID + `","turnId":"native-outside","serverName":"wagie","mode":"form","message":"Need input"}`),
	})
	require.NoError(t, err)
	require.Equal(t, "cancel", asType[map[string]any](t, userElicitation)["action"])
	require.Empty(t, conn.scopes)

	outsideRequest, err := codex.MCPElicitationRequest(map[string]any{"mode": "form"}, nil)
	require.NoError(t, err)

	_, associated, err := session.createElicitationForMCPTool(
		ctx,
		conn,
		outsideRequest,
		"exec-outside-turn",
		map[string]any{"turnId": "native-outside", "serverName": "wagie"},
	)
	require.NoError(t, err)
	require.False(t, associated)
}

func TestMCPUserElicitationCanonicalizesPublishedToolIdentity(t *testing.T) {
	agent, session, _, turnCtx := newStrictPermissionSession(t)
	agent.setAgentClient(newRecordingAgentClient())
	incarnation, err := session.openIncarnation(turnCtx, lifecycle.Negotiated{Version: 1})
	require.NoError(t, err)
	require.NoError(t, incarnation.accept(turnCtx, lifecycle.Submission{SubmissionID: "submission", ClientNonce: "nonce"}))
	emitNativePermissionToolEvent(t, session, turnCtx, codex.Event{
		Kind: codex.EventToolStarted,
		Tool: codex.ToolEvent{
			ID:    "exec-74d34bc0-canonical",
			Title: "wagie execute",
			Kind:  toolKindMcpToolCall,
			Raw: map[string]any{
				"id": "exec-74d34bc0-canonical", "server": "wagie", "tool": "execute",
			},
		},
	})

	conn := &blockingElicitationAgentClient{
		recordingAgentClient: newRecordingAgentClient(),
		entered:              make(chan elicitationScope, 1),
		release:              make(chan struct{}),
	}
	agent.setAgentClient(conn)
	enableClientElicitation(agent, true, true)

	result := make(chan struct {
		response any
		err      error
	}, 1)
	go func() {
		response, err := agent.handleCodexServerRequest(turnCtx, codex.ServerRequest{
			ID:     json.RawMessage(`"mcp-user-elicitation"`),
			Method: codex.RequestMCPElicitation,
			Params: json.RawMessage(`{"threadId":"` + session.codexThreadID + `","turnId":"native-permission-turn","itemId":"ctc-native-mismatch","mode":"form","message":"Need input","requestedSchema":{"type":"object"},"_meta":{"serverName":"wagie","toolName":"execute"}}`),
		})
		result <- struct {
			response any
			err      error
		}{response: response, err: err}
	}()

	scope := <-conn.entered
	require.Equal(t, acp.ToolCallId("exec-74d34bc0-canonical"), scope.ToolCallID)
	require.Nil(t, scope.RequestID)

	completion := make(chan error, 1)
	go func() {
		state := &promptEventState{snapshot: session.snapshot()}
		event := codex.Event{
			Kind: codex.EventToolCompleted,
			Tool: codex.ToolEvent{
				ID:   "exec-74d34bc0-canonical",
				Kind: toolKindMcpToolCall,
				Raw:  map[string]any{"server": "wagie", "tool": "execute"},
			},
		}
		completion <- session.emitPromptUpdates(turnCtx, event, event, state)
	}()

	require.NoError(t, <-completion)
	close(conn.release)
	completed := <-result
	require.NoError(t, completed.err)
	require.Equal(t, "accept", asType[map[string]any](t, completed.response)["action"])
	require.True(t, session.permissionTools.tools["exec-74d34bc0-canonical"].terminal)
}

func TestPermissionAndElicitationReleaseToolLeaseBeforeHostResponseAtOneClientCall(t *testing.T) {
	for _, kind := range []string{"permission", "elicitation"} {
		t.Run(kind, func(t *testing.T) {
			inputR, inputW := io.Pipe()
			defer inputR.Close()
			defer inputW.Close()
			wire := &gatedHostActionWriter{
				requestStarted: make(chan json.RawMessage, 1), releaseRequest: make(chan struct{}),
			}
			agent := NewAgent(WithConcurrencyLimits(ConcurrencyLimits{MaxConcurrentClientCalls: 1}))
			agent.lifecycle = lifecycle.Negotiated{Version: 1}
			conn := newLocalAgentConnection(agent, wire, inputR)
			agent.setAgentClient(conn)
			client := newSpyCodexClient()
			s := newSession(agent, "session", t.TempDir(), nil, codex.Thread{ID: "thread"}, client, sessionMeta{}, nil)
			turnCtx := s.beginTurn(t.Context(), "lease-turn")
			s.setTurnID("native-turn")
			defer s.finishTurn()
			defer s.fenceSession()
			in, err := s.openIncarnation(turnCtx, agent.lifecycle)
			require.NoError(t, err)
			require.NoError(t, in.accept(turnCtx, lifecycle.Submission{SubmissionID: "submission", ClientNonce: "nonce"}))
			start := codex.Event{
				Kind: codex.EventToolStarted,
				Tool: codex.ToolEvent{ID: "tool", Kind: toolKindMcpToolCall, Raw: map[string]any{"server": "wagie", "tool": "execute"}},
			}
			emitNativePermissionToolEvent(t, s, turnCtx, start)

			requestDone := make(chan error, 1)
			go func() {
				actionCtx := withLifecycleActionTurn(turnCtx, in)
				if kind == "permission" {
					_, _, permissionErr := s.requestPermissionForTool(actionCtx, conn, acp.RequestPermissionRequest{
						ToolCall: acp.ToolCallUpdate{ToolCallId: "tool", RawInput: map[string]any{
							"turnId": "native-turn", "serverName": "wagie", "toolName": "execute",
						}},
					}, permissionToolMCP)
					requestDone <- permissionErr

					return
				}

				request, requestErr := codex.MCPElicitationRequest(map[string]any{
					"mode": "form", "message": "Need input", "requestedSchema": map[string]any{"type": "object"},
				}, nil)
				if requestErr != nil {
					requestDone <- requestErr

					return
				}
				_, _, elicitationErr := s.createElicitationForMCPTool(
					actionCtx, conn, request, "tool",
					map[string]any{"turnId": "native-turn", "serverName": "wagie", "toolName": "execute"},
				)
				requestDone <- elicitationErr
			}()

			requestID := <-wire.requestStarted
			s.permissionTools.mu.Lock()
			leaseDone := s.permissionTools.tools["tool"].leaseDone
			s.permissionTools.mu.Unlock()
			require.NotNil(t, leaseDone)
			close(wire.releaseRequest)
			<-leaseDone

			if release, acquireErr := agent.acquireClientCall(t.Context()); acquireErr != nil {
				t.Fatal("host response wait retained the single client-call permit")
			} else {
				release()
			}

			completion := make(chan error, 1)
			go func() {
				state := &promptEventState{snapshot: s.snapshot(), toolContents: make(map[acp.ToolCallId][]acp.ToolCallContent)}
				event := codex.Event{
					Kind: codex.EventToolCompleted,
					Tool: codex.ToolEvent{ID: "tool", Kind: toolKindMcpToolCall, Raw: map[string]any{"server": "wagie", "tool": "execute"}},
				}
				completion <- s.emitPromptUpdates(turnCtx, event, event, state)
			}()
			require.NoError(t, <-completion)
			s.permissionTools.mu.Lock()
			require.True(t, s.permissionTools.tools["tool"].terminal)
			s.permissionTools.mu.Unlock()

			_, err = fmt.Fprintf(inputW, `{"jsonrpc":"2.0","id":%s,"error":{"code":-32000,"message":"declined"}}`+"\n", requestID)
			require.NoError(t, err)
			require.Error(t, <-requestDone)
			_ = conn.InterruptTransport()
		})
	}
}

func TestPermissionHostWaitDoesNotBlockNativeToolDelta(t *testing.T) {
	agent, session, _, turnCtx := newStrictPermissionSession(t)
	agent.setAgentClient(newRecordingAgentClient())
	incarnation, err := session.openIncarnation(turnCtx, lifecycle.Negotiated{Version: 1})
	require.NoError(t, err)
	require.NoError(t, incarnation.accept(turnCtx, lifecycle.Submission{SubmissionID: "submission", ClientNonce: "nonce"}))
	emitNativePermissionToolEvent(t, session, turnCtx, codex.Event{
		Kind: codex.EventToolStarted,
		Tool: codex.ToolEvent{
			ID: lifecycleTestToolID, Kind: toolKindMcpToolCall,
			Raw: map[string]any{"server": "wagie", "tool": "execute"},
		},
	})

	conn := &blockingPermissionAgentClient{
		recordingAgentClient: newRecordingAgentClient(),
		started:              make(chan struct{}),
		release:              make(chan struct{}),
	}
	agent.setAgentClient(conn)
	done := make(chan error, 1)
	go func() {
		_, _, err := session.requestPermissionForTool(withLifecycleActionTurn(turnCtx, incarnation), conn, acp.RequestPermissionRequest{
			ToolCall: acp.ToolCallUpdate{
				ToolCallId: lifecycleTestToolID,
				RawInput: map[string]any{
					"turnId": "native-permission-turn", "serverName": "wagie", "toolName": "execute",
				},
			},
		}, permissionToolMCP)
		done <- err
	}()
	<-conn.started

	state := &promptEventState{snapshot: session.snapshot(), toolContents: make(map[acp.ToolCallId][]acp.ToolCallContent)}
	delta := codex.Event{
		Kind: codex.EventToolDelta,
		Tool: codex.ToolEvent{
			ID: lifecycleTestToolID, Kind: toolKindMcpToolCall, Content: "progress",
			Raw: map[string]any{"server": "wagie", "tool": "execute"},
		},
	}
	require.NoError(t, session.emitPromptUpdates(turnCtx, delta, delta, state))
	close(conn.release)
	require.NoError(t, <-done)
}

func TestPermissionAndCompletionWaitForSuccessfulStartPublication(t *testing.T) {
	_, session, _, turnCtx := newStrictPermissionSession(t)
	conn := &orderedBarrierPermissionClient{
		strictPermissionClient: newStrictPermissionClient("permission-turn"),
		startEntered:           make(chan struct{}),
		releaseStart:           make(chan struct{}),
		permissionEntered:      make(chan struct{}),
		releasePermission:      make(chan struct{}),
	}
	session.agent.setAgentClient(conn)

	startEvent := codex.Event{
		Kind: codex.EventToolStarted,
		Tool: codex.ToolEvent{
			ID: "ordered-tool", Kind: toolKindCommandExecution,
			Raw: map[string]any{"id": "ordered-tool", "type": toolKindCommandExecution},
		},
	}
	startDone := make(chan error, 1)
	go func() {
		state := &promptEventState{snapshot: session.snapshot()}
		startDone <- session.emitPromptUpdates(turnCtx, startEvent, startEvent, state)
	}()
	<-conn.startEntered

	permissionDone := make(chan error, 1)
	go func() {
		_, _, err := session.requestPermissionForTool(turnCtx, conn, acp.RequestPermissionRequest{
			ToolCall: acp.ToolCallUpdate{
				ToolCallId: "ordered-tool",
				RawInput: map[string]any{
					"turnId": "native-permission-turn", "id": "ordered-tool", "type": toolKindCommandExecution,
				},
			},
		}, permissionToolCommand)
		permissionDone <- err
	}()

	order, _ := conn.snapshot()
	require.Empty(t, order)
	close(conn.releaseStart)
	require.NoError(t, <-startDone)
	<-conn.permissionEntered
	order, _ = conn.snapshot()
	require.Equal(t, []string{"start:ordered-tool:in_progress", "permission:ordered-tool"}, order)

	completionEvent := codex.Event{
		Kind: codex.EventToolCompleted,
		Tool: codex.ToolEvent{
			ID: "ordered-tool", Kind: toolKindCommandExecution,
			Raw: map[string]any{"id": "ordered-tool", "type": toolKindCommandExecution},
		},
	}
	completionDone := make(chan error, 1)
	go func() {
		state := &promptEventState{snapshot: session.snapshot()}
		completionDone <- session.emitPromptUpdates(turnCtx, completionEvent, completionEvent, state)
	}()
	require.NoError(t, <-completionDone, "native completion waited for the held host permission response")

	close(conn.releasePermission)
	require.NoError(t, <-permissionDone)
	order, _ = conn.snapshot()
	require.Equal(t, []string{
		"start:ordered-tool:in_progress",
		"permission:ordered-tool",
		"update:ordered-tool:completed",
	}, order)
}

func TestPermissionToolRegistryIsBoundedAndFailsClosed(t *testing.T) {
	t.Run("tools", func(t *testing.T) {
		session := &session{}
		for index := 0; index < permissionToolLimit; index++ {
			_, err := session.preparePermissionToolEvent(t.Context(), codex.Event{
				Kind: codex.EventToolCompleted,
				Tool: codex.ToolEvent{ID: fmt.Sprintf("tool-%d", index), Kind: toolKindMcpToolCall},
			})
			require.NoError(t, err)
		}
		_, err := session.preparePermissionToolEvent(t.Context(), codex.Event{
			Kind: codex.EventToolCompleted,
			Tool: codex.ToolEvent{ID: "overflow", Kind: toolKindMcpToolCall},
		})
		require.ErrorIs(t, err, codex.ErrTurnEventOverflow)
		require.Len(t, session.permissionTools.tools, permissionToolLimit)
		require.ErrorIs(t, session.permissionTools.failure, codex.ErrTurnEventOverflow)
	})

	t.Run("aliases", func(t *testing.T) {
		registry := &permissionToolRegistry{}
		registry.mu.Lock()
		require.NoError(t, registry.ensure())
		for index := 0; index < permissionAliasLimit; index++ {
			require.NoError(t, registry.addAlias(fmt.Sprintf("alias-%d", index), "tool"))
		}
		err := registry.addAlias("overflow", "tool")
		registry.mu.Unlock()
		require.ErrorIs(t, err, codex.ErrTurnEventOverflow)
		require.Len(t, registry.aliases, permissionAliasLimit)
	})
}

func TestAutonomousTurnAdmissionRotatesPermissionRegistry(t *testing.T) {
	agent := NewAgent()
	agent.setAgentClient(newRecordingAgentClient())
	session := &session{agent: agent, id: "session", codexThreadID: "thread"}
	negotiated := lifecycle.Negotiated{Version: 1, ActivityKinds: []lifecycle.ActivityKind{}}
	require.NoError(t, session.openLifecycleStream(t.Context(), negotiated))

	session.permissionTools.mu.Lock()
	session.permissionTools.tools = make(map[acp.ToolCallId]*permissionToolRecord, permissionToolLimit)
	for index := range permissionToolLimit {
		id := acp.ToolCallId(fmt.Sprintf("stale-%d", index))
		session.permissionTools.tools[id] = &permissionToolRecord{id: id, terminal: true}
	}
	session.permissionTools.aliases = map[string]acp.ToolCallId{"reused": "stale-0"}
	session.permissionTools.failure = codex.ErrTurnEventOverflow
	session.permissionTools.mu.Unlock()

	session.lifecycleMu.Lock()
	incarnation, err := session.openAutonomousTurnLocked(t.Context(), "native-autonomous")
	session.lifecycleMu.Unlock()
	require.NoError(t, err)
	require.NotNil(t, incarnation)

	publication, err := session.preparePermissionToolEvent(t.Context(), codex.Event{
		Kind: codex.EventToolStarted,
		Tool: codex.ToolEvent{ID: "reused", Kind: toolKindMcpToolCall},
	})
	require.NoError(t, err)
	publication.finish(session, nil)
	session.permissionTools.mu.Lock()
	require.NoError(t, session.permissionTools.failure)
	require.Len(t, session.permissionTools.tools, 1)
	require.Same(t, publication.record, session.permissionTools.tools["reused"])
	session.permissionTools.mu.Unlock()
}

func TestPermissionToolRegistryCoordinatesStartAndLeaseLifetimes(t *testing.T) {
	registry := &permissionToolRegistry{}
	require.NoError(t, registry.ensure())
	require.NoError(t, registry.ensure())
	require.NoError(t, registry.addAlias("", "ignored"))
	require.Error(t, registry.addTool(nil))
	require.Error(t, registry.addTool(&permissionToolRecord{}))

	record := newPermissionToolRecord("tool", permissionToolCommand, permissionToolFingerprint{})
	require.NoError(t, registry.addTool(record))
	require.NoError(t, registry.addAlias("native", record.id))
	registry.acquire(record)
	registry.acquire(record)

	waiting := make(chan error, 1)
	go func() { waiting <- registry.waitForLeases(t.Context(), record) }()
	registry.release(nil)
	registry.release(&permissionToolRecord{})
	registry.release(record)
	select {
	case err := <-waiting:
		require.Fail(t, "lease wait returned before the final release", "error: %v", err)
	default:
	}
	registry.release(record)
	require.NoError(t, <-waiting)
	require.NoError(t, registry.waitForLeases(t.Context(), nil))
	require.NoError(t, registry.waitForLeases(t.Context(), record))

	registry.acquire(record)
	cancelled, cancel := context.WithCancel(t.Context())
	cancel()
	require.ErrorIs(t, registry.waitForLeases(cancelled, record), context.Canceled)
	registry.release(record)

	startWaiting := make(chan error, 1)
	go func() { startWaiting <- registry.waitForStart(t.Context(), record) }()
	registry.completeStart(record, nil)
	require.NoError(t, <-startWaiting)
	require.NoError(t, registry.waitForStart(t.Context(), record))
	registry.completeStart(record, errors.New("ignored after settlement"))
	registry.completeStart(nil, nil)

	require.NoError(t, registry.waitForStart(t.Context(), nil))
	withoutDone := &permissionToolRecord{startErr: errors.New("settled")}
	require.ErrorContains(t, registry.waitForStart(t.Context(), withoutDone), "settled")

	cancelRecord := newPermissionToolRecord("cancel", permissionToolCommand, permissionToolFingerprint{})
	cancelStart, cancelStartWait := context.WithCancel(t.Context())
	cancelStartWait()
	require.ErrorIs(t, registry.waitForStart(cancelStart, cancelRecord), context.Canceled)

	failing := newPermissionToolRecord("failing", permissionToolCommand, permissionToolFingerprint{})
	require.NoError(t, registry.addTool(failing))
	require.NoError(t, registry.addAlias("failing-native", failing.id))
	startErr := errors.New("start failed")
	registry.completeStart(failing, startErr)
	require.ErrorIs(t, registry.waitForStart(t.Context(), failing), startErr)
	require.NotContains(t, registry.tools, failing.id)
	require.NotContains(t, registry.aliases, "failing-native")

	open := newPermissionToolRecord("open", permissionToolCommand, permissionToolFingerprint{})
	registry.acquire(open)
	registry.tools[open.id] = open
	registry.tools["nil"] = nil
	registry.reset()
	require.ErrorIs(t, registry.waitForStart(t.Context(), open), codex.ErrConnectionClosed)
	require.NoError(t, registry.waitForLeases(t.Context(), open))
	require.Nil(t, registry.tools)
	require.Nil(t, registry.aliases)
	require.NoError(t, registry.failure)
}

func TestPermissionToolRegistryKeepsExistingEntriesAtItsBounds(t *testing.T) {
	registry := &permissionToolRegistry{
		tools:   make(map[acp.ToolCallId]*permissionToolRecord, permissionToolLimit),
		aliases: make(map[string]acp.ToolCallId, permissionAliasLimit),
	}
	existing := newPermissionToolRecord("existing", permissionToolCommand, permissionToolFingerprint{})
	registry.tools[existing.id] = existing
	for index := 1; index < permissionToolLimit; index++ {
		id := acp.ToolCallId(fmt.Sprintf("tool-%d", index))
		registry.tools[id] = newPermissionToolRecord(id, permissionToolCommand, permissionToolFingerprint{})
	}
	for index := range permissionAliasLimit {
		registry.aliases[fmt.Sprintf("alias-%d", index)] = existing.id
	}
	require.NoError(t, registry.addTool(existing))
	require.NoError(t, registry.addAlias("alias-0", existing.id))
	require.ErrorIs(t, registry.addTool(newPermissionToolRecord("overflow", permissionToolCommand, permissionToolFingerprint{})), codex.ErrTurnEventOverflow)

	failed := &permissionToolRegistry{failure: errors.New("fenced")}
	require.ErrorContains(t, failed.ensure(), "fenced")
	require.ErrorContains(t, failed.fail(errors.New("later")), "later")
	require.ErrorContains(t, failed.failure, "fenced")
	require.NoError(t, failed.fail(nil))
}

func TestPermissionToolRegistryWaitsForAlreadySignaledTransitions(t *testing.T) {
	registry := &permissionToolRegistry{}
	leaseDone := make(chan struct{})
	close(leaseDone)
	require.NoError(t, registry.waitForLeases(t.Context(), &permissionToolRecord{
		leases: 1, leaseDone: leaseDone,
	}))

	startErr := errors.New("start publication failed")
	startDone := make(chan struct{})
	close(startDone)
	require.ErrorIs(t, registry.waitForStart(t.Context(), &permissionToolRecord{
		startDone: startDone, startErr: startErr,
	}), startErr)
}

func TestPermissionToolRequestsFailClosedAtRegistryAndLifecycleBoundaries(t *testing.T) {
	request := func(id acp.ToolCallId) acp.RequestPermissionRequest {
		return acp.RequestPermissionRequest{ToolCall: acp.ToolCallUpdate{
			ToolCallId: id,
			RawInput:   map[string]any{"turnId": "native-permission-turn"},
		}}
	}

	t.Run("cancelled request", func(t *testing.T) {
		_, session, conn, turnCtx := newStrictPermissionSession(t)
		ctx, cancel := context.WithCancel(turnCtx)
		cancel()
		_, requested, err := session.requestPermissionForTool(ctx, conn, request("cancelled"), permissionToolCommand)
		require.NoError(t, err)
		require.False(t, requested)
	})

	t.Run("request registry failure", func(t *testing.T) {
		_, session, conn, turnCtx := newStrictPermissionSession(t)
		failure := errors.New("registry failed")
		session.permissionTools.failure = failure
		_, requested, err := session.requestPermissionForTool(turnCtx, conn, request("failed"), permissionToolCommand)
		require.True(t, requested)
		require.ErrorIs(t, err, failure)
	})

	t.Run("request tool bound", func(t *testing.T) {
		_, session, conn, turnCtx := newStrictPermissionSession(t)
		session.permissionTools.tools = make(map[acp.ToolCallId]*permissionToolRecord, permissionToolLimit)
		for index := range permissionToolLimit {
			id := acp.ToolCallId(fmt.Sprintf("terminal-%d", index))
			session.permissionTools.tools[id] = &permissionToolRecord{id: id, terminal: true}
		}
		session.permissionTools.aliases = make(map[string]acp.ToolCallId)
		_, requested, err := session.requestPermissionForTool(turnCtx, conn, request("overflow"), permissionToolCommand)
		require.True(t, requested)
		require.ErrorIs(t, err, codex.ErrTurnEventOverflow)
	})

	t.Run("request alias bound", func(t *testing.T) {
		_, session, conn, turnCtx := newStrictPermissionSession(t)
		session.permissionTools.tools = make(map[acp.ToolCallId]*permissionToolRecord)
		session.permissionTools.aliases = make(map[string]acp.ToolCallId, permissionAliasLimit)
		for index := range permissionAliasLimit {
			session.permissionTools.aliases[fmt.Sprintf("alias-%d", index)] = "terminal"
		}
		_, requested, err := session.requestPermissionForTool(turnCtx, conn, request("overflow"), permissionToolCommand)
		require.True(t, requested)
		require.ErrorIs(t, err, codex.ErrTurnEventOverflow)
	})

	t.Run("request pending publication failure", func(t *testing.T) {
		agent, session, _, turnCtx := newStrictPermissionSession(t)
		failure := errors.New("pending publication failed")
		conn := &errorAgentClient{recordingAgentClient: newRecordingAgentClient(), updateErr: failure}
		agent.setAgentClient(conn)
		_, requested, err := session.requestPermissionForTool(turnCtx, conn, request("pending"), permissionToolCommand)
		require.False(t, requested)
		require.ErrorIs(t, err, failure)
	})

	t.Run("request existing start failure", func(t *testing.T) {
		_, session, conn, turnCtx := newStrictPermissionSession(t)
		failure := errors.New("existing start failed")
		done := make(chan struct{})
		close(done)
		record := &permissionToolRecord{
			id: "existing", class: permissionToolCommand, startDone: done, startErr: failure,
		}
		session.permissionTools.tools = map[acp.ToolCallId]*permissionToolRecord{record.id: record}
		session.permissionTools.aliases = map[string]acp.ToolCallId{"existing": record.id}
		_, requested, err := session.requestPermissionForTool(turnCtx, conn, request("existing"), permissionToolCommand)
		require.True(t, requested)
		require.ErrorIs(t, err, failure)
	})

	t.Run("request lifecycle identity", func(t *testing.T) {
		agent, session, _, turnCtx := newStrictPermissionSession(t)
		conn := newRecordingAgentClient()
		agent.setAgentClient(conn)
		require.NoError(t, session.openLifecycleStream(t.Context(), lifecycle.Negotiated{Version: 1}))
		_, requested, err := session.requestPermissionForTool(turnCtx, conn, request("lifecycle"), permissionToolCommand)
		require.False(t, requested)
		require.ErrorContains(t, err, "exact native turn")
	})

	elicitation := func(session *session, ctx context.Context, conn agentClient, nativeID string) (bool, error) {
		_, requested, err := session.createElicitationForMCPTool(ctx, conn, acp.UnstableCreateElicitationRequest{}, nativeID,
			map[string]any{"turnId": "native-permission-turn"})

		return requested, err
	}

	t.Run("cancelled elicitation", func(t *testing.T) {
		_, session, conn, turnCtx := newStrictPermissionSession(t)
		ctx, cancel := context.WithCancel(turnCtx)
		cancel()
		requested, err := elicitation(session, ctx, conn, "cancelled")
		require.NoError(t, err)
		require.False(t, requested)
	})

	t.Run("elicitation registry failure", func(t *testing.T) {
		_, session, conn, turnCtx := newStrictPermissionSession(t)
		failure := errors.New("registry failed")
		session.permissionTools.failure = failure
		requested, err := elicitation(session, turnCtx, conn, "failed")
		require.True(t, requested)
		require.ErrorIs(t, err, failure)
	})

	t.Run("elicitation start failure", func(t *testing.T) {
		_, session, conn, turnCtx := newStrictPermissionSession(t)
		failure := errors.New("start failed")
		done := make(chan struct{})
		close(done)
		record := &permissionToolRecord{id: "existing", class: permissionToolMCP, startDone: done, startErr: failure}
		session.permissionTools.tools = map[acp.ToolCallId]*permissionToolRecord{record.id: record}
		session.permissionTools.aliases = map[string]acp.ToolCallId{"native": record.id}
		requested, err := elicitation(session, turnCtx, conn, "native")
		require.True(t, requested)
		require.ErrorIs(t, err, failure)
	})

	t.Run("elicitation lifecycle identity", func(t *testing.T) {
		agent, session, _, turnCtx := newStrictPermissionSession(t)
		conn := newRecordingAgentClient()
		agent.setAgentClient(conn)
		record := newPermissionToolRecord("existing", permissionToolMCP, permissionToolFingerprint{})
		record.startSettled = true
		session.permissionTools.tools = map[acp.ToolCallId]*permissionToolRecord{record.id: record}
		session.permissionTools.aliases = map[string]acp.ToolCallId{"native": record.id}
		require.NoError(t, session.openLifecycleStream(t.Context(), lifecycle.Negotiated{Version: 1}))
		requested, err := elicitation(session, turnCtx, conn, "native")
		require.False(t, requested)
		require.ErrorContains(t, err, "exact native turn")
	})
}

func TestPermissionToolEventPreparationWaitsAndFailsAtEveryRegistryBoundary(t *testing.T) {
	t.Run("registry failure", func(t *testing.T) {
		failure := errors.New("registry failed")
		session := &session{permissionTools: permissionToolRegistry{failure: failure}}
		_, err := session.preparePermissionToolEvent(t.Context(), codex.Event{Kind: codex.EventToolStarted})
		require.ErrorIs(t, err, failure)
	})

	t.Run("pending start", func(t *testing.T) {
		session := &session{}
		record := newPermissionToolRecord("tool", permissionToolMCP, permissionToolFingerprint{})
		record.pendingNativeStart = true
		session.permissionTools.tools = map[acp.ToolCallId]*permissionToolRecord{record.id: record}
		session.permissionTools.aliases = map[string]acp.ToolCallId{"native": record.id}
		go func() {
			time.Sleep(time.Millisecond)
			session.permissionTools.completeStart(record, nil)
		}()
		publication, err := session.preparePermissionToolEvent(t.Context(), codex.Event{
			Kind: codex.EventToolDelta, Tool: codex.ToolEvent{ID: "native", Kind: toolKindMcpToolCall},
		})
		require.NoError(t, err)
		require.Same(t, record, publication.record)
	})

	t.Run("active lease", func(t *testing.T) {
		session := &session{}
		record := newPermissionToolRecord("tool", permissionToolMCP, permissionToolFingerprint{})
		record.startSettled = true
		session.permissionTools.tools = map[acp.ToolCallId]*permissionToolRecord{record.id: record}
		session.permissionTools.aliases = map[string]acp.ToolCallId{"native": record.id}
		session.permissionTools.acquire(record)
		go func() {
			time.Sleep(time.Millisecond)
			session.permissionTools.release(record)
		}()
		publication, err := session.preparePermissionToolEvent(t.Context(), codex.Event{
			Kind: codex.EventToolCompleted, Tool: codex.ToolEvent{ID: "native", Kind: toolKindMcpToolCall},
		})
		require.NoError(t, err)
		require.Same(t, record, publication.record)
	})

	t.Run("failed start", func(t *testing.T) {
		failure := errors.New("start failed")
		session := &session{}
		record := &permissionToolRecord{
			id: "tool", class: permissionToolMCP, startSettled: true, startErr: failure,
		}
		session.permissionTools.tools = map[acp.ToolCallId]*permissionToolRecord{record.id: record}
		session.permissionTools.aliases = map[string]acp.ToolCallId{"native": record.id}
		_, err := session.preparePermissionToolEvent(t.Context(), codex.Event{
			Kind: codex.EventToolStarted, Tool: codex.ToolEvent{ID: "native", Kind: toolKindMcpToolCall},
		})
		require.ErrorIs(t, err, failure)
	})

	t.Run("pending start cancellation", func(t *testing.T) {
		session := &session{}
		record := newPermissionToolRecord("tool", permissionToolMCP, permissionToolFingerprint{})
		session.permissionTools.tools = map[acp.ToolCallId]*permissionToolRecord{record.id: record}
		session.permissionTools.aliases = map[string]acp.ToolCallId{"native": record.id}
		ctx, cancel := context.WithCancel(t.Context())
		cancel()
		_, err := session.preparePermissionToolEvent(ctx, codex.Event{
			Kind: codex.EventToolDelta, Tool: codex.ToolEvent{ID: "native", Kind: toolKindMcpToolCall},
		})
		require.ErrorIs(t, err, context.Canceled)
	})

	t.Run("active lease cancellation", func(t *testing.T) {
		session := &session{}
		record := newPermissionToolRecord("tool", permissionToolMCP, permissionToolFingerprint{})
		record.startSettled = true
		session.permissionTools.tools = map[acp.ToolCallId]*permissionToolRecord{record.id: record}
		session.permissionTools.aliases = map[string]acp.ToolCallId{"native": record.id}
		session.permissionTools.acquire(record)
		defer session.permissionTools.release(record)
		ctx, cancel := context.WithCancel(t.Context())
		cancel()
		_, err := session.preparePermissionToolEvent(ctx, codex.Event{
			Kind: codex.EventToolCompleted, Tool: codex.ToolEvent{ID: "native", Kind: toolKindMcpToolCall},
		})
		require.ErrorIs(t, err, context.Canceled)
	})

	fullTools := func() map[acp.ToolCallId]*permissionToolRecord {
		tools := make(map[acp.ToolCallId]*permissionToolRecord, permissionToolLimit)
		for index := range permissionToolLimit {
			id := acp.ToolCallId(fmt.Sprintf("terminal-%d", index))
			tools[id] = &permissionToolRecord{id: id, terminal: true}
		}

		return tools
	}
	fullAliases := func() map[string]acp.ToolCallId {
		aliases := make(map[string]acp.ToolCallId, permissionAliasLimit)
		for index := range permissionAliasLimit {
			aliases[fmt.Sprintf("alias-%d", index)] = "terminal"
		}

		return aliases
	}

	for _, kind := range []codex.EventKind{codex.EventToolStarted, codex.EventToolDelta, codex.EventToolCompleted} {
		t.Run("tool bound "+string(kind), func(t *testing.T) {
			session := &session{permissionTools: permissionToolRegistry{
				tools: fullTools(), aliases: make(map[string]acp.ToolCallId),
			}}
			_, err := session.preparePermissionToolEvent(t.Context(), codex.Event{
				Kind: kind, Tool: codex.ToolEvent{ID: "overflow", Kind: toolKindMcpToolCall},
			})
			require.ErrorIs(t, err, codex.ErrTurnEventOverflow)
		})

		t.Run("original alias bound "+string(kind), func(t *testing.T) {
			session := &session{permissionTools: permissionToolRegistry{
				tools: make(map[acp.ToolCallId]*permissionToolRecord), aliases: fullAliases(),
			}}
			_, err := session.preparePermissionToolEvent(t.Context(), codex.Event{
				Kind: kind, Tool: codex.ToolEvent{ID: "overflow", Kind: toolKindMcpToolCall},
			})
			require.ErrorIs(t, err, codex.ErrTurnEventOverflow)
		})
	}

	for _, kind := range []codex.EventKind{codex.EventToolStarted, codex.EventToolCompleted} {
		t.Run("canonical alias bound "+string(kind), func(t *testing.T) {
			record := &permissionToolRecord{id: "canonical", class: permissionToolMCP, startSettled: true}
			aliases := fullAliases()
			delete(aliases, "alias-0")
			aliases["native"] = record.id
			session := &session{permissionTools: permissionToolRegistry{
				tools: map[acp.ToolCallId]*permissionToolRecord{record.id: record}, aliases: aliases,
			}}
			_, err := session.preparePermissionToolEvent(t.Context(), codex.Event{
				Kind: kind, Tool: codex.ToolEvent{ID: "native", Kind: toolKindMcpToolCall},
			})
			require.ErrorIs(t, err, codex.ErrTurnEventOverflow)
		})
	}
}

func TestPermissionToolPublicationPrependsSyntheticStart(t *testing.T) {
	publication := permissionToolEventPublication{
		prependStart: true,
		event: codex.Event{Kind: codex.EventToolDelta, Text: "progress", Tool: codex.ToolEvent{
			ID: "tool", Title: "Tool", Kind: toolKindMcpToolCall, Raw: map[string]any{"server": "wagie"},
		}},
	}
	updates := publication.updates(make(map[acp.ToolCallId][]acp.ToolCallContent))
	require.Len(t, updates, 2)
	require.NotNil(t, updates[0].ToolCall)
	require.Equal(t, acp.ToolCallId("tool"), updates[0].ToolCall.ToolCallId)
}

func TestMCPUserElicitationLiveShapeUsesStringRequestCorrelation(t *testing.T) {
	agent, session, _, turnCtx := newStrictPermissionSession(t)
	session.setTurnID("turn-live")

	for _, event := range []codex.Event{
		{
			Kind: codex.EventToolStarted,
			Tool: codex.ToolEvent{
				ID: "exec-decoy", Kind: toolKindMcpToolCall,
				Raw: map[string]any{"server": "other", "tool": "execute"},
			},
		},
		{
			Kind: codex.EventToolStarted,
			Tool: codex.ToolEvent{
				ID: "exec-8876236c-canonical", Kind: toolKindMcpToolCall,
				Raw: map[string]any{"server": "wagie", "tool": "execute"},
			},
		},
	} {
		emitNativePermissionToolEvent(t, session, turnCtx, event)
	}

	conn := newRecordingAgentClient()
	conn.elicitation = acp.UnstableCreateElicitationResponse{
		Accept: &acp.UnstableCreateElicitationAccept{Action: "accept"},
	}
	agent.setAgentClient(conn)
	enableClientElicitation(agent, true, true)

	response, err := agent.handleCodexServerRequest(turnCtx, codex.ServerRequest{
		ID:     json.RawMessage(`81`),
		Method: codex.RequestMCPElicitation,
		Params: json.RawMessage(`{"threadId":"` + session.codexThreadID + `","turnId":"turn-live","serverName":"wagie","mode":"form","message":"Need input","requestedSchema":{"type":"object","properties":{}}}`),
	})
	require.NoError(t, err)
	require.Equal(t, "accept", asType[map[string]any](t, response)["action"])
	require.Len(t, conn.scopes, 1)
	require.Empty(t, conn.scopes[0].ToolCallID)
	require.NotNil(t, conn.scopes[0].RequestID)
	require.NotNil(t, conn.scopes[0].RequestID.Str)
	require.Equal(t, acp.RequestIdStr("jsonrpc:number:81"), *conn.scopes[0].RequestID.Str)
	require.Nil(t, conn.scopes[0].RequestID.Number)

	wire, err := scopedElicitationParams(conn.elicitations[0], conn.scopes[0])
	require.NoError(t, err)
	var payload map[string]json.RawMessage
	require.NoError(t, json.Unmarshal(wire, &payload))
	var meta map[string]json.RawMessage
	require.NoError(t, json.Unmarshal(payload[jsonFieldMeta], &meta))
	var route map[string]json.RawMessage
	require.NoError(t, json.Unmarshal(meta[routeMetaKey], &route))
	require.Equal(t, `"jsonrpc:number:81"`, string(route[routeRequestIDKey]))
	require.NotContains(t, route, routeToolCallIDKey)
}

func TestMCPUserElicitationMintsMissingRequestCorrelation(t *testing.T) {
	agent, session, _, turnCtx := newStrictPermissionSession(t)
	conn := newRecordingAgentClient()
	conn.elicitation = acp.UnstableCreateElicitationResponse{
		Accept: &acp.UnstableCreateElicitationAccept{Action: "accept"},
	}
	agent.setAgentClient(conn)
	enableClientElicitation(agent, true, true)

	response, err := agent.handleCodexServerRequest(turnCtx, codex.ServerRequest{
		Method: codex.RequestMCPElicitation,
		Params: json.RawMessage(`{"threadId":"` + session.codexThreadID + `","turnId":"native-permission-turn","serverName":"wagie","mode":"form","message":"Need input","requestedSchema":{"type":"object","properties":{}}}`),
	})
	require.NoError(t, err)
	require.Equal(t, "accept", asType[map[string]any](t, response)["action"])
	require.Len(t, conn.scopes, 1)
	require.Empty(t, conn.scopes[0].ToolCallID)
	require.NotNil(t, conn.scopes[0].RequestID)
	require.NotNil(t, conn.scopes[0].RequestID.Str)
	require.Regexp(t, `^codex-elicitation-[0-9a-f-]{36}$`, string(*conn.scopes[0].RequestID.Str))
}

func TestStandaloneElicitationFailsClosedAtInteractionLimit(t *testing.T) {
	agent, session, _, turnCtx := newStrictPermissionSession(t)
	conn := newRecordingAgentClient()
	agent.setAgentClient(conn)
	enableClientElicitation(agent, true, true)

	session.mu.Lock()
	session.interactions = make(map[string]*sessionInteraction, sessionInteractionLimit)
	for index := range sessionInteractionLimit {
		_, cancel := context.WithCancel(turnCtx)
		session.interactions[fmt.Sprintf("held-%d", index)] = &sessionInteraction{cancel: cancel}
	}
	session.mu.Unlock()

	response, err := agent.handleCodexServerRequest(turnCtx, codex.ServerRequest{
		ID:     json.RawMessage(`"overflow-elicitation"`),
		Method: codex.RequestMCPElicitation,
		Params: json.RawMessage(`{"threadId":"` + session.codexThreadID + `","turnId":"native-permission-turn","serverName":"wagie","mode":"form","message":"Need input","requestedSchema":{"type":"object"}}`),
	})
	require.ErrorIs(t, err, context.Canceled)
	require.Nil(t, response)
	require.Empty(t, conn.elicitations)
	session.mu.Lock()
	require.Len(t, session.interactions, sessionInteractionLimit)
	session.mu.Unlock()
}

func TestMCPUserElicitationRejectsMissingOrStaleNativeTurn(t *testing.T) {
	for name, turn := range map[string]string{
		"null":  `null`,
		"stale": `"stale-native-turn"`,
	} {
		t.Run(name, func(t *testing.T) {
			agent, session, _, turnCtx := newStrictPermissionSession(t)
			conn := newRecordingAgentClient()
			conn.elicitation = acp.UnstableCreateElicitationResponse{
				Accept: &acp.UnstableCreateElicitationAccept{Action: "accept"},
			}
			agent.setAgentClient(conn)
			enableClientElicitation(agent, true, true)

			response, err := agent.handleCodexServerRequest(turnCtx, codex.ServerRequest{
				ID:     json.RawMessage(`72`),
				Method: codex.RequestMCPElicitation,
				Params: json.RawMessage(`{"threadId":"` + session.codexThreadID + `","turnId":` + turn + `,"serverName":"wagie","mode":"form","message":"Need input"}`),
			})
			require.NoError(t, err)
			require.Equal(t, "cancel", asType[map[string]any](t, response)["action"])
			require.Empty(t, conn.scopes)
		})
	}
}

func TestMCPUserElicitationURLRemainsStandaloneDuringActiveTool(t *testing.T) {
	agent, session, _, turnCtx := newStrictPermissionSession(t)
	session.setTurnID("turn-live")
	emitNativePermissionToolEvent(t, session, turnCtx, codex.Event{
		Kind: codex.EventToolStarted,
		Tool: codex.ToolEvent{
			ID: "exec-active", Kind: toolKindMcpToolCall,
			Raw: map[string]any{"server": "wagie", "tool": "execute"},
		},
	})

	conn := newRecordingAgentClient()
	conn.elicitation = acp.UnstableCreateElicitationResponse{
		Accept: &acp.UnstableCreateElicitationAccept{Action: "accept"},
	}
	agent.setAgentClient(conn)
	enableClientElicitation(agent, true, true)

	response, err := agent.handleCodexServerRequest(turnCtx, codex.ServerRequest{
		ID:     json.RawMessage(`83`),
		Method: codex.RequestMCPElicitation,
		Params: json.RawMessage(`{"threadId":"` + session.codexThreadID + `","turnId":"turn-live","serverName":"wagie","mode":"url","message":"Open","url":"https://example.test","elicitationId":"standalone-url"}`),
	})
	require.NoError(t, err)
	require.Equal(t, "accept", asType[map[string]any](t, response)["action"])
	require.Len(t, conn.scopes, 1)
	require.Empty(t, conn.scopes[0].ToolCallID)
	require.NotNil(t, conn.scopes[0].RequestID)
	require.NotNil(t, conn.scopes[0].RequestID.Str)
	require.Equal(t, acp.RequestIdStr("jsonrpc:number:83"), *conn.scopes[0].RequestID.Str)
}

func TestMCPUserElicitationCorrelationFailsClosed(t *testing.T) {
	tests := []struct {
		name   string
		events []codex.Event
	}{
		{name: "missing"},
		{
			name: "completed association",
			events: []codex.Event{
				{Kind: codex.EventToolStarted, Tool: codex.ToolEvent{ID: "exec-terminal", Kind: toolKindMcpToolCall, Raw: map[string]any{"server": "wagie", "tool": "execute"}}},
				{Kind: codex.EventToolCompleted, Tool: codex.ToolEvent{ID: "exec-terminal", Kind: toolKindMcpToolCall, Raw: map[string]any{"server": "wagie", "tool": "execute"}}},
			},
		},
		{
			name: "ambiguous",
			events: []codex.Event{
				{Kind: codex.EventToolStarted, Tool: codex.ToolEvent{ID: "exec-one", Kind: toolKindMcpToolCall, Raw: map[string]any{"server": "wagie", "tool": "execute"}}},
				{Kind: codex.EventToolStarted, Tool: codex.ToolEvent{ID: "exec-two", Kind: toolKindMcpToolCall, Raw: map[string]any{"server": "wagie", "tool": "execute"}}},
			},
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			agent, session, _, turnCtx := newStrictPermissionSession(t)
			for _, event := range test.events {
				emitNativePermissionToolEvent(t, session, turnCtx, event)
			}

			conn := newRecordingAgentClient()
			conn.elicitation = acp.UnstableCreateElicitationResponse{
				Accept: &acp.UnstableCreateElicitationAccept{Action: "accept"},
			}
			agent.setAgentClient(conn)
			enableClientElicitation(agent, true, true)

			response, err := agent.handleCodexServerRequest(turnCtx, codex.ServerRequest{
				ID:     json.RawMessage(`"mcp-user-fail-closed"`),
				Method: codex.RequestMCPElicitation,
				Params: json.RawMessage(`{"threadId":"` + session.codexThreadID + `","turnId":"native-permission-turn","itemId":"ctc-unresolved","mode":"form","message":"Need input","_meta":{"serverName":"wagie","toolName":"execute"}}`),
			})
			require.NoError(t, err)
			require.Equal(t, "cancel", asType[map[string]any](t, response)["action"])
			require.Empty(t, conn.scopes)
		})
	}
}
