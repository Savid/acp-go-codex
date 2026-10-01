package codexacp

import (
	"cmp"
	"context"
	"encoding/json"
	"strings"
	"sync"
	"time"

	"github.com/coder/acp-go-sdk"

	"github.com/savid/acp-go-codex/internal/codex"
	"github.com/savid/acp-go-core/wire"
)

const (
	planStatusInProgress = "inProgress"
	planStatusCompleted  = "completed"

	// Native item vocabulary the projections read.
	fieldType                = "type"
	nativeResultKey          = "result"
	itemTypeCommandExecution = "commandExecution"
	itemTypeFileChange       = "fileChange"
	itemTypeWebSearch        = "webSearch"
	itemTypeImageGeneration  = "imageGeneration"
	itemTypeImageView        = "imageView"
	itemStatusFailed         = "failed"
	itemStatusDeclined       = "declined"
)

// cycleState accumulates what one cycle streamed.
type cycleState struct {
	toolsMu sync.Mutex
	// usage sums the usage of every model request the cycle recorded.
	usage         *acp.Usage
	stop          codex.StopReason
	failure       *codex.TurnFailure
	imagesEmitted bool
	// agentText is the whole assistant text of the cycle, kept only for a
	// session with structured output.
	agentText strings.Builder
	// streamed holds what each open item already streamed, so its terminal
	// frame contributes only the suffix. A completed item leaves it; the last
	// one stays in completed so a repeated terminal frame adds nothing.
	streamed  map[string]string
	completed struct{ key, text string }
	// tools holds each tool call until the app-server completes its item.
	tools map[string]*toolState
}

// emit delivers session updates to the host. A session with no attached
// connection delivers nothing. Delivery never rides a request's cancellation.
func (s *session) emit(ctx context.Context, updates ...acp.SessionUpdate) error {
	conn := s.agent.connection()
	if conn == nil {
		return nil
	}

	ctx = context.WithoutCancel(ctx)

	for _, update := range updates {
		if err := conn.SessionUpdate(ctx, acp.SessionNotification{SessionId: s.id, Update: update}); err != nil {
			return err
		}
	}

	return nil
}

// projectEvent maps one native event of a cycle to session updates and
// reports whether the run settled. An update the host refused is returned so
// the cycle can record it, but the native run keeps draining to its terminal.
func (s *session) projectEvent(ctx context.Context, c *cycle, event codex.Event) (bool, error) {
	state := &c.state

	s.mu.Lock()
	if event.TurnID != "" && c.nativeTurnID == "" {
		c.nativeTurnID = event.TurnID
	}

	nativeTurnID := c.nativeTurnID
	cancelled := c.cancelled
	s.mu.Unlock()

	foreign := nativeTurnID != "" && event.TurnID != "" && event.TurnID != nativeTurnID

	switch event.Kind {
	case codex.EventUsageUpdated:
		if foreign {
			s.observeUsage(event.Usage)
		} else {
			s.emitTokenUsage(ctx, state, event.Usage, !cancelled)
		}

		return false, nil
	case codex.EventResponseCompleted:
		if !foreign {
			s.emitResponseCompleted(ctx, state, event.Response, !cancelled)
		}

		return false, nil
	}

	if foreign {
		return false, nil
	}

	if cancelled {
		return event.Kind == codex.EventTurnCompleted, nil
	}

	switch event.Kind {
	case codex.EventTurnCompleted:
		state.stop = event.Stop
		state.failure = event.Failure

		return true, nil
	case codex.EventError:
		return false, wire.TurnFailed(vendor, wire.TurnFailure{Cause: wire.CauseProvider, Message: event.Text})
	case codex.EventAgentMessageDelta:
		return false, s.emitText(ctx, state, event, false)
	case codex.EventReasoningDelta:
		return false, s.emitText(ctx, state, event, true)
	case codex.EventPlanUpdated:
		return false, s.emitPlan(ctx, event.Plan)
	case codex.EventToolStarted:
		return false, s.publishToolStart(ctx, state, event.Tool)
	case codex.EventToolDelta:
		return false, s.publishToolDelta(ctx, state, event.Tool.ID, event.Text)
	case codex.EventToolCompleted:
		return false, s.publishToolTerminal(ctx, state, event.Tool)
	case codex.EventImageStarted:
		return false, s.publishImageStart(ctx, state, event.Image)
	case codex.EventImageCompleted:
		return false, s.publishImageTerminal(ctx, state, event.Image)
	default:
		return false, nil
	}
}

// emitText projects agent or reasoning text as append-only deltas: a
// completed frame contributes only what its deltas did not carry.
func (s *session) emitText(ctx context.Context, state *cycleState, event codex.Event, thought bool) error {
	if state.streamed == nil {
		state.streamed = make(map[string]string)
	}

	key := event.ItemID
	if thought {
		key = "reasoning:" + key
	}

	text := event.Text

	switch {
	case event.Completed && key == state.completed.key:
		text = wire.UnstreamedSuffix(state.completed.text, text)
		state.completed.text += text
	case event.Completed:
		text = wire.UnstreamedSuffix(state.streamed[key], text)
		state.completed.key, state.completed.text = key, state.streamed[key]+text
		delete(state.streamed, key)
	default:
		state.streamed[key] += text
	}

	if text == "" {
		return nil
	}

	if thought {
		return s.emit(ctx, acp.UpdateAgentThoughtText(text))
	}

	if s.options.OutputSchema != nil {
		state.agentText.WriteString(text)
	}

	return s.emit(ctx, acp.UpdateAgentMessageText(text))
}

func (s *session) emitPlan(ctx context.Context, steps []codex.PlanStep) error {
	if len(steps) == 0 {
		return nil
	}

	entries := make([]acp.PlanEntry, 0, len(steps))

	for _, step := range steps {
		status := acp.PlanEntryStatusPending

		switch strings.ToLower(step.Status) {
		case strings.ToLower(planStatusInProgress), "in_progress", "active", "running":
			status = acp.PlanEntryStatusInProgress
		case planStatusCompleted, "complete", "done":
			status = acp.PlanEntryStatusCompleted
		}

		entries = append(entries, acp.PlanEntry{Content: step.Text, Priority: acp.PlanEntryPriorityMedium, Status: status})
	}

	return s.emit(ctx, acp.UpdatePlan(entries...))
}

// observeUsage records a usage report as the thread's last and returns the
// one it replaces.
func (s *session) observeUsage(usage codex.TokenUsage) codex.TokenUsage {
	s.mu.Lock()
	defer s.mu.Unlock()

	previous := s.usage
	s.usage = usage

	return previous
}

// restoreUsage takes the thread usage codex restores from the rollout when it
// resumes the thread as the last report, so the thread's next report is
// compared against it and a restatement of it reports nothing.
func (s *session) restoreUsage(rows [][]byte) {
	usage := codex.LastTokenUsage(rows)

	s.mu.Lock()
	s.usage = usage
	s.mu.Unlock()
}

// contextTokens is the context a model request leaves occupied, counted as
// codex counts it: the request's total tokens. After a compaction codex
// reports its estimate of the compacted history the same way.
func contextTokens(usage codex.Usage) (int, bool) {
	return int(usage.Total), usage.Total > 0
}

// callUsage is one model request's breakdown. Codex reports every figure for
// every request, and its input includes the input read from and written to a
// prompt cache, so the uncached input is what remains of it; a remainder
// below zero is no figure.
func callUsage(usage codex.Usage) wire.CallUsage {
	call := wire.CallUsage{
		CachedReadTokens:  new(int(usage.CachedRead)),
		CachedWriteTokens: new(int(usage.CacheWrite)),
		OutputTokens:      new(int(usage.Output)),
	}

	if uncached := usage.Input - usage.CachedRead - usage.CacheWrite; uncached >= 0 {
		call.InputTokens = new(int(uncached))
	}

	return call
}

// reportsResponses reports whether the bound thread reports each model
// response's usage as the response completes.
func (s *session) reportsResponses() bool {
	s.mu.Lock()
	defer s.mu.Unlock()

	return s.responseUsage
}

// recordRequest records one finished model request of the cycle: its usage
// joins the cycle's sum and, while the cycle is live, the update reports the
// context the request left occupied with the request's breakdown. A request
// whose usage reports no token is unknown and records nothing.
func (s *session) recordRequest(ctx context.Context, state *cycleState, usage codex.Usage, live bool) {
	call := callUsage(usage)
	if !call.Known() {
		return
	}

	state.usage = addUsage(state.usage, usage)

	used, ok := contextTokens(usage)
	if !live || !ok {
		return
	}

	update := &acp.SessionUsageUpdate{Size: s.knownContextWindow(), Used: used, Meta: call.Apply(nil)}
	_ = s.emit(ctx, acp.SessionUpdate{UsageUpdate: update})
}

// emitResponseCompleted records the model request a response completion
// reports, on a thread that reports its responses. It arrives as the
// response completes, before the tools the response started run.
func (s *session) emitResponseCompleted(ctx context.Context, state *cycleState, usage *codex.Usage, live bool) {
	if usage == nil || !s.reportsResponses() {
		return
	}

	s.recordRequest(ctx, state, *usage, live)
}

// emitTokenUsage handles one of the cycle's thread usage reports. Codex
// reports once per model request, after the tools that request started have
// finished. A report whose cumulative total moved records a new request,
// unless the thread already reported it at its completion; one that restates
// the previous report sends nothing; one that keeps the total and replaces
// the last usage carries codex's estimate of the history it just compacted,
// which replaces the figure and describes no request.
func (s *session) emitTokenUsage(ctx context.Context, state *cycleState, usage codex.TokenUsage, live bool) {
	previous := s.observeUsage(usage)

	if usage.Total != previous.Total {
		if !s.reportsResponses() {
			s.recordRequest(ctx, state, usage.Last, live)
		}

		return
	}

	used, ok := contextTokens(usage.Last)
	if !live || !ok || usage.Last == previous.Last {
		return
	}

	_ = s.emit(ctx, acp.SessionUpdate{UsageUpdate: &acp.SessionUsageUpdate{Size: s.knownContextWindow(), Used: used}})
}

// knownContextWindow is the selected model's context window: the gateway's
// model list's, else the one codex last reported, else 0. Codex reports the
// share of a known model's window it lets the conversation use, and a fixed
// fallback for a model it does not know, so a window the gateway states wins.
func (s *session) knownContextWindow() int {
	s.mu.Lock()
	defer s.mu.Unlock()

	return int(cmp.Or(s.contextWindow, s.usage.ModelContextWindow))
}

// addUsage adds one model request's usage to a cycle's sum.
func addUsage(sum *acp.Usage, usage codex.Usage) *acp.Usage {
	if sum == nil {
		sum = &acp.Usage{}
	}

	sum.InputTokens += int(usage.Input)
	sum.OutputTokens += int(usage.Output)
	sum.TotalTokens += int(usage.Total)
	sum.CachedReadTokens = addCount(sum.CachedReadTokens, usage.CachedRead)
	sum.CachedWriteTokens = addCount(sum.CachedWriteTokens, usage.CacheWrite)
	sum.ThoughtTokens = addCount(sum.ThoughtTokens, usage.Reasoning)

	return sum
}

// addCount adds an optional breakdown count, leaving it absent while every
// request reported none.
func addCount(sum *int, count int64) *int {
	if count <= 0 {
		return sum
	}

	if sum == nil {
		return new(int(count))
	}

	return new(*sum + int(count))
}

// emitSessionInfo records the turn's time and, on the first prompt, a title.
func (s *session) emitSessionInfo(ctx context.Context, prompt []acp.ContentBlock) {
	updatedAt := time.Now().UTC().Format(time.RFC3339)
	update := acp.SessionSessionInfoUpdate{UpdatedAt: &updatedAt}

	s.mu.Lock()
	s.updatedAt = updatedAt

	if s.title == "" {
		if title := wire.PromptTitle(prompt); title != "" {
			s.title = title
			update.Title = &title
		}
	}
	s.mu.Unlock()

	_ = s.emit(ctx, acp.SessionUpdate{SessionInfoUpdate: &update})
}

// emitRawEvent forwards one native record on the raw-event channel when the
// session opted in. Image payloads are replaced by their size so diagnostics
// never carry a second copy of the bytes.
func (s *session) emitRawEvent(ctx context.Context, event codex.Event) {
	if !s.rawEvents.Enabled() {
		return
	}

	conn := s.agent.connection()
	if conn == nil {
		return
	}

	payload := map[string]any{}
	if len(event.Params) > 0 {
		if err := json.Unmarshal(event.Params, &payload); err != nil {
			return
		}
	}

	redactImages(payload)
	payload["method"] = event.Method

	notify := func(ctx context.Context, method string, params map[string]any) error {
		return conn.NotifyExtension(ctx, method, params)
	}

	if err := s.rawEvents.Emit(ctx, notify, payload); err != nil {
		s.agent.observe.RecordRawMessageEmitFailure(ctx, err)
	}
}

// redactImages replaces image result payloads with their encoded size.
func redactImages(value any) {
	switch typed := value.(type) {
	case map[string]any:
		if result, ok := typed[nativeResultKey].(string); ok && result != "" && isImageItem(typed) {
			typed[nativeResultKey] = ""
			typed["resultBytes"] = len(result) / 4 * 3
		}

		for _, item := range typed {
			redactImages(item)
		}
	case []any:
		for _, item := range typed {
			redactImages(item)
		}
	}
}

func isImageItem(item map[string]any) bool {
	itemType, _ := item[fieldType].(string)

	return itemType == itemTypeImageGeneration || itemType == itemTypeImageView || itemType == itemImageGen
}
