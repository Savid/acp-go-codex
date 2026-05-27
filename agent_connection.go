package codexacp

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
	"sync"
	"sync/atomic"

	"github.com/coder/acp-go-sdk"
)

const jsonFieldMethod = "method"

type agentClient interface {
	Done() <-chan struct{}
	UnstableCreateElicitation(context.Context, acp.UnstableCreateElicitationRequest) (acp.UnstableCreateElicitationResponse, error)
	UnstableConnectMcp(context.Context, acp.UnstableConnectMcpRequest) (acp.UnstableConnectMcpResponse, error)
	UnstableDisconnectMcp(context.Context, acp.UnstableDisconnectMcpRequest) (acp.UnstableDisconnectMcpResponse, error)
	RequestPermission(context.Context, acp.RequestPermissionRequest) (acp.RequestPermissionResponse, error)
	SessionUpdate(context.Context, acp.SessionNotification) error
	NotifyExtension(context.Context, string, any) error
}

type mcpAgentClient interface {
	UnstableConnectMcp(context.Context, acp.UnstableConnectMcpRequest) (acp.UnstableConnectMcpResponse, error)
	UnstableDisconnectMcp(context.Context, acp.UnstableDisconnectMcpRequest) (acp.UnstableDisconnectMcpResponse, error)
	UnstableMessageMcp(context.Context, acp.UnstableMessageMcpRequest) (acp.UnstableMessageMcpResponse, error)
	UnstableNotifyMcp(context.Context, acp.UnstableMessageMcpNotification) error
}

type localAgentConnection struct {
	agent       *Agent
	conn        *acp.Connection
	initialized atomic.Bool
}

type localAgentHandler func(context.Context, *Agent, json.RawMessage) (any, *acp.RequestError)

type localAgentParams[Req any] interface {
	*Req
	Validate() error
}

var (
	_ agentClient    = (*localAgentConnection)(nil)
	_ mcpAgentClient = (*localAgentConnection)(nil)

	localAgentHandlers = map[string]localAgentHandler{
		acp.AgentMethodAuthenticate:           localResponse((*Agent).Authenticate),
		acp.AgentMethodInitialize:             localResponse((*Agent).Initialize),
		acp.AgentMethodLogout:                 localResponse((*Agent).UnstableLogout),
		acp.AgentMethodSessionCancel:          localNotification((*Agent).Cancel),
		acp.AgentMethodSessionClose:           localResponse((*Agent).CloseSession),
		acp.AgentMethodSessionFork:            localResponse((*Agent).UnstableForkSession),
		acp.AgentMethodSessionList:            localResponse((*Agent).ListSessions),
		acp.AgentMethodSessionLoad:            localResponse((*Agent).LoadSession),
		acp.AgentMethodSessionNew:             localResponse((*Agent).NewSession),
		acp.AgentMethodSessionPrompt:          localResponse((*Agent).Prompt),
		acp.AgentMethodSessionResume:          localResponse((*Agent).ResumeSession),
		acp.AgentMethodSessionSetConfigOption: localResponse((*Agent).SetSessionConfigOption),
		acp.AgentMethodSessionSetMode:         localResponse((*Agent).SetSessionMode),
		acp.AgentMethodSessionSetModel:        localResponse((*Agent).UnstableSetSessionModel),
	}
)

func newLocalAgentConnection(agent *Agent, output io.Writer, input io.Reader) *localAgentConnection {
	conn := &localAgentConnection{agent: agent}
	inputGate := newConnectionInputGate(input)
	conn.conn = acp.NewConnection(conn.handle, output, inputGate)
	conn.conn.SetLogger(agent.log)
	inputGate.open()

	return conn
}

type connectionInputGate struct {
	reader io.Reader
	ready  chan struct{}
	once   sync.Once
}

func newConnectionInputGate(reader io.Reader) *connectionInputGate {
	return &connectionInputGate{reader: reader, ready: make(chan struct{})}
}

func (g *connectionInputGate) open() {
	g.once.Do(func() { close(g.ready) })
}

func (g *connectionInputGate) Read(p []byte) (int, error) {
	<-g.ready

	return g.reader.Read(p)
}

func (c *localAgentConnection) Done() <-chan struct{} {
	return c.conn.Done()
}

func (c *localAgentConnection) handle(ctx context.Context, method string, params json.RawMessage) (any, *acp.RequestError) {
	ctx, finish := c.agent.observe.StartACPRequest(ctx, method)
	var reqErr *acp.RequestError
	defer func() {
		if reqErr != nil {
			finish(reqErr)
		} else {
			finish(nil)
		}
	}()

	if method != acp.AgentMethodInitialize && !c.initialized.Load() {
		reqErr = acp.NewInvalidRequest(map[string]any{
			jsonFieldMethod: method,
			jsonFieldError:  "initialize must be called before other ACP methods",
		})
		return nil, reqErr
	}
	if strings.HasPrefix(method, "_") {
		result, err := c.agent.HandleExtensionMethod(ctx, method, params)
		reqErr = requestError(err)
		return result, reqErr
	}
	if method == acp.AgentMethodMcpMessage {
		result, err := c.agent.handleMCPMessage(ctx, params)
		reqErr = err
		return result, reqErr
	}

	handler, ok := localAgentHandlers[method]
	if !ok {
		reqErr = acp.NewMethodNotFound(method)
		return nil, reqErr
	}
	result, reqErr := handler(ctx, c.agent, params)
	if method == acp.AgentMethodInitialize && reqErr == nil {
		c.initialized.Store(true)
	}

	return result, reqErr
}

func localResponse[Req any, ReqPtr localAgentParams[Req], Resp any](
	call func(*Agent, context.Context, Req) (Resp, error),
) localAgentHandler {
	return func(ctx context.Context, agent *Agent, params json.RawMessage) (any, *acp.RequestError) {
		value, reqErr := decodeLocalAgentParams[Req, ReqPtr](params)
		if reqErr != nil {
			return nil, reqErr
		}
		resp, err := call(agent, ctx, value)
		if err != nil {
			return nil, requestError(err)
		}

		return resp, nil
	}
}

func localNotification[Req any, ReqPtr localAgentParams[Req]](
	call func(*Agent, context.Context, Req) error,
) localAgentHandler {
	return func(ctx context.Context, agent *Agent, params json.RawMessage) (any, *acp.RequestError) {
		value, reqErr := decodeLocalAgentParams[Req, ReqPtr](params)
		if reqErr != nil {
			return nil, reqErr
		}
		if err := call(agent, ctx, value); err != nil {
			return nil, requestError(err)
		}

		return nil, nil
	}
}

func decodeLocalAgentParams[Req any, ReqPtr localAgentParams[Req]](params json.RawMessage) (Req, *acp.RequestError) {
	var value Req
	if err := json.Unmarshal(params, &value); err != nil {
		return value, acp.NewInvalidParams(map[string]any{jsonFieldError: err.Error()})
	}
	if err := ReqPtr(&value).Validate(); err != nil {
		return value, acp.NewInvalidParams(map[string]any{jsonFieldError: err.Error()})
	}

	return value, nil
}

func (c *localAgentConnection) UnstableCreateElicitation(
	ctx context.Context,
	params acp.UnstableCreateElicitationRequest,
) (acp.UnstableCreateElicitationResponse, error) {
	return acp.SendRequest[acp.UnstableCreateElicitationResponse](c.conn, ctx, acp.ClientMethodElicitationCreate, params)
}

func (c *localAgentConnection) UnstableConnectMcp(
	ctx context.Context,
	params acp.UnstableConnectMcpRequest,
) (acp.UnstableConnectMcpResponse, error) {
	return acp.SendRequest[acp.UnstableConnectMcpResponse](c.conn, ctx, acp.ClientMethodMcpConnect, params)
}

func (c *localAgentConnection) UnstableDisconnectMcp(
	ctx context.Context,
	params acp.UnstableDisconnectMcpRequest,
) (acp.UnstableDisconnectMcpResponse, error) {
	return acp.SendRequest[acp.UnstableDisconnectMcpResponse](c.conn, ctx, acp.ClientMethodMcpDisconnect, params)
}

func (c *localAgentConnection) UnstableMessageMcp(
	ctx context.Context,
	params acp.UnstableMessageMcpRequest,
) (acp.UnstableMessageMcpResponse, error) {
	return acp.SendRequest[acp.UnstableMessageMcpResponse](c.conn, ctx, acp.ClientMethodMcpMessage, params)
}

func (c *localAgentConnection) UnstableNotifyMcp(
	ctx context.Context,
	params acp.UnstableMessageMcpNotification,
) error {
	return c.conn.SendNotification(ctx, acp.ClientMethodMcpMessage, params)
}

func (c *localAgentConnection) RequestPermission(
	ctx context.Context,
	params acp.RequestPermissionRequest,
) (acp.RequestPermissionResponse, error) {
	return acp.SendRequest[acp.RequestPermissionResponse](c.conn, ctx, acp.ClientMethodSessionRequestPermission, params)
}

func (c *localAgentConnection) SessionUpdate(ctx context.Context, params acp.SessionNotification) error {
	return c.conn.SendNotification(ctx, acp.ClientMethodSessionUpdate, params)
}

func (c *localAgentConnection) NotifyExtension(ctx context.Context, method string, params any) error {
	if method == "" || !strings.HasPrefix(method, "_") {
		return fmt.Errorf("extension method name must start with '_' (got %q)", method)
	}

	return c.conn.SendNotification(ctx, method, params)
}

func requestError(err error) *acp.RequestError {
	if err == nil {
		return nil
	}
	var reqErr *acp.RequestError
	if errors.As(err, &reqErr) {
		return reqErr
	}
	if errors.Is(err, context.Canceled) {
		return acp.NewRequestCancelled(map[string]any{jsonFieldError: err.Error()})
	}

	return acp.NewInternalError(map[string]any{jsonFieldError: err.Error()})
}
