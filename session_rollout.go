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
	return s.mirrorAndEmitRolloutLive(ctx, nil)
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

func (s *session) mirrorAndEmitRolloutLive(
	ctx context.Context,
	events chan<- codex.Event,
) error {
	s.mirrorMu.Lock()
	defer s.mirrorMu.Unlock()

	return s.mirrorRolloutLocked(ctx, events)
}

// mirrorRolloutLocked is one mirror pass over the rollout file. The caller holds
// the commit lock.
//
// A pass has two halves, and only the second one retains anything: the capture
// reads the rows the store has not seen, and the commit places them, retaining
// exactly what it could not place. A pass that fails in the first half therefore
// holds nothing afterwards to say a durable prefix is still owed, so it latches
// that failure instead — the close boundary's rung is the only thing left that
// can make the capture the settlement never made.
func (s *session) mirrorRolloutLocked(
	ctx context.Context,
	events chan<- codex.Event,
) error {
	store := s.agent.options.SessionStore

	if s.rolloutLiveFenced {
		events = nil
	}

	if s.rolloutPath == "" || (store == nil && events == nil) {
		return nil
	}

	startRow := s.rolloutStartRow(store != nil, events != nil)

	file, err := os.Open(s.rolloutPath)
	if err != nil {
		return s.latchCaptureFailure(store, err)
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
		return s.latchCaptureFailure(store, scanErr)
	}

	if len(entries) == 0 {
		s.captureStands(store)

		return nil
	}

	clean, _, err := validateSessionImportEntries(entries)
	if err != nil {
		return s.latchCaptureFailure(store, err)
	}

	if store != nil {
		clean, err = s.prepareDurableImageRolloutEntries(ctx, clean)
		if err != nil {
			return s.latchCaptureFailure(store, err)
		}
	}

	// Everything the store is owed is in hand from here on: a commit that fails
	// retains it, so the capture no longer needs the latch.
	s.captureStands(store)

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
	eventsEnabled bool,
) int {
	startRow := 0
	set := false

	for _, cursor := range []struct {
		enabled bool
		row     int
	}{
		{storeEnabled, s.mirroredRows},
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

func (s *session) startRolloutTail(
	ctx context.Context,
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
				_ = s.mirrorAndEmitRolloutLive(tailCtx, events)
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

// latchCaptureFailure records that a mirror pass failed before it captured the
// durable prefix it was reading, and returns the failure unchanged. Only a pass
// that was mirroring for a store latches: without one there is no durable prefix
// to owe, and the live tail's own rows are accounted for by their own cursor.
func (s *session) latchCaptureFailure(store SessionStore, err error) error {
	if store != nil {
		s.captureFailed = true
	}

	return err
}

// captureStands clears the latch once a pass has the durable prefix in hand. The
// pass read from the unadvanced cursor, so what it holds covers everything an
// earlier failed pass never read.
func (s *session) captureStands(store SessionStore) {
	if store != nil {
		s.captureFailed = false
	}
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

// commitResumableSnapshot places the durable state the closing session leaves
// behind. It is the close boundary's own rung, so it runs after the containment
// proof and after the stream fence, on a context the request's cancellation
// cannot reach, and under the deadline the rest of the boundary works to.
func (s *session) commitResumableSnapshot(ctx context.Context) error {
	commitCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), closeTimeout)
	defer cancel()

	if err := s.recaptureFailedMirror(commitCtx); err != nil {
		return err
	}

	return s.ensureMirrorSynced(commitCtx)
}

// recaptureFailedMirror makes the capture a settlement's mirror pass failed to
// make. An ordinary close reads nothing new off the rollout file: the prefix a
// settlement captured is retained, and the rung below places it. A pass that
// failed before its commit retained nothing at all, so the same rung would find
// nothing to place and the close would report success owing a prefix it never
// wrote. The latch is what separates the two: it is set only by that failure, so
// a close no failure marked still reads nothing.
//
// A fenced session is skipped because no commit can follow the fence, and the
// latch is set only while a store is configured, so a session without one never
// reaches the read.
func (s *session) recaptureFailedMirror(ctx context.Context) error {
	s.mirrorMu.Lock()
	defer s.mirrorMu.Unlock()

	if !s.captureFailed || s.persistenceFenced {
		return nil
	}

	return s.mirrorRolloutLocked(ctx, nil)
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
	s.captureFailed = false
}
