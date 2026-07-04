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
		switch firstNonEmpty(stringFromAny(item["type"]), stringFromAny(item["kind"])) {
		case "userMessage", "user_message", "user":
			if text := firstNonEmpty(stringFromAny(item["text"]), stringFromAny(item["message"]), responseItemText(item)); text != "" {
				updates = append(updates, acp.UpdateUserMessageText(text))
			}
		case "agentMessage", "agent_message", "assistant", "message":
			role := stringFromAny(item["role"])
			text := firstNonEmpty(stringFromAny(item["text"]), stringFromAny(item["message"]), responseItemText(item))
			if text == "" {
				continue
			}
			if role == "user" {
				updates = append(updates, acp.UpdateUserMessageText(text))
			} else {
				updates = append(updates, acp.UpdateAgentMessageText(text))
			}
		case "reasoning", "agentReasoning", "agent_reasoning":
			if text := firstNonEmpty(stringFromAny(item["text"]), stringFromAny(item["summary"]), responseItemText(item)); text != "" {
				updates = append(updates, acp.UpdateAgentThoughtText(text))
			}
		case "commandExecution", "fileChange", "mcpToolCall", "dynamicToolCall", "function_call", "custom_tool_call":
			title := firstNonEmpty(stringFromAny(item["title"]), stringFromAny(item["name"]), stringFromAny(item["type"]))
			kind := toolKind(codex.ToolEvent{Kind: firstNonEmpty(stringFromAny(item["type"]), stringFromAny(item["kind"]))})
			update := replayToolStart(item, title, kind, acp.ToolCallStatusCompleted, item)
			if text := firstNonEmpty(stringFromAny(item["output"]), stringFromAny(item["result"]), stringFromAny(item["message"])); text != "" && update.ToolCall != nil {
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
		if row.Type != "event_msg" {
			continue
		}
		switch stringFromAny(row.Payload["type"]) {
		case "user_message":
			hasEventUser = true
		case "agent_message":
			hasEventAgent = true
		case "agent_reasoning", "agent_reasoning_raw_content":
			hasEventReasoning = true
		}
	}

	var updates []acp.SessionUpdate
	for _, row := range rows {
		switch row.Type {
		case "event_msg":
			updates = append(updates, replayEventMsg(row.Payload)...)
		case "response_item":
			updates = append(updates, replayResponseItem(row.Payload, replayFallbacks{
				messageUser:      !hasEventUser,
				messageAgent:     !hasEventAgent,
				messageReasoning: !hasEventReasoning,
			})...)
		case "compacted":
			if text := firstNonEmpty(stringFromAny(row.Payload["message"]), "Context compacted"); text != "" {
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
	switch stringFromAny(payload["type"]) {
	case "user_message":
		if text := stringFromAny(payload["message"]); text != "" {
			return []acp.SessionUpdate{acp.UpdateUserMessageText(text)}
		}
	case "agent_message":
		if text := stringFromAny(payload["message"]); text != "" {
			return []acp.SessionUpdate{acp.UpdateAgentMessageText(text)}
		}
	case "agent_reasoning", "agent_reasoning_raw_content":
		if text := firstNonEmpty(stringFromAny(payload["text"]), stringFromAny(payload["message"])); text != "" {
			return []acp.SessionUpdate{acp.UpdateAgentThoughtText(text)}
		}
	case "context_compacted":
		if text := firstNonEmpty(stringFromAny(payload["message"]), "Context compacted"); text != "" {
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
	switch stringFromAny(payload["type"]) {
	case "message":
		role := stringFromAny(payload["role"])
		text := responseItemText(payload)
		if text == "" {
			return nil
		}
		switch role {
		case "user":
			if fallbacks.messageUser {
				return []acp.SessionUpdate{acp.UpdateUserMessageText(text)}
			}
		case "assistant", "agent":
			if fallbacks.messageAgent {
				return []acp.SessionUpdate{acp.UpdateAgentMessageText(text)}
			}
		}
	case "reasoning":
		if !fallbacks.messageReasoning {
			return nil
		}
		if text := responseItemText(payload); text != "" {
			return []acp.SessionUpdate{acp.UpdateAgentThoughtText(text)}
		}
	case "function_call":
		return []acp.SessionUpdate{replayToolStart(payload, stringFromAny(payload["name"]), acp.ToolKindOther, acp.ToolCallStatusCompleted, payload["arguments"])}
	case "function_call_output":
		return replayToolOutput(payload)
	case "custom_tool_call":
		name := stringFromAny(payload["name"])
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
		title = firstNonEmpty(stringFromAny(payload["type"]), id)
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
	if action := mapFromAny(payload["action"]); action != nil {
		if exec := mapFromAny(action["exec"]); exec != nil {
			title = firstNonEmpty(commandText(exec["command"]), title)
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
	case "completed", "complete", "done", "succeeded", "success":
		return acp.ToolCallStatusCompleted
	case "failed", "error", "errored":
		return acp.ToolCallStatusFailed
	case "pending":
		return acp.ToolCallStatusPending
	default:
		return acp.ToolCallStatusInProgress
	}
}

func responseItemText(payload map[string]any) string {
	if text := firstNonEmpty(stringFromAny(payload["text"]), stringFromAny(payload["message"]), stringFromAny(payload["summary"])); text != "" {
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
			text.WriteString(firstNonEmpty(
				stringFromAny(typed["text"]),
				stringFromAny(typed["content"]),
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
