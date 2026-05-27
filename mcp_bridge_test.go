package codexacp

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/coder/acp-go-sdk"
)

func TestMCPBridgeHandshakeAndForwarding(t *testing.T) {
	agent := NewAgent(WithMCPProxyCommand("/bin/acp-go-codex"))
	conn := &recordingMCPAgentClient{recordingAgentClient: newRecordingAgentClient()}
	agent.setAgentClient(conn)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	bridge, err := agent.newMCPSessionBridge(ctx, "session-1", map[string]struct{}{"client-1": {}})
	if err != nil {
		t.Fatalf("newMCPSessionBridge returned error: %v", err)
	}
	tokenFile := bridge.tokenFile
	defer bridge.Close()

	rawConn, err := net.Dial(bridge.ln.Addr().Network(), bridge.ln.Addr().String())
	if err != nil {
		t.Fatalf("dial bridge: %v", err)
	}
	defer rawConn.Close()
	dec := json.NewDecoder(rawConn)
	enc := json.NewEncoder(rawConn)
	if err := enc.Encode(mcpProxyHello{Version: mcpProxyVersion, Token: bridge.token, ACPID: "client-1"}); err != nil {
		t.Fatalf("send hello: %v", err)
	}

	reqID := json.RawMessage("1")
	if err := enc.Encode(mcpRPCMessage{JSONRPC: mcpJSONRPCVersion, ID: &reqID, Method: "tools/list", Params: json.RawMessage(`{"cursor":"a"}`)}); err != nil {
		t.Fatalf("send MCP request: %v", err)
	}
	var response mcpRPCMessage
	if err := dec.Decode(&response); err != nil {
		t.Fatalf("decode MCP response: %v", err)
	}
	if response.ID == nil || string(*response.ID) != "1" || response.Error != nil {
		t.Fatalf("response = %#v", response)
	}
	if got := conn.waitMessage(t); got.Method != "tools/list" || got.Params["cursor"] != "a" {
		t.Fatalf("forwarded ACP request = %#v", got)
	}

	if err := enc.Encode(mcpRPCMessage{JSONRPC: mcpJSONRPCVersion, Method: "notifications/initialized", Params: json.RawMessage(`{"ok":true}`)}); err != nil {
		t.Fatalf("send MCP notification: %v", err)
	}
	if got := conn.waitNotify(t); got.Method != "notifications/initialized" || got.Params["ok"] != true {
		t.Fatalf("forwarded ACP notification = %#v", got)
	}

	resultCh := make(chan any, 1)
	errCh := make(chan *acp.RequestError, 1)
	go func() {
		params := mustJSONRaw(acp.UnstableMessageMcpRequest{
			ConnectionId: "mcp-1",
			Method:       "tools/call",
			Params:       map[string]any{"name": "echo"},
		})
		result, reqErr := agent.handleMCPMessage(ctx, params)
		resultCh <- result
		errCh <- reqErr
	}()

	var proxyRequest mcpRPCMessage
	if err := dec.Decode(&proxyRequest); err != nil {
		t.Fatalf("decode proxy request: %v", err)
	}
	if proxyRequest.Method != "tools/call" || proxyRequest.ID == nil {
		t.Fatalf("proxy request = %#v", proxyRequest)
	}
	if err := enc.Encode(mcpRPCMessage{JSONRPC: mcpJSONRPCVersion, ID: proxyRequest.ID, Result: json.RawMessage(`{"ok":true}`)}); err != nil {
		t.Fatalf("send proxy response: %v", err)
	}
	if reqErr := <-errCh; reqErr != nil {
		t.Fatalf("handleMCPMessage request error: %v", reqErr)
	}
	if result := <-resultCh; result.(map[string]any)["ok"] != true {
		t.Fatalf("handleMCPMessage result = %#v", result)
	}

	if err := bridge.Close(); err != nil {
		t.Fatalf("bridge Close returned error: %v", err)
	}
	if _, err := os.Stat(tokenFile); !os.IsNotExist(err) {
		t.Fatalf("token file still exists or stat failed differently: %v", err)
	}
	if !conn.disconnected("mcp-1") {
		t.Fatal("MCP disconnect was not forwarded")
	}
}

type recordingMCPAgentClient struct {
	*recordingAgentClient

	mu          sync.Mutex
	messages    []acp.UnstableMessageMcpRequest
	notifies    []acp.UnstableMessageMcpNotification
	disconnects []acp.UnstableMcpConnectionId
}

func (c *recordingMCPAgentClient) UnstableMessageMcp(_ context.Context, req acp.UnstableMessageMcpRequest) (acp.UnstableMessageMcpResponse, error) {
	c.mu.Lock()
	c.messages = append(c.messages, req)
	c.mu.Unlock()
	return map[string]any{"tools": []any{map[string]any{"name": "echo"}}}, nil
}

func (c *recordingMCPAgentClient) UnstableNotifyMcp(_ context.Context, req acp.UnstableMessageMcpNotification) error {
	c.mu.Lock()
	c.notifies = append(c.notifies, req)
	c.mu.Unlock()
	return nil
}

func (c *recordingMCPAgentClient) UnstableDisconnectMcp(ctx context.Context, req acp.UnstableDisconnectMcpRequest) (acp.UnstableDisconnectMcpResponse, error) {
	c.mu.Lock()
	c.disconnects = append(c.disconnects, req.ConnectionId)
	c.mu.Unlock()
	return c.recordingAgentClient.UnstableDisconnectMcp(ctx, req)
}

func (c *recordingMCPAgentClient) waitMessage(t *testing.T) acp.UnstableMessageMcpRequest {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		c.mu.Lock()
		if len(c.messages) > 0 {
			msg := c.messages[0]
			c.mu.Unlock()
			return msg
		}
		c.mu.Unlock()
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("timed out waiting for MCP message")
	return acp.UnstableMessageMcpRequest{}
}

func (c *recordingMCPAgentClient) waitNotify(t *testing.T) acp.UnstableMessageMcpNotification {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		c.mu.Lock()
		if len(c.notifies) > 0 {
			msg := c.notifies[0]
			c.mu.Unlock()
			return msg
		}
		c.mu.Unlock()
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("timed out waiting for MCP notification")
	return acp.UnstableMessageMcpNotification{}
}

func (c *recordingMCPAgentClient) disconnected(id acp.UnstableMcpConnectionId) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	for _, disconnect := range c.disconnects {
		if disconnect == id {
			return true
		}
	}
	return false
}

func mustJSONRaw(value any) json.RawMessage {
	raw, err := json.Marshal(value)
	if err != nil {
		panic(err)
	}
	return raw
}

func TestMCPBridgePreparationAndProxy(t *testing.T) {
	agent := NewAgent(WithMCPProxyCommand("/bin/acp-go-codex", "--flag"))
	servers := []acp.McpServer{{Stdio: &acp.McpServerStdio{Name: "stdio", Command: "echo"}}}
	prepared, bridge, err := agent.prepareMCPServers(context.Background(), "s", servers)
	if err != nil || bridge != nil || len(prepared) != 1 {
		t.Fatalf("prepare non-ACP servers prepared=%#v bridge=%#v err=%v", prepared, bridge, err)
	}
	if !hasACPMCPServer([]acp.McpServer{{Acp: &acp.McpServerAcpInline{Id: "acp", Name: "ACP"}}}) {
		t.Fatal("hasACPMCPServer missed ACP server")
	}
	if _, _, err := agent.prepareMCPServers(context.Background(), "s", []acp.McpServer{{Acp: &acp.McpServerAcpInline{Id: "acp", Name: "ACP"}}}); err == nil {
		t.Fatal("prepare ACP server without MCP client succeeded")
	}

	conn := &recordingMCPAgentClient{recordingAgentClient: newRecordingAgentClient()}
	agent.setAgentClient(conn)
	prepared, bridge, err = agent.prepareMCPServers(context.Background(), "s", []acp.McpServer{{Acp: &acp.McpServerAcpInline{Id: "acp", Name: "ACP"}}})
	if err != nil {
		t.Fatalf("prepare ACP server returned error: %v", err)
	}
	defer bridge.Close()
	if len(prepared) != 1 || prepared[0].Stdio == nil || prepared[0].Stdio.Command != "/bin/acp-go-codex" {
		t.Fatalf("prepared servers = %#v", prepared)
	}
	args := bridge.proxyArgs("acp")
	if !containsAll(jsonString(args), "--flag", mcpProxySubcommand, "-acp-id", "acp") {
		t.Fatalf("proxy args = %#v", args)
	}
	if strings.Contains(jsonString(args), bridge.token) {
		t.Fatal("proxy args leaked token")
	}

	prepared, bridge, err = agent.prepareMCPServers(context.Background(), "s", []acp.McpServer{
		{Stdio: &acp.McpServerStdio{Name: "stdio", Command: "echo"}},
		{Acp: &acp.McpServerAcpInline{Id: "acp", Name: "ACP"}},
	})
	if err != nil {
		t.Fatalf("prepare mixed MCP servers returned error: %v", err)
	}
	defer bridge.Close()
	if len(prepared) != 2 || prepared[0].Stdio.Command != "echo" || prepared[1].Stdio.Command != "/bin/acp-go-codex" {
		t.Fatalf("mixed prepared servers = %#v", prepared)
	}

	origExecutable := currentExecutable
	t.Cleanup(func() { currentExecutable = origExecutable })
	currentExecutable = func() (string, error) { return "", errors.New("executable failed") }
	agent = NewAgent()
	agent.setAgentClient(conn)
	if _, _, err := agent.prepareMCPServers(context.Background(), "s", []acp.McpServer{{Acp: &acp.McpServerAcpInline{Id: "acp", Name: "ACP"}}}); err == nil {
		t.Fatal("prepare MCP servers accepted bridge creation failure")
	}
}

func TestMCPBridgeErrorAndUtilityBranches(t *testing.T) {
	origReader := mcpRandReader
	t.Cleanup(func() { mcpRandReader = origReader })
	mcpRandReader = strings.NewReader("short")
	if _, err := newMCPProxyToken(); err == nil {
		t.Fatal("short random reader generated token")
	}

	if tokenPath, err := writeMCPProxyTokenFile(strings.Repeat("x", 16)); err != nil {
		t.Fatalf("writeMCPProxyTokenFile returned error: %v", err)
	} else if err := removePrivateTempFile(tokenPath, mcpTokenDirPrefix, os.Remove); err != nil {
		t.Fatalf("cleanup token file: %v", err)
	}
	if mcpIDKey(nil) != "" || mcpIDKey(rawPtr(json.RawMessage(` "id" `))) != `"id"` {
		t.Fatal("mcpIDKey returned unexpected value")
	}
	if raw, err := marshalMCPParams(nil); err != nil || string(raw) != "null" {
		t.Fatalf("marshal nil params raw=%s err=%v", raw, err)
	}
	if _, err := mcpParamsMap(json.RawMessage(`[]`)); err == nil {
		t.Fatal("mcpParamsMap accepted array")
	}
	if result, err := unmarshalMCPResult(nil); err != nil || string(result.(json.RawMessage)) != "null" {
		t.Fatalf("empty MCP result = %#v err=%v", result, err)
	}
	if _, err := unmarshalMCPResult(json.RawMessage(`{bad}`)); err == nil {
		t.Fatal("unmarshalMCPResult accepted bad JSON")
	}
	var copied bytes.Buffer
	done := make(chan error, 1)
	proxyCopy(done, &copied, strings.NewReader("one\n"))
	if err := <-done; err != io.EOF || copied.String() != "one\n" {
		t.Fatalf("proxyCopy copied %q err=%v", copied.String(), err)
	}
	done = make(chan error, 1)
	proxyCopy(done, errorWriter{}, strings.NewReader("one\n"))
	if err := <-done; err == nil {
		t.Fatal("proxyCopy ignored writer error")
	}
	done = make(chan error, 1)
	proxyCopy(done, io.Discard, strings.NewReader(strings.Repeat("x", mcpProxyMaxBuf+1)))
	if err := <-done; err == nil {
		t.Fatal("proxyCopy ignored scanner error")
	}
	if params, err := mcpParamsMap(nil); err != nil || len(params) != 0 {
		t.Fatalf("nil MCP params = %#v err=%v", params, err)
	}
	doneClosed := make(chan struct{})
	close(doneClosed)
	if (&mcpSessionBridge{done: doneClosed, conns: make(map[*mcpBridgeConn]struct{})}).addConn(&mcpBridgeConn{}) {
		t.Fatal("addConn accepted closed bridge")
	}
	fullBridge := &mcpSessionBridge{done: make(chan struct{}), conns: make(map[*mcpBridgeConn]struct{})}
	for i := 0; i < mcpMaxConnections; i++ {
		fullBridge.conns[&mcpBridgeConn{}] = struct{}{}
	}
	if fullBridge.addConn(&mcpBridgeConn{}) {
		t.Fatal("addConn accepted too many connections")
	}
	emptyBridge := &mcpSessionBridge{done: make(chan struct{})}
	if !emptyBridge.addConn(&mcpBridgeConn{}) {
		t.Fatal("addConn rejected bridge with nil connection map")
	}
}

func TestMCPBridgeDirectConnectionBranches(t *testing.T) {
	left, right := net.Pipe()
	defer left.Close()
	defer right.Close()
	conn := &mcpBridgeConn{
		conn:         left,
		enc:          json.NewEncoder(left),
		connectionID: "conn",
		pending:      make(map[string]chan mcpRPCMessage),
		closed:       make(chan struct{}),
	}
	id := json.RawMessage("1")
	readCh := make(chan mcpRPCMessage, 2)
	go func() {
		dec := json.NewDecoder(right)
		for i := 0; i < 2; i++ {
			var msg mcpRPCMessage
			if err := dec.Decode(&msg); err == nil {
				readCh <- msg
			}
		}
	}()
	if err := conn.sendMCPError(id, -1, "bad", map[string]any{"x": true}); err != nil {
		t.Fatalf("sendMCPError returned error: %v", err)
	}
	if err := conn.forwardACPNotification(context.Background(), acp.UnstableMessageMcpRequest{Method: "notifications/test", Params: map[string]any{"ok": true}}); err != nil {
		t.Fatalf("forwardACPNotification returned error: %v", err)
	}
	for i := 0; i < 2; i++ {
		select {
		case msg := <-readCh:
			if msg.JSONRPC != mcpJSONRPCVersion {
				t.Fatalf("message missing jsonrpc: %#v", msg)
			}
		case <-time.After(time.Second):
			t.Fatal("timed out waiting for MCP message")
		}
	}
	if err := conn.forwardACPNotification(canceledContext(), acp.UnstableMessageMcpRequest{Method: "notifications/test"}); err == nil {
		t.Fatal("forwardACPNotification with canceled context succeeded")
	}
	if _, err := conn.forwardACPRequest(canceledContext(), acp.UnstableMessageMcpRequest{Method: "tools/list"}); err == nil {
		t.Fatal("forwardACPRequest with canceled context succeeded")
	}
	full := &mcpBridgeConn{conn: left, enc: json.NewEncoder(left), pending: make(map[string]chan mcpRPCMessage), closed: make(chan struct{})}
	for i := 0; i < mcpMaxPending; i++ {
		full.pending[fmt.Sprint(i)] = make(chan mcpRPCMessage)
	}
	if _, err := full.forwardACPRequest(context.Background(), acp.UnstableMessageMcpRequest{Method: "tools/list"}); err == nil {
		t.Fatal("forwardACPRequest accepted too many pending requests")
	}

	if err := conn.forwardACPNotification(context.Background(), acp.UnstableMessageMcpRequest{Method: "notifications/test", Params: map[string]any{"bad": func() {}}}); err == nil {
		t.Fatal("forwardACPNotification accepted unmarshalable params")
	}
	if _, err := conn.forwardACPRequest(context.Background(), acp.UnstableMessageMcpRequest{Method: "tools/list", Params: map[string]any{"bad": func() {}}}); err == nil {
		t.Fatal("forwardACPRequest accepted unmarshalable params")
	}

	closedLeft, closedRight := net.Pipe()
	_ = closedRight.Close()
	sendErrConn := &mcpBridgeConn{conn: closedLeft, enc: json.NewEncoder(closedLeft), pending: make(map[string]chan mcpRPCMessage), closed: make(chan struct{})}
	if _, err := sendErrConn.forwardACPRequest(context.Background(), acp.UnstableMessageMcpRequest{Method: "tools/list"}); err == nil {
		t.Fatal("forwardACPRequest ignored send error")
	}
	_ = closedLeft.Close()

	respLeft, respRight := net.Pipe()
	defer respLeft.Close()
	defer respRight.Close()
	respConn := &mcpBridgeConn{conn: respLeft, enc: json.NewEncoder(respLeft), dec: json.NewDecoder(respLeft), pending: make(map[string]chan mcpRPCMessage), closed: make(chan struct{})}
	responseDone := make(chan error, 1)
	go func() {
		var req mcpRPCMessage
		if err := json.NewDecoder(respRight).Decode(&req); err != nil {
			responseDone <- err
			return
		}
		respConn.handleProxyResponse(mcpRPCMessage{ID: req.ID, Error: &mcpRPCError{Code: -1, Message: "bad"}})
		responseDone <- nil
	}()
	if _, err := respConn.forwardACPRequest(context.Background(), acp.UnstableMessageMcpRequest{Method: "tools/list"}); err == nil {
		t.Fatal("forwardACPRequest accepted proxy error response")
	}
	if err := <-responseDone; err != nil {
		t.Fatalf("proxy error response failed: %v", err)
	}
}

func TestMCPBridgeErrorBranches(t *testing.T) {
	origExecutable := currentExecutable
	origListen := mcpNetListen
	origReader := mcpRandReader
	t.Cleanup(func() {
		currentExecutable = origExecutable
		mcpNetListen = origListen
		mcpRandReader = origReader
	})

	agent := NewAgent()
	currentExecutable = func() (string, error) { return "", errors.New("no executable") }
	if _, err := agent.newMCPSessionBridge(context.Background(), "s", map[string]struct{}{"acp": {}}); err == nil {
		t.Fatal("newMCPSessionBridge accepted executable failure")
	}
	currentExecutable = origExecutable
	mcpRandReader = strings.NewReader("short")
	if _, err := agent.newMCPSessionBridge(context.Background(), "s", map[string]struct{}{"acp": {}}); err == nil {
		t.Fatal("newMCPSessionBridge accepted token failure")
	}
	mcpRandReader = origReader
	mcpNetListen = func(network, address string) (net.Listener, error) { return nil, errors.New("listen failed") }
	if _, err := agent.newMCPSessionBridge(context.Background(), "s", map[string]struct{}{"acp": {}}); err == nil {
		t.Fatal("newMCPSessionBridge accepted listen failure")
	}

	left, right := net.Pipe()
	defer left.Close()
	defer right.Close()
	conn := &mcpBridgeConn{
		agent:   NewAgent(),
		conn:    left,
		enc:     json.NewEncoder(left),
		pending: make(map[string]chan mcpRPCMessage),
		closed:  make(chan struct{}),
	}
	readCh := make(chan mcpRPCMessage, 4)
	go func() {
		dec := json.NewDecoder(right)
		for {
			var msg mcpRPCMessage
			if err := dec.Decode(&msg); err != nil {
				return
			}
			readCh <- msg
		}
	}()
	reqID := json.RawMessage("1")
	conn.forwardProxyMessage(context.Background(), mcpRPCMessage{ID: &reqID, Method: "tools/list", Params: json.RawMessage(`[]`)})
	msg := <-readCh
	if msg.Error == nil {
		t.Fatalf("invalid params did not produce MCP error: %#v", msg)
	}
	conn.forwardProxyMessage(context.Background(), mcpRPCMessage{ID: &reqID, Method: "tools/list", Params: json.RawMessage(`{}`)})
	msg = <-readCh
	if msg.Error == nil || !strings.Contains(msg.Error.Message, "unavailable") {
		t.Fatalf("missing ACP client error = %#v", msg)
	}
	if release, err := (&mcpBridgeConn{}).acquireForward(context.Background()); err != nil {
		t.Fatalf("nil-session acquireForward returned error: %v", err)
	} else {
		release()
	}
	if _, err := (&mcpBridgeConn{}).acquireForward(canceledContext()); err == nil {
		t.Fatal("acquireForward accepted canceled context")
	}
	fullForwardBridge := &mcpSessionBridge{forwards: make(chan struct{}, 1)}
	fullForwardBridge.forwards <- struct{}{}
	if _, err := (&mcpBridgeConn{session: fullForwardBridge}).acquireForward(context.Background()); err == nil {
		t.Fatal("acquireForward accepted too many forwards")
	}
	cappedAgent := NewAgent()
	cappedAgent.setAgentClient(&recordingMCPAgentClient{recordingAgentClient: newRecordingAgentClient()})
	capLeft, capRight := net.Pipe()
	defer capLeft.Close()
	defer capRight.Close()
	cappedConn := &mcpBridgeConn{
		agent:   cappedAgent,
		session: fullForwardBridge,
		conn:    capLeft,
		enc:     json.NewEncoder(capLeft),
		closed:  make(chan struct{}),
		pending: make(map[string]chan mcpRPCMessage),
	}
	capRead := make(chan mcpRPCMessage, 1)
	go func() {
		var msg mcpRPCMessage
		_ = json.NewDecoder(capRight).Decode(&msg)
		capRead <- msg
	}()
	cappedConn.forwardProxyMessage(context.Background(), mcpRPCMessage{ID: &reqID, Method: "tools/list", Params: json.RawMessage(`{}`)})
	if msg := <-capRead; msg.Error == nil || !strings.Contains(msg.Error.Message, "too many") {
		t.Fatalf("capped forward response = %#v", msg)
	}
	conn.forwardProxyMessage(context.Background(), mcpRPCMessage{Method: "notifications/test", Params: json.RawMessage(`{}`)})
	if err := conn.sendMCPResult(reqID, func() {}); err == nil {
		t.Fatal("sendMCPResult accepted unmarshalable result")
	}
	if err := conn.sendMCPError(reqID, -1, "bad", func() {}); err == nil {
		t.Fatal("sendMCPError accepted unmarshalable data")
	}

	agent = NewAgent()
	if _, reqErr := agent.handleMCPMessage(context.Background(), json.RawMessage(`{bad}`)); reqErr == nil {
		t.Fatal("handleMCPMessage accepted bad JSON")
	}
	if _, reqErr := agent.handleMCPMessage(context.Background(), json.RawMessage(`{}`)); reqErr == nil {
		t.Fatal("handleMCPMessage accepted invalid request")
	}
	if _, reqErr := agent.handleMCPMessage(context.Background(), mustJSONRaw(acp.UnstableMessageMcpRequest{ConnectionId: "missing", Method: "tools/list"})); reqErr == nil {
		t.Fatal("handleMCPMessage accepted missing connection")
	}
	agent.registerMCPConnection(conn)
	if _, reqErr := agent.handleMCPMessage(canceledContext(), mustJSONRaw(acp.UnstableMessageMcpRequest{ConnectionId: conn.connectionID, Method: "notifications/test"})); reqErr == nil {
		t.Fatal("handleMCPMessage notification canceled succeeded")
	}

	notifyLeft, notifyRight := net.Pipe()
	defer notifyLeft.Close()
	defer notifyRight.Close()
	notifyConn := &mcpBridgeConn{
		agent:        agent,
		session:      &mcpSessionBridge{agent: agent, done: make(chan struct{}), conns: make(map[*mcpBridgeConn]struct{})},
		conn:         notifyLeft,
		enc:          json.NewEncoder(notifyLeft),
		connectionID: "notify",
		pending:      make(map[string]chan mcpRPCMessage),
		closed:       make(chan struct{}),
	}
	agent.registerMCPConnection(notifyConn)
	notifyDone := make(chan error, 1)
	go func() {
		var msg mcpRPCMessage
		notifyDone <- json.NewDecoder(notifyRight).Decode(&msg)
	}()
	if result, reqErr := agent.handleMCPMessage(context.Background(), mustJSONRaw(acp.UnstableMessageMcpRequest{ConnectionId: "notify", Method: "notifications/test"})); reqErr != nil || result != nil {
		t.Fatalf("handleMCPMessage notification result=%#v err=%v", result, reqErr)
	}
	if err := <-notifyDone; err != nil {
		t.Fatalf("notification decode failed: %v", err)
	}
}

func TestMCPBridgeHandleConnHandshakeFailures(t *testing.T) {
	base := &mcpSessionBridge{agent: NewAgent(), token: "tok", allowed: map[string]struct{}{"acp": {}}, done: make(chan struct{}), conns: make(map[*mcpBridgeConn]struct{})}
	cases := []mcpProxyHello{
		{Version: 0, Token: "tok", ACPID: "acp"},
		{Version: mcpProxyVersion, Token: "bad", ACPID: "acp"},
		{Version: mcpProxyVersion, Token: "tok", ACPID: "missing"},
		{Version: mcpProxyVersion, Token: "tok"},
	}
	for _, hello := range cases {
		left, right := net.Pipe()
		done := make(chan struct{})
		go func() {
			base.handleConn(context.Background(), left)
			close(done)
		}()
		if err := json.NewEncoder(right).Encode(hello); err != nil {
			t.Fatalf("encode hello: %v", err)
		}
		<-done
		_ = right.Close()
	}

	left, right := net.Pipe()
	done := make(chan struct{})
	go func() {
		base.handleConn(context.Background(), left)
		close(done)
	}()
	_, _ = right.Write([]byte("{bad}\n"))
	<-done
	_ = right.Close()

	left, right = net.Pipe()
	done = make(chan struct{})
	go func() {
		base.handleConn(context.Background(), left)
		close(done)
	}()
	if err := json.NewEncoder(right).Encode(mcpProxyHello{Version: mcpProxyVersion, Token: "tok", ACPID: "acp"}); err != nil {
		t.Fatalf("encode valid hello: %v", err)
	}
	<-done
	_ = right.Close()

	errAgent := NewAgent()
	errAgent.setAgentClient(&connectErrorMCPAgentClient{recordingMCPAgentClient: &recordingMCPAgentClient{recordingAgentClient: newRecordingAgentClient()}})
	bridge := &mcpSessionBridge{agent: errAgent, token: "tok", allowed: map[string]struct{}{"acp": {}}, done: make(chan struct{}), conns: make(map[*mcpBridgeConn]struct{})}
	left, right = net.Pipe()
	done = make(chan struct{})
	go func() {
		bridge.handleConn(context.Background(), left)
		close(done)
	}()
	if err := json.NewEncoder(right).Encode(mcpProxyHello{Version: mcpProxyVersion, Token: "tok", ACPID: "acp"}); err != nil {
		t.Fatalf("encode valid hello: %v", err)
	}
	<-done
	_ = right.Close()
}

func TestMCPBridgeCloseAndProxyErrors(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "child"), []byte("x"), 0o600); err != nil {
		t.Fatalf("write child: %v", err)
	}
	bridge := &mcpSessionBridge{done: make(chan struct{}), tokenFile: dir, conns: make(map[*mcpBridgeConn]struct{})}
	if err := bridge.Close(); err == nil {
		t.Fatal("bridge Close ignored token cleanup error")
	}

	origDial := mcpDialContext
	t.Cleanup(func() { mcpDialContext = origDial })
	mcpDialContext = func(context.Context, string, string) (net.Conn, error) {
		return nil, errors.New("dial failed")
	}
	if err := RunMCPProxy(context.Background(), strings.NewReader(""), io.Discard, MCPProxyOptions{Network: "tcp", Address: "bad"}); err == nil {
		t.Fatal("RunMCPProxy ignored dial error")
	}
	mcpDialContext = func(context.Context, string, string) (net.Conn, error) {
		left, right := net.Pipe()
		_ = right.Close()
		return left, nil
	}
	if err := RunMCPProxy(context.Background(), strings.NewReader(""), io.Discard, MCPProxyOptions{Network: "tcp", Address: "bad"}); err == nil {
		t.Fatal("RunMCPProxy ignored hello write error")
	}

	ctx, cancel := context.WithCancel(context.Background())
	stdinR, stdinW := io.Pipe()
	defer stdinR.Close()
	defer stdinW.Close()
	mcpDialContext = func(context.Context, string, string) (net.Conn, error) {
		left, right := net.Pipe()
		t.Cleanup(func() {
			_ = left.Close()
			_ = right.Close()
		})
		go func() {
			var hello mcpProxyHello
			_ = json.NewDecoder(right).Decode(&hello)
			cancel()
		}()
		return left, nil
	}
	if err := RunMCPProxy(ctx, stdinR, io.Discard, MCPProxyOptions{Network: "tcp", Address: "ok"}); err == nil {
		t.Fatal("RunMCPProxy ignored context cancellation")
	}
}

func TestMCPStableFromUnstableAndConfigBranches(t *testing.T) {
	stable := stableMCPServersFromUnstable([]acp.UnstableMcpServer{
		{Stdio: &acp.McpServerStdio{Name: "stdio", Command: "cmd", Env: []acp.EnvVariable{{Name: "A", Value: "B"}}}},
		{Http: &acp.UnstableMcpServerHttp{Name: "http", Url: "http://example.com", Headers: []acp.HttpHeader{{Name: "H", Value: "V"}}}},
		{Acp: &acp.UnstableMcpServerAcpInline{Id: "acp", Name: "acp"}},
		{Sse: &acp.UnstableMcpServerSse{Name: "sse", Url: "http://example.com/sse"}},
		{},
	})
	if len(stable) != 4 || stable[0].Stdio == nil || stable[1].Http == nil || stable[2].Acp == nil || stable[3].Sse == nil {
		t.Fatalf("stable MCP servers = %#v", stable)
	}
	if stableMCPServersFromUnstable(nil) != nil {
		t.Fatal("nil unstable MCP servers did not stay nil")
	}
	seen := map[string]int{}
	name1 := mcpServerConfigName(acp.McpServer{Stdio: &acp.McpServerStdio{Name: " bad name "}}, 0, seen)
	name2 := mcpServerConfigName(acp.McpServer{Stdio: &acp.McpServerStdio{Name: " bad name "}}, 1, seen)
	if name1 != "bad_name" || name2 != "bad_name_2" {
		t.Fatalf("MCP config names = %q %q", name1, name2)
	}
	if name := mcpServerConfigName(acp.McpServer{}, 2, seen); name != "server_3" {
		t.Fatalf("default MCP config name = %q", name)
	}
	if got := codexConfigArg("x", 3); len(got) != 2 || got[1] != "x=3" {
		t.Fatalf("codexConfigArg = %#v", got)
	}
	if !strings.Contains(tomlEnvTable([]acp.EnvVariable{{}, {Name: "A", Value: "B"}}), "A") {
		t.Fatal("tomlEnvTable skipped valid env")
	}
	if !strings.Contains(tomlHeaderTable([]acp.HttpHeader{{}, {Name: "H", Value: "V"}}), "H") {
		t.Fatal("tomlHeaderTable skipped valid header")
	}
}

func TestRunMCPProxyCopiesLines(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer ln.Close()

	serverDone := make(chan error, 1)
	go func() {
		conn, err := ln.Accept()
		if err != nil {
			serverDone <- err
			return
		}
		defer conn.Close()
		var hello mcpProxyHello
		if err := json.NewDecoder(conn).Decode(&hello); err != nil {
			serverDone <- err
			return
		}
		if hello.Token != "token" || hello.ACPID != "acp" {
			serverDone <- fmt.Errorf("hello = %#v", hello)
			return
		}
		line, err := bufioReadLine(conn)
		if err != nil {
			serverDone <- err
			return
		}
		if strings.TrimSpace(line) != `{"jsonrpc":"2.0"}` {
			serverDone <- fmt.Errorf("line = %q", line)
			return
		}
		_, _ = conn.Write([]byte(`{"ok":true}` + "\n"))
		serverDone <- nil
	}()

	var stdout bytes.Buffer
	stdinR, stdinW := io.Pipe()
	t.Cleanup(func() {
		_ = stdinR.Close()
		_ = stdinW.Close()
	})
	go func() {
		_, _ = stdinW.Write([]byte(`{"jsonrpc":"2.0"}` + "\n"))
	}()
	err = RunMCPProxy(context.Background(), stdinR, &stdout, MCPProxyOptions{
		Network: "tcp",
		Address: ln.Addr().String(),
		Token:   "token",
		ACPID:   "acp",
	})
	if err != nil {
		t.Fatalf("RunMCPProxy returned error: %v", err)
	}
	if err := <-serverDone; err != nil {
		t.Fatalf("proxy server error: %v", err)
	}
	if strings.TrimSpace(stdout.String()) != `{"ok":true}` {
		t.Fatalf("proxy stdout = %q", stdout.String())
	}
}

func TestMCPBridgeHandshakeAndIOErrors(t *testing.T) {
	agent := NewAgent()
	bridge := &mcpSessionBridge{agent: agent, ln: errorListener{}, done: make(chan struct{}), conns: make(map[*mcpBridgeConn]struct{})}
	bridge.accept(context.Background())

	noClientConn := &mcpBridgeConn{
		agent:        NewAgent(),
		session:      &mcpSessionBridge{agent: NewAgent(), done: make(chan struct{}), conns: make(map[*mcpBridgeConn]struct{})},
		conn:         noopConn{},
		connectionID: "mcp",
		pending:      make(map[string]chan mcpRPCMessage),
		closed:       make(chan struct{}),
	}
	noClientConn.close(context.Background())

	mcpAgent := NewAgent()
	mcpAgent.setAgentClient(&recordingMCPAgentClient{recordingAgentClient: newRecordingAgentClient()})
	closedDone := make(chan struct{})
	close(closedDone)
	closedBridge := &mcpSessionBridge{agent: mcpAgent, token: "tok", allowed: map[string]struct{}{"acp": {}}, done: closedDone, conns: make(map[*mcpBridgeConn]struct{})}
	left, right := net.Pipe()
	done := make(chan struct{})
	go func() {
		closedBridge.handleConn(context.Background(), left)
		close(done)
	}()
	if err := json.NewEncoder(right).Encode(mcpProxyHello{Version: mcpProxyVersion, Token: "tok", ACPID: "acp"}); err != nil {
		t.Fatalf("encode hello: %v", err)
	}
	<-done
	_ = right.Close()

	reqLeft, reqRight := net.Pipe()
	defer reqLeft.Close()
	defer reqRight.Close()
	reqConn := &mcpBridgeConn{conn: reqLeft, enc: json.NewEncoder(reqLeft), pending: make(map[string]chan mcpRPCMessage), closed: make(chan struct{})}
	readDone := make(chan struct{})
	go func() {
		var msg mcpRPCMessage
		_ = json.NewDecoder(reqRight).Decode(&msg)
		close(readDone)
	}()
	shortCtx, cancel := context.WithTimeout(context.Background(), time.Millisecond)
	defer cancel()
	if _, err := reqConn.forwardACPRequest(shortCtx, acp.UnstableMessageMcpRequest{Method: "tools/list"}); err == nil {
		t.Fatal("forwardACPRequest ignored timeout")
	}
	<-readDone

	closedLeft, closedRight := net.Pipe()
	defer closedLeft.Close()
	defer closedRight.Close()
	closedReqConn := &mcpBridgeConn{conn: closedLeft, enc: json.NewEncoder(closedLeft), pending: make(map[string]chan mcpRPCMessage), closed: make(chan struct{})}
	readDone = make(chan struct{})
	go func() {
		var msg mcpRPCMessage
		_ = json.NewDecoder(closedRight).Decode(&msg)
		closedReqConn.Close()
		close(readDone)
	}()
	if _, err := closedReqConn.forwardACPRequest(context.Background(), acp.UnstableMessageMcpRequest{Method: "tools/list"}); err == nil {
		t.Fatal("forwardACPRequest ignored closed connection")
	}
	<-readDone

	closedSelectLeft, closedSelectRight := net.Pipe()
	defer closedSelectLeft.Close()
	defer closedSelectRight.Close()
	closedSelectConn := &mcpBridgeConn{conn: closedSelectLeft, enc: json.NewEncoder(closedSelectLeft), pending: make(map[string]chan mcpRPCMessage), closed: make(chan struct{})}
	readDone = make(chan struct{})
	go func() {
		var msg mcpRPCMessage
		_ = json.NewDecoder(closedSelectRight).Decode(&msg)
		close(closedSelectConn.closed)
		close(readDone)
	}()
	if _, err := closedSelectConn.forwardACPRequest(context.Background(), acp.UnstableMessageMcpRequest{Method: "tools/list"}); err == nil {
		t.Fatal("forwardACPRequest ignored closed signal")
	}
	<-readDone

	badLeft, badRight := net.Pipe()
	defer badLeft.Close()
	defer badRight.Close()
	badConn := &mcpBridgeConn{conn: badLeft, enc: json.NewEncoder(badLeft), pending: make(map[string]chan mcpRPCMessage), closed: make(chan struct{})}
	doneErr := make(chan error, 1)
	go func() {
		var msg mcpRPCMessage
		if err := json.NewDecoder(badRight).Decode(&msg); err != nil {
			doneErr <- err
			return
		}
		badConn.handleProxyResponse(mcpRPCMessage{ID: msg.ID, Result: json.RawMessage(`{bad}`)})
		doneErr <- nil
	}()
	if _, err := badConn.forwardACPRequest(context.Background(), acp.UnstableMessageMcpRequest{Method: "tools/list"}); err == nil {
		t.Fatal("forwardACPRequest accepted bad JSON result")
	}
	if err := <-doneErr; err != nil {
		t.Fatalf("bad result response failed: %v", err)
	}

	origDial := mcpDialContext
	t.Cleanup(func() { mcpDialContext = origDial })
	mcpDialContext = func(context.Context, string, string) (net.Conn, error) {
		left, right := net.Pipe()
		go func() {
			defer right.Close()
			var hello mcpProxyHello
			_ = json.NewDecoder(right).Decode(&hello)
			_, _ = right.Write([]byte(strings.Repeat("x", mcpProxyMaxBuf+1) + "\n"))
		}()
		return left, nil
	}
	blockingStdin, blockingWriter := io.Pipe()
	defer blockingStdin.Close()
	defer blockingWriter.Close()
	if err := RunMCPProxy(context.Background(), blockingStdin, io.Discard, MCPProxyOptions{Network: "tcp", Address: "ok"}); err == nil {
		t.Fatal("RunMCPProxy ignored proxy copy error")
	}
}

func TestMCPProxyTokenFileRejectsInvalidTempDir(t *testing.T) {
	t.Setenv("TMPDIR", "/path/that/does/not/exist")
	if _, err := writeMCPProxyTokenFile("token"); err == nil {
		t.Fatal("writeMCPProxyTokenFile accepted invalid TMPDIR")
	}
}

func TestMCPProxyTokenFileHookErrorBranches(t *testing.T) {
	origCreateToken := mcpCreateTokenTemp
	t.Cleanup(func() { mcpCreateTokenTemp = origCreateToken })

	mcpCreateTokenTemp = func() (mcpTokenFile, error) {
		return &fakeTokenFile{name: "token", writeErr: errors.New("write failed")}, nil
	}
	if _, err := writeMCPProxyTokenFile("token"); err == nil {
		t.Fatal("writeMCPProxyTokenFile ignored write error")
	}
	mcpCreateTokenTemp = func() (mcpTokenFile, error) {
		return &fakeTokenFile{name: "token", syncErr: errors.New("sync failed")}, nil
	}
	if _, err := writeMCPProxyTokenFile("token"); err == nil {
		t.Fatal("writeMCPProxyTokenFile ignored sync error")
	}
	mcpCreateTokenTemp = func() (mcpTokenFile, error) {
		return &fakeTokenFile{name: "token", closeErr: errors.New("close failed")}, nil
	}
	if _, err := writeMCPProxyTokenFile("token"); err == nil {
		t.Fatal("writeMCPProxyTokenFile ignored close error")
	}
	mcpCreateTokenTemp = func() (mcpTokenFile, error) {
		return nil, errors.New("create failed")
	}
	if _, err := writeMCPProxyTokenFile("token"); err == nil {
		t.Fatal("writeMCPProxyTokenFile ignored create error")
	}
}

type fakeTokenFile struct {
	name     string
	writeErr error
	syncErr  error
	closeErr error
}

func (f *fakeTokenFile) Name() string { return f.name }
func (f *fakeTokenFile) WriteString(value string) (int, error) {
	if f.writeErr != nil {
		return 0, f.writeErr
	}
	return len(value), nil
}
func (f *fakeTokenFile) Sync() error  { return f.syncErr }
func (f *fakeTokenFile) Close() error { return f.closeErr }
