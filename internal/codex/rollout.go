package codex

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"
)

// Rollout row types the adapter reads.
const (
	RowTypeSessionMeta  = "session_meta"
	RowTypeEventMsg     = "event_msg"
	RowTypeResponseItem = "response_item"
	RowTypeCompacted    = "compacted"
	// RowTypeUsageRecord is the row codex writes as a model response
	// completes with usage, naming the gateway's id for the response.
	RowTypeUsageRecord = "token_usage_record"
)

// Rollout payload types that bound a turn or carry a model response's
// output.
const (
	eventTaskStarted       = "task_started"
	eventTaskComplete      = "task_complete"
	eventTurnAborted       = "turn_aborted"
	eventAgentMessage      = "agent_message"
	eventAgentReasoning    = "agent_reasoning"
	eventAgentReasoningRaw = "agent_reasoning_raw_content"
	itemMessage            = "message"
	itemReasoning          = "reasoning"
	roleAssistant          = "assistant"
)

// The app-server keeps rollouts under `sessions/YYYY/MM/DD/` in its home,
// named from the session's local start time and thread id, and resolves a
// thread id against them. A stored session is restored by making its rows
// resident at exactly that path.
const (
	sessionsDirName   = "sessions"
	rolloutNamePrefix = "rollout-"
	rolloutNameSuffix = ".jsonl"
	rolloutStampForm  = "2006-01-02T15-04-05"
	rolloutDayForm    = "2006/01/02"
	maxRolloutLine    = 10 * 1024 * 1024
)

// RolloutRow is one decoded rollout row.
type RolloutRow struct {
	Type    string         `json:"type"`
	Payload map[string]any `json:"payload"`
}

// DecodeRow decodes one rollout row.
func DecodeRow(row []byte) (RolloutRow, error) {
	var decoded RolloutRow
	if err := json.Unmarshal(row, &decoded); err != nil {
		return RolloutRow{}, fmt.Errorf("decode rollout row: %w", err)
	}

	if decoded.Type == "" {
		return RolloutRow{}, errors.New("rollout row has no type")
	}

	return decoded, nil
}

// SessionMeta is what the session_meta row names about the thread.
type SessionMeta struct {
	ID        string
	Timestamp time.Time
}

// ParseSessionMeta reads the thread identity from a session_meta row. It
// reports false for any other row.
func ParseSessionMeta(row []byte) (SessionMeta, bool) {
	decoded, err := DecodeRow(row)
	if err != nil || decoded.Type != RowTypeSessionMeta {
		return SessionMeta{}, false
	}

	meta := SessionMeta{ID: stringValue(decoded.Payload, fieldID)}
	if meta.ID == "" {
		return SessionMeta{}, false
	}

	if stamp, parseErr := time.Parse(time.RFC3339, stringValue(decoded.Payload, "timestamp")); parseErr == nil {
		meta.Timestamp = stamp
	}

	return meta, true
}

// ValidThreadID keeps a stored thread id from naming anything but one file
// in its day directory.
func ValidThreadID(threadID string) bool {
	if threadID == "" || len(threadID) > 128 {
		return false
	}

	for _, r := range threadID {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '-', r == '_':
		default:
			return false
		}
	}

	return true
}

// RolloutPath is the path a rollout for threadID started at stamp occupies
// under home. Codex names the file in local time, so the reconstruction does
// too.
func RolloutPath(home string, threadID string, stamp time.Time) string {
	local := stamp.Local()
	name := rolloutNamePrefix + local.Format(rolloutStampForm) + "-" + threadID + rolloutNameSuffix

	return filepath.Join(home, sessionsDirName, filepath.FromSlash(local.Format(rolloutDayForm)), name)
}

// ReadRows reads a rollout file's non-empty lines. A missing file reads as
// empty.
func ReadRows(path string) ([][]byte, error) {
	file, err := os.Open(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}

	if err != nil {
		return nil, err
	}

	defer file.Close()

	scanner := bufio.NewScanner(file)
	scanner.Buffer(make([]byte, 0, initialLineBytes), maxRolloutLine)

	rows := make([][]byte, 0)

	for scanner.Scan() {
		if line := bytes.TrimSpace(scanner.Bytes()); len(line) > 0 {
			rows = append(rows, bytes.Clone(line))
		}
	}

	return rows, scanner.Err()
}

// WriteRows materializes rows as a rollout file, creating its day directory.
// The file is staged beside its final name and renamed into place.
func WriteRows(path string, rows [][]byte) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return fmt.Errorf("create rollout directory: %w", err)
	}

	var content bytes.Buffer

	for _, row := range rows {
		content.Write(bytes.TrimSpace(row))
		content.WriteByte('\n')
	}

	staging, err := os.CreateTemp(filepath.Dir(path), ".acp-go-codex-*.jsonl")
	if err != nil {
		return fmt.Errorf("stage rollout file: %w", err)
	}

	_, writeErr := staging.Write(content.Bytes())
	if writeErr == nil {
		writeErr = staging.Sync()
	}

	if closeErr := staging.Close(); writeErr == nil {
		writeErr = closeErr
	}

	if writeErr != nil {
		_ = os.Remove(staging.Name())

		return fmt.Errorf("write rollout file: %w", writeErr)
	}

	if err := os.Rename(staging.Name(), path); err != nil {
		_ = os.Remove(staging.Name())

		return fmt.Errorf("publish rollout file: %w", err)
	}

	return nil
}

// FirstUserMessage returns the first user message text a rollout carries.
func FirstUserMessage(rows [][]byte) string {
	for _, row := range rows {
		decoded, err := DecodeRow(row)
		if err != nil || decoded.Type != RowTypeEventMsg || stringValue(decoded.Payload, fieldType) != "user_message" {
			continue
		}

		if text := strings.TrimSpace(stringValue(decoded.Payload, fieldMessage)); text != "" {
			return text
		}
	}

	return ""
}

// LastTokenUsage is the thread usage a rollout last recorded, which codex
// restores when it resumes the thread. A rollout with none reads as zero.
func LastTokenUsage(rows [][]byte) TokenUsage {
	for _, row := range slices.Backward(rows) {
		decoded, err := DecodeRow(row)
		if err != nil || decoded.Type != RowTypeEventMsg || stringValue(decoded.Payload, fieldType) != "token_count" {
			continue
		}

		info := mapValue(decoded.Payload, "info")
		if info == nil {
			continue
		}

		return TokenUsage{
			Last:               rolloutUsage(mapValue(info, "last_token_usage")),
			Total:              rolloutUsage(mapValue(info, "total_token_usage")),
			ModelContextWindow: int64Value(info, "model_context_window"),
		}
	}

	return TokenUsage{}
}

func rolloutUsage(raw map[string]any) Usage {
	return Usage{
		Input:      int64Value(raw, "input_tokens"),
		CachedRead: int64Value(raw, "cached_input_tokens"),
		CacheWrite: int64Value(raw, "cache_write_input_tokens"),
		Output:     int64Value(raw, "output_tokens"),
		Reasoning:  int64Value(raw, "reasoning_output_tokens"),
		Total:      int64Value(raw, "total_tokens"),
	}
}

// ResponseIDs names, for each row, the gateway's id for the model response
// that produced it, or "" for a row no response produced or none names.
// Codex writes a response's output rows as each completes and then, once the
// response completes with usage, one token_usage_record naming it, all within
// the turn. A record therefore claims the output rows written in its turn
// since the previous record. Codex records no id for a response that failed
// or reported no usage: its output rows keep "" when no later record in the
// turn follows, and otherwise fall to that record, since nothing in the
// rollout separates them from the later response's rows.
func ResponseIDs(rows []RolloutRow) []string {
	ids := make([]string, len(rows))
	pending := make([]int, 0)
	turn := ""

	for index, row := range rows {
		switch {
		case row.Type == RowTypeUsageRecord:
			if turn != "" && stringValue(row.Payload, "turn_id") != turn {
				continue
			}

			for _, owned := range pending {
				ids[owned] = stringValue(row.Payload, "response_id")
			}

			pending = pending[:0]
		case row.Type == RowTypeEventMsg && boundsTurn(stringValue(row.Payload, fieldType)):
			pending = pending[:0]
			turn = ""

			if stringValue(row.Payload, fieldType) == eventTaskStarted {
				turn = stringValue(row.Payload, "turn_id")
			}
		case responseOutput(row):
			pending = append(pending, index)
		}
	}

	return ids
}

func boundsTurn(kind string) bool {
	return kind == eventTaskStarted || kind == eventTaskComplete || kind == eventTurnAborted
}

// responseOutput reports whether a row is a model response's assistant text
// or reasoning, as a response item or its event copy.
func responseOutput(row RolloutRow) bool {
	kind := stringValue(row.Payload, fieldType)

	switch row.Type {
	case RowTypeResponseItem:
		return kind == itemReasoning || (kind == itemMessage && stringValue(row.Payload, "role") == roleAssistant)
	case RowTypeEventMsg:
		return kind == eventAgentMessage || kind == eventAgentReasoning || kind == eventAgentReasoningRaw
	default:
		return false
	}
}
