package codexacp

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/coder/acp-go-sdk"

	"github.com/savid/acp-go-codex/internal/codex"
	"github.com/savid/acp-go-core/image"
)

// Rollout payload types the replay projects.
const (
	eventUserMessage   = "user_message"
	eventAgentMessage  = "agent_message"
	eventAgentReason   = "agent_reasoning"
	eventReasonRaw     = "agent_reasoning_raw_content"
	eventCompacted     = "context_compacted"
	itemMessage        = "message"
	itemReasoning      = "reasoning"
	itemFunctionCall   = "function_call"
	itemFunctionOutput = "function_call_output"
	itemCustomCall     = "custom_tool_call"
	itemCustomOutput   = "custom_tool_call_output"
	itemLocalShell     = "local_shell_call"
	itemWebSearch      = "web_search_call"
	itemImageGen       = "image_generation_call"

	compactedText = "Context compacted"
)

// replay delivers mirrored rollout rows as session updates in row order.
// Replay runs the same image validation as live emission; a stored artifact
// the adapter can no longer reproduce fails the whole load.
func (s *session) replay(ctx context.Context, rows [][]byte) error {
	limits := s.agent.options.ImageLimits.core()

	decoded := make([]codex.RolloutRow, 0, len(rows))
	hasEvents := replayEventKinds{}

	for _, row := range rows {
		parsed, err := codex.DecodeRow(row)
		if err != nil {
			continue
		}

		decoded = append(decoded, parsed)

		if parsed.Type == codex.RowTypeEventMsg {
			hasEvents.observe(payloadString(parsed.Payload, fieldType))
		}
	}

	for _, row := range decoded {
		updates, failure := replayRow(row, hasEvents, limits)
		if failure != nil {
			return s.agent.restoreRefused(ctx, s.id, failure)
		}

		if err := s.emit(ctx, updates...); err != nil {
			return err
		}
	}

	return nil
}

// replayEventKinds records which event_msg kinds the rollout carries, so the
// response_item copy of a message is replayed only when the event copy is
// absent.
type replayEventKinds struct {
	user, agent, reasoning bool
}

func (k *replayEventKinds) observe(kind string) {
	switch kind {
	case eventUserMessage:
		k.user = true
	case eventAgentMessage:
		k.agent = true
	case eventAgentReason, eventReasonRaw:
		k.reasoning = true
	}
}

func replayRow(row codex.RolloutRow, events replayEventKinds, limits image.Limits) ([]acp.SessionUpdate, *image.OutputError) {
	switch row.Type {
	case codex.RowTypeEventMsg:
		return replayEvent(row.Payload), nil
	case codex.RowTypeResponseItem:
		return replayResponseItem(row.Payload, events, limits)
	case codex.RowTypeCompacted:
		return []acp.SessionUpdate{acp.UpdateAgentThoughtText(firstNonEmpty(payloadString(row.Payload, "message"), compactedText))}, nil
	default:
		return nil, nil
	}
}

func replayEvent(payload map[string]any) []acp.SessionUpdate {
	switch payloadString(payload, fieldType) {
	case eventUserMessage:
		if text := payloadString(payload, "message"); text != "" {
			return []acp.SessionUpdate{acp.UpdateUserMessageText(text)}
		}
	case eventAgentMessage:
		if text := payloadString(payload, "message"); text != "" {
			return []acp.SessionUpdate{acp.UpdateAgentMessageText(text)}
		}
	case eventAgentReason, eventReasonRaw:
		if text := firstNonEmpty(payloadString(payload, "text"), payloadString(payload, "message")); text != "" {
			return []acp.SessionUpdate{acp.UpdateAgentThoughtText(text)}
		}
	case eventCompacted:
		return []acp.SessionUpdate{acp.UpdateAgentThoughtText(firstNonEmpty(payloadString(payload, "message"), compactedText))}
	}

	return nil
}

func replayResponseItem(payload map[string]any, events replayEventKinds, limits image.Limits) ([]acp.SessionUpdate, *image.OutputError) {
	switch payloadString(payload, fieldType) {
	case itemMessage:
		text := responseItemText(payload)
		if text == "" {
			return nil, nil
		}

		switch payloadString(payload, "role") {
		case "user":
			if !events.user {
				return []acp.SessionUpdate{acp.UpdateUserMessageText(text)}, nil
			}
		case "assistant":
			if !events.agent {
				return []acp.SessionUpdate{acp.UpdateAgentMessageText(text)}, nil
			}
		}
	case itemReasoning:
		if text := responseItemText(payload); text != "" && !events.reasoning {
			return []acp.SessionUpdate{acp.UpdateAgentThoughtText(text)}, nil
		}
	case itemFunctionCall:
		return []acp.SessionUpdate{replayToolStart(payload, payloadString(payload, "name"), acp.ToolKindOther, payload["arguments"])}, nil
	case itemCustomCall:
		name := payloadString(payload, "name")
		kind := acp.ToolKindOther

		if name == "apply_patch" {
			kind = acp.ToolKindEdit
		}

		return []acp.SessionUpdate{replayToolStart(payload, name, kind, payload["input"])}, nil
	case itemFunctionOutput, itemCustomOutput:
		return replayToolOutput(payload), nil
	case itemLocalShell:
		return []acp.SessionUpdate{replayToolStart(payload, "Run command", acp.ToolKindExecute, payload["action"])}, nil
	case itemWebSearch:
		return []acp.SessionUpdate{replayToolStart(payload, "Web search", acp.ToolKindSearch, payload)}, nil
	case itemImageGen:
		return replayImage(payload, limits)
	}

	return nil, nil
}

func replayToolStart(payload map[string]any, title string, kind acp.ToolKind, rawInput any) acp.SessionUpdate {
	id := firstNonEmpty(payloadString(payload, "call_id"), payloadString(payload, "id"), title)

	return acp.StartToolCall(acp.ToolCallId(id), title,
		acp.WithStartKind(kind), acp.WithStartStatus(acp.ToolCallStatusCompleted), acp.WithStartRawInput(rawInput))
}

func replayToolOutput(payload map[string]any) []acp.SessionUpdate {
	id := firstNonEmpty(payloadString(payload, "call_id"), payloadString(payload, "id"))
	if id == "" {
		return nil
	}

	output := payload["output"]
	opts := []acp.ToolCallUpdateOpt{acp.WithUpdateStatus(acp.ToolCallStatusCompleted), acp.WithUpdateRawOutput(output)}

	if text := textFromAny(output); text != "" {
		opts = append(opts, acp.WithUpdateContent([]acp.ToolCallContent{acp.ToolContent(acp.TextBlock(text))}))
	}

	return []acp.SessionUpdate{acp.UpdateToolCall(acp.ToolCallId(id), opts...)}
}

// replayImage decodes an image generation row through the output gate. The
// row carries the bytes, so replay reads nothing beyond the store.
func replayImage(payload map[string]any, limits image.Limits) ([]acp.SessionUpdate, *image.OutputError) {
	id := acp.ToolCallId(firstNonEmpty(payloadString(payload, "id"), payloadString(payload, "call_id"), "image"))
	start := acp.StartToolCall(id, imageToolTitle(itemTypeImageGeneration), acp.WithStartKind(acp.ToolKindOther), acp.WithStartStatus(acp.ToolCallStatusCompleted))

	result := payloadString(payload, nativeResultKey)
	if result == "" {
		return []acp.SessionUpdate{start}, nil
	}

	output, failure := decodeOutputImage(result, "", limits.EffectiveOutputPerImage())
	if failure != nil {
		if _, recoverable := failure.Guidance(); recoverable {
			return []acp.SessionUpdate{start}, nil
		}

		return nil, failure
	}

	return []acp.SessionUpdate{start, acp.UpdateToolCall(id, acp.WithUpdateContent([]acp.ToolCallContent{acp.ToolContent(acp.ImageBlock(output.data, output.mime))}))}, nil
}

func responseItemText(payload map[string]any) string {
	if text := firstNonEmpty(payloadString(payload, "text"), payloadString(payload, "summary")); text != "" {
		return text
	}

	content, ok := payload["content"].([]any)
	if !ok {
		return ""
	}

	var text strings.Builder

	for _, item := range content {
		switch typed := item.(type) {
		case string:
			text.WriteString(typed)
		case map[string]any:
			text.WriteString(firstNonEmpty(payloadString(typed, "text"), payloadString(typed, "summary_text")))
		}
	}

	return text.String()
}

func payloadString(payload map[string]any, key string) string {
	value, _ := payload[key].(string)

	return value
}

func textFromAny(value any) string {
	switch typed := value.(type) {
	case nil:
		return ""
	case string:
		return typed
	default:
		encoded, err := json.Marshal(typed)
		if err != nil {
			return fmt.Sprint(typed)
		}

		return string(encoded)
	}
}
