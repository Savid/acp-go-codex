package codex

import (
	"context"
	"encoding/json"
	"errors"
	"sort"
	"sync"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestAppServerBackgroundTerminalMethodsStayThreadScoped(t *testing.T) {
	transport := newBackgroundTerminalTransport()
	client := &AppServerClient{rpc: newRPCConn(transport, nil)}
	t.Cleanup(func() { require.NoError(t, client.Close(context.Background())) })

	target, err := client.ListBackgroundTerminals(context.Background(), BackgroundTerminalListRequest{
		ThreadID: "target-thread",
		Cursor:   "target-cursor",
		Limit:    25,
	})
	require.NoError(t, err)
	require.Equal(t, "next-target", target.NextCursor)
	require.Len(t, target.Terminals, 1)
	require.Equal(t, "target-item", target.Terminals[0].ItemID)
	require.Equal(t, "target-process", target.Terminals[0].ProcessID)
	require.NotNil(t, target.Terminals[0].OSPID)
	require.EqualValues(t, 101, *target.Terminals[0].OSPID)
	require.Equal(t, "target-thread", transport.lastParams(methodBackgroundTerminalList)[fieldThreadID])
	require.Equal(t, "target-cursor", transport.lastParams(methodBackgroundTerminalList)["cursor"])
	require.EqualValues(t, 25, transport.lastParams(methodBackgroundTerminalList)["limit"])

	terminated, err := client.TerminateBackgroundTerminal(context.Background(), BackgroundTerminalTerminateRequest{
		ThreadID:  "target-thread",
		ProcessID: "target-process",
	})
	require.NoError(t, err)
	require.True(t, terminated)
	require.Equal(t, "target-thread", transport.lastParams(methodBackgroundTerminalTerminate)[fieldThreadID])
	require.Equal(t, "target-process", transport.lastParams(methodBackgroundTerminalTerminate)["processId"])

	target, err = client.ListBackgroundTerminals(context.Background(), BackgroundTerminalListRequest{ThreadID: "target-thread"})
	require.NoError(t, err)
	require.Empty(t, target.Terminals)

	peer, err := client.ListBackgroundTerminals(context.Background(), BackgroundTerminalListRequest{ThreadID: "peer-thread"})
	require.NoError(t, err)
	require.Len(t, peer.Terminals, 1)
	require.Equal(t, "peer-process", peer.Terminals[0].ProcessID)
	require.Nil(t, peer.Terminals[0].OSPID)

	terminated, err = client.TerminateBackgroundTerminal(context.Background(), BackgroundTerminalTerminateRequest{
		ThreadID:  "target-thread",
		ProcessID: "peer-process",
	})
	require.NoError(t, err)
	require.False(t, terminated)

	peer, err = client.ListBackgroundTerminals(context.Background(), BackgroundTerminalListRequest{ThreadID: "peer-thread"})
	require.NoError(t, err)
	require.Len(t, peer.Terminals, 1, "a target-thread termination must not reach a peer thread")
}

func TestAppServerBackgroundTerminalMethodsRejectUnscopedAndMalformedOperations(t *testing.T) {
	client := &AppServerClient{rpc: newRPCConn(&responseTransport{responses: map[string]any{
		methodBackgroundTerminalList: map[string]any{
			"data": []any{map[string]any{"itemId": "missing-process-id"}},
		},
	}}, nil)}
	t.Cleanup(func() { require.NoError(t, client.Close(context.Background())) })

	_, err := client.ListBackgroundTerminals(context.Background(), BackgroundTerminalListRequest{})
	require.ErrorContains(t, err, "threadId is required")
	_, err = client.TerminateBackgroundTerminal(context.Background(), BackgroundTerminalTerminateRequest{ProcessID: "process"})
	require.ErrorContains(t, err, "threadId is required")
	_, err = client.TerminateBackgroundTerminal(context.Background(), BackgroundTerminalTerminateRequest{ThreadID: "thread"})
	require.ErrorContains(t, err, "processId is required")
	_, err = client.ListBackgroundTerminals(context.Background(), BackgroundTerminalListRequest{ThreadID: "thread"})
	require.ErrorContains(t, err, "missing processId")
}

func TestAppServerBackgroundTerminalMethodsSurfaceRPCErrors(t *testing.T) {
	transport := newScriptTransport()
	transport.fail(methodBackgroundTerminalList, "list failed")
	transport.fail(methodBackgroundTerminalTerminate, "terminate failed")
	client := &AppServerClient{rpc: newRPCConn(transport, nil)}
	t.Cleanup(func() { require.NoError(t, client.Close(context.Background())) })

	_, err := client.ListBackgroundTerminals(
		context.Background(),
		BackgroundTerminalListRequest{ThreadID: "thread"},
	)
	require.ErrorContains(t, err, "list failed")

	_, err = client.TerminateBackgroundTerminal(
		context.Background(),
		BackgroundTerminalTerminateRequest{ThreadID: "thread", ProcessID: "process"},
	)
	require.ErrorContains(t, err, "terminate failed")
}

func TestOptionalInt64Value(t *testing.T) {
	int64Value := optionalInt64Value(map[string]any{"value": int64(12)}, "value")
	require.NotNil(t, int64Value)
	require.EqualValues(t, 12, *int64Value)

	intValue := optionalInt64Value(map[string]any{"value": 13}, "value")
	require.NotNil(t, intValue)
	require.EqualValues(t, 13, *intValue)

	require.Nil(t, optionalInt64Value(map[string]any{"value": "14"}, "value"))
	require.Nil(t, optionalInt64Value(map[string]any{}, "value"))
}

type backgroundTerminalTransport struct {
	mu        sync.Mutex
	recv      chan rpcMessage
	closed    bool
	sent      []rpcMessage
	terminals map[string]map[string]map[string]any
}

func newBackgroundTerminalTransport() *backgroundTerminalTransport {
	return &backgroundTerminalTransport{
		recv: make(chan rpcMessage, 16),
		terminals: map[string]map[string]map[string]any{
			"target-thread": {
				"target-process": {
					"itemId":    "target-item",
					"processId": "target-process",
					"osPid":     float64(101),
				},
			},
			"peer-thread": {
				"peer-process": {
					"itemId":    "peer-item",
					"processId": "peer-process",
					"osPid":     nil,
				},
			},
		},
	}
}

func (t *backgroundTerminalTransport) Send(_ context.Context, msg rpcMessage) error {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.closed {
		return errors.New("closed")
	}

	t.sent = append(t.sent, msg)
	if len(msg.ID) == 0 {
		return nil
	}

	var params map[string]any
	if err := json.Unmarshal(msg.Params, &params); err != nil {
		return err
	}
	threadID, _ := params[fieldThreadID].(string)

	var result map[string]any
	switch msg.Method {
	case methodBackgroundTerminalList:
		processIDs := make([]string, 0, len(t.terminals[threadID]))
		for processID := range t.terminals[threadID] {
			processIDs = append(processIDs, processID)
		}
		sort.Strings(processIDs)
		data := make([]any, 0, len(processIDs))
		for _, processID := range processIDs {
			data = append(data, t.terminals[threadID][processID])
		}
		result = map[string]any{"data": data}
		if params["cursor"] == "target-cursor" {
			result["nextCursor"] = "next-target"
		}
	case methodBackgroundTerminalTerminate:
		processID, _ := params["processId"].(string)
		_, terminated := t.terminals[threadID][processID]
		delete(t.terminals[threadID], processID)
		result = map[string]any{"terminated": terminated}
	default:
		result = map[string]any{}
	}

	t.recv <- rpcMessage{JSONRPC: jsonRPCVersion, ID: msg.ID, Result: mustRaw(result)}

	return nil
}

func (t *backgroundTerminalTransport) Recv() (rpcMessage, string, error) {
	msg, ok := <-t.recv
	if !ok {
		return rpcMessage{}, "", errors.New("closed")
	}

	return msg, string(mustRaw(msg)), nil
}

func (t *backgroundTerminalTransport) Close() error {
	t.mu.Lock()
	defer t.mu.Unlock()
	if !t.closed {
		t.closed = true
		close(t.recv)
	}

	return nil
}

func (t *backgroundTerminalTransport) lastParams(method string) map[string]any {
	t.mu.Lock()
	defer t.mu.Unlock()
	for i := len(t.sent) - 1; i >= 0; i-- {
		if t.sent[i].Method != method {
			continue
		}

		var params map[string]any
		_ = json.Unmarshal(t.sent[i].Params, &params)

		return params
	}

	return nil
}
