package codex

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
)

// PlaceholderClient is a deterministic in-memory client used by unit tests and
// as an explicit fallback when the real app-server client is not requested.
type PlaceholderClient struct {
	options Options

	nextID  atomic.Uint64
	mu      sync.Mutex
	closed  bool
	threads map[string]Thread
	goals   map[string]Goal
}

func NewPlaceholderClient(options Options) *PlaceholderClient {
	options.Env = cloneStringMap(options.Env)

	return &PlaceholderClient{
		options: options,
		threads: make(map[string]Thread),
		goals:   make(map[string]Goal),
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

	id := fmt.Sprintf("codex-thread-%d", c.nextID.Add(1))
	thread := Thread{
		ID:        id,
		SessionID: id,
		Cwd:       req.Cwd,
		Model:     firstNonEmpty(req.Model, c.options.DefaultModel),
		Provider:  "placeholder",
		Title:     "Codex placeholder session",
	}
	c.threads[id] = thread
	if c.goals == nil {
		c.goals = make(map[string]Goal)
	}

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

	id := firstNonEmpty(req.ThreadID, fmt.Sprintf("codex-thread-%d", c.nextID.Add(1)))
	thread := Thread{ID: id, SessionID: id, Cwd: req.Cwd, Model: firstNonEmpty(c.options.DefaultModel, "default"), Provider: "placeholder", Title: "Codex placeholder session"}
	c.threads[id] = thread
	if c.goals == nil {
		c.goals = make(map[string]Goal)
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
	for _, thread := range c.threads {
		if req.Cwd != "" && thread.Cwd != req.Cwd {
			continue
		}
		out = append(out, thread)
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

	return ThreadTurnsListResponse{Turns: []map[string]any{{"id": "placeholder-turn"}}}, nil
}

func (c *PlaceholderClient) RunTurn(ctx context.Context, req TurnStartRequest) (<-chan Event, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	c.mu.Lock()
	_, ok := c.threads[req.ThreadID]
	closed := c.closed
	c.mu.Unlock()

	if closed {
		return nil, errors.New("codex placeholder client is closed")
	}
	if !ok {
		return nil, ErrThreadNotFound
	}

	events := make(chan Event)
	go func() {
		defer recoverCodexGoroutine(ctx, "Codex placeholder turn")
		defer close(events)

		send := func(event Event) bool {
			event.ThreadID = req.ThreadID
			if event.TurnID == "" {
				event.TurnID = "placeholder-turn"
			}
			select {
			case <-ctx.Done():
				return false
			case events <- event:
				return true
			}
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

	return events, nil
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

	return map[string]any{"threadId": req.ThreadID, "status": "compacted"}, nil
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

	return map[string]any{"threadId": req.ThreadID, "target": req.Target}, nil
}

func (c *PlaceholderClient) SetGoal(ctx context.Context, req GoalSetRequest) (Goal, error) {
	if err := ctx.Err(); err != nil {
		return Goal{}, err
	}

	c.mu.Lock()
	defer c.mu.Unlock()
	if _, ok := c.threads[req.ThreadID]; !ok {
		return Goal{}, ErrThreadNotFound
	}
	goal := Goal{
		ThreadID:    req.ThreadID,
		Objective:   req.Objective,
		Status:      firstNonEmpty(req.Status, "active"),
		TokenBudget: cloneInt64Ptr(req.TokenBudget),
	}
	if c.goals == nil {
		c.goals = make(map[string]Goal)
	}
	c.goals[req.ThreadID] = goal

	return goal, nil
}

func (c *PlaceholderClient) GetGoal(ctx context.Context, threadID string) (*Goal, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	c.mu.Lock()
	defer c.mu.Unlock()
	if _, ok := c.threads[threadID]; !ok {
		return nil, ErrThreadNotFound
	}
	goal, ok := c.goals[threadID]
	if !ok {
		//nolint:nilnil // nil goal with nil error is the GetGoal "not set" result.
		return nil, nil
	}
	goal.TokenBudget = cloneInt64Ptr(goal.TokenBudget)

	return &goal, nil
}

func (c *PlaceholderClient) ClearGoal(ctx context.Context, threadID string) (bool, error) {
	if err := ctx.Err(); err != nil {
		return false, err
	}

	c.mu.Lock()
	defer c.mu.Unlock()
	if _, ok := c.threads[threadID]; !ok {
		return false, ErrThreadNotFound
	}
	_, existed := c.goals[threadID]
	delete(c.goals, threadID)

	return existed, nil
}

func (c *PlaceholderClient) CollaborationModeList(context.Context) (CollaborationModeListResponse, error) {
	return CollaborationModeListResponse{
		Modes: []CollaborationMode{
			{ID: "default", Name: "Default"},
			{ID: "plan", Name: "Plan"},
		},
		Raw: map[string]any{"placeholder": true},
	}, nil
}

func (c *PlaceholderClient) MCPServerStatusList(context.Context) (MCPServerStatusListResponse, error) {
	return MCPServerStatusListResponse{Raw: map[string]any{"placeholder": true}}, nil
}

func (c *PlaceholderClient) UnsubscribeThread(context.Context, string) error { return nil }

func (c *PlaceholderClient) ModelList(context.Context) ([]Model, error) {
	model := firstNonEmpty(c.options.DefaultModel, "default")

	return []Model{{ID: model, Name: model}}, nil
}

func (c *PlaceholderClient) AccountRead(context.Context) (Account, error) {
	return Account{Raw: map[string]any{"type": "placeholder"}}, nil
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
	c.goals = make(map[string]Goal)

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

func cloneInt64Ptr(value *int64) *int64 {
	if value == nil {
		return nil
	}
	cloned := *value

	return &cloned
}
