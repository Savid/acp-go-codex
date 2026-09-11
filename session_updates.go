package codexacp

import (
	"context"
	"encoding/json"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/coder/acp-go-sdk"

	"github.com/savid/acp-go-codex/internal/codex"
)

const (
	sessionTitleMaxRunes = 256

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
	usage         *acp.Usage
	stop          codex.StopReason
	failure       *codex.TurnFailure
	imagesEmitted bool
	// agentText is the whole assistant text of the cycle, for structured
	// output.
	agentText strings.Builder
	// streamed holds what each open item already streamed, so its terminal
	// frame contributes only the suffix.
	streamed map[string]string
	tools    map[string]*toolState
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

	if event.TurnID != "" && c.nativeTurnID == "" {
		c.nativeTurnID = event.TurnID
	}

	switch event.Kind {
	case codex.EventTurnCompleted:
		if c.nativeTurnID != "" && event.TurnID != "" && event.TurnID != c.nativeTurnID {
			return false, nil
		}

		state.stop = event.Stop
		state.failure = event.Failure

		return true, nil
	case codex.EventError:
		return false, turnFailure("provider", event.Text)
	case codex.EventAgentMessageDelta:
		return false, s.emitText(ctx, state, event, false)
	case codex.EventReasoningDelta:
		return false, s.emitText(ctx, state, event, true)
	case codex.EventPlanUpdated:
		return false, s.emitPlan(ctx, event.Plan)
	case codex.EventUsageUpdated:
		s.emitUsage(ctx, state, event.Usage)

		return false, nil
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

	if event.Completed {
		text = unstreamedSuffix(state.streamed[key], text)
		delete(state.streamed, key)
	} else {
		state.streamed[key] += text
	}

	if text == "" {
		return nil
	}

	if thought {
		return s.emit(ctx, acp.UpdateAgentThoughtText(text))
	}

	state.agentText.WriteString(text)

	return s.emit(ctx, acp.UpdateAgentMessageText(text))
}

// unstreamedSuffix reports the part of a terminal frame's text no delta of
// this item already carried. Text that diverges from the streamed prefix
// contributes nothing: the prefix is already with the client.
func unstreamedSuffix(streamed string, full string) string {
	if streamed == "" {
		return full
	}

	if !strings.HasPrefix(full, streamed) {
		return ""
	}

	return full[len(streamed):]
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

// emitUsage reports the latest model request's usage. size is the model's
// context window from the report, else the catalog value, else 0.
func (s *session) emitUsage(ctx context.Context, state *cycleState, usage codex.TokenUsage) {
	state.usage = acpUsage(usage.Last)

	size := usage.ModelContextWindow

	s.mu.Lock()
	if size == 0 {
		size = s.contextWindow
	} else {
		s.contextWindow = size
	}
	s.mu.Unlock()

	if state.usage == nil && size == 0 {
		return
	}

	used := 0
	if state.usage != nil {
		used = state.usage.TotalTokens
	}

	_ = s.emit(ctx, acp.SessionUpdate{UsageUpdate: &acp.SessionUsageUpdate{Size: int(size), Used: used}})
}

func acpUsage(usage codex.Usage) *acp.Usage {
	if usage == (codex.Usage{}) {
		return nil
	}

	result := &acp.Usage{InputTokens: int(usage.Input), OutputTokens: int(usage.Output), TotalTokens: int(usage.Total)}
	if usage.CachedRead > 0 {
		result.CachedReadTokens = new(int(usage.CachedRead))
	}

	if usage.Reasoning > 0 {
		result.ThoughtTokens = new(int(usage.Reasoning))
	}

	return result
}

// emitSessionInfo records the turn's time and, on the first prompt, a title.
func (s *session) emitSessionInfo(ctx context.Context, prompt []acp.ContentBlock) {
	updatedAt := time.Now().UTC().Format(time.RFC3339)
	update := acp.SessionSessionInfoUpdate{UpdatedAt: &updatedAt}

	s.mu.Lock()
	s.updatedAt = updatedAt

	if s.title == "" {
		if title := promptTitle(prompt); title != "" {
			s.title = title
			update.Title = &title
		}
	}
	s.mu.Unlock()

	_ = s.emit(ctx, acp.SessionUpdate{SessionInfoUpdate: &update})
}

func promptTitle(prompt []acp.ContentBlock) string {
	for _, block := range prompt {
		if block.Text == nil {
			continue
		}

		if title := normalizeTitle(block.Text.Text); title != "" {
			return title
		}
	}

	return ""
}

func normalizeTitle(text string) string {
	title := strings.Join(strings.Fields(text), " ")
	if utf8.RuneCountInString(title) <= sessionTitleMaxRunes {
		return title
	}

	runes := []rune(title)

	return strings.TrimSpace(string(runes[:sessionTitleMaxRunes-3])) + "..."
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
