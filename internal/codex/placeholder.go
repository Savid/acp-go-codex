package codex

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
)

const placeholderTurnID = "placeholder-turn"

// PlaceholderClient is a deterministic in-memory client used by unit tests and
// as an explicit fallback when the real app-server client is not requested.
type PlaceholderClient struct {
	options Options

	nextID  atomic.Uint64
	mu      sync.Mutex
	closed  bool
	threads map[string]Thread
	streams map[string]*threadStream
}

func NewPlaceholderClient(options Options) *PlaceholderClient {
	options.Env = cloneStringMap(options.Env)

	return &PlaceholderClient{
		options: options,
		threads: make(map[string]Thread),
		streams: make(map[string]*threadStream),
	}
}

func (c *PlaceholderClient) StartThread(ctx context.Context, req ThreadStartRequest) (Thread, error) {
	if err := ctx.Err(); err != nil {
		return Thread{}, err
	}

	c.mu.Lock()
	defer c.mu.Unlock()

	if c.closed {
		return Thread{}, errors.New("codex placeholder client is closed")
	}

	if c.threads == nil {
		c.threads = make(map[string]Thread)
	}

	if c.streams == nil {
		c.streams = make(map[string]*threadStream)
	}

	id := fmt.Sprintf("codex-thread-%d", c.nextID.Add(1))
	thread := Thread{
		ID:        id,
		SessionID: id,
		Cwd:       req.Cwd,
		Model:     firstNonEmpty(req.Model, c.options.DefaultModel),
		Provider:  valuePlaceholder,
		Title:     "Codex placeholder session",
	}
	c.threads[id] = thread
	c.streams[id] = newThreadStream(id)

	return thread, nil
}

func (c *PlaceholderClient) ResumeThread(ctx context.Context, req ThreadResumeRequest) (Thread, error) {
	if err := ctx.Err(); err != nil {
		return Thread{}, err
	}

	c.mu.Lock()
	defer c.mu.Unlock()

	if c.closed {
		return Thread{}, errors.New("codex placeholder client is closed")
	}

	if c.threads == nil {
		c.threads = make(map[string]Thread)
	}

	if c.streams == nil {
		c.streams = make(map[string]*threadStream)
	}

	id := firstNonEmpty(req.ThreadID, fmt.Sprintf("codex-thread-%d", c.nextID.Add(1)))
	thread := Thread{ID: id, SessionID: id, Cwd: req.Cwd, Model: firstNonEmpty(c.options.DefaultModel, valueDefault), Provider: valuePlaceholder, Title: "Codex placeholder session"}

	c.threads[id] = thread
	if c.streams[id] == nil {
		c.streams[id] = newThreadStream(id)
	}

	return thread, nil
}

func (c *PlaceholderClient) ForkThread(ctx context.Context, req ThreadForkRequest) (Thread, error) {
	if err := ctx.Err(); err != nil {
		return Thread{}, err
	}

	c.mu.Lock()
	_, ok := c.threads[req.ThreadID]
	c.mu.Unlock()

	if !ok {
		return Thread{}, ErrThreadNotFound
	}

	return c.StartThread(ctx, ThreadStartRequest{Cwd: req.Cwd})
}

func (c *PlaceholderClient) ListThreads(ctx context.Context, req ThreadListRequest) ([]Thread, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	c.mu.Lock()
	defer c.mu.Unlock()

	out := make([]Thread, 0, len(c.threads))
	for i := range c.threads {
		if req.Cwd != "" && c.threads[i].Cwd != req.Cwd {
			continue
		}

		out = append(out, c.threads[i])
	}

	return out, nil
}

func (c *PlaceholderClient) ReadThread(ctx context.Context, req ThreadReadRequest) (ThreadHistory, error) {
	if err := ctx.Err(); err != nil {
		return ThreadHistory{}, err
	}

	c.mu.Lock()
	thread, ok := c.threads[req.ThreadID]
	c.mu.Unlock()

	if !ok {
		return ThreadHistory{}, ErrThreadNotFound
	}

	return ThreadHistory{Thread: thread}, nil
}

func (c *PlaceholderClient) ListTurns(ctx context.Context, req ThreadTurnsListRequest) (ThreadTurnsListResponse, error) {
	if err := ctx.Err(); err != nil {
		return ThreadTurnsListResponse{}, err
	}

	c.mu.Lock()
	_, ok := c.threads[req.ThreadID]
	c.mu.Unlock()

	if !ok {
		return ThreadTurnsListResponse{}, ErrThreadNotFound
	}

	return ThreadTurnsListResponse{Turns: []map[string]any{{fieldID: placeholderTurnID}}}, nil
}

func (c *PlaceholderClient) RunTurn(ctx context.Context, req TurnStartRequest) (Turn, error) {
	if err := ctx.Err(); err != nil {
		return Turn{}, err
	}

	c.mu.Lock()
	_, ok := c.threads[req.ThreadID]
	stream := c.streams[req.ThreadID]
	closed := c.closed
	c.mu.Unlock()

	if closed {
		return Turn{}, errors.New("codex placeholder client is closed")
	}

	if !ok {
		return Turn{}, ErrThreadNotFound
	}

	if stream == nil || !stream.claimedAndLive() {
		return Turn{}, errors.New("codex placeholder turn requires a live claimed thread event stream")
	}

	go func() {
		defer recoverCodexGoroutine(ctx, "Codex placeholder turn")

		send := func(event Event) bool {
			event.Scope = EventScopeThread

			event.ThreadID = req.ThreadID
			if event.TurnID == "" {
				event.TurnID = placeholderTurnID
			}

			if ctx.Err() != nil {
				return false
			}

			return stream.send(event)
		}

		if !send(Event{Kind: EventPlanUpdated, Plan: []PlanStep{
			{Text: "Wire Codex app-server transport", Status: PlanStepCompleted},
			{Text: "Map Codex thread events to ACP updates", Status: PlanStepInProgress},
		}}) {
			return
		}

		if !send(Event{Kind: EventReasoningDelta, Text: "Using placeholder Codex provider boundary.\n"}) {
			return
		}

		if !send(Event{Kind: EventAgentMessageDelta, Text: "Codex ACP placeholder provider is running."}) {
			return
		}

		send(Event{Kind: EventCompleted, StopReason: StopReasonEndTurn})
	}()

	return Turn{ID: placeholderTurnID}, nil
}

func (c *PlaceholderClient) SubscribeThread(ctx context.Context, threadID string) (ThreadEventStream, error) {
	if err := ctx.Err(); err != nil {
		return ThreadEventStream{}, err
	}

	c.mu.Lock()

	stream := c.streams[threadID]
	if c.closed || stream == nil || !stream.claim() {
		c.mu.Unlock()

		return ThreadEventStream{}, errors.New("codex placeholder thread event stream is unavailable")
	}
	c.mu.Unlock()

	var once sync.Once

	release := func() {
		once.Do(func() {
			c.mu.Lock()
			if c.streams[threadID] == stream {
				delete(c.streams, threadID)
			}
			c.mu.Unlock()
			stream.stop()
		})
	}

	go func() {
		select {
		case <-ctx.Done():
			release()
		case <-stream.done:
		}
	}()

	return ThreadEventStream{Events: stream.out, Release: release}, nil
}

func (c *PlaceholderClient) SteerTurn(ctx context.Context, req TurnSteerRequest) error {
	if err := ctx.Err(); err != nil {
		return err
	}

	c.mu.Lock()
	_, ok := c.threads[req.ThreadID]
	c.mu.Unlock()

	if !ok {
		return ErrThreadNotFound
	}

	return nil
}

func (c *PlaceholderClient) CancelTurn(context.Context, string, string) error { return nil }

func (c *PlaceholderClient) CompactThread(ctx context.Context, req ThreadCompactRequest) (map[string]any, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	c.mu.Lock()
	_, ok := c.threads[req.ThreadID]
	c.mu.Unlock()

	if !ok {
		return nil, ErrThreadNotFound
	}

	return map[string]any{fieldThreadID: req.ThreadID, fieldStatus: "compacted"}, nil
}

func (c *PlaceholderClient) StartReview(ctx context.Context, req ReviewStartRequest) (map[string]any, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	c.mu.Lock()
	_, ok := c.threads[req.ThreadID]
	c.mu.Unlock()

	if !ok {
		return nil, ErrThreadNotFound
	}

	return map[string]any{fieldThreadID: req.ThreadID, "target": req.Target}, nil
}

func (c *PlaceholderClient) CollaborationModeList(context.Context) (CollaborationModeListResponse, error) {
	return CollaborationModeListResponse{
		Modes: []CollaborationMode{
			{ID: valueDefault, Name: "Default"},
			{ID: valuePlan, Name: "Plan"},
		},
		Raw: map[string]any{valuePlaceholder: true},
	}, nil
}

func (c *PlaceholderClient) MCPServerStatusList(context.Context, string) (MCPServerStatusListResponse, error) {
	return MCPServerStatusListResponse{Raw: map[string]any{valuePlaceholder: true}}, nil
}

func (c *PlaceholderClient) UnsubscribeThread(_ context.Context, threadID string) error {
	c.mu.Lock()
	stream := c.streams[threadID]
	delete(c.streams, threadID)
	c.mu.Unlock()

	if stream != nil {
		stream.stop()
	}

	return nil
}

func (c *PlaceholderClient) DeleteThread(_ context.Context, req ThreadDeleteRequest) error {
	if req.ThreadID == "" {
		return nil
	}

	c.mu.Lock()
	defer c.mu.Unlock()

	if _, ok := c.threads[req.ThreadID]; !ok {
		return ErrThreadNotFound
	}

	delete(c.threads, req.ThreadID)

	if stream := c.streams[req.ThreadID]; stream != nil {
		delete(c.streams, req.ThreadID)
		stream.stop()
	}

	return nil
}

func (c *PlaceholderClient) ModelList(context.Context) ([]Model, error) {
	model := firstNonEmpty(c.options.DefaultModel, valueDefault)

	return []Model{{ID: model, Name: model}}, nil
}

func (c *PlaceholderClient) AccountRead(context.Context) (Account, error) {
	return Account{Raw: map[string]any{fieldType: valuePlaceholder}}, nil
}

// ReadRateLimits returns an empty snapshot: the placeholder client never has
// harness-reported rate limits.
func (c *PlaceholderClient) ReadRateLimits(context.Context) (RateLimitSnapshot, error) {
	return RateLimitSnapshot{}, nil
}

func (c *PlaceholderClient) LoginWithChatGPTTokens(context.Context, ChatGPTAuthTokens) error {
	return nil
}

func (c *PlaceholderClient) Logout(context.Context) error { return nil }

func (c *PlaceholderClient) Close(context.Context) error {
	c.mu.Lock()
	defer c.mu.Unlock()

	c.closed = true

	c.threads = make(map[string]Thread)
	for id, stream := range c.streams {
		delete(c.streams, id)
		stream.stop()
	}

	return nil
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if value != "" {
			return value
		}
	}

	return ""
}

func cloneStringMap(values map[string]string) map[string]string {
	if values == nil {
		return nil
	}

	cloned := make(map[string]string, len(values))
	for key, value := range values {
		cloned[key] = value
	}

	return cloned
}
