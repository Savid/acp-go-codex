package codex

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os/exec"
	"strings"
	"sync"
	"time"
)

const (
	methodInitialize        = "initialize"
	methodInitialized       = "initialized"
	methodThreadStart       = "thread/start"
	methodThreadResume      = "thread/resume"
	methodThreadFork        = "thread/fork"
	methodThreadList        = "thread/list"
	methodThreadRead        = "thread/read"
	methodThreadTurnsList   = "thread/turns/list"
	methodThreadDelete      = "thread/delete"
	methodThreadUnsubscribe = "thread/unsubscribe"
	methodThreadCompact     = "thread/compact/start"
	methodTurnStart         = "turn/start"
	methodTurnSteer         = "turn/steer"
	methodTurnInterrupt     = "turn/interrupt"
	methodReviewStart       = "review/start"
	methodCollaborationList = "collaborationMode/list"
	methodMCPStatusList     = "mcpServerStatus/list"
	methodModelList         = "model/list"
	methodAccountRead       = "account/read"
	methodAccountLoginStart = "account/login/start"
	methodAccountLogout     = "account/logout"
)

type AppServerClient struct {
	options    Options
	rpc        *rpcConn
	cmd        *exec.Cmd
	procCancel context.CancelFunc

	eventPumpOnce sync.Once
	eventDone     chan struct{}

	mu      sync.Mutex
	closed  bool
	turns   map[*turnStream]struct{}
	account Account
}

var _ Client = (*AppServerClient)(nil)

const turnEventBuffer = 1024

type turnStream struct {
	cancel   context.CancelFunc
	closed   chan struct{}
	done     <-chan struct{}
	in       chan Event
	threadID string
	turnID   string
	out      chan Event
}

func NewAppServerClient(ctx context.Context, options Options) (*AppServerClient, error) {
	launchCtx := ctx
	var cancel context.CancelFunc
	if options.LaunchTimeout > 0 {
		launchCtx, cancel = context.WithTimeout(ctx, options.LaunchTimeout)
		defer cancel()
	}

	// The codex app-server process must outlive the request that created the
	// session: exec.CommandContext SIGKILLs the process when its context is
	// done, so the process is bound to a dedicated context that is only
	// cancelled by Close. The request ctx still bounds the launch handshake
	// (version check and initialize) below.
	procCtx, procCancel := context.WithCancel(context.Background())

	transport, cmd, err := launchAppServer(launchCtx, procCtx, options)
	if err != nil {
		procCancel()
		return nil, err
	}

	client := &AppServerClient{
		options:    options,
		rpc:        newRPCConn(transport, options.RequestHandler),
		cmd:        cmd,
		procCancel: procCancel,
	}
	client.ensureEventPump()

	if err := client.initialize(launchCtx); err != nil {
		_ = client.Close(context.Background())
		return nil, err
	}

	return client, nil
}

func (c *AppServerClient) initialize(ctx context.Context) error {
	var resp map[string]any
	if err := c.rpc.Call(ctx, methodInitialize, map[string]any{
		"clientInfo": map[string]any{
			"name":    "acp-go-codex",
			"title":   "acp-go-codex",
			"version": "0.1.0",
		},
		"capabilities": map[string]any{
			"experimentalApi": true,
		},
	}, &resp); err != nil {
		return err
	}

	return c.rpc.Notify(ctx, methodInitialized, map[string]any{})
}

func (c *AppServerClient) StartThread(ctx context.Context, req ThreadStartRequest) (Thread, error) {
	params := map[string]any{}
	setNonEmpty(params, "cwd", req.Cwd)
	setNonEmpty(params, "model", firstNonEmpty(req.Model, c.options.DefaultModel))
	setNonEmpty(params, "modelProvider", req.ModelProvider)
	setNonEmpty(params, "serviceTier", req.ServiceTier)
	setNonEmpty(params, "developerInstructions", req.DeveloperInstructions)
	setNonNil(params, "approvalPolicy", req.ApprovalPolicy)
	setNonNil(params, "sandbox", req.Sandbox)
	if len(req.AdditionalDirectories) > 0 {
		params["permissions"] = permissionProfile(req.AdditionalDirectories)
	}
	setNonNil(params, "personality", req.Personality)
	if req.Ephemeral != nil {
		params["ephemeral"] = *req.Ephemeral
	}
	if len(req.Config) > 0 {
		params["config"] = req.Config
	}
	var resp map[string]any
	if err := c.rpc.Call(ctx, methodThreadStart, params, &resp); err != nil {
		return Thread{}, err
	}

	return threadFromResponse(resp), nil
}

func (c *AppServerClient) ResumeThread(ctx context.Context, req ThreadResumeRequest) (Thread, error) {
	params := map[string]any{}
	setNonEmpty(params, "threadId", req.ThreadID)
	setNonEmpty(params, "path", req.Path)
	setNonEmpty(params, "cwd", req.Cwd)
	var resp map[string]any
	if err := c.rpc.Call(ctx, methodThreadResume, params, &resp); err != nil {
		return Thread{}, normalizeThreadError(err)
	}

	thread := threadFromResponse(resp)
	if thread.ID == "" {
		thread.ID = req.ThreadID
	}
	return thread, nil
}

func (c *AppServerClient) ForkThread(ctx context.Context, req ThreadForkRequest) (Thread, error) {
	params := map[string]any{"threadId": req.ThreadID}
	setNonEmpty(params, "cwd", req.Cwd)
	var resp map[string]any
	if err := c.rpc.Call(ctx, methodThreadFork, params, &resp); err != nil {
		return Thread{}, normalizeThreadError(err)
	}

	return threadFromResponse(resp), nil
}

func (c *AppServerClient) ListThreads(ctx context.Context, req ThreadListRequest) ([]Thread, error) {
	params := map[string]any{}
	setNonEmpty(params, "cwd", req.Cwd)

	var resp map[string]any
	if err := c.rpc.Call(ctx, methodThreadList, params, &resp); err != nil {
		return nil, err
	}

	return threadsFromResponse(resp), nil
}

func (c *AppServerClient) ReadThread(ctx context.Context, req ThreadReadRequest) (ThreadHistory, error) {
	var resp map[string]any
	if err := c.rpc.Call(ctx, methodThreadRead, map[string]any{"threadId": req.ThreadID}, &resp); err != nil {
		return ThreadHistory{}, normalizeThreadError(err)
	}

	return ThreadHistory{
		Thread: threadFromResponse(resp),
		Items:  mapSlice(resp, "items", "messages"),
		Raw:    resp,
	}, nil
}

func (c *AppServerClient) ListTurns(ctx context.Context, req ThreadTurnsListRequest) (ThreadTurnsListResponse, error) {
	params := map[string]any{"threadId": req.ThreadID}
	setNonEmpty(params, "cursor", req.Cursor)
	setNonEmpty(params, "sortDirection", req.SortDirection)
	if req.Limit > 0 {
		params["limit"] = req.Limit
	}

	var resp map[string]any
	if err := c.rpc.Call(ctx, methodThreadTurnsList, params, &resp); err != nil {
		return ThreadTurnsListResponse{}, normalizeThreadError(err)
	}

	return ThreadTurnsListResponse{
		Turns:      mapSlice(resp, "turns", "items", "data"),
		NextCursor: firstNonEmpty(stringValue(resp, "nextCursor"), stringValue(resp, "cursor")),
		Raw:        resp,
	}, nil
}

func (c *AppServerClient) RunTurn(ctx context.Context, req TurnStartRequest) (<-chan Event, error) {
	c.ensureEventPump()
	stream, err := c.registerTurn(ctx, req.ThreadID)
	if err != nil {
		return nil, err
	}

	params := map[string]any{
		"threadId": req.ThreadID,
		"input":    req.Prompt,
	}
	setNonEmpty(params, "model", req.Model)
	setNonEmpty(params, "serviceTier", req.ServiceTier)
	setNonEmpty(params, "effort", req.ReasoningEffort)
	setNonNil(params, "personality", req.Personality)
	setNonNil(params, "approvalPolicy", req.ApprovalPolicy)
	setNonNil(params, "sandboxPolicy", req.SandboxPolicy)
	setNonNil(params, "outputSchema", req.OutputSchema)
	setNonNil(params, "collaborationMode", req.CollaborationMode)

	var resp map[string]any
	if err := c.rpc.Call(ctx, methodTurnStart, params, &resp); err != nil {
		c.closeTurn(stream)
		return nil, normalizeThreadError(err)
	}

	turnID := stringValue(mapValue(resp, "turn"), "id")
	if turnID == "" {
		turnID = stringValue(resp, "turnId")
	}
	c.setTurnID(stream, turnID)

	return stream.out, nil
}

func (c *AppServerClient) SteerTurn(ctx context.Context, req TurnSteerRequest) error {
	params := map[string]any{
		"threadId":       req.ThreadID,
		"expectedTurnId": req.ExpectedTurnID,
		"input":          req.Input,
	}

	return normalizeThreadError(c.rpc.Call(ctx, methodTurnSteer, params, nil))
}

func (c *AppServerClient) CancelTurn(ctx context.Context, threadID string, turnID string) error {
	params := map[string]any{"threadId": threadID}
	setNonEmpty(params, "turnId", turnID)
	return normalizeThreadError(c.rpc.Call(ctx, methodTurnInterrupt, params, nil))
}

func (c *AppServerClient) CompactThread(ctx context.Context, req ThreadCompactRequest) (map[string]any, error) {
	var resp map[string]any
	if err := c.rpc.Call(ctx, methodThreadCompact, map[string]any{"threadId": req.ThreadID}, &resp); err != nil {
		return nil, normalizeThreadError(err)
	}

	return resp, nil
}

func (c *AppServerClient) StartReview(ctx context.Context, req ReviewStartRequest) (map[string]any, error) {
	params := map[string]any{
		"threadId": req.ThreadID,
		"target":   req.Target,
	}
	setNonEmpty(params, "delivery", req.Delivery)

	var resp map[string]any
	if err := c.rpc.Call(ctx, methodReviewStart, params, &resp); err != nil {
		return nil, normalizeThreadError(err)
	}

	return resp, nil
}

func (c *AppServerClient) CollaborationModeList(ctx context.Context) (CollaborationModeListResponse, error) {
	var resp map[string]any
	if err := c.rpc.Call(ctx, methodCollaborationList, map[string]any{}, &resp); err != nil {
		return CollaborationModeListResponse{}, err
	}

	raw := mapSlice(resp, "modes", "items", "data")
	modes := make([]CollaborationMode, 0, len(raw))
	for _, item := range raw {
		id := firstNonEmpty(stringValue(item, "id"), stringValue(item, "mode"), stringValue(item, "name"))
		if id == "" {
			continue
		}
		modes = append(modes, CollaborationMode{
			ID:   id,
			Name: firstNonEmpty(stringValue(item, "name"), id),
			Raw:  item,
		})
	}

	return CollaborationModeListResponse{Modes: modes, Raw: resp}, nil
}

func (c *AppServerClient) MCPServerStatusList(ctx context.Context) (MCPServerStatusListResponse, error) {
	var resp map[string]any
	if err := c.rpc.Call(ctx, methodMCPStatusList, map[string]any{}, &resp); err != nil {
		return MCPServerStatusListResponse{}, err
	}

	raw := mapSlice(resp, "servers", "items", "data")
	servers := make([]MCPServerStatus, 0, len(raw))
	for _, item := range raw {
		name := firstNonEmpty(stringValue(item, "name"), stringValue(item, "id"), stringValue(item, "server"))
		servers = append(servers, MCPServerStatus{
			Name:      name,
			Status:    firstNonEmpty(stringValue(item, "status"), stringValue(item, "state")),
			Tools:     mapSlice(item, "tools"),
			Resources: mapSlice(item, "resources"),
			Templates: firstNonEmptyMapSlice(item, "resourceTemplates", "templates"),
			Raw:       item,
		})
	}

	return MCPServerStatusListResponse{Servers: servers, Raw: resp}, nil
}

func (c *AppServerClient) UnsubscribeThread(ctx context.Context, threadID string) error {
	if threadID == "" {
		return nil
	}

	return normalizeThreadError(c.rpc.Call(ctx, methodThreadUnsubscribe, map[string]any{"threadId": threadID}, nil))
}

func (c *AppServerClient) DeleteThread(ctx context.Context, req ThreadDeleteRequest) error {
	if req.ThreadID == "" {
		return nil
	}

	return normalizeThreadError(c.rpc.Call(ctx, methodThreadDelete, map[string]any{"threadId": req.ThreadID}, nil))
}

func (c *AppServerClient) ModelList(ctx context.Context) ([]Model, error) {
	var resp map[string]any
	if err := c.rpc.Call(ctx, methodModelList, map[string]any{}, &resp); err != nil {
		return nil, err
	}

	raw := mapSlice(resp, "models", "items", "data")
	models := make([]Model, 0, len(raw))
	for _, item := range raw {
		id := firstNonEmpty(stringValue(item, "id"), stringValue(item, "model"), stringValue(item, "name"))
		if id == "" {
			continue
		}
		models = append(models, Model{
			ID:                     id,
			Name:                   firstNonEmpty(stringValue(item, "displayName"), stringValue(item, "name"), id),
			Description:            stringValue(item, "description"),
			Context:                int64Value(item, "contextWindow"),
			DefaultReasoningEffort: stringValue(item, "defaultReasoningEffort"),
			ReasoningEfforts:       modelReasoningEfforts(item),
			Raw:                    item,
		})
	}

	return models, nil
}

func modelReasoningEfforts(model map[string]any) []ModelReasoningEffort {
	raw := mapSlice(model, "supportedReasoningEfforts")
	efforts := make([]ModelReasoningEffort, 0, len(raw))
	for _, item := range raw {
		id := stringValue(item, "reasoningEffort")
		if id == "" {
			continue
		}
		efforts = append(efforts, ModelReasoningEffort{
			ID:          id,
			Description: stringValue(item, "description"),
			Raw:         item,
		})
	}

	return efforts
}

func (c *AppServerClient) AccountRead(ctx context.Context) (Account, error) {
	var resp map[string]any
	if err := c.rpc.Call(ctx, methodAccountRead, map[string]any{}, &resp); err != nil {
		return Account{}, err
	}

	account := accountFromResponse(resp)
	c.setAccount(account)

	return account, nil
}

func (c *AppServerClient) LoginWithChatGPTTokens(ctx context.Context, tokens ChatGPTAuthTokens) error {
	params := map[string]any{
		"type":             "chatgptAuthTokens",
		"accessToken":      tokens.AccessToken,
		"refreshToken":     tokens.RefreshToken,
		"chatgptAccountId": tokens.AccountID,
		"chatgptPlanType":  tokens.PlanType,
	}
	if tokens.ExpiresAtUnixSec != 0 {
		params["expiresAt"] = tokens.ExpiresAtUnixSec
	}

	return c.rpc.Call(ctx, methodAccountLoginStart, params, nil)
}

func (c *AppServerClient) Logout(ctx context.Context) error {
	return c.rpc.Call(ctx, methodAccountLogout, map[string]any{}, nil)
}

func (c *AppServerClient) Close(context.Context) error {
	c.mu.Lock()
	if c.closed {
		c.mu.Unlock()
		return nil
	}
	c.closed = true
	c.mu.Unlock()

	err := c.rpc.Close()
	if done := c.eventPumpDone(); done != nil {
		<-done
	} else {
		c.closeAllTurns()
	}
	if c.procCancel != nil {
		c.procCancel()
	}

	return err
}

func (c *AppServerClient) ensureEventPump() {
	c.eventPumpOnce.Do(func() {
		c.mu.Lock()
		if c.turns == nil {
			c.turns = make(map[*turnStream]struct{})
		}
		c.eventDone = make(chan struct{})
		done := c.eventDone
		c.mu.Unlock()

		go func() {
			defer recoverCodexGoroutine(context.Background(), "Codex event pump")
			c.runEventPump(done)
		}()
	})
}

func (c *AppServerClient) eventPumpDone() <-chan struct{} {
	c.mu.Lock()
	defer c.mu.Unlock()

	return c.eventDone
}

func (c *AppServerClient) runEventPump(done chan<- struct{}) {
	defer close(done)
	defer c.closeAllTurns()

	for notification := range c.rpc.Events() {
		c.dispatchEvent(eventFromRPC(notification))
	}
	if err := c.rpc.closeError(); err != nil {
		c.dispatchEvent(Event{Kind: EventError, Err: err})
	}
}

func (c *AppServerClient) dispatchEvent(event Event) {
	if event.Kind == EventAccountUpdated {
		c.setAccount(event.Account)
		if c.options.EventHandler != nil {
			c.options.EventHandler(context.Background(), event)
		}
	}

	streams := c.matchingTurns(event)
	for _, stream := range streams {
		if !stream.send(event) {
			c.removeTurn(stream)
			continue
		}
	}
}

func (c *AppServerClient) registerTurn(ctx context.Context, threadID string) (*turnStream, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	turnCtx, cancel := context.WithCancel(ctx)
	stream := &turnStream{
		cancel:   cancel,
		closed:   make(chan struct{}),
		done:     turnCtx.Done(),
		in:       make(chan Event),
		threadID: threadID,
		out:      make(chan Event, turnEventBuffer),
	}

	c.mu.Lock()
	if c.closed {
		c.mu.Unlock()
		stream.abort()
		return nil, errors.New("codex app-server client is closed")
	}
	if c.turns == nil {
		c.turns = make(map[*turnStream]struct{})
	}
	c.turns[stream] = struct{}{}
	c.mu.Unlock()

	go func() {
		defer recoverCodexGoroutine(ctx, "Codex turn stream forwarder")
		stream.forward()
	}()
	go func() {
		defer recoverCodexGoroutine(ctx, "Codex turn context watcher")
		c.closeTurnOnContext(stream)
	}()

	return stream, nil
}

func (c *AppServerClient) closeTurnOnContext(stream *turnStream) {
	<-stream.done
	c.removeTurn(stream)
}

func (c *AppServerClient) setTurnID(stream *turnStream, turnID string) {
	if turnID == "" {
		return
	}
	c.mu.Lock()
	stream.turnID = turnID
	c.mu.Unlock()
}

func (c *AppServerClient) matchingTurns(event Event) []*turnStream {
	c.mu.Lock()
	defer c.mu.Unlock()

	streams := make([]*turnStream, 0, len(c.turns))
	for stream := range c.turns {
		if event.ThreadID != "" && stream.threadID != "" && event.ThreadID != stream.threadID {
			continue
		}
		if event.TurnID != "" && stream.turnID != "" && event.TurnID != stream.turnID {
			continue
		}
		streams = append(streams, stream)
	}

	return streams
}

func (c *AppServerClient) closeTurn(stream *turnStream) {
	c.removeTurn(stream)
	stream.stop()
}

func (c *AppServerClient) removeTurn(stream *turnStream) {
	c.mu.Lock()
	delete(c.turns, stream)
	c.mu.Unlock()
}

func (c *AppServerClient) closeAllTurns() {
	c.mu.Lock()
	streams := make([]*turnStream, 0, len(c.turns))
	for stream := range c.turns {
		delete(c.turns, stream)
		streams = append(streams, stream)
	}
	c.mu.Unlock()

	for _, stream := range streams {
		stream.stop()
	}
}

func (c *AppServerClient) setAccount(account Account) {
	if account.ID == "" && account.Email == "" && account.PlanType == "" && len(account.Raw) == 0 {
		return
	}
	c.mu.Lock()
	c.account = account
	c.mu.Unlock()
}

func (s *turnStream) send(event Event) bool {
	select {
	case <-s.done:
		return false
	case <-s.closed:
		return false
	default:
	}

	select {
	case s.in <- event:
		return true
	case <-s.done:
		return false
	case <-s.closed:
		return false
	}
}

func (s *turnStream) forward() {
	defer close(s.closed)
	defer close(s.out)
	defer s.cancel()

	for {
		select {
		case <-s.done:
			return
		case event := <-s.in:
			select {
			case s.out <- event:
				if event.Kind == EventCompleted || event.Kind == EventError {
					return
				}
				continue
			default:
			}
			select {
			case s.out <- event:
			case <-s.done:
				return
			}
			if event.Kind == EventCompleted || event.Kind == EventError {
				return
			}
		}
	}
}

func (s *turnStream) stop() {
	s.cancel()
	<-s.closed
}

func (s *turnStream) abort() {
	s.cancel()
	close(s.out)
	close(s.closed)
}

func permissionProfile(additional []string) map[string]any {
	mods := make([]map[string]any, 0, len(additional))
	for _, path := range additional {
		if strings.TrimSpace(path) == "" {
			continue
		}
		mods = append(mods, map[string]any{"type": "additionalWritableRoot", "path": path})
	}

	return map[string]any{
		"type":          "profile",
		"id":            ":workspace",
		"modifications": mods,
	}
}

func threadFromResponse(resp map[string]any) Thread {
	rawThread := mapValue(resp, "thread")
	if rawThread == nil {
		rawThread = resp
	}

	return Thread{
		ID:              firstNonEmpty(stringValue(rawThread, "id"), stringValue(resp, "threadId")),
		SessionID:       firstNonEmpty(stringValue(rawThread, "sessionId"), stringValue(rawThread, "id"), stringValue(resp, "threadId")),
		Path:            stringValue(rawThread, "path"),
		Cwd:             firstNonEmpty(stringValue(resp, "cwd"), stringValue(rawThread, "cwd")),
		Model:           firstNonEmpty(stringValue(resp, "model"), stringValue(rawThread, "model")),
		Provider:        firstNonEmpty(stringValue(resp, "modelProvider"), stringValue(rawThread, "modelProvider")),
		ReasoningEffort: firstNonEmpty(stringValue(resp, "reasoningEffort"), stringValue(rawThread, "reasoningEffort")),
		Title:           firstNonEmpty(stringValue(rawThread, "name"), stringValue(rawThread, "title"), stringValue(rawThread, "preview")),
		UpdatedAt:       firstNonEmpty(timestampValue(rawThread, "updatedAt"), timestampValue(rawThread, "mtime")),
		Raw:             rawThread,
	}
}

func threadsFromResponse(resp map[string]any) []Thread {
	raw := mapSlice(resp, "threads", "items", "data")
	threads := make([]Thread, 0, len(raw))
	for _, item := range raw {
		threads = append(threads, threadFromResponse(item))
	}

	return threads
}

func eventFromRPC(raw rpcEvent) Event {
	event := Event{RawMethod: raw.Method, RawParams: append(json.RawMessage(nil), raw.Params...), RawJSON: raw.Raw}
	var params map[string]any
	if len(raw.Params) > 0 {
		_ = json.Unmarshal(raw.Params, &params)
	}
	event.ThreadID = stringValue(params, "threadId")
	event.TurnID = firstNonEmpty(stringValue(params, "turnId"), stringValue(mapValue(params, "turn"), "id"))
	event.ItemID = stringValue(params, "itemId")

	switch raw.Method {
	case "item/agentMessage/delta":
		event.Kind = EventAgentMessageDelta
		event.Text = stringValue(params, "delta")
	case "item/reasoning/textDelta", "item/reasoning/summaryTextDelta":
		event.Kind = EventReasoningDelta
		event.Text = stringValue(params, "delta")
	case "item/plan/delta", "turn/plan/updated":
		event.Kind = EventPlanUpdated
		event.Plan = planFromParams(params)
	case "item/started":
		event = startedItemEvent(event, params)
	case "item/completed":
		event = completedItemEvent(event, params)
	case "item/agentMessage/completed":
		event.Kind = EventAgentMessageDelta
		event.Text = firstNonEmpty(stringValue(params, "text"), stringValue(params, "content"), contentText(params["content"]))
		event.Completed = true
	case "item/reasoning/completed":
		event.Kind = EventReasoningDelta
		event.Text = firstNonEmpty(stringValue(params, "text"), stringValue(params, "summary"), stringValue(params, "content"), contentText(params["content"]))
		event.Completed = true
	case "enteredReviewMode":
		event.Kind = EventReasoningDelta
		event.Text = firstNonEmpty(stringValue(params, "message"), stringValue(params, "text"), "Entered review mode")
	case "exitedReviewMode":
		event.Kind = EventReasoningDelta
		event.Text = firstNonEmpty(stringValue(params, "message"), stringValue(params, "text"), "Exited review mode")
	case "command/exec/outputDelta", "process/outputDelta", "item/commandExecution/outputDelta", "item/fileChange/outputDelta":
		event.Kind = EventToolDelta
		event.Text = firstNonEmpty(stringValue(params, "delta"), stringValue(params, "text"))
		event.Tool = ToolEvent{ID: firstNonEmpty(event.ItemID, stringValue(params, "callId")), Content: event.Text}
	case "item/fileChange/patchUpdated", "turn/diff/updated":
		event.Kind = EventDiffUpdated
		event.Diff = firstNonEmpty(stringValue(params, "diff"), stringValue(params, "patch"))
	case "thread/tokenUsage/updated":
		event.Kind = EventUsageUpdated
		event.TokenUsage = tokenUsageFromParams(params)
		event.Usage = event.TokenUsage.Last
	case "turn/completed":
		event.Kind = EventCompleted
		event.StopReason = stopReasonFromTurn(mapValue(params, "turn"))
		event.Usage = usageFromParams(params)
	case "account/updated":
		event.Kind = EventAccountUpdated
		event.Account = accountFromResponse(params)
	case "warning", "guardianWarning", "deprecationNotice", "configWarning":
		event.Kind = EventWarning
		event.Text = firstNonEmpty(stringValue(params, "message"), stringValue(params, "text"))
	case "error":
		event.Kind = EventError
		event.Text = firstNonEmpty(stringValue(params, "message"), stringValue(params, "error"))
		event.Err = errors.New(event.Text)
	case "rawResponseItem/completed":
		event.Kind = EventRaw
	default:
		event.Kind = EventRaw
	}

	return event
}

func startedItemEvent(event Event, params map[string]any) Event {
	item := mapValue(params, "item")
	if item == nil {
		item = params
	}
	if !toolLikeItemType(stringValue(item, "type")) {
		event.Kind = EventRaw

		return event
	}

	event.Kind = EventToolStarted
	event.Tool = toolEventFromItem(params, "inProgress")

	return event
}

func accountFromResponse(resp map[string]any) Account {
	rawAccount := mapValue(resp, "account")
	if rawAccount == nil {
		rawAccount = resp
	}

	return Account{
		ID:       firstNonEmpty(stringValue(rawAccount, "chatgptAccountId"), stringValue(rawAccount, "id")),
		Email:    stringValue(rawAccount, "email"),
		PlanType: firstNonEmpty(stringValue(rawAccount, "chatgptPlanType"), stringValue(rawAccount, "planType")),
		Raw:      rawAccount,
	}
}

func completedItemEvent(event Event, params map[string]any) Event {
	item := mapValue(params, "item")
	if item == nil {
		item = params
	}
	switch stringValue(item, "type") {
	case "agentMessage", "agent_message", "message":
		event.Kind = EventAgentMessageDelta
		event.Text = firstNonEmpty(stringValue(item, "text"), stringValue(item, "content"), stringValue(item, "message"), contentText(item["content"]))
		event.Completed = true
	case "reasoning", "agentReasoning", "agent_reasoning":
		event.Kind = EventReasoningDelta
		event.Text = firstNonEmpty(stringValue(item, "text"), stringValue(item, "summary"), stringValue(item, "content"), contentText(item["content"]))
		event.Completed = true
	case "userMessage", "user_message", "user":
		event.Kind = EventRaw
	case "commandExecution", "fileChange", "mcpToolCall", "dynamicToolCall", "function_call", "custom_tool_call", "tool":
		event.Kind = EventToolCompleted
		event.Tool = toolEventFromItem(params, "completed")
	default:
		event.Kind = EventRaw
	}

	return event
}

func toolLikeItemType(itemType string) bool {
	switch itemType {
	case "commandExecution", "fileChange", "mcpToolCall", "dynamicToolCall", "function_call", "custom_tool_call", "tool":
		return true
	default:
		return false
	}
}

func planFromParams(params map[string]any) []PlanStep {
	rawItems := mapSlice(params, "plan", "items", "entries")
	if len(rawItems) == 0 {
		if text := stringValue(params, "delta"); text != "" {
			return []PlanStep{{Text: text, Status: PlanStepInProgress}}
		}
	}

	steps := make([]PlanStep, 0, len(rawItems))
	for _, item := range rawItems {
		steps = append(steps, PlanStep{
			Text:   firstNonEmpty(stringValue(item, "text"), stringValue(item, "content"), stringValue(item, "message")),
			Status: planStatusFromString(firstNonEmpty(stringValue(item, "status"), stringValue(item, "state"))),
		})
	}

	return steps
}

func toolEventFromItem(params map[string]any, status string) ToolEvent {
	item := mapValue(params, "item")
	if item == nil {
		item = params
	}
	title := toolTitleFromItem(item)
	id := firstNonEmpty(stringValue(item, "id"), stringValue(params, "itemId"))

	return ToolEvent{
		ID:        id,
		Title:     title,
		Kind:      firstNonEmpty(stringValue(item, "type"), "tool"),
		Status:    firstNonEmpty(stringValue(item, "status"), status),
		Locations: itemLocations(item),
		Content: firstNonEmpty(
			stringValue(item, "output"),
			stringValue(item, "result"),
			stringValue(item, "message"),
			contentText(item["output"]),
			contentText(item["result"]),
			contentText(item["content"]),
		),
		Raw: item,
	}
}

func toolTitleFromItem(item map[string]any) string {
	if title := firstNonEmpty(stringValue(item, "title"), stringValue(item, "name")); title != "" {
		return title
	}
	if server, tool := stringValue(item, "server"), stringValue(item, "tool"); server != "" && tool != "" {
		return server + " " + tool
	}
	if tool := stringValue(item, "tool"); tool != "" {
		return tool
	}

	return stringValue(item, "type")
}

func itemLocations(item map[string]any) []string {
	locations := stringSliceValue(item["locations"])
	locations = append(locations, stringSliceValue(item["files"])...)
	for _, key := range []string{"path", "filePath", "filename"} {
		if value := stringValue(item, key); value != "" {
			locations = append(locations, value)
		}
	}

	return locations
}

func stringSliceValue(value any) []string {
	switch typed := value.(type) {
	case []string:
		return append([]string(nil), typed...)
	case []any:
		out := make([]string, 0, len(typed))
		for _, item := range typed {
			switch value := item.(type) {
			case string:
				out = append(out, value)
			case map[string]any:
				if path := firstNonEmpty(stringValue(value, "path"), stringValue(value, "filePath")); path != "" {
					out = append(out, path)
				}
			}
		}
		return out
	default:
		return nil
	}
}

func contentText(value any) string {
	switch typed := value.(type) {
	case string:
		return typed
	case []any:
		var text strings.Builder
		for _, item := range typed {
			text.WriteString(contentText(item))
		}
		return text.String()
	case map[string]any:
		if text := firstNonEmpty(
			stringValue(typed, "text"),
			stringValue(typed, "content"),
			stringValue(typed, "summary"),
			stringValue(typed, "summary_text"),
		); text != "" {
			return text
		}
		if nested := contentText(typed["content"]); nested != "" {
			return nested
		}
		return contentText(typed["result"])
	default:
		return ""
	}
}

func stopReasonFromTurn(turn map[string]any) StopReason {
	status := strings.ToLower(firstNonEmpty(stringValue(turn, "status"), stringValue(turn, "stopReason")))
	switch status {
	case "interrupted", "cancelled", "canceled":
		return StopReasonCancelled
	case "failed", "errored", "error":
		return StopReasonError
	default:
		return StopReasonEndTurn
	}
}

func usageFromParams(params map[string]any) Usage {
	usage := mapValue(params, "usage")
	if usage == nil {
		usage = mapValue(mapValue(params, "turn"), "usage")
	}

	return usageFromMap(usage)
}

func tokenUsageFromParams(params map[string]any) TokenUsage {
	tokenUsage := mapValue(params, "tokenUsage")
	if tokenUsage == nil {
		tokenUsage = mapValue(params, "usage")
	}

	usage := TokenUsage{
		Last:               usageFromMap(mapValue(tokenUsage, "last")),
		Total:              usageFromMap(mapValue(tokenUsage, "total")),
		ModelContextWindow: int64Value(tokenUsage, "modelContextWindow"),
		Raw:                tokenUsage,
	}
	if isZeroUsage(usage.Last) {
		usage.Last = usageFromMap(tokenUsage)
	}
	if isZeroUsage(usage.Total) {
		usage.Total = usage.Last
	}

	return usage
}

func usageFromMap(usage map[string]any) Usage {
	input := firstInt64(usage, "inputTokens", "promptTokens", "input_tokens", "prompt_tokens")
	output := firstInt64(usage, "outputTokens", "completionTokens", "output_tokens", "completion_tokens")
	cachedRead := firstInt64(usage, "cachedReadTokens", "cacheReadInputTokens", "cachedInputTokens", "cache_read_input_tokens", "cached_input_tokens")
	cachedWrite := firstInt64(usage, "cachedWriteTokens", "cacheCreationInputTokens", "cacheWriteInputTokens", "cache_creation_input_tokens", "cache_write_input_tokens")
	reasoning := firstInt64(usage, "reasoningOutputTokens", "thoughtTokens", "reasoningTokens", "reasoning_output_tokens", "thought_tokens")
	total := firstInt64(usage, "totalTokens", "total_tokens")
	if total == 0 {
		total = input + output
	}

	return Usage{
		CachedReadTokens:      cachedRead,
		CachedWriteTokens:     cachedWrite,
		InputTokens:           input,
		OutputTokens:          output,
		ReasoningOutputTokens: reasoning,
		TotalTokens:           total,
	}
}

func isZeroUsage(usage Usage) bool {
	return usage.CachedReadTokens == 0 &&
		usage.CachedWriteTokens == 0 &&
		usage.InputTokens == 0 &&
		usage.OutputTokens == 0 &&
		usage.ReasoningOutputTokens == 0 &&
		usage.TotalTokens == 0
}

func planStatusFromString(status string) PlanStepStatus {
	switch strings.ToLower(status) {
	case "inprogress", "in_progress", "active", "running":
		return PlanStepInProgress
	case "completed", "complete", "done":
		return PlanStepCompleted
	default:
		return PlanStepPending
	}
}

func setNonEmpty(values map[string]any, key string, value string) {
	if value != "" {
		values[key] = value
	}
}

func setNonNil(values map[string]any, key string, value any) {
	if text, ok := value.(string); ok && text == "" {
		return
	}
	if value != nil {
		values[key] = value
	}
}

func mapValue(values map[string]any, key string) map[string]any {
	if values == nil {
		return nil
	}
	child, _ := values[key].(map[string]any)
	return child
}

func mapSlice(values map[string]any, keys ...string) []map[string]any {
	for _, key := range keys {
		raw, ok := values[key].([]any)
		if !ok {
			continue
		}
		out := make([]map[string]any, 0, len(raw))
		for _, item := range raw {
			if obj, ok := item.(map[string]any); ok {
				out = append(out, obj)
			}
		}
		return out
	}

	return nil
}

func firstNonEmptyMapSlice(values map[string]any, keys ...string) []map[string]any {
	for _, key := range keys {
		if raw := mapSlice(values, key); len(raw) > 0 {
			return raw
		}
	}

	return nil
}

func stringValue(values map[string]any, key string) string {
	if values == nil {
		return ""
	}
	switch value := values[key].(type) {
	case string:
		return value
	case fmt.Stringer:
		return value.String()
	default:
		return ""
	}
}

func int64Value(values map[string]any, key string) int64 {
	if values == nil {
		return 0
	}
	switch value := values[key].(type) {
	case float64:
		return int64(value)
	case int64:
		return value
	case int:
		return int64(value)
	default:
		return 0
	}
}

func timestampValue(values map[string]any, key string) string {
	if value := stringValue(values, key); value != "" {
		return value
	}
	seconds := int64Value(values, key)
	if seconds == 0 {
		return ""
	}

	return time.Unix(seconds, 0).UTC().Format(time.RFC3339)
}

func firstInt64(values map[string]any, keys ...string) int64 {
	for _, key := range keys {
		if value := int64Value(values, key); value != 0 {
			return value
		}
	}

	return 0
}
