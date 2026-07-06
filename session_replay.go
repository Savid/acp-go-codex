package codexacp

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/coder/acp-go-sdk"
	"github.com/savid/acp-go-codex/internal/codex"
)

type rolloutRow struct {
	Type    string         `json:"type"`
	Payload map[string]any `json:"payload"`
}

func rolloutNativeThreadID(entries []SessionStoreEntry) string {
	for _, entry := range entries {
		row, err := decodeRolloutRow(entry)
		if err != nil || row.Type != "session_meta" {
			continue
		}

		if id := stringFromAny(row.Payload["id"]); id != "" {
			return id
		}
	}

	return ""
}

func (s *session) replayRollout(ctx context.Context, entries []SessionStoreEntry) error {
	updates, err := rolloutReplayUpdates(entries)
	if err != nil {
		return err
	}

	for _, update := range updates {
		if err := s.emitUpdates(ctx, update); err != nil {
			return err
		}
	}

	return nil
}

func (s *session) replayThreadHistory(ctx context.Context) error {
	if s.client == nil || s.codexThreadID == "" {
		return nil
	}

	history, err := s.client.ReadThread(ctx, codex.ThreadReadRequest{ThreadID: s.codexThreadID})
	if err != nil {
		return codexThreadACPError(err, s.accountMetaSnapshot(), codexThreadErrorData(s.id, s.codexThreadID))
	}

	for _, update := range threadHistoryReplayUpdates(history.Items) {
		if err := s.emitUpdates(ctx, update); err != nil {
			return err
		}
	}

	return nil
}

func threadHistoryReplayUpdates(items []map[string]any) []acp.SessionUpdate {
	updates := make([]acp.SessionUpdate, 0, len(items))
	for _, item := range items {
		switch firstNonEmpty(stringFromAny(item[jsonFieldType]), stringFromAny(item["kind"])) {
		case "userMessage", eventUserMessage, roleUser:
			if text := firstNonEmpty(stringFromAny(item[jsonFieldText]), stringFromAny(item[jsonFieldMessage]), responseItemText(item)); text != "" {
				updates = append(updates, acp.UpdateUserMessageText(text))
			}
		case "agentMessage", valueAgentMessage, roleAssistant, jsonFieldMessage:
			role := stringFromAny(item["role"])

			text := firstNonEmpty(stringFromAny(item[jsonFieldText]), stringFromAny(item[jsonFieldMessage]), responseItemText(item))
			if text == "" {
				continue
			}

			if role == roleUser {
				updates = append(updates, acp.UpdateUserMessageText(text))
			} else {
				updates = append(updates, acp.UpdateAgentMessageText(text))
			}
		case valueReasoning, "agentReasoning", valueAgentReasoning:
			if text := firstNonEmpty(stringFromAny(item[jsonFieldText]), stringFromAny(item["summary"]), responseItemText(item)); text != "" {
				updates = append(updates, acp.UpdateAgentThoughtText(text))
			}
		case toolKindCommandExecution, toolKindFileChange, toolKindMcpToolCall, "dynamicToolCall", "function_call", "custom_tool_call":
			title := firstNonEmpty(stringFromAny(item[jsonFieldTitle]), stringFromAny(item[jsonFieldName]), stringFromAny(item[jsonFieldType]))
			kind := toolKind(codex.ToolEvent{Kind: firstNonEmpty(stringFromAny(item[jsonFieldType]), stringFromAny(item["kind"]))})

			update := replayToolStart(item, title, kind, acp.ToolCallStatusCompleted, item)
			if text := firstNonEmpty(stringFromAny(item["output"]), stringFromAny(item["result"]), stringFromAny(item[jsonFieldMessage])); text != "" && update.ToolCall != nil {
				update.ToolCall.Content = []acp.ToolCallContent{textToolContent(text)}
			}

			updates = append(updates, update)
		}
	}

	return updates
}

func rolloutReplayUpdates(entries []SessionStoreEntry) ([]acp.SessionUpdate, error) {
	rows := make([]rolloutRow, 0, len(entries))
	hasEventUser := false
	hasEventAgent := false
	hasEventReasoning := false

	for index, entry := range entries {
		row, err := decodeRolloutRow(entry)
		if err != nil {
			return nil, acp.NewInvalidParams(map[string]any{jsonFieldEntries: map[string]any{jsonFieldIndex: index, jsonFieldError: err.Error()}})
		}

		rows = append(rows, row)
		if row.Type != valueEventMsg {
			continue
		}

		switch stringFromAny(row.Payload[jsonFieldType]) {
		case eventUserMessage:
			hasEventUser = true
		case valueAgentMessage:
			hasEventAgent = true
		case valueAgentReasoning, "agent_reasoning_raw_content":
			hasEventReasoning = true
		}
	}

	var updates []acp.SessionUpdate

	for _, row := range rows {
		switch row.Type {
		case valueEventMsg:
			updates = append(updates, replayEventMsg(row.Payload)...)
		case "response_item":
			updates = append(updates, replayResponseItem(row.Payload, replayFallbacks{
				messageUser:      !hasEventUser,
				messageAgent:     !hasEventAgent,
				messageReasoning: !hasEventReasoning,
			})...)
		case "compacted":
			if text := firstNonEmpty(stringFromAny(row.Payload[jsonFieldMessage]), "Context compacted"); text != "" {
				updates = append(updates, acp.UpdateAgentThoughtText(text))
			}
		}
	}

	return updates, nil
}

func decodeRolloutRow(entry SessionStoreEntry) (rolloutRow, error) {
	trimmed := bytes.TrimSpace(entry)
	if len(trimmed) == 0 {
		return rolloutRow{}, errors.New(validationRequired)
	}

	var row rolloutRow
	if err := json.Unmarshal(trimmed, &row); err != nil {
		return rolloutRow{}, err
	}

	if row.Type == "" {
		return rolloutRow{}, fmt.Errorf("type is required")
	}

	if row.Payload == nil {
		row.Payload = map[string]any{}
	}

	return row, nil
}

func replayEventMsg(payload map[string]any) []acp.SessionUpdate {
	switch stringFromAny(payload[jsonFieldType]) {
	case eventUserMessage:
		if text := stringFromAny(payload[jsonFieldMessage]); text != "" {
			return []acp.SessionUpdate{acp.UpdateUserMessageText(text)}
		}
	case valueAgentMessage:
		if text := stringFromAny(payload[jsonFieldMessage]); text != "" {
			return []acp.SessionUpdate{acp.UpdateAgentMessageText(text)}
		}
	case valueAgentReasoning, "agent_reasoning_raw_content":
		if text := firstNonEmpty(stringFromAny(payload[jsonFieldText]), stringFromAny(payload[jsonFieldMessage])); text != "" {
			return []acp.SessionUpdate{acp.UpdateAgentThoughtText(text)}
		}
	case "context_compacted":
		if text := firstNonEmpty(stringFromAny(payload[jsonFieldMessage]), "Context compacted"); text != "" {
			return []acp.SessionUpdate{acp.UpdateAgentThoughtText(text)}
		}
	}

	return nil
}

type replayFallbacks struct {
	messageUser      bool
	messageAgent     bool
	messageReasoning bool
}

func replayResponseItem(payload map[string]any, fallbacks replayFallbacks) []acp.SessionUpdate {
	switch stringFromAny(payload[jsonFieldType]) {
	case jsonFieldMessage:
		role := stringFromAny(payload["role"])

		text := responseItemText(payload)
		if text == "" {
			return nil
		}

		switch role {
		case roleUser:
			if fallbacks.messageUser {
				return []acp.SessionUpdate{acp.UpdateUserMessageText(text)}
			}
		case roleAssistant, roleAgent:
			if fallbacks.messageAgent {
				return []acp.SessionUpdate{acp.UpdateAgentMessageText(text)}
			}
		}
	case valueReasoning:
		if !fallbacks.messageReasoning {
			return nil
		}

		if text := responseItemText(payload); text != "" {
			return []acp.SessionUpdate{acp.UpdateAgentThoughtText(text)}
		}
	case "function_call":
		return []acp.SessionUpdate{replayToolStart(payload, stringFromAny(payload[jsonFieldName]), acp.ToolKindOther, acp.ToolCallStatusCompleted, payload["arguments"])}
	case "function_call_output":
		return replayToolOutput(payload)
	case "custom_tool_call":
		name := stringFromAny(payload[jsonFieldName])
		kind := acp.ToolKindOther
		content := []acp.ToolCallContent(nil)

		if name == "apply_patch" {
			kind = acp.ToolKindEdit

			if input := stringFromAny(payload["input"]); input != "" {
				content = []acp.ToolCallContent{textToolContent(input)}
			}
		}

		update := replayToolStart(payload, name, kind, acp.ToolCallStatusCompleted, payload["input"])
		if len(content) > 0 && update.ToolCall != nil {
			update.ToolCall.Content = content
		}

		return []acp.SessionUpdate{update}
	case "custom_tool_call_output":
		return replayToolOutput(payload)
	case "local_shell_call":
		return []acp.SessionUpdate{replayLocalShellCall(payload)}
	case "web_search_call":
		return []acp.SessionUpdate{replayToolStart(payload, "Web Search", acp.ToolKindSearch, acp.ToolCallStatusCompleted, payload)}
	case "image_generation_call":
		update := replayToolStart(payload, "Image generation", acp.ToolKindOther, replayStatus(stringFromAny(payload["status"])), payload)
		if text := stringFromAny(payload["revised_prompt"]); text != "" && update.ToolCall != nil {
			update.ToolCall.Content = []acp.ToolCallContent{textToolContent(text)}
		}

		return []acp.SessionUpdate{update}
	}

	return nil
}

func replayToolStart(payload map[string]any, title string, kind acp.ToolKind, status acp.ToolCallStatus, rawInput any) acp.SessionUpdate {
	id := firstNonEmpty(stringFromAny(payload["call_id"]), stringFromAny(payload["id"]), title, "codex-tool")
	if title == "" {
		title = firstNonEmpty(stringFromAny(payload[jsonFieldType]), id)
	}

	return acp.StartToolCall(
		acp.ToolCallId(id),
		title,
		acp.WithStartKind(kind),
		acp.WithStartStatus(status),
		acp.WithStartRawInput(rawInput),
	)
}

func replayToolOutput(payload map[string]any) []acp.SessionUpdate {
	id := firstNonEmpty(stringFromAny(payload["call_id"]), stringFromAny(payload["id"]))
	if id == "" {
		return nil
	}

	output := firstNonNil(payload["output"], payload["result"])

	opts := []acp.ToolCallUpdateOpt{
		acp.WithUpdateStatus(acp.ToolCallStatusCompleted),
		acp.WithUpdateRawOutput(output),
	}
	if text := textFromAny(output); text != "" {
		opts = append(opts, acp.WithUpdateContent([]acp.ToolCallContent{textToolContent(text)}))
	}

	return []acp.SessionUpdate{acp.UpdateToolCall(acp.ToolCallId(id), opts...)}
}

func replayLocalShellCall(payload map[string]any) acp.SessionUpdate {
	title := "Run command"

	if action := mapFromAny(payload[jsonFieldAction]); action != nil {
		if exec := mapFromAny(action["exec"]); exec != nil {
			title = firstNonEmpty(commandText(exec[valueCommand]), title)
		}
	}

	status := replayStatus(stringFromAny(payload["status"]))
	if status == acp.ToolCallStatusInProgress {
		status = acp.ToolCallStatusFailed
	}

	return replayToolStart(payload, title, acp.ToolKindExecute, status, payload)
}

func replayStatus(status string) acp.ToolCallStatus {
	switch strings.ToLower(status) {
	case "completed", "complete", statusDone, "succeeded", "success":
		return acp.ToolCallStatusCompleted
	case "failed", jsonFieldError, "errored":
		return acp.ToolCallStatusFailed
	case "pending":
		return acp.ToolCallStatusPending
	default:
		return acp.ToolCallStatusInProgress
	}
}

func responseItemText(payload map[string]any) string {
	if text := firstNonEmpty(stringFromAny(payload[jsonFieldText]), stringFromAny(payload[jsonFieldMessage]), stringFromAny(payload["summary"])); text != "" {
		return text
	}

	content, ok := payload[jsonFieldContent].([]any)
	if !ok {
		return ""
	}

	var text strings.Builder

	for _, item := range content {
		switch typed := item.(type) {
		case string:
			text.WriteString(typed)
		case map[string]any:
			text.WriteString(firstNonEmpty(
				stringFromAny(typed[jsonFieldText]),
				stringFromAny(typed[jsonFieldContent]),
				stringFromAny(typed["summary_text"]),
			))
		}
	}

	return text.String()
}

func commandText(value any) string {
	switch typed := value.(type) {
	case string:
		return typed
	case []any:
		parts := make([]string, 0, len(typed))
		for _, item := range typed {
			if part := stringFromAny(item); part != "" {
				parts = append(parts, part)
			}
		}

		return strings.Join(parts, " ")
	case []string:
		return strings.Join(typed, " ")
	default:
		return ""
	}
}

func textFromAny(value any) string {
	switch typed := value.(type) {
	case nil:
		return ""
	case string:
		return typed
	default:
		raw, err := json.Marshal(typed)
		if err != nil {
			return fmt.Sprint(typed)
		}

		return string(raw)
	}
}

func firstNonNil(values ...any) any {
	for _, value := range values {
		if value != nil {
			return value
		}
	}

	return nil
}
