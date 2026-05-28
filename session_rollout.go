package codexacp

import (
	"bufio"
	"context"
	"encoding/json"
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

type rolloutMirrorRow struct {
	index int
	entry SessionStoreEntry
}

func (s *Session) mirrorAndEmitRollout(ctx context.Context) error {
	return s.mirrorAndEmitRolloutWithCompletion(ctx, nil, nil)
}

func (s *Session) prepareRolloutLiveCursors() {
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

func (s *Session) mirrorAndEmitRolloutWithCompletion(
	ctx context.Context,
	completed chan<- struct{},
	events chan<- codex.Event,
) error {
	s.mirrorMu.Lock()
	defer s.mirrorMu.Unlock()

	store := s.agent.options.SessionStore
	rawEnabled := s.rawMessages.Enabled()
	if s.rolloutPath == "" || (store == nil && !rawEnabled && completed == nil && events == nil) {
		return nil
	}
	startRow := s.rolloutStartRow(store != nil, rawEnabled, completed != nil, events != nil)

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
	if err := scanner.Err(); err != nil {
		return err
	}
	if len(entries) == 0 {
		return nil
	}

	clean, _, err := validateSessionImportEntries(entries)
	if err != nil {
		return err
	}
	rows := make([]rolloutMirrorRow, len(clean))
	for index, entry := range clean {
		rows[index] = rolloutMirrorRow{index: startRow + index, entry: entry}
	}
	if store != nil {
		projectKey, err := projectKeyForDirectory(s.cwd)
		if err != nil {
			return err
		}
		durableEntries, nextMirroredRow := s.durableRolloutEntries(rows)
		if err := appendRolloutEntries(ctx, store, SessionKey{
			ProjectKey: projectKey,
			SessionID:  string(s.id),
		}, durableEntries); err != nil {
			return err
		}
		if nextMirroredRow > s.mirroredRows {
			s.mirroredRows = nextMirroredRow
		}
	}
	if rawEnabled {
		s.emitRawRolloutRows(ctx, rows)
	}
	if events != nil {
		s.emitRolloutEvents(rows, events)
	}
	if completed != nil {
		s.emitRolloutCompletions(rows, completed)
	}

	return nil
}

func (s *Session) rolloutStartRow(
	storeEnabled bool,
	rawEnabled bool,
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
		{rawEnabled, s.emittedRawRows},
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

func (s *Session) durableRolloutEntries(rows []rolloutMirrorRow) ([]SessionStoreEntry, int) {
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

func (s *Session) emitRawRolloutRows(ctx context.Context, rows []rolloutMirrorRow) {
	for _, row := range rows {
		if row.index < s.emittedRawRows {
			continue
		}
		if err := s.emitRawRolloutRow(ctx, row.entry); err != nil {
			return
		}
		s.emittedRawRows = row.index + 1
	}
}

func (s *Session) emitRolloutCompletions(rows []rolloutMirrorRow, completed chan<- struct{}) {
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

func (s *Session) emitRolloutEvents(rows []rolloutMirrorRow, events chan<- codex.Event) {
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
	var row struct {
		Type    string `json:"type"`
		Payload struct {
			Type    string `json:"type"`
			Message string `json:"message"`
		} `json:"payload"`
	}
	if err := json.Unmarshal(entry, &row); err != nil {
		return codex.Event{}, false
	}
	if row.Type != "event_msg" || row.Payload.Type != "agent_message" || row.Payload.Message == "" {
		return codex.Event{}, false
	}

	return codex.Event{
		Kind:      codex.EventAgentMessageDelta,
		Text:      row.Payload.Message,
		Completed: true,
		RawJSON:   string(entry),
	}, true
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

	return row.Type == "event_msg" && row.Payload.Type == "task_complete"
}

func (s *Session) startRolloutTail(
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

func (s *Session) emitRawRolloutRow(ctx context.Context, entry SessionStoreEntry) error {
	message := decodedRawEvent(entry)
	if !s.rawMessages.ShouldEmit(message) {
		return nil
	}

	conn := s.agent.connection()
	if conn == nil {
		return nil
	}

	return conn.NotifyExtension(ctx, rawCodexSDKMessageMethod, map[string]any{
		"sessionId": s.id,
		"message":   message,
		"rawJSON":   string(entry),
	})
}
