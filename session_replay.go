package codexacp

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"path/filepath"
	"strings"

	"github.com/coder/acp-go-sdk"
	"github.com/savid/acp-go-codex/internal/codex"
)

type rolloutRow struct {
	Type    string         `json:"type"`
	Payload map[string]any `json:"payload"`
}

const valueAgentMessageCamel = "agentMessage"

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
	imageState := newImageToolState()

	updates, err := rolloutReplayUpdatesWithImages(entries, func(payload map[string]any) ([]acp.SessionUpdate, error) {
		image := rolloutImageEvent(payload)
		if reference, _ := payload[jsonFieldResult].(map[string]any); reference != nil {
			subpath := stringFromAny(reference[imageArtifactRefKey])

			artifact, loadErr := s.loadImageArtifact(ctx, subpath)
			if loadErr != nil {
				return nil, &imageOutputError{
					reason:  imageOutputStorageFailure,
					message: fmt.Sprintf("load image output for replay: %v", loadErr),
				}
			}

			image.Result = artifact.Data
		}

		return s.imageEventUpdates(ctx, codex.Event{
			Kind:  codex.EventImageCompleted,
			Image: image,
		}, &imageState)
	})
	if err != nil {
		return err
	}

	for _, update := range updates {
		if err := s.emitUpdates(ctx, update); err != nil {
			return err
		}
	}

	if identity := rolloutNativeTerminalIdentity(entries); nativeIdentityChanged(identity, nativeTurnIdentity{}) {
		if err := s.emitUpdatesWithNativeIdentity(ctx, identity, acp.SessionUpdate{
			SessionInfoUpdate: &acp.SessionSessionInfoUpdate{},
		}); err != nil {
			return err
		}
	}

	return nil
}

func rolloutReplayUpdates(entries []SessionStoreEntry) ([]acp.SessionUpdate, error) {
	return rolloutReplayUpdatesWithImages(entries, nil)
}

func rolloutReplayUpdatesWithImages(
	entries []SessionStoreEntry,
	imageUpdates func(map[string]any) ([]acp.SessionUpdate, error),
) ([]acp.SessionUpdate, error) {
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
		case valueResponseItem:
			if stringFromAny(row.Payload[jsonFieldType]) == valueImageGenerationCall && imageUpdates != nil {
				imageOutput, err := imageUpdates(row.Payload)
				if err != nil {
					return nil, err
				}

				updates = append(updates, imageOutput...)

				continue
			}

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

	if err := rejectDuplicateRolloutKeys(trimmed); err != nil {
		return rolloutRow{}, err
	}

	members, err := decodeExactJSONObject(trimmed)
	if err != nil {
		return rolloutRow{}, errors.New("rollout row must be one JSON object")
	}

	for name := range members {
		switch name {
		case "timestamp", jsonFieldType, "payload":
		default:
			return rolloutRow{}, fmt.Errorf("unknown rollout row field %q", name)
		}
	}

	var rowType string
	if rawType, ok := members[jsonFieldType]; !ok || json.Unmarshal(rawType, &rowType) != nil || rowType == "" {
		return rolloutRow{}, errors.New("type is required")
	}

	var payload map[string]any
	if rawPayload, ok := members["payload"]; !ok || json.Unmarshal(rawPayload, &payload) != nil || payload == nil {
		return rolloutRow{}, errors.New("payload must be an object")
	}

	if rawTimestamp, ok := members["timestamp"]; ok {
		var timestamp string
		if json.Unmarshal(rawTimestamp, &timestamp) != nil || timestamp == "" {
			return rolloutRow{}, errors.New("timestamp must be a non-empty string")
		}
	}

	return rolloutRow{Type: rowType, Payload: payload}, nil
}

func rejectDuplicateRolloutKeys(data []byte) error {
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.UseNumber()

	if err := consumeRolloutJSONValue(decoder); err != nil {
		return err
	}

	if err := decoder.Decode(new(any)); !errors.Is(err, io.EOF) {
		return errors.New("rollout row carries trailing input")
	}

	return nil
}

func consumeRolloutJSONValue(decoder *json.Decoder) error {
	token, err := decoder.Token()
	if err != nil {
		return err
	}

	delim, composite := token.(json.Delim)
	if !composite {
		return nil
	}

	switch delim {
	case '{':
		seen := make(map[string]struct{})

		for decoder.More() {
			keyToken, tokenErr := decoder.Token()
			if tokenErr != nil {
				return tokenErr
			}

			// encoding/json emits only string member names after an object opener.
			key, _ := keyToken.(string)

			if _, duplicate := seen[key]; duplicate {
				return fmt.Errorf("rollout object repeats field %q", key)
			}

			seen[key] = struct{}{}

			if valueErr := consumeRolloutJSONValue(decoder); valueErr != nil {
				return valueErr
			}
		}
	default: // encoding/json emits only an array opener for the other composite token.
		for decoder.More() {
			if valueErr := consumeRolloutJSONValue(decoder); valueErr != nil {
				return valueErr
			}
		}
	}

	// encoding/json matches the closing delimiter to the opener; malformed
	// input is reported by Token rather than returned as a mismatched token.
	if _, err := decoder.Token(); err != nil {
		return err
	}

	return nil
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
	case valueImageGenerationCall:
		return nil
	}

	return nil
}

func rolloutImageEvent(payload map[string]any) codex.ImageEvent {
	raw := make(map[string]any, len(payload))
	for key, value := range payload {
		switch key {
		case jsonFieldResult:
		case "saved_path", "savedPath":
			if path := stringFromAny(value); path != "" {
				raw["savedPath"] = filepath.Base(path)
			}
		default:
			raw[key] = value
		}
	}

	return codex.ImageEvent{
		ID:            firstNonEmpty(stringFromAny(payload["id"]), stringFromAny(payload["call_id"])),
		Kind:          valueImageGeneration,
		Status:        stringFromAny(payload["status"]),
		Result:        stringFromAny(payload[jsonFieldResult]),
		SavedPath:     firstNonEmpty(stringFromAny(payload["saved_path"]), stringFromAny(payload["savedPath"])),
		RevisedPrompt: firstNonEmpty(stringFromAny(payload["revised_prompt"]), stringFromAny(payload["revisedPrompt"])),
		Raw:           raw,
	}
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

	output := firstNonNil(payload["output"], payload[jsonFieldResult])

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
	case statusCompleted, "complete", statusDone, "succeeded", "success":
		return acp.ToolCallStatusCompleted
	case statusFailed, jsonFieldError, statusErrored:
		return acp.ToolCallStatusFailed
	case authStatePending:
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

func stringFromAny(value any) string {
	switch typed := value.(type) {
	case string:
		return typed
	case fmt.Stringer:
		return typed.String()
	default:
		return ""
	}
}

func mapFromAny(value any) map[string]any {
	switch typed := value.(type) {
	case map[string]any:
		return typed
	default:
		return nil
	}
}
