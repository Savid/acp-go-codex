package codexacp

import (
	"bufio"
	"bytes"
	"context"
	"crypto/rand"
	"crypto/subtle"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"os"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/coder/acp-go-sdk"
)

const (
	mcpProxyNetwork    = "tcp"
	mcpProxyHost       = "127.0.0.1:0"
	mcpProxySubcommand = "mcp-proxy"
	mcpProxyTokenBytes = 32
	mcpProxyTimeout    = 30 * time.Second
	mcpProxyInitialBuf = 1024 * 1024
	mcpProxyMaxBuf     = 10 * 1024 * 1024
	mcpJSONRPCVersion  = "2.0"
	mcpProxyVersion    = 1
	mcpMaxPending      = 64
	mcpMaxConnections  = 64
	mcpMaxForwards     = 64
	mcpTokenDirPrefix  = "acp-go-codex-mcp-token-" // #nosec G101 -- temp directory prefix, not a token value.

	// MCPProxyTokenFileEnv carries the path to a file containing the bridge token.
	// #nosec G101 -- this is the environment variable name, not the token value.
	MCPProxyTokenFileEnv = "ACP_GO_CODEX_MCP_PROXY_TOKEN_FILE"
)

var (
	currentExecutable            = os.Executable
	mcpRandReader      io.Reader = rand.Reader
	mcpNetListen                 = net.Listen
	mcpDialContext               = (&net.Dialer{}).DialContext
	mcpCreateTokenTemp           = func() (mcpTokenFile, error) {
		return createPrivateTempFile(mcpTokenDirPrefix, "token-*")
	}
)

type mcpSessionBridge struct {
	agent     *Agent
	session   acp.SessionId
	token     string
	tokenFile string
	command   string
	args      []string
	allowed   map[string]struct{}
	ln        net.Listener
	cancel    context.CancelFunc

	closeOnce sync.Once
	done      chan struct{}
	wg        sync.WaitGroup

	mu    sync.Mutex
	conns map[*mcpBridgeConn]struct{}

	forwards chan struct{}
}

type mcpBridgeConn struct {
	agent        *Agent
	session      *mcpSessionBridge
	conn         net.Conn
	dec          *json.Decoder
	enc          *json.Encoder
	connectionID acp.UnstableMcpConnectionId

	nextID  atomic.Uint64
	writeMu sync.Mutex

	mu      sync.Mutex
	pending map[string]chan mcpRPCMessage
	closed  chan struct{}
	once    sync.Once
}

type mcpProxyHello struct {
	Version int    `json:"version"`
	Token   string `json:"token"`
	ACPID   string `json:"acpId"`
}

type mcpTokenFile interface {
	Name() string
	WriteString(string) (int, error)
	Sync() error
	Close() error
}

type mcpRPCMessage struct {
	JSONRPC string           `json:"jsonrpc,omitempty"`
	ID      *json.RawMessage `json:"id,omitempty"`
	Method  string           `json:"method,omitempty"`
	Params  json.RawMessage  `json:"params,omitempty"`
	Result  json.RawMessage  `json:"result,omitempty"`
	Error   *mcpRPCError     `json:"error,omitempty"`
}

type mcpRPCError struct {
	Code    int             `json:"code"`
	Message string          `json:"message"`
	Data    json.RawMessage `json:"data,omitempty"`
}

// MCPProxyOptions configures RunMCPProxy for the internal mcp-proxy subcommand.
type MCPProxyOptions struct {
	Network string
	Address string
	Token   string
	ACPID   string
}

func (a *Agent) prepareMCPServers(ctx context.Context, sessionID acp.SessionId, servers []acp.McpServer) ([]acp.McpServer, *mcpSessionBridge, error) {
	if !hasACPMCPServer(servers) {
		return servers, nil, nil
	}
	if _, ok := a.connection().(mcpAgentClient); !ok {
		return nil, nil, errors.New("ACP connection with MCP message support is required for ACP-transport MCP servers")
	}

	allowed := make(map[string]struct{})
	for _, server := range servers {
		if server.Acp != nil && server.Acp.Id != "" {
			allowed[string(server.Acp.Id)] = struct{}{}
		}
	}

	bridge, err := a.newMCPSessionBridge(ctx, sessionID, allowed)
	if err != nil {
		return nil, nil, err
	}

	out := make([]acp.McpServer, 0, len(servers))
	for _, server := range servers {
		if server.Acp == nil {
			out = append(out, server)
			continue
		}
		out = append(out, acp.McpServer{Stdio: &acp.McpServerStdio{
			Name:    server.Acp.Name,
			Command: bridge.command,
			Args:    bridge.proxyArgs(string(server.Acp.Id)),
			Env: []acp.EnvVariable{
				{Name: MCPProxyTokenFileEnv, Value: bridge.tokenFile},
			},
		}})
	}

	return out, bridge, nil
}

func hasACPMCPServer(servers []acp.McpServer) bool {
	for _, server := range servers {
		if server.Acp != nil {
			return true
		}
	}

	return false
}

func (a *Agent) newMCPSessionBridge(ctx context.Context, sessionID acp.SessionId, allowed map[string]struct{}) (*mcpSessionBridge, error) {
	command := strings.TrimSpace(a.options.MCPProxyCommand)
	if command == "" {
		executable, err := currentExecutable()
		if err != nil {
			return nil, fmt.Errorf("resolve MCP proxy executable: %w", err)
		}
		command = executable
	}

	token, err := newMCPProxyToken()
	if err != nil {
		return nil, err
	}
	tokenFile, err := writeMCPProxyTokenFile(token)
	if err != nil {
		return nil, err
	}
	ln, err := mcpNetListen(mcpProxyNetwork, mcpProxyHost)
	if err != nil {
		_ = removePrivateTempFile(tokenFile, mcpTokenDirPrefix, os.Remove)
		return nil, fmt.Errorf("listen for MCP proxy: %w", err)
	}

	bridgeCtx, cancel := context.WithCancel(context.WithoutCancel(ctx))
	bridge := &mcpSessionBridge{
		agent:     a,
		session:   sessionID,
		token:     token,
		tokenFile: tokenFile,
		command:   command,
		args:      append([]string(nil), a.options.MCPProxyArgs...),
		allowed:   cloneMCPAllowedACPIDs(allowed),
		ln:        ln,
		cancel:    cancel,
		done:      make(chan struct{}),
		conns:     make(map[*mcpBridgeConn]struct{}),
		forwards:  make(chan struct{}, mcpMaxForwards),
	}

	bridge.wg.Add(1)
	go func() {
		defer recoverAgentGoroutine(bridgeCtx, a.log, "MCP bridge accept")
		defer bridge.wg.Done()
		bridge.accept(bridgeCtx)
	}()

	return bridge, nil
}

func cloneMCPAllowedACPIDs(sets ...map[string]struct{}) map[string]struct{} {
	allowed := make(map[string]struct{})
	for _, set := range sets {
		for id := range set {
			if id != "" {
				allowed[id] = struct{}{}
			}
		}
	}

	return allowed
}

func newMCPProxyToken() (string, error) {
	buf := make([]byte, mcpProxyTokenBytes)
	if _, err := io.ReadFull(mcpRandReader, buf); err != nil {
		return "", fmt.Errorf("generate MCP proxy token: %w", err)
	}

	return base64.RawURLEncoding.EncodeToString(buf), nil
}

func writeMCPProxyTokenFile(token string) (string, error) {
	file, err := mcpCreateTokenTemp()
	if err != nil {
		return "", fmt.Errorf("create MCP proxy token file: %w", err)
	}
	name := file.Name()
	if _, err := file.WriteString(token); err != nil {
		_ = file.Close()
		_ = removePrivateTempFile(name, mcpTokenDirPrefix, os.Remove)
		return "", fmt.Errorf("write MCP proxy token file: %w", err)
	}
	if err := file.Sync(); err != nil {
		_ = file.Close()
		_ = removePrivateTempFile(name, mcpTokenDirPrefix, os.Remove)
		return "", fmt.Errorf("sync MCP proxy token file: %w", err)
	}
	if err := file.Close(); err != nil {
		_ = removePrivateTempFile(name, mcpTokenDirPrefix, os.Remove)
		return "", fmt.Errorf("close MCP proxy token file: %w", err)
	}

	return name, nil
}

func (b *mcpSessionBridge) proxyArgs(acpID string) []string {
	args := append([]string(nil), b.args...)
	args = append(args,
		mcpProxySubcommand,
		"-network", b.ln.Addr().Network(),
		"-address", b.ln.Addr().String(),
		"-acp-id", acpID,
	)

	return args
}

func (b *mcpSessionBridge) accept(ctx context.Context) {
	for {
		conn, err := b.ln.Accept()
		if err != nil {
			select {
			case <-b.done:
				return
			default:
				b.agent.log.DebugContext(ctx, "accept MCP proxy connection failed", slog.String("error", err.Error()))
				return
			}
		}

		b.wg.Add(1)
		go func() {
			defer recoverAgentGoroutine(ctx, b.agent.log, "MCP bridge connection")
			defer b.wg.Done()
			b.handleConn(ctx, conn)
		}()
	}
}

func (b *mcpSessionBridge) handleConn(ctx context.Context, conn net.Conn) {
	_ = conn.SetReadDeadline(time.Now().Add(mcpProxyTimeout))
	dec := json.NewDecoder(io.LimitReader(conn, mcpProxyMaxBuf))

	var hello mcpProxyHello
	if err := dec.Decode(&hello); err != nil {
		_ = conn.Close()
		return
	}
	_ = conn.SetReadDeadline(time.Time{})

	if hello.Version != mcpProxyVersion || hello.ACPID == "" ||
		subtle.ConstantTimeCompare([]byte(hello.Token), []byte(b.token)) != 1 ||
		!b.allowsACPID(hello.ACPID) {
		_ = conn.Close()
		return
	}

	acpConn, ok := b.agent.connection().(mcpAgentClient)
	if !ok {
		_ = conn.Close()
		return
	}
	connectCtx, cancel := context.WithTimeout(ctx, mcpProxyTimeout)
	defer cancel()

	resp, err := acpConn.UnstableConnectMcp(connectCtx, acp.UnstableConnectMcpRequest{
		AcpId: acp.UnstableMcpServerAcpId(hello.ACPID),
	})
	if err != nil {
		_ = conn.Close()
		return
	}

	proxy := &mcpBridgeConn{
		agent:        b.agent,
		session:      b,
		conn:         conn,
		dec:          dec,
		enc:          json.NewEncoder(conn),
		connectionID: resp.ConnectionId,
		pending:      make(map[string]chan mcpRPCMessage),
		closed:       make(chan struct{}),
	}
	if !b.addConn(proxy) {
		proxy.Close()
		return
	}
	b.agent.registerMCPConnection(proxy)
	proxy.run(ctx)
}

func (b *mcpSessionBridge) allowsACPID(id string) bool {
	_, ok := b.allowed[id]
	return ok
}

func (b *mcpSessionBridge) addConn(conn *mcpBridgeConn) bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	select {
	case <-b.done:
		return false
	default:
	}
	if b.conns == nil {
		b.conns = make(map[*mcpBridgeConn]struct{})
	}
	if len(b.conns) >= mcpMaxConnections {
		return false
	}
	b.conns[conn] = struct{}{}

	return true
}

func (b *mcpSessionBridge) removeConn(conn *mcpBridgeConn) {
	b.mu.Lock()
	defer b.mu.Unlock()
	delete(b.conns, conn)
}

func (b *mcpSessionBridge) Close() error {
	var closeErr error
	b.closeOnce.Do(func() {
		close(b.done)
		if b.cancel != nil {
			b.cancel()
		}
		if b.ln != nil {
			closeErr = errors.Join(closeErr, b.ln.Close())
		}
		b.mu.Lock()
		conns := make([]*mcpBridgeConn, 0, len(b.conns))
		for conn := range b.conns {
			conns = append(conns, conn)
		}
		b.mu.Unlock()
		for _, conn := range conns {
			conn.Close()
		}
		b.wg.Wait()
		if b.tokenFile != "" {
			if err := removePrivateTempFile(b.tokenFile, mcpTokenDirPrefix, os.Remove); err != nil {
				closeErr = errors.Join(closeErr, err)
			}
		}
	})

	return closeErr
}

func (a *Agent) registerMCPConnection(conn *mcpBridgeConn) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.mcpConnections[conn.connectionID] = conn
}

func (a *Agent) unregisterMCPConnection(conn *mcpBridgeConn) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.mcpConnections[conn.connectionID] == conn {
		delete(a.mcpConnections, conn.connectionID)
	}
}

func (a *Agent) mcpConnection(connectionID acp.UnstableMcpConnectionId) *mcpBridgeConn {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.mcpConnections[connectionID]
}

func (c *mcpBridgeConn) run(ctx context.Context) {
	defer c.close(ctx)
	for {
		var msg mcpRPCMessage
		if err := c.dec.Decode(&msg); err != nil {
			return
		}
		switch {
		case msg.ID != nil && msg.Method == "":
			c.handleProxyResponse(msg)
		case msg.Method != "":
			c.forwardProxyMessage(ctx, msg)
		}
	}
}

func (c *mcpBridgeConn) handleProxyResponse(msg mcpRPCMessage) {
	id := mcpIDKey(msg.ID)
	c.mu.Lock()
	ch := c.pending[id]
	if ch != nil {
		delete(c.pending, id)
	}
	c.mu.Unlock()
	if ch != nil {
		ch <- msg
	}
}

func (c *mcpBridgeConn) forwardProxyMessage(ctx context.Context, msg mcpRPCMessage) {
	params, err := mcpParamsMap(msg.Params)
	if err != nil {
		if msg.ID != nil {
			_ = c.sendMCPError(*msg.ID, -32602, err.Error(), nil)
		}
		return
	}

	acpConn, ok := c.agent.connection().(mcpAgentClient)
	if !ok {
		if msg.ID != nil {
			_ = c.sendMCPError(*msg.ID, -32603, "ACP client is unavailable", nil)
		}
		return
	}
	release, err := c.acquireForward(ctx)
	if err != nil {
		if msg.ID != nil {
			_ = c.sendMCPError(*msg.ID, -32000, err.Error(), nil)
		}
		return
	}
	defer release()

	if msg.ID == nil {
		_ = acpConn.UnstableNotifyMcp(ctx, acp.UnstableMessageMcpNotification{
			ConnectionId: c.connectionID,
			Method:       msg.Method,
			Params:       params,
		})
		return
	}

	result, err := acpConn.UnstableMessageMcp(ctx, acp.UnstableMessageMcpRequest{
		ConnectionId: c.connectionID,
		Method:       msg.Method,
		Params:       params,
	})
	if err != nil {
		reqErr := requestError(err)
		_ = c.sendMCPError(*msg.ID, reqErr.Code, reqErr.Message, reqErr.Data)
		return
	}
	_ = c.sendMCPResult(*msg.ID, result)
}

func (c *mcpBridgeConn) acquireForward(ctx context.Context) (func(), error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if c.session == nil || c.session.forwards == nil {
		return func() {}, nil
	}

	select {
	case c.session.forwards <- struct{}{}:
		return func() { <-c.session.forwards }, nil
	default:
		return nil, errors.New("too many in-flight MCP proxy forwards")
	}
}

func (c *mcpBridgeConn) sendMCPResult(id json.RawMessage, result any) error {
	raw, err := json.Marshal(result)
	if err != nil {
		return err
	}
	if bytes.Equal(bytes.TrimSpace(raw), []byte("null")) {
		raw = nil
	}

	return c.send(mcpRPCMessage{ID: &id, Result: raw})
}

func (c *mcpBridgeConn) sendMCPError(id json.RawMessage, code int, message string, data any) error {
	var rawData json.RawMessage
	if data != nil {
		raw, err := json.Marshal(data)
		if err != nil {
			return err
		}
		rawData = raw
	}

	return c.send(mcpRPCMessage{
		ID: &id,
		Error: &mcpRPCError{
			Code:    code,
			Message: message,
			Data:    rawData,
		},
	})
}

func (c *mcpBridgeConn) send(msg mcpRPCMessage) error {
	msg.JSONRPC = mcpJSONRPCVersion
	c.writeMu.Lock()
	defer c.writeMu.Unlock()

	return c.enc.Encode(msg)
}

func (c *mcpBridgeConn) Close() {
	c.once.Do(func() {
		close(c.closed)
		_ = c.conn.Close()
		c.mu.Lock()
		for id, ch := range c.pending {
			close(ch)
			delete(c.pending, id)
		}
		c.mu.Unlock()
	})
}

func (c *mcpBridgeConn) close(ctx context.Context) {
	c.Close()
	c.session.removeConn(c)
	c.agent.unregisterMCPConnection(c)

	acpConn, ok := c.agent.connection().(mcpAgentClient)
	if !ok {
		return
	}
	disconnectCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), mcpProxyTimeout)
	defer cancel()
	_, _ = acpConn.UnstableDisconnectMcp(disconnectCtx, acp.UnstableDisconnectMcpRequest{
		ConnectionId: c.connectionID,
	})
}

func (a *Agent) handleMCPMessage(ctx context.Context, params json.RawMessage) (any, *acp.RequestError) {
	var request acp.UnstableMessageMcpRequest
	if err := json.Unmarshal(params, &request); err != nil {
		return nil, acp.NewInvalidParams(map[string]any{jsonFieldError: err.Error()})
	}
	if err := request.Validate(); err != nil {
		return nil, acp.NewInvalidParams(map[string]any{jsonFieldError: err.Error()})
	}

	conn := a.mcpConnection(request.ConnectionId)
	if conn == nil {
		return nil, acp.NewInvalidParams(map[string]any{"connectionId": request.ConnectionId})
	}
	if isMCPNotificationMethod(request.Method) {
		if err := conn.forwardACPNotification(ctx, request); err != nil {
			return nil, requestError(err)
		}
		return nil, nil
	}

	result, err := conn.forwardACPRequest(ctx, request)
	if err != nil {
		return nil, requestError(err)
	}

	return result, nil
}

func isMCPNotificationMethod(method string) bool {
	return strings.HasPrefix(method, "notifications/") || strings.HasPrefix(method, "$/")
}

func (c *mcpBridgeConn) forwardACPNotification(ctx context.Context, request acp.UnstableMessageMcpRequest) error {
	params, err := marshalMCPParams(request.Params)
	if err != nil {
		return err
	}
	if ctxErr := ctx.Err(); ctxErr != nil {
		return ctxErr
	}

	return c.send(mcpRPCMessage{Method: request.Method, Params: params})
}

func (c *mcpBridgeConn) forwardACPRequest(ctx context.Context, request acp.UnstableMessageMcpRequest) (acp.UnstableMessageMcpResponse, error) {
	params, err := marshalMCPParams(request.Params)
	if err != nil {
		return nil, err
	}
	if ctxErr := ctx.Err(); ctxErr != nil {
		return nil, ctxErr
	}

	requestCtx, cancel := context.WithTimeout(ctx, mcpProxyTimeout)
	defer cancel()

	id := c.nextID.Add(1)
	rawID := json.RawMessage(strconv.FormatUint(id, 10))
	idKey := mcpIDKey(&rawID)
	ch := make(chan mcpRPCMessage, 1)

	c.mu.Lock()
	if len(c.pending) >= mcpMaxPending {
		c.mu.Unlock()
		return nil, errors.New("too many pending MCP proxy requests")
	}
	c.pending[idKey] = ch
	c.mu.Unlock()

	if err := c.send(mcpRPCMessage{ID: &rawID, Method: request.Method, Params: params}); err != nil {
		c.mu.Lock()
		delete(c.pending, idKey)
		c.mu.Unlock()
		return nil, err
	}

	select {
	case msg, ok := <-ch:
		if !ok {
			return nil, errors.New("MCP proxy connection closed")
		}
		if msg.Error != nil {
			return nil, acp.NewInternalError(map[string]any{
				"code":         msg.Error.Code,
				"message":      msg.Error.Message,
				jsonFieldError: msg.Error.Message,
			})
		}
		return unmarshalMCPResult(msg.Result)
	case <-requestCtx.Done():
		c.mu.Lock()
		delete(c.pending, idKey)
		c.mu.Unlock()
		return nil, requestCtx.Err()
	case <-c.closed:
		c.mu.Lock()
		delete(c.pending, idKey)
		c.mu.Unlock()
		return nil, errors.New("MCP proxy connection closed")
	}
}

func mcpIDKey(id *json.RawMessage) string {
	if id == nil {
		return ""
	}

	return string(bytes.TrimSpace(*id))
}

func marshalMCPParams(params map[string]any) (json.RawMessage, error) {
	if params == nil {
		return json.RawMessage("null"), nil
	}

	return json.Marshal(params)
}

func mcpParamsMap(raw json.RawMessage) (map[string]any, error) {
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 || bytes.Equal(trimmed, []byte("null")) {
		return map[string]any{}, nil
	}

	var params map[string]any
	if err := json.Unmarshal(trimmed, &params); err != nil {
		return nil, fmt.Errorf("decode MCP params: %w", err)
	}

	return params, nil
}

func unmarshalMCPResult(raw json.RawMessage) (any, error) {
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 {
		return json.RawMessage("null"), nil
	}

	var result any
	if err := json.Unmarshal(trimmed, &result); err != nil {
		return nil, fmt.Errorf("decode MCP result: %w", err)
	}

	return result, nil
}

// RunMCPProxy runs the stdio shim launched by Codex for ACP-transport MCP.
func RunMCPProxy(ctx context.Context, stdin io.Reader, stdout io.Writer, options MCPProxyOptions) error {
	conn, err := mcpDialContext(ctx, options.Network, options.Address)
	if err != nil {
		return fmt.Errorf("connect MCP proxy bridge: %w", err)
	}
	defer conn.Close()

	if err := json.NewEncoder(conn).Encode(mcpProxyHello{
		Version: mcpProxyVersion,
		Token:   options.Token,
		ACPID:   options.ACPID,
	}); err != nil {
		return fmt.Errorf("send MCP proxy hello: %w", err)
	}

	errCh := make(chan error, 2)
	go proxyCopy(errCh, conn, stdin)
	go proxyCopy(errCh, stdout, conn)

	select {
	case <-ctx.Done():
		return ctx.Err()
	case err := <-errCh:
		if errors.Is(err, io.EOF) {
			return nil
		}
		return err
	}
}

func proxyCopy(errCh chan<- error, dst io.Writer, src io.Reader) {
	scanner := bufio.NewScanner(src)
	scanner.Buffer(make([]byte, 0, mcpProxyInitialBuf), mcpProxyMaxBuf)
	for scanner.Scan() {
		line := append([]byte(nil), scanner.Bytes()...)
		line = append(line, '\n')
		if _, err := dst.Write(line); err != nil {
			errCh <- err
			return
		}
	}
	if err := scanner.Err(); err != nil {
		errCh <- err
		return
	}
	errCh <- io.EOF
}
