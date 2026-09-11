package codex

import (
	"encoding/json"
	"strings"
)

// EventKind classifies one app-server notification for the adapter.
type EventKind string

// The event kinds the adapter projects.
const (
	EventAgentMessageDelta EventKind = "agent_message_delta"
	EventReasoningDelta    EventKind = "reasoning_delta"
	EventPlanUpdated       EventKind = "plan_updated"
	EventToolStarted       EventKind = "tool_started"
	EventToolDelta         EventKind = "tool_delta"
	EventToolCompleted     EventKind = "tool_completed"
	EventImageStarted      EventKind = "image_started"
	EventImageCompleted    EventKind = "image_completed"
	EventDiffUpdated       EventKind = "diff_updated"
	EventUsageUpdated      EventKind = "usage_updated"
	EventTurnStarted       EventKind = "turn_started"
	EventTurnCompleted     EventKind = "turn_completed"
	EventError             EventKind = "error"
	EventRaw               EventKind = "raw"
)

// StopReason is how the app-server reported a turn ended.
type StopReason string

// The closed turn status vocabulary; anything else is an error.
const (
	StopReasonEndTurn   StopReason = "end_turn"
	StopReasonCancelled StopReason = "cancelled"
	StopReasonError     StopReason = "error"
)

// Notification method names the adapter decodes.
const (
	notifyAgentMessageDelta     = "item/agentMessage/delta"
	notifyAgentMessageCompleted = "item/agentMessage/completed"
	notifyReasoningDelta        = "item/reasoning/textDelta"
	notifyReasoningSummaryDelta = "item/reasoning/summaryTextDelta"
	notifyReasoningCompleted    = "item/reasoning/completed"
	notifyPlanDelta             = "item/plan/delta"
	notifyTurnPlanUpdated       = "turn/plan/updated"
	notifyItemStarted           = "item/started"
	notifyItemCompleted         = "item/completed"
	notifyCommandOutputDelta    = "item/commandExecution/outputDelta"
	notifyFileChangeOutputDelta = "item/fileChange/outputDelta"
	notifyPatchUpdated          = "item/fileChange/patchUpdated"
	notifyTurnDiffUpdated       = "turn/diff/updated"
	notifyTokenUsageUpdated     = "thread/tokenUsage/updated"
	notifyTurnStarted           = "turn/started"
	notifyTurnCompleted         = "turn/completed"
	notifyError                 = "error"

	itemTypeAgentMessage     = "agentMessage"
	itemTypeReasoning        = "reasoning"
	itemTypeUserMessage      = "userMessage"
	itemTypeCommandExecution = "commandExecution"
	itemTypeFileChange       = "fileChange"
	itemTypeMCPToolCall      = "mcpToolCall"
	itemTypeDynamicToolCall  = "dynamicToolCall"
	itemTypeImageGeneration  = "imageGeneration"
	itemTypeImageView        = "imageView"
	itemTypeWebSearch        = "webSearch"

	statusCompleted = "completed"
	statusFailed    = "failed"
)

// Event is one decoded app-server notification.
type Event struct {
	Kind     EventKind
	Method   string
	Params   json.RawMessage
	ThreadID string
	TurnID   string
	ItemID   string
	// Text carries a delta or, when Completed, the whole text of the item.
	Text      string
	Completed bool
	Diff      string
	Plan      []PlanStep
	Tool      ToolEvent
	Image     ImageEvent
	Usage     TokenUsage
	Stop      StopReason
	Failure   *TurnFailure
}

// PlanStep is one entry of a plan update.
type PlanStep struct {
	Text   string
	Status string
}

// ToolEvent is a tool-like item at start, delta, or completion.
type ToolEvent struct {
	ID      string
	Title   string
	Kind    string
	Status  string
	Content string
	Raw     map[string]any
}

// ImageEvent is an image-generation or image-view item.
type ImageEvent struct {
	ID        string
	Kind      string
	Status    string
	Result    string
	SavedPath string
	Raw       map[string]any
}

// Usage is one token usage block.
type Usage struct {
	Input      int64
	Output     int64
	CachedRead int64
	Reasoning  int64
	Total      int64
}

// TokenUsage is the thread/tokenUsage/updated payload.
type TokenUsage struct {
	Last               Usage
	Total              Usage
	ModelContextWindow int64
}

// TurnFailure is the provider failure a completed turn carried.
type TurnFailure struct {
	StatusCode   int
	ProviderCode string
	Message      string
}

// DecodeEvent classifies one notification. Every notification decodes; ones
// the adapter does not model are EventRaw.
func DecodeEvent(notification Notification) Event {
	event := Event{Kind: EventRaw, Method: notification.Method, Params: notification.Params}

	var params map[string]any
	if len(notification.Params) > 0 {
		_ = json.Unmarshal(notification.Params, &params)
	}

	event.ThreadID = stringValue(params, fieldThreadID)
	event.TurnID = firstNonEmpty(stringValue(params, fieldTurnID), stringValue(mapValue(params, "turn"), fieldID))
	event.ItemID = stringValue(params, fieldItemID)

	switch notification.Method {
	case notifyAgentMessageDelta:
		event.Kind = EventAgentMessageDelta
		event.Text = stringValue(params, "delta")
	case notifyAgentMessageCompleted:
		event.Kind = EventAgentMessageDelta
		event.Completed = true
		event.Text = firstNonEmpty(stringValue(params, "text"), contentText(params["content"]))
	case notifyReasoningDelta, notifyReasoningSummaryDelta:
		event.Kind = EventReasoningDelta
		event.Text = stringValue(params, "delta")
	case notifyReasoningCompleted:
		event.Kind = EventReasoningDelta
		event.Completed = true
		event.Text = firstNonEmpty(stringValue(params, "text"), stringValue(params, "summary"), contentText(params["content"]))
	case notifyPlanDelta, notifyTurnPlanUpdated:
		event.Kind = EventPlanUpdated
		event.Plan = planFromParams(params)
	case notifyItemStarted:
		startedItem(&event, params)
	case notifyItemCompleted:
		completedItem(&event, params)
	case notifyCommandOutputDelta, notifyFileChangeOutputDelta:
		event.Kind = EventToolDelta
		event.Text = firstNonEmpty(stringValue(params, "delta"), stringValue(params, "text"))
		event.Tool = ToolEvent{ID: event.ItemID, Content: event.Text}
	case notifyPatchUpdated, notifyTurnDiffUpdated:
		event.Kind = EventDiffUpdated
		event.Diff = firstNonEmpty(stringValue(params, "diff"), stringValue(params, "patch"))
	case notifyTokenUsageUpdated:
		event.Kind = EventUsageUpdated
		event.Usage = tokenUsageFromParams(params)
	case notifyTurnStarted:
		event.Kind = EventTurnStarted
	case notifyTurnCompleted:
		event.Kind = EventTurnCompleted
		turn := mapValue(params, "turn")
		event.Stop = stopReasonFromTurn(turn)

		if event.Stop == StopReasonError {
			event.Failure = turnFailure(turn, params)
		}
	case notifyError:
		// An error the app-server will retry leaves its turn live; its only
		// terminal is turn/completed.
		if willRetry, _ := params["willRetry"].(bool); !willRetry {
			event.Kind = EventError
			event.Text = firstNonEmpty(stringValue(params, fieldMessage), stringValue(mapValue(params, "error"), fieldMessage))
		}
	}

	return event
}

func startedItem(event *Event, params map[string]any) {
	item := mapValue(params, fieldItem)
	if item == nil {
		item = params
	}

	if event.ItemID == "" {
		event.ItemID = stringValue(item, fieldID)
	}

	switch itemType := stringValue(item, fieldType); {
	case itemType == itemTypeImageGeneration:
		event.Kind = EventImageStarted
		event.Image = imageFromItem(item)
	case toolLikeItem(itemType):
		event.Kind = EventToolStarted
		event.Tool = toolFromItem(item, "inProgress")
	}
}

func completedItem(event *Event, params map[string]any) {
	item := mapValue(params, fieldItem)
	if item == nil {
		item = params
	}

	if event.ItemID == "" {
		event.ItemID = stringValue(item, fieldID)
	}

	switch itemType := stringValue(item, fieldType); {
	case itemType == itemTypeAgentMessage:
		event.Kind = EventAgentMessageDelta
		event.Completed = true
		event.Text = firstNonEmpty(stringValue(item, "text"), contentText(item["content"]))
	case itemType == itemTypeReasoning:
		event.Kind = EventReasoningDelta
		event.Completed = true
		event.Text = firstNonEmpty(stringValue(item, "text"), stringValue(item, "summary"), contentText(item["content"]))
	case itemType == itemTypeImageGeneration, itemType == itemTypeImageView:
		event.Kind = EventImageCompleted
		event.Image = imageFromItem(item)
	case toolLikeItem(itemType):
		event.Kind = EventToolCompleted
		event.Tool = toolFromItem(item, statusCompleted)
	}
}

func toolLikeItem(itemType string) bool {
	switch itemType {
	case itemTypeCommandExecution, itemTypeFileChange, itemTypeMCPToolCall, itemTypeDynamicToolCall, itemTypeWebSearch:
		return true
	default:
		return false
	}
}

func toolFromItem(item map[string]any, status string) ToolEvent {
	return ToolEvent{
		ID:     stringValue(item, fieldID),
		Title:  toolTitle(item),
		Kind:   stringValue(item, fieldType),
		Status: firstNonEmpty(stringValue(item, fieldStatus), status),
		Content: firstNonEmpty(
			stringValue(item, "aggregatedOutput"),
			stringValue(item, "output"),
			stringValue(item, fieldResult),
			contentText(item["output"]),
			contentText(item[fieldResult]),
		),
		Raw: item,
	}
}

func toolTitle(item map[string]any) string {
	switch stringValue(item, fieldType) {
	case itemTypeCommandExecution:
		return firstNonEmpty(commandText(item["command"]), "Run command")
	case itemTypeFileChange:
		return "Apply file changes"
	case itemTypeMCPToolCall, itemTypeDynamicToolCall:
		if server, tool := stringValue(item, "server"), stringValue(item, "tool"); server != "" && tool != "" {
			return server + " " + tool
		}

		return firstNonEmpty(stringValue(item, "tool"), stringValue(item, fieldName), stringValue(item, fieldType))
	case itemTypeWebSearch:
		return firstNonEmpty(stringValue(item, "query"), "Web search")
	default:
		return firstNonEmpty(stringValue(item, "title"), stringValue(item, fieldName), stringValue(item, fieldType))
	}
}

func commandText(value any) string {
	switch typed := value.(type) {
	case string:
		return typed
	case []any:
		parts := make([]string, 0, len(typed))

		for _, item := range typed {
			if text, ok := item.(string); ok && text != "" {
				parts = append(parts, text)
			}
		}

		return strings.Join(parts, " ")
	default:
		return ""
	}
}

func imageFromItem(item map[string]any) ImageEvent {
	return ImageEvent{
		ID:        stringValue(item, fieldID),
		Kind:      stringValue(item, fieldType),
		Status:    stringValue(item, fieldStatus),
		Result:    stringValue(item, fieldResult),
		SavedPath: firstNonEmpty(stringValue(item, "savedPath"), stringValue(item, fieldPath)),
		Raw:       item,
	}
}

func planFromParams(params map[string]any) []PlanStep {
	raw := mapSlice(params, "plan", "items", "entries")
	if len(raw) == 0 {
		if text := stringValue(params, "delta"); text != "" {
			return []PlanStep{{Text: text, Status: "inProgress"}}
		}
	}

	steps := make([]PlanStep, 0, len(raw))

	for _, item := range raw {
		steps = append(steps, PlanStep{
			Text:   firstNonEmpty(stringValue(item, "text"), stringValue(item, "step"), stringValue(item, "content")),
			Status: stringValue(item, fieldStatus),
		})
	}

	return steps
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
		return firstNonEmpty(stringValue(typed, "text"), stringValue(typed, "summary_text"), contentText(typed["content"]))
	default:
		return ""
	}
}

func stopReasonFromTurn(turn map[string]any) StopReason {
	switch strings.ToLower(stringValue(turn, fieldStatus)) {
	case statusCompleted, "done":
		return StopReasonEndTurn
	case "interrupted", "cancelled", "canceled":
		return StopReasonCancelled
	default:
		return StopReasonError
	}
}

func turnFailure(turn map[string]any, params map[string]any) *TurnFailure {
	errInfo := mapValue(turn, "error")
	if errInfo == nil {
		errInfo = mapValue(params, "error")
	}

	codexInfo := mapValue(errInfo, "codexErrorInfo")

	return &TurnFailure{
		StatusCode:   int(int64Value(codexInfo, "httpStatusCode")),
		ProviderCode: firstNonEmpty(stringValue(codexInfo, "code"), stringValue(codexInfo, fieldType), stringValue(errInfo, "code")),
		Message:      firstNonEmpty(stringValue(errInfo, fieldMessage), stringValue(turn, fieldStatus), "codex turn failed"),
	}
}

func tokenUsageFromParams(params map[string]any) TokenUsage {
	raw := mapValue(params, "tokenUsage")
	if raw == nil {
		raw = mapValue(params, "usage")
	}

	usage := TokenUsage{
		Last:               usageFromMap(mapValue(raw, "last")),
		Total:              usageFromMap(mapValue(raw, "total")),
		ModelContextWindow: int64Value(raw, "modelContextWindow"),
	}
	if usage.Last == (Usage{}) {
		usage.Last = usageFromMap(raw)
	}

	if usage.Total == (Usage{}) {
		usage.Total = usage.Last
	}

	return usage
}

func usageFromMap(raw map[string]any) Usage {
	usage := Usage{
		Input:      int64Value(raw, "inputTokens"),
		Output:     int64Value(raw, "outputTokens"),
		CachedRead: int64Value(raw, "cachedInputTokens"),
		Reasoning:  int64Value(raw, "reasoningOutputTokens"),
		Total:      int64Value(raw, "totalTokens"),
	}
	if usage.Total == 0 {
		usage.Total = usage.Input + usage.Output
	}

	return usage
}
