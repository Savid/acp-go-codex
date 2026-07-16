package codexacp

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sync"
	"testing"

	"github.com/coder/acp-go-sdk"
	"github.com/savid/acp-go-codex/internal/codex"
	"github.com/stretchr/testify/require"
)

// strictPermissionClient models the Wagie hard-cutover invariant: a
// permission request is accepted only when its toolCallId was already
// published and has not reached a terminal state.
type strictPermissionClient struct {
	*recordingAgentClient

	mu          sync.Mutex
	states      map[acp.ToolCallId]acp.ToolCallStatus
	order       []string
	permissions []acp.RequestPermissionRequest
}

func newStrictPermissionClient() *strictPermissionClient {
	return &strictPermissionClient{
		recordingAgentClient: newRecordingAgentClient(),
		states:               make(map[acp.ToolCallId]acp.ToolCallStatus),
	}
}

func (c *strictPermissionClient) SessionUpdate(_ context.Context, notification acp.SessionNotification) error {
	c.mu.Lock()
	defer c.mu.Unlock()

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
	_ context.Context,
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
	c.order = append(c.order, "permission:"+string(request.ToolCall.ToolCallId))

	return acp.RequestPermissionResponse{Outcome: acp.NewRequestPermissionOutcomeSelected("accept")}, nil
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
	conn := newStrictPermissionClient()
	agent.setAgentClient(conn)

	response, err := agent.NewSession(ctx, NewSessionRequest(t.TempDir()))
	require.NoError(t, err)
	session := agent.sessionMust(response.SessionId)
	turnCtx := session.beginTurn(ctx, "permission-turn")
	t.Cleanup(session.finishTurn)

	return agent, session, conn, turnCtx
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
		ID:     json.RawMessage(`"native-approval-ctc-02b134"`),
		Method: codex.RequestMCPElicitation,
		Params: json.RawMessage(`{"threadId":"` + session.codexThreadID + `","message":"Allow the wagie MCP server to run tool \"execute\"?","tool_params":{"code":"return api.request({})"},"_meta":{"codex":{"serverName":"wagie","_meta":{"codex_approval_kind":"mcp_tool_call","tool_title":"Execute"}}}}`),
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

func TestPermissionPublishesPendingBeforeCallbackAndDedupesNativeStart(t *testing.T) {
	agent, session, conn, turnCtx := newStrictPermissionSession(t)

	result, err := agent.handleCodexServerRequest(turnCtx, codex.ServerRequest{
		ID:     json.RawMessage(`"approval-before-start"`),
		Method: codex.RequestMCPElicitation,
		Params: json.RawMessage(`{"threadId":"` + session.codexThreadID + `","message":"Allow the wagie MCP server to run tool \"execute\"?","tool_params":{"code":"return 1"},"_meta":{"codex":{"serverName":"wagie","_meta":{"codex_approval_kind":"mcp_tool_call","tool_title":"Execute"}}}}`),
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
				Params: json.RawMessage(`{"threadId":"` + session.codexThreadID + `"` + test.params + `}`),
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

	inactive := newSession(agent, "inactive", "/tmp/project", nil, codex.Thread{ID: "inactive-thread"}, newSpyCodexClient(), sessionMeta{}, nil)
	_, requested, err := inactive.requestPermissionForTool(context.Background(), conn, acp.RequestPermissionRequest{}, permissionToolMCP)
	require.NoError(t, err)
	require.False(t, requested)

	canceled, cancel := context.WithCancel(turnCtx)
	cancel()
	_, requested, err = session.requestPermissionForTool(canceled, conn, acp.RequestPermissionRequest{}, permissionToolMCP)
	require.NoError(t, err)
	require.False(t, requested)

	session.permissionTools.reset()
	_, requested, err = session.requestPermissionForTool(turnCtx, conn, acp.RequestPermissionRequest{}, permissionToolMCP)
	require.NoError(t, err)
	require.False(t, requested)

	session.permissionTools.reset()
	session.permissionTools.tools = map[acp.ToolCallId]*permissionToolRecord{
		"finished-record": {id: "finished-record", class: permissionToolMCP, terminal: true},
	}
	session.permissionTools.aliases = map[string]acp.ToolCallId{"approval": "finished-record"}
	_, requested, err = session.requestPermissionForTool(turnCtx, conn, acp.RequestPermissionRequest{
		ToolCall: acp.ToolCallUpdate{ToolCallId: "approval"},
	}, permissionToolMCP)
	require.NoError(t, err)
	require.False(t, requested)

	session.permissionTools.reset()
	session.permissionTools.aliases = map[string]acp.ToolCallId{"approval": "missing"}
	_, requested, err = session.requestPermissionForTool(turnCtx, conn, acp.RequestPermissionRequest{
		ToolCall: acp.ToolCallUpdate{ToolCallId: "approval"},
	}, permissionToolMCP)
	require.NoError(t, err)
	require.False(t, requested)

	session.permissionTools.reset()
	session.permissionTools.tools = map[acp.ToolCallId]*permissionToolRecord{
		"one": {id: "one", class: permissionToolMCP},
		"two": {id: "two", class: permissionToolMCP},
	}
	_, requested, err = session.requestPermissionForTool(turnCtx, conn, acp.RequestPermissionRequest{
		ToolCall: acp.ToolCallUpdate{ToolCallId: "unmatched"},
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
		ToolCall: acp.ToolCallUpdate{ToolCallId: "awaiting-start"},
	}, permissionToolMCP)
	require.ErrorContains(t, err, "pending start failed")
	require.False(t, requested)

	var unlocked permissionToolEventPublication
	unlocked.finish(true)

	failedPublication := session.preparePermissionToolEvent(codex.Event{
		Kind: codex.EventToolStarted,
		Tool: codex.ToolEvent{ID: "failed-publication", Kind: toolKindMcpToolCall},
	})
	failedPublication.finish(false)

	completedPublication := session.preparePermissionToolEvent(codex.Event{
		Kind: codex.EventToolCompleted,
		Tool: codex.ToolEvent{ID: "completed-without-start", Kind: toolKindMcpToolCall},
	})
	completedPublication.finish(true)
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
	agent.setAgentClient(newStrictPermissionClient())
	created, err := agent.NewSession(ctx, NewSessionRequest(t.TempDir()))
	require.NoError(t, err)
	session := agent.sessionMust(created.SessionId)

	permissions, err := agent.handleCodexServerRequest(ctx, codex.ServerRequest{
		ID:     json.RawMessage(`"permissions-outside-turn"`),
		Method: codex.RequestPermissionsApproval,
		Params: json.RawMessage(`{"threadId":"` + session.codexThreadID + `","itemId":"profile","permissions":{"filesystem":{"write":true}}}`),
	})
	require.NoError(t, err)
	require.NotNil(t, permissions)

	mcp, err := agent.handleCodexServerRequest(ctx, codex.ServerRequest{
		ID:     json.RawMessage(`"mcp-outside-turn"`),
		Method: codex.RequestMCPElicitation,
		Params: json.RawMessage(`{"threadId":"` + session.codexThreadID + `","message":"Allow tool?","_meta":{"codex":{"serverName":"wagie","_meta":{"codex_approval_kind":"mcp_tool_call","tool_title":"execute"}}}}`),
	})
	require.NoError(t, err)
	require.Equal(t, "cancel", asType[map[string]any](t, mcp)["action"])
}
