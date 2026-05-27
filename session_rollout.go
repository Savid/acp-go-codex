package codexacp

import (
	"bufio"
	"context"
	"encoding/json"
	"os"
	"strings"
	"time"
)

var (
	sessionRolloutAppendTimeout = 60 * time.Second
	sessionRolloutAppendDelays  = []time.Duration{0, 200 * time.Millisecond, 800 * time.Millisecond}
)

type rolloutMirrorRow struct {
	index int
	entry SessionStoreEntry
}

func (s *Session) mirrorAndEmitRollout(ctx context.Context) error {
	s.mirrorMu.Lock()
	defer s.mirrorMu.Unlock()

	store := s.agent.options.SessionStore
	rawEnabled := s.rawMessages.Enabled()
	if s.rolloutPath == "" || (store == nil && !rawEnabled) {
		return nil
	}
	startRow := s.mirroredRows
	if store == nil {
		startRow = s.emittedRawRows
	} else if rawEnabled {
		startRow = min(s.mirroredRows, s.emittedRawRows)
	}

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
			SessionID:  firstNonEmpty(s.codexThreadID, string(s.id)),
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

	return nil
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

func (s *Session) startRolloutTail(ctx context.Context) (context.CancelFunc, <-chan struct{}) {
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
				_ = s.mirrorAndEmitRollout(context.WithoutCancel(ctx))
				return
			case <-ticker.C:
				_ = s.mirrorAndEmitRollout(tailCtx)
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
