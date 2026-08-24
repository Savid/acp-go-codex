package codexacp

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"time"
)

var (
	sessionRolloutAppendTimeout = 60 * time.Second
	sessionRolloutAppendDelays  = []time.Duration{0, 200 * time.Millisecond, 800 * time.Millisecond}
)

// promptSettlementTimeout bounds the settlement a cancelled request must not
// abort. Settlement runs on a detached context, so it needs a bound of its own.
const promptSettlementTimeout = 2 * time.Minute

const maxSessionImportLineBytes = 10 * 1024 * 1024

type rolloutMirrorRow struct {
	index int
	entry SessionStoreEntry
}

func (s *session) mirrorAndEmitRollout(ctx context.Context) error {
	s.mirrorMu.Lock()
	defer s.mirrorMu.Unlock()

	return s.mirrorRolloutLocked(ctx)
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
func (s *session) mirrorRolloutLocked(ctx context.Context) error {
	store := s.agent.options.SessionStore

	if s.rolloutPath == "" || store == nil {
		return nil
	}

	startRow := s.mirroredRows

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

	clean, err = s.prepareDurableImageRolloutEntries(ctx, clean)
	if err != nil {
		return s.latchCaptureFailure(store, err)
	}

	// Everything the store is owed is in hand from here on: a commit that fails
	// retains it, so the capture no longer needs the latch.
	s.captureStands(store)

	rows := make([]rolloutMirrorRow, len(clean))
	for index, entry := range clean {
		rows[index] = rolloutMirrorRow{index: startRow + index, entry: entry}
	}

	durableEntries, nextMirroredRow := s.durableRolloutEntries(rows)
	if err := s.commitRolloutEntries(ctx, store, durableEntries, nextMirroredRow); err != nil {
		return err
	}

	return nil
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
// to owe.
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
// behind. It is the close boundary's own rung, so it runs after containment and
// terminal lifecycle settlement but before the final stream fence, on a context
// the request's cancellation cannot reach and under the deadline the rest of
// the boundary works to.
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

	return s.mirrorRolloutLocked(ctx)
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
