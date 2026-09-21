package codex

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
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
