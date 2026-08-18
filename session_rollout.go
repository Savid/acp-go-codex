package codexacp

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"strings"
	"time"

	"github.com/savid/acp-go-codex/internal/codex"
)

var (
	sessionRolloutAppendTimeout = 60 * time.Second
	sessionRolloutAppendDelays  = []time.Duration{0, 200 * time.Millisecond, 800 * time.Millisecond}
)

// sessionRolloutEventBuffer bounds the live rollout tail's hand-off to the
// prompt loop. The tail blocks on a full buffer rather than dropping, so a
// visible frame is delivered or the incarnation ends: it is never silently lost.
const sessionRolloutEventBuffer = 128

// promptSettlementTimeout bounds the settlement a cancelled request must not
// abort. Settlement runs on a detached context, so it needs a bound of its own.
const promptSettlementTimeout = 2 * time.Minute

const maxSessionImportLineBytes = 10 * 1024 * 1024

type rolloutMirrorRow struct {
	index int
	entry SessionStoreEntry
}

func (s *session) mirrorAndEmitRollout(ctx context.Context) error {
	return s.mirrorAndEmitRolloutWithCompletion(ctx, nil, nil)
}

// prepareRolloutLiveCursors fences the live rollout cursors to the incarnation
// that is about to open. The rollout file is shared across every turn of a
// thread, so a row a previous incarnation already accounted for must never enter
// this one. When the fence itself cannot be read the live tail publishes nothing
// for this incarnation: the durable mirror still runs, but nothing guesses which
// rows are old.
func (s *session) prepareRolloutLiveCursors() {
	rows, err := countRolloutRows(s.rolloutPath)
	if errors.Is(err, fs.ErrNotExist) {
		rows, err = 0, nil
	}

	s.mirrorMu.Lock()
	defer s.mirrorMu.Unlock()

	s.rolloutIdentity = nativeTurnIdentity{}
	s.rolloutLiveFenced = err != nil

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

	if s.rolloutLiveFenced {
		completed, events = nil, nil
	}

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
		if err := s.commitRolloutEntries(ctx, store, durableEntries, nextMirroredRow); err != nil {
			return err
		}
	}

	if events != nil {
		s.emitRolloutEvents(ctx, rows, events)
	}

	if completed != nil {
		s.emitRolloutCompletions(ctx, rows, completed)
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

// emitRolloutCompletions hands the rollout's own task-complete row to the prompt
// loop. The cursor advances only over rows actually delivered, so a hand-off the
// incarnation ended before is re-read by whoever owns the rollout next rather
// than silently skipped.
func (s *session) emitRolloutCompletions(ctx context.Context, rows []rolloutMirrorRow, completed chan<- struct{}) {
	for _, row := range rows {
		if row.index < s.completionRows {
			continue
		}

		if rolloutTaskComplete(row.entry) && !deliverRolloutSignal(ctx, completed) {
			return
		}

		s.completionRows = row.index + 1
	}
}

// deliverRolloutSignal reports whether the terminal signal reached the prompt.
// The buffer holds one, so a second task-complete row for a turn already told is
// not a loss.
func deliverRolloutSignal(ctx context.Context, completed chan<- struct{}) bool {
	select {
	case completed <- struct{}{}:
		return true
	case <-ctx.Done():
		return false
	}
}

// emitRolloutEvents hands the visible rows the app-server never forwarded to the
// prompt loop. Delivery is backpressured rather than dropped, and the cursor
// advances only over rows the loop actually received, so an undelivered row
// stays unaccounted instead of disappearing.
func (s *session) emitRolloutEvents(ctx context.Context, rows []rolloutMirrorRow, events chan<- codex.Event) {
	for _, row := range rows {
		if row.index < s.visibleRows {
			continue
		}

		event, ok := rolloutEvent(row.entry)
		if ok {
			select {
			case events <- event:
			case <-ctx.Done():
				return
			}
		}

		s.visibleRows = row.index + 1
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
				// The prompt loop is gone by the time the tail stops, so the
				// final pass is durability only. Publishing into a hand-off
				// nobody reads would block on a full buffer forever.
				_ = s.mirrorAndEmitRollout(context.WithoutCancel(ctx))

				return
			case <-ticker.C:
				_ = s.mirrorAndEmitRolloutWithCompletion(tailCtx, completed, events)
			}
		}
	}()

	return cancel, done
}

// commitRolloutEntries places one durable prefix. A failed commit retains the
// exact entries it did not place instead of dropping them, and the next prompt
// blocks on that retention until the store holds it: the alternative is a turn
// whose frames a host was shown and the store never received.
func (s *session) commitRolloutEntries(
	ctx context.Context,
	store SessionStore,
	entries []SessionStoreEntry,
	nextRow int,
) error {
	if s.persistenceFenced {
		return nil
	}

	if err := appendRolloutEntries(ctx, store, SessionKey{SessionID: string(s.id)}, entries); err != nil {
		s.unsyncedEntries = entries
		s.unsyncedRow = nextRow

		return err
	}

	s.unsyncedEntries = nil
	s.unsyncedRow = 0

	if nextRow > s.mirroredRows {
		s.mirroredRows = nextRow
	}

	return nil
}

// ensureMirrorSynced blocks the next prompt until the prefix a previous
// settlement failed to commit is durable. A store outage is reported loudly
// rather than papered over, because the alternative is a session whose durable
// state silently skips a turn.
func (s *session) ensureMirrorSynced(ctx context.Context) error {
	s.mirrorMu.Lock()
	defer s.mirrorMu.Unlock()

	store := s.agent.options.SessionStore
	if store == nil || len(s.unsyncedEntries) == 0 {
		return nil
	}

	return s.commitRolloutEntries(ctx, store, s.unsyncedEntries, s.unsyncedRow)
}

// fencePersistence stops every later commit for this session. It takes the
// commit's own lock, so a settlement already inside the store finishes before
// the fence stands and no commit can start after it.
func (s *session) fencePersistence() {
	s.mirrorMu.Lock()
	defer s.mirrorMu.Unlock()

	s.persistenceFenced = true
	s.unsyncedEntries = nil
	s.unsyncedRow = 0
}
