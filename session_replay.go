package codexacp

import (
	"cmp"
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
func (s *session) replay(ctx context.Context, rows [][]byte, images []storedImage) error {
	limits := s.agent.options.ImageLimits.core()

	decoded := make([]codex.RolloutRow, 0, len(rows))
	hasEvents := replayEventKinds{}

	for _, row := range rows {
		parsed, err := codex.DecodeRow(row)
		if err != nil {
			return s.agent.restoreRefused(ctx, s.id, err)
		}

		decoded = append(decoded, parsed)

		if parsed.Type == codex.RowTypeEventMsg {
			hasEvents.observe(payloadString(parsed.Payload, fieldType))
		}
	}

	captured := make(map[string]storedImage, len(images))
	for _, saved := range images {
		if saved.ID == "" {
			return s.agent.restoreRefused(ctx, s.id, fmt.Errorf("stored image has no identity"))
		}

		if _, exists := captured[saved.ID]; exists {
			return s.agent.restoreRefused(ctx, s.id, fmt.Errorf("duplicate stored image identity"))
		}

		if _, failure := image.DecodeOutput(saved.Data, saved.MIME, limits.EffectiveOutputPerImage()); failure != nil {
			return s.agent.restoreRefused(ctx, s.id, failure)
		}

		captured[saved.ID] = saved
	}

	emitted := make(map[string]bool)

	for _, row := range decoded {
		id := cmp.Or(payloadString(row.Payload, "id"), payloadString(row.Payload, "call_id"))
		if row.Type == codex.RowTypeResponseItem && payloadString(row.Payload, fieldType) == itemImageGen {
			if saved, ok := captured[id]; ok {
				row.Payload[nativeResultKey] = saved.Data
				emitted[id] = true
			}
		}

		updates, failure := replayRow(row, hasEvents, limits)
		if failure != nil {
			return s.agent.restoreRefused(ctx, s.id, failure)
		}

		if err := s.emit(ctx, updates...); err != nil {
			return err
		}

		if saved, ok := captured[id]; ok && !emitted[id] && (payloadString(row.Payload, fieldType) == itemFunctionOutput || payloadString(row.Payload, fieldType) == itemCustomOutput) {
			if err := s.emitStoredImage(ctx, saved); err != nil {
				return err
			}

			emitted[id] = true
		}
	}

	for _, saved := range images {
		if !emitted[saved.ID] {
			if err := s.emitStoredImage(ctx, saved); err != nil {
				return err
			}
		}
	}

	return nil
}

// emitStoredImage restores an admitted artifact whose native row contains only a path.
func (s *session) emitStoredImage(ctx context.Context, saved storedImage) error {
	return s.emit(ctx,
		acp.StartToolCall(acp.ToolCallId(saved.ID), imageToolTitle(saved.Kind), acp.WithStartKind(acp.ToolKindOther), acp.WithStartStatus(acp.ToolCallStatusCompleted)),
		acp.UpdateToolCall(acp.ToolCallId(saved.ID), acp.WithUpdateStatus(acp.ToolCallStatusCompleted), acp.WithUpdateContent([]acp.ToolCallContent{acp.ToolContent(acp.ImageBlock(saved.Data, saved.MIME))})),
	)
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
		return []acp.SessionUpdate{acp.UpdateAgentThoughtText(cmp.Or(payloadString(row.Payload, "message"), compactedText))}, nil
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
		if text := cmp.Or(payloadString(payload, "text"), payloadString(payload, "message")); text != "" {
			return []acp.SessionUpdate{acp.UpdateAgentThoughtText(text)}
		}
	case eventCompacted:
		return []acp.SessionUpdate{acp.UpdateAgentThoughtText(cmp.Or(payloadString(payload, "message"), compactedText))}
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
	id := cmp.Or(payloadString(payload, "call_id"), payloadString(payload, "id"), title)

	return acp.StartToolCall(acp.ToolCallId(id), title,
		acp.WithStartKind(kind), acp.WithStartStatus(acp.ToolCallStatusCompleted), acp.WithStartRawInput(rawInput))
}

func replayToolOutput(payload map[string]any) []acp.SessionUpdate {
	id := cmp.Or(payloadString(payload, "call_id"), payloadString(payload, "id"))
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

// replayImage decodes an image generation row through the output gate. A row
// whose bytes the store never admitted replays the way the live turn reported
// it: a failed call carrying the same guidance text. Bytes that are present
// but invalid still fail the restore.
func replayImage(payload map[string]any, limits image.Limits) ([]acp.SessionUpdate, *image.OutputError) {
	id := acp.ToolCallId(cmp.Or(payloadString(payload, "id"), payloadString(payload, "call_id"), "image"))
	start := acp.StartToolCall(id, imageToolTitle(itemTypeImageGeneration), acp.WithStartKind(acp.ToolKindOther), acp.WithStartStatus(acp.ToolCallStatusCompleted))

	result := payloadString(payload, nativeResultKey)
	if result == "" {
		return []acp.SessionUpdate{
			start,
			acp.UpdateToolCall(id, acp.WithUpdateStatus(acp.ToolCallStatusFailed),
				acp.WithUpdateContent([]acp.ToolCallContent{acp.ToolContent(acp.TextBlock(image.GuidanceMissingFile))})),
		}, nil
	}

	output, failure := image.DecodeOutput(result, "", limits.EffectiveOutputPerImage())
	if failure != nil {
		return nil, failure
	}

	return []acp.SessionUpdate{start, acp.UpdateToolCall(id, acp.WithUpdateContent([]acp.ToolCallContent{acp.ToolContent(acp.ImageBlock(output.Data, output.MIME))}))}, nil
}

func responseItemText(payload map[string]any) string {
	if text := cmp.Or(payloadString(payload, "text"), payloadString(payload, "summary")); text != "" {
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
			text.WriteString(cmp.Or(payloadString(typed, "text"), payloadString(typed, "summary_text")))
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
