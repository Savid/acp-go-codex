package codexacp

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/savid/acp-go-codex/internal/codex"
)

var (
	sessionRolloutAppendTimeout      = 60 * time.Second
	sessionRolloutAppendDelays       = []time.Duration{0, 200 * time.Millisecond, 800 * time.Millisecond}
	sessionRolloutCompletionFallback = 2 * time.Second
)

const maxSessionImportLineBytes = 10 * 1024 * 1024

type rolloutMirrorRow struct {
	index int
	entry SessionStoreEntry
}

func (s *session) mirrorAndEmitRollout(ctx context.Context) error {
	return s.mirrorAndEmitRolloutWithCompletion(ctx, nil, nil)
}

func (s *session) prepareRolloutLiveCursors() {
	s.mirrorMu.Lock()
	s.rolloutIdentity = nativeTurnIdentity{}
	s.mirrorMu.Unlock()

	rows, err := countRolloutRows(s.rolloutPath)
	if err != nil {
		return
	}

	s.mirrorMu.Lock()
	defer s.mirrorMu.Unlock()

	if rows > s.completionRows {
		s.completionRows = rows
	}

	if rows > s.visibleRows {
		s.visibleRows = rows
	}
}

func countRolloutRows(path string) (int, error) {
	if path == "" {
		return 0, nil
	}

	file, err := os.Open(path)
	if err != nil {
		return 0, err
	}
	defer file.Close()

	scanner := bufio.NewScanner(file)
	scanner.Buffer(nil, maxSessionImportLineBytes)

	rows := 0

	for scanner.Scan() {
		if strings.TrimSpace(scanner.Text()) != "" {
			rows++
		}
	}

	if err := scanner.Err(); err != nil {
		return 0, err
	}

	return rows, nil
}

func (s *session) mirrorAndEmitRolloutWithCompletion(
	ctx context.Context,
	completed chan<- struct{},
	events chan<- codex.Event,
) error {
	s.mirrorMu.Lock()
	defer s.mirrorMu.Unlock()

	store := s.agent.options.SessionStore

	if s.rolloutPath == "" || (store == nil && completed == nil && events == nil) {
		return nil
	}

	startRow := s.rolloutStartRow(store != nil, completed != nil, events != nil)

	file, err := os.Open(s.rolloutPath)
	if err != nil {
		return err
	}
	defer file.Close()

	scanner := bufio.NewScanner(file)
	scanner.Buffer(nil, maxSessionImportLineBytes)

	row := 0
	entries := make([]json.RawMessage, 0)

	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" {
			continue
		}

		if row >= startRow {
			entries = append(entries, json.RawMessage(line))
		}

		row++
	}

	if scanErr := scanner.Err(); scanErr != nil {
		return scanErr
	}

	if len(entries) == 0 {
		return nil
	}

	clean, _, err := validateSessionImportEntries(entries)
	if err != nil {
		return err
	}

	if store != nil {
		clean, err = s.prepareDurableImageRolloutEntries(ctx, clean)
		if err != nil {
			return err
		}
	}

	rows := make([]rolloutMirrorRow, len(clean))
	for index, entry := range clean {
		rows[index] = rolloutMirrorRow{index: startRow + index, entry: entry}
	}

	s.mergeRolloutIdentity(rolloutNativeTerminalIdentity(clean))

	if store != nil {
		durableEntries, nextMirroredRow := s.durableRolloutEntries(rows)
		if err := appendRolloutEntries(ctx, store, SessionKey{SessionID: string(s.id)}, durableEntries); err != nil {
			return err
		}

		if nextMirroredRow > s.mirroredRows {
			s.mirroredRows = nextMirroredRow
		}
	}

	if events != nil {
		s.emitRolloutEvents(rows, events)
	}

	if completed != nil {
		s.emitRolloutCompletions(rows, completed)
	}

	return nil
}

func (s *session) mergeRolloutIdentity(next nativeTurnIdentity) {
	if next.turnID != "" && next.turnID != s.rolloutIdentity.turnID {
		s.rolloutIdentity = nativeTurnIdentity{turnID: next.turnID}
	}

	if next.messageID != "" {
		s.rolloutIdentity.messageID = next.messageID
	}
}

func (s *session) rolloutIdentitySnapshot() nativeTurnIdentity {
	s.mirrorMu.Lock()
	defer s.mirrorMu.Unlock()

	return s.rolloutIdentity
}

func validateSessionImportEntries(entries []SessionStoreEntry) ([]SessionStoreEntry, int, error) {
	clean := make([]SessionStoreEntry, 0, len(entries))
	for index, entry := range entries {
		var obj map[string]any
		if err := json.Unmarshal(entry, &obj); err != nil || obj == nil {
			return nil, index, fmt.Errorf("entry %d must be a JSON object", index)
		}

		clean = append(clean, cloneStoreEntry(entry))
	}

	return clean, len(clean), nil
}

func (s *session) rolloutStartRow(
	storeEnabled bool,
	completionEnabled bool,
	eventsEnabled bool,
) int {
	startRow := 0
	set := false

	for _, cursor := range []struct {
		enabled bool
		row     int
	}{
		{storeEnabled, s.mirroredRows},
		{completionEnabled, s.completionRows},
		{eventsEnabled, s.visibleRows},
	} {
		if !cursor.enabled {
			continue
		}

		if !set || cursor.row < startRow {
			startRow = cursor.row
			set = true
		}
	}

	return startRow
}

func appendRolloutEntries(ctx context.Context, store SessionStore, key SessionKey, entries []SessionStoreEntry) error {
	if len(entries) == 0 {
		return nil
	}

	var lastErr error

	for _, delay := range sessionRolloutAppendDelays {
		if delay > 0 {
			select {
			case <-time.After(delay):
			case <-ctx.Done():
				return ctx.Err()
			}
		}

		appendCtx, cancel := context.WithTimeout(ctx, sessionRolloutAppendTimeout)
		err := store.Append(appendCtx, key, entries)

		cancel()

		if err == nil {
			return nil
		}

		lastErr = err

		if appendCtx.Err() == context.DeadlineExceeded {
			break
		}
	}

	return lastErr
}

func (s *session) durableRolloutEntries(rows []rolloutMirrorRow) ([]SessionStoreEntry, int) {
	entries := make([]SessionStoreEntry, 0, len(rows))

	var nextRow int

	for _, row := range rows {
		if row.index < s.mirroredRows {
			continue
		}

		entries = append(entries, row.entry)
		nextRow = row.index + 1
	}

	return entries, nextRow
}

func (s *session) emitRolloutCompletions(rows []rolloutMirrorRow, completed chan<- struct{}) {
	nextRow := s.completionRows
	for _, row := range rows {
		if row.index < s.completionRows {
			continue
		}

		nextRow = row.index + 1
		if rolloutTaskComplete(row.entry) {
			select {
			case completed <- struct{}{}:
			default:
			}
		}
	}

	if nextRow > s.completionRows {
		s.completionRows = nextRow
	}
}

func (s *session) emitRolloutEvents(rows []rolloutMirrorRow, events chan<- codex.Event) {
	nextRow := s.visibleRows
	for _, row := range rows {
		if row.index < s.visibleRows {
			continue
		}

		nextRow = row.index + 1

		event, ok := rolloutEvent(row.entry)
		if !ok {
			continue
		}

		select {
		case events <- event:
		default:
		}
	}

	if nextRow > s.visibleRows {
		s.visibleRows = nextRow
	}
}

func rolloutEvent(entry SessionStoreEntry) (codex.Event, bool) {
	row, err := decodeRolloutRow(entry)
	if err != nil {
		return codex.Event{}, false
	}

	if row.Type == valueEventMsg &&
		stringFromAny(row.Payload[jsonFieldType]) == valueAgentMessage &&
		stringFromAny(row.Payload[jsonFieldMessage]) != "" {
		return codex.Event{
			Kind:      codex.EventAgentMessageDelta,
			Text:      stringFromAny(row.Payload[jsonFieldMessage]),
			Completed: true,
			RawJSON:   string(entry),
		}, true
	}

	if row.Type == valueResponseItem && stringFromAny(row.Payload[jsonFieldType]) == valueImageGenerationCall {
		image := rolloutImageEvent(row.Payload)
		if reference, _ := row.Payload[jsonFieldResult].(map[string]any); reference != nil {
			image.ArtifactRef = stringFromAny(reference[imageArtifactRefKey])
		}

		return codex.Event{
			Kind:    codex.EventImageCompleted,
			ItemID:  image.ID,
			Image:   image,
			RawJSON: string(entry),
		}, true
	}

	return codex.Event{}, false
}

func rolloutTaskComplete(entry SessionStoreEntry) bool {
	var row struct {
		Type    string `json:"type"`
		Payload struct {
			Type string `json:"type"`
		} `json:"payload"`
	}
	if err := json.Unmarshal(entry, &row); err != nil {
		return false
	}

	return row.Type == valueEventMsg && row.Payload.Type == "task_complete"
}

func (s *session) startRolloutTail(
	ctx context.Context,
	completed chan<- struct{},
	events chan<- codex.Event,
) (context.CancelFunc, <-chan struct{}) {
	tailCtx, cancel := context.WithCancel(ctx)
	done := make(chan struct{})

	go func() {
		defer recoverAgentGoroutine(ctx, agentLogger(s.agent), "Codex rollout tail")
		defer close(done)

		ticker := time.NewTicker(100 * time.Millisecond)
		defer ticker.Stop()

		for {
			select {
			case <-tailCtx.Done():
				_ = s.mirrorAndEmitRolloutWithCompletion(context.WithoutCancel(ctx), completed, events)

				return
			case <-ticker.C:
				_ = s.mirrorAndEmitRolloutWithCompletion(tailCtx, completed, events)
			}
		}
	}()

	return cancel, done
}
