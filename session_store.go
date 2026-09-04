package codexacp

import (
	"cmp"
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"strings"
	"sync"
	"time"
)

const (
	// SessionStoreMainSubpath is the main rollout JSONL subpath.
	SessionStoreMainSubpath = ""
	// SessionStoreFormat is the canonical Codex rollout store format.
	SessionStoreFormat = "codex-rollout-jsonl-v1"
)

// SessionStoreEntry is one JSON object in the session's main rollout or an
// adapter-owned subrecord.
//
// Implementations should preserve the raw JSON bytes. The agent validates
// rollout entries as single JSON objects before appending them.
type SessionStoreEntry = json.RawMessage

// SessionKey addresses one session-store record.
type SessionKey struct {
	// SessionID is the ACP-visible session ID being stored.
	SessionID string
	// Subpath is empty for the main rollout or names a session-owned subrecord.
	Subpath string
}

// SessionSummary is a lightweight entry returned by session-store listers.
type SessionSummary struct {
	// SessionID is the session ID for a main rollout JSONL.
	SessionID string
	// UpdatedAtUnixMilli is a Unix millisecond timestamp used for session/list ordering.
	UpdatedAtUnixMilli int64
	// Cwd is the absolute working directory associated with the session.
	Cwd string
	// Title is the display title associated with the session.
	Title string
	// Meta is optional host-provided session metadata.
	Meta map[string]any
}

// SessionStore mirrors Codex rollout entries into a host-provided backend.
//
// Append should be durable before returning. Load must return a copy or
// otherwise immutable entries because callers may retain or trim the returned
// byte slices.
type SessionStore interface {
	Append(ctx context.Context, key SessionKey, entries []SessionStoreEntry) error
	Load(ctx context.Context, key SessionKey) ([]SessionStoreEntry, error)
	Replace(ctx context.Context, main SessionKey, replacements []SessionStoreReplacement) error
	Delete(ctx context.Context, key SessionKey) error
	ListSessions(ctx context.Context) ([]SessionSummary, error)
	ListSubkeys(ctx context.Context, key SessionKey) ([]string, error)
}

// SessionStoreReplacement is one rollout JSONL written during an atomic store replace.
type SessionStoreReplacement struct {
	Key     SessionKey
	Entries []SessionStoreEntry
}

// InMemorySessionStore is a development and test store for Codex rollout rows.
type InMemorySessionStore struct {
	mu         sync.Mutex
	entries    map[SessionKey][]SessionStoreEntry
	updatedAt  map[SessionKey]int64
	tombstones map[SessionKey]int64
}

var _ SessionStore = (*InMemorySessionStore)(nil)

// NewInMemorySessionStore creates an empty process-local rollout store.
func NewInMemorySessionStore() *InMemorySessionStore {
	return &InMemorySessionStore{
		entries:    make(map[SessionKey][]SessionStoreEntry),
		updatedAt:  make(map[SessionKey]int64),
		tombstones: make(map[SessionKey]int64),
	}
}

// Append stores rollout entries under key. Keys with an empty SessionID are
// rejected.
func (s *InMemorySessionStore) Append(ctx context.Context, key SessionKey, entries []SessionStoreEntry) error {
	if err := ctx.Err(); err != nil {
		return err
	}

	if s == nil {
		return fmt.Errorf("nil InMemorySessionStore")
	}

	if len(entries) == 0 {
		return nil
	}

	if key.SessionID == "" {
		return fmt.Errorf("session id is required")
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	if s.entries == nil {
		s.entries = make(map[SessionKey][]SessionStoreEntry)
	}

	if s.updatedAt == nil {
		s.updatedAt = make(map[SessionKey]int64)
	}

	if s.tombstones == nil {
		s.tombstones = make(map[SessionKey]int64)
	}

	if s.isTombstonedLocked(key) {
		return nil
	}

	for _, entry := range entries {
		s.entries[key] = append(s.entries[key], cloneStoreEntry(entry))
	}

	s.updatedAt[key] = time.Now().UnixMilli()

	return nil
}

// Load returns a copy of rollout entries for key.
func (s *InMemorySessionStore) Load(ctx context.Context, key SessionKey) ([]SessionStoreEntry, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	if s == nil {
		return nil, fmt.Errorf("nil InMemorySessionStore")
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	if s.isTombstonedLocked(key) {
		return nil, nil
	}

	return cloneStoreEntries(s.entries[key]), nil
}

// Replace atomically replaces a complete committed generation for one session.
func (s *InMemorySessionStore) Replace(ctx context.Context, main SessionKey, replacements []SessionStoreReplacement) error {
	if err := ctx.Err(); err != nil {
		return err
	}

	if s == nil {
		return fmt.Errorf("nil InMemorySessionStore")
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	if s.entries == nil {
		s.entries = make(map[SessionKey][]SessionStoreEntry)
	}

	if s.updatedAt == nil {
		s.updatedAt = make(map[SessionKey]int64)
	}

	if s.tombstones == nil {
		s.tombstones = make(map[SessionKey]int64)
	}

	if main.SessionID == "" {
		return fmt.Errorf("session id is required")
	}

	if main.Subpath != SessionStoreMainSubpath {
		return fmt.Errorf("main subpath must be %q", SessionStoreMainSubpath)
	}

	mainCount := 0

	// Two replacements naming one key are refused before anything is written,
	// rather than resolved by letting the later one win: a set that names a key
	// twice states two different truths about it, and picking one would commit a
	// generation neither the caller nor any other store would agree on.
	seen := make(map[SessionKey]struct{}, len(replacements))

	for _, replacement := range replacements {
		if replacement.Key.SessionID != main.SessionID {
			return fmt.Errorf("replacement key does not match main session")
		}

		if _, duplicate := seen[replacement.Key]; duplicate {
			return fmt.Errorf("duplicate replacement key %q", replacement.Key.Subpath)
		}

		seen[replacement.Key] = struct{}{}

		if replacement.Key.Subpath == SessionStoreMainSubpath {
			mainCount++
		}
	}

	if mainCount != 1 {
		return fmt.Errorf("replacements must include the main key exactly once")
	}

	// A tombstone this write did not create is final. The store enforces that
	// itself rather than trusting an adapter-level deletion marker: a
	// replacement landing after a delete would answer for a session the host has
	// already been told is gone, and it would answer with a whole generation.
	if s.isTombstonedLocked(main) {
		return nil
	}

	for candidate := range s.entries {
		if candidate.SessionID == main.SessionID {
			delete(s.entries, candidate)
			delete(s.updatedAt, candidate)
			s.tombstones[candidate] = time.Now().UnixMilli()
		}
	}

	updatedAt := time.Now().UnixMilli()

	for _, replacement := range replacements {
		s.entries[replacement.Key] = cloneStoreEntries(replacement.Entries)
		s.updatedAt[replacement.Key] = updatedAt
		delete(s.tombstones, replacement.Key)
	}

	return nil
}

// ListSessions lists committed, non-tombstoned main sessions.
func (s *InMemorySessionStore) ListSessions(ctx context.Context) ([]SessionSummary, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	if s == nil {
		return nil, fmt.Errorf("nil InMemorySessionStore")
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	summaries := make([]SessionSummary, 0)

	for key := range s.entries {
		if key.SessionID == "" || key.Subpath != SessionStoreMainSubpath || s.isTombstonedLocked(key) {
			continue
		}

		summaries = append(summaries, SessionSummary{
			SessionID:          key.SessionID,
			UpdatedAtUnixMilli: s.updatedAt[key],
		})
	}

	slices.SortFunc(summaries, func(left, right SessionSummary) int {
		if byTime := cmp.Compare(right.UpdatedAtUnixMilli, left.UpdatedAtUnixMilli); byTime != 0 {
			return byTime
		}

		return strings.Compare(left.SessionID, right.SessionID)
	})

	return summaries, nil
}

// Delete writes a tombstone. Deleting the main key cascades to subpaths.
// Deleting a key with an empty SessionID is a pure no-op.
func (s *InMemorySessionStore) Delete(ctx context.Context, key SessionKey) error {
	if err := ctx.Err(); err != nil {
		return err
	}

	if s == nil {
		return fmt.Errorf("nil InMemorySessionStore")
	}

	if key.SessionID == "" {
		return nil
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	if s.entries == nil {
		s.entries = make(map[SessionKey][]SessionStoreEntry)
	}

	if s.updatedAt == nil {
		s.updatedAt = make(map[SessionKey]int64)
	}

	if s.tombstones == nil {
		s.tombstones = make(map[SessionKey]int64)
	}

	now := time.Now().UnixMilli()
	matched := false

	for candidate := range s.entries {
		if candidate.SessionID != key.SessionID {
			continue
		}

		if key.Subpath != SessionStoreMainSubpath && candidate.Subpath != key.Subpath {
			continue
		}

		delete(s.entries, candidate)
		delete(s.updatedAt, candidate)
		s.tombstones[candidate] = now
		matched = true
	}

	if !matched {
		s.tombstones[key] = now
	}

	if key.Subpath == SessionStoreMainSubpath {
		s.tombstones[mainSessionKey(key.SessionID)] = now
	}

	return nil
}

// ListSubkeys lists committed, non-tombstoned subpaths for a session.
func (s *InMemorySessionStore) ListSubkeys(ctx context.Context, key SessionKey) ([]string, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	if s == nil {
		return nil, fmt.Errorf("nil InMemorySessionStore")
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	subpaths := make([]string, 0)

	for candidate := range s.entries {
		if candidate.SessionID != key.SessionID || candidate.Subpath == SessionStoreMainSubpath || s.isTombstonedLocked(candidate) {
			continue
		}

		subpaths = append(subpaths, candidate.Subpath)
	}

	slices.Sort(subpaths)

	return subpaths, nil
}

func cloneStoreEntry(entry SessionStoreEntry) SessionStoreEntry {
	return append(SessionStoreEntry(nil), entry...)
}

func cloneStoreEntries(entries []SessionStoreEntry) []SessionStoreEntry {
	if len(entries) == 0 {
		return nil
	}

	clone := make([]SessionStoreEntry, 0, len(entries))
	for _, entry := range entries {
		clone = append(clone, cloneStoreEntry(entry))
	}

	return clone
}

func (s *InMemorySessionStore) isTombstonedLocked(key SessionKey) bool {
	if s.tombstones == nil {
		return false
	}

	if _, ok := s.tombstones[key]; ok {
		return true
	}

	if key.Subpath != SessionStoreMainSubpath {
		_, ok := s.tombstones[mainSessionKey(key.SessionID)]

		return ok
	}

	return false
}

func mainSessionKey(sessionID string) SessionKey {
	return SessionKey{SessionID: sessionID, Subpath: SessionStoreMainSubpath}
}
