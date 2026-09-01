package codexacp

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestRolloutMirrorDoesNotDuplicateDurableRows(t *testing.T) {
	store := NewInMemorySessionStore()
	agent := NewAgent(WithSessionStore(store))
	agent.setAgentClient(newRecordingAgentClient())
	rollout := filepath.Join(t.TempDir(), "rollout.jsonl")
	if err := os.WriteFile(rollout, []byte("{\"type\":\"one\",\"payload\":{}}\n{\"type\":\"two\",\"payload\":{}}\n"), 0o600); err != nil {
		t.Fatalf("write rollout: %v", err)
	}
	session := &session{
		agent:         agent,
		id:            "session",
		cwd:           "/tmp/project",
		codexThreadID: "thread",
		rolloutPath:   rollout,
	}

	if err := session.mirrorAndEmitRollout(context.Background()); err != nil {
		t.Fatalf("first mirror returned error: %v", err)
	}
	entries, err := store.Load(context.Background(), SessionKey{SessionID: "session"})
	if err != nil || len(entries) != 2 {
		t.Fatalf("durable entries after first mirror len=%d err=%v", len(entries), err)
	}
	if session.mirroredRows != 2 {
		t.Fatalf("mirrored cursor after first mirror = %d", session.mirroredRows)
	}

	if mirrorErr := session.mirrorAndEmitRollout(context.Background()); mirrorErr != nil {
		t.Fatalf("second mirror returned error: %v", mirrorErr)
	}
	entries, err = store.Load(context.Background(), SessionKey{SessionID: "session"})
	if err != nil || len(entries) != 2 {
		t.Fatalf("durable entries were duplicated len=%d err=%v", len(entries), err)
	}
}

func TestRolloutMirrorKeepsDurableCursorWhenRowsAlreadyMirrored(t *testing.T) {
	store := NewInMemorySessionStore()
	agent := NewAgent(WithSessionStore(store))
	rollout := filepath.Join(t.TempDir(), "rollout.jsonl")
	if err := os.WriteFile(rollout, []byte("{\"type\":\"one\",\"payload\":{}}\n\n{\"type\":\"two\",\"payload\":{}}\n"), 0o600); err != nil {
		t.Fatalf("write rollout: %v", err)
	}
	session := &session{
		agent:        agent,
		id:           "session",
		rolloutPath:  rollout,
		mirroredRows: 2,
	}

	if err := session.mirrorAndEmitRollout(context.Background()); err != nil {
		t.Fatalf("mirror returned error: %v", err)
	}
	if session.mirroredRows != 2 {
		t.Fatalf("durable cursor advanced past already-mirrored rows: %d", session.mirroredRows)
	}
}

func TestAppendRolloutEntriesRetriesAndBoundsStoreCalls(t *testing.T) {
	withRolloutAppendSettings(t, time.Second, []time.Duration{0, 0})
	var attempts int
	transient := errors.New("transient")
	store := &appendFuncStore{append: func(context.Context, SessionKey, []SessionStoreEntry) error {
		attempts++
		if attempts == 1 {
			return transient
		}

		return nil
	}}
	err := appendRolloutEntries(context.Background(), store, SessionKey{SessionID: "s"}, []SessionStoreEntry{SessionStoreEntry(`{"type":"one"}`)})
	if err != nil || attempts != 2 {
		t.Fatalf("retry append err=%v attempts=%d", err, attempts)
	}

	withRolloutAppendSettings(t, 50*time.Millisecond, []time.Duration{0, 0})
	attempts = 0
	store = &appendFuncStore{append: func(ctx context.Context, _ SessionKey, _ []SessionStoreEntry) error {
		attempts++
		<-ctx.Done()

		return ctx.Err()
	}}
	err = appendRolloutEntries(context.Background(), store, SessionKey{SessionID: "s"}, []SessionStoreEntry{SessionStoreEntry(`{"type":"one"}`)})
	if !errors.Is(err, context.DeadlineExceeded) || attempts != 1 {
		t.Fatalf("deadline append err=%v attempts=%d", err, attempts)
	}

	ctx, cancel := context.WithCancel(context.Background())
	attempts = 0
	withRolloutAppendSettings(t, time.Second, []time.Duration{0, time.Hour})
	store = &appendFuncStore{append: func(context.Context, SessionKey, []SessionStoreEntry) error {
		attempts++
		cancel()

		return transient
	}}
	err = appendRolloutEntries(ctx, store, SessionKey{SessionID: "s"}, []SessionStoreEntry{SessionStoreEntry(`{"type":"one"}`)})
	if !errors.Is(err, context.Canceled) || attempts != 1 {
		t.Fatalf("canceled backoff append err=%v attempts=%d", err, attempts)
	}
	if err := appendRolloutEntries(context.Background(), store, SessionKey{}, nil); err != nil {
		t.Fatalf("empty append returned error: %v", err)
	}
	withRolloutAppendSettings(t, time.Second, []time.Duration{time.Nanosecond})
	store = &appendFuncStore{append: func(context.Context, SessionKey, []SessionStoreEntry) error {
		return nil
	}}
	if err := appendRolloutEntries(context.Background(), store, SessionKey{SessionID: "s"}, []SessionStoreEntry{SessionStoreEntry(`{"type":"one"}`)}); err != nil {
		t.Fatalf("delayed append returned error: %v", err)
	}
}

func withRolloutAppendSettings(t *testing.T, timeout time.Duration, delays []time.Duration) {
	t.Helper()
	originalTimeout := sessionRolloutAppendTimeout
	originalDelays := sessionRolloutAppendDelays
	sessionRolloutAppendTimeout = timeout
	sessionRolloutAppendDelays = append([]time.Duration(nil), delays...)
	t.Cleanup(func() {
		sessionRolloutAppendTimeout = originalTimeout
		sessionRolloutAppendDelays = originalDelays
	})
}

type appendFuncStore struct {
	mu     sync.Mutex
	append func(context.Context, SessionKey, []SessionStoreEntry) error
}

func (s *appendFuncStore) Append(ctx context.Context, key SessionKey, entries []SessionStoreEntry) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	if s.append == nil {
		return nil
	}

	return s.append(ctx, key, entries)
}

func (s *appendFuncStore) Load(context.Context, SessionKey) ([]SessionStoreEntry, error) {
	return nil, nil
}

func (s *appendFuncStore) Replace(context.Context, SessionKey, []SessionStoreReplacement) error {
	return nil
}

func (s *appendFuncStore) Delete(context.Context, SessionKey) error {
	return nil
}

func (s *appendFuncStore) ListSessions(context.Context) ([]SessionSummary, error) {
	return nil, nil
}

func (s *appendFuncStore) ListSubkeys(context.Context, SessionKey) ([]string, error) {
	return nil, nil
}

func TestDurableRolloutEntriesSkipsMirroredRows(t *testing.T) {
	skipSession := &session{mirroredRows: 1}
	entries, next := skipSession.durableRolloutEntries([]rolloutMirrorRow{
		{index: 0, entry: SessionStoreEntry(`{"type":"already"}`)},
		{index: 1, entry: SessionStoreEntry(`{"type":"new"}`)},
	})
	if len(entries) != 1 || next != 2 {
		t.Fatalf("durable entries=%d next=%d, want 1 and 2", len(entries), next)
	}
}

func TestCommitRolloutEntriesAndSyncErrorBranches(t *testing.T) {
	injected := errors.New("append failed")
	store := &appendFuncStore{append: func(context.Context, SessionKey, []SessionStoreEntry) error {
		return injected
	}}
	s := &session{
		agent:                  NewAgent(WithSessionStore(store)),
		id:                     "session",
		durableConfigCommitted: true,
	}
	require.ErrorIs(t, s.commitRolloutEntries(t.Context(), store, []SessionStoreEntry{[]byte(`{}`)}, 1), injected)
	require.Len(t, s.unsyncedEntries, 1)

	withoutStore := &session{agent: NewAgent()}
	withoutStore.agent.options.SessionStore = nil
	require.NoError(t, withoutStore.ensureMirrorSynced(t.Context()))
}

func TestValidateStoredRolloutEntriesRequiresOneNativeIdentity(t *testing.T) {
	valid := []SessionStoreEntry{
		SessionStoreEntry(`{"type":"session_meta","payload":{"id":"thread"}}`),
		SessionStoreEntry(`{"type":"event_msg","payload":{"type":"agent_message"}}`),
	}
	clean, err := validateStoredRolloutEntries(valid)
	if err != nil || len(clean) != len(valid) {
		t.Fatalf("valid stored rollout clean=%q err=%v", clean, err)
	}

	for _, invalid := range [][]SessionStoreEntry{
		{SessionStoreEntry(`not-json`)},
		{SessionStoreEntry(`{"type":"event_msg","payload":{}}`)},
		{SessionStoreEntry(`{"type":"session_meta","payload":{}}`)},
		{
			SessionStoreEntry(`{"type":"session_meta","payload":{"id":"one"}}`),
			SessionStoreEntry(`{"type":"session_meta","payload":{"id":"two"}}`),
		},
	} {
		if _, err := validateStoredRolloutEntries(invalid); err == nil {
			t.Fatalf("stored rollout accepted %q", invalid)
		}
	}
}
