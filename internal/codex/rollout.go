package codex

import (
	"encoding/json"
	"errors"
	"fmt"
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

// RowKind is a rollout row's type and its payload's type.
type RowKind struct {
	Type        string
	PayloadType string
}

// DecodeRowKind validates one rollout row as DecodeRow does and reads only
// its row and payload types.
func DecodeRowKind(row []byte) (RowKind, error) {
	var decoded struct {
		Type    string `json:"type"`
		Payload *struct {
			Type any `json:"type"`
		} `json:"payload"`
	}

	if err := json.Unmarshal(row, &decoded); err != nil {
		return RowKind{}, fmt.Errorf("decode rollout row: %w", err)
	}

	if decoded.Type == "" {
		return RowKind{}, errors.New("rollout row has no type")
	}

	kind := RowKind{Type: decoded.Type}
	if decoded.Payload != nil {
		kind.PayloadType, _ = decoded.Payload.Type.(string)
	}

	return kind, nil
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

// RelocateRollout is the path under home of the rollout a recorded location
// names. The recorded file name must be threadID's rollout; its start time
// places the file in its day directory. It reports false for any other name.
func RelocateRollout(home string, recorded string, threadID string) (string, bool) {
	name := filepath.Base(recorded)

	stamp, ok := strings.CutPrefix(name, rolloutNamePrefix)
	if ok {
		stamp, ok = strings.CutSuffix(stamp, "-"+threadID+rolloutNameSuffix)
	}

	if !ok {
		return "", false
	}

	// The stamp is the wall-clock time codex named the file with; parsing it
	// without a zone keeps exactly those fields for the day directory.
	start, err := time.Parse(rolloutStampForm, stamp)
	if err != nil {
		return "", false
	}

	return filepath.Join(home, sessionsDirName, filepath.FromSlash(start.Format(rolloutDayForm)), name), true
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
