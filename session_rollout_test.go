package codexacp

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/savid/acp-go-codex/internal/codex"
)

func TestRolloutMirrorDoesNotDuplicateDurableRows(t *testing.T) {
	store := NewInMemorySessionStore()
	agent := NewAgent(WithSessionStore(store))
	agent.setAgentClient(newRecordingAgentClient())
	rollout := filepath.Join(t.TempDir(), "rollout.jsonl")
	if err := os.WriteFile(rollout, []byte("{\"type\":\"one\"}\n{\"type\":\"two\"}\n"), 0o600); err != nil {
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
	if err := os.WriteFile(rollout, []byte("{\"type\":\"one\"}\n\n{\"type\":\"two\"}\n"), 0o600); err != nil {
		t.Fatalf("write rollout: %v", err)
	}
	session := &session{
		agent:        agent,
		id:           "session",
		rolloutPath:  rollout,
		mirroredRows: 2,
		visibleRows:  0,
	}

	events := make(chan codex.Event, 4)
	if err := session.mirrorAndEmitRolloutWithCompletion(context.Background(), nil, events); err != nil {
		t.Fatalf("mirror with lagging events cursor returned error: %v", err)
	}
	if session.mirroredRows != 2 {
		t.Fatalf("durable cursor advanced past already-mirrored rows: %d", session.mirroredRows)
	}
	if session.visibleRows != 2 {
		t.Fatalf("events cursor = %d, want 2", session.visibleRows)
	}
}

func TestRolloutCompletionCursor(t *testing.T) {
	rolloutSession := &session{completionRows: 1, visibleRows: 1}
	completed := make(chan struct{}, 1)
	rolloutSession.emitRolloutCompletions([]rolloutMirrorRow{
		{index: 0, entry: SessionStoreEntry(`{"type":"event_msg","payload":{"type":"task_complete"}}`)},
		{index: 1, entry: SessionStoreEntry(`{"type":"event_msg","payload":{"type":"token_count"}}`)},
		{index: 2, entry: SessionStoreEntry(`{"type":"event_msg","payload":{"type":"task_complete"}}`)},
		{index: 3, entry: SessionStoreEntry(`not-json`)},
	}, completed)

	select {
	case <-completed:
	default:
		t.Fatal("task_complete row did not signal completion")
	}
	select {
	case <-completed:
		t.Fatal("completion signal was not coalesced")
	default:
	}
	if rolloutSession.completionRows != 4 {
		t.Fatalf("completion cursor before unbuffered signal = %d", rolloutSession.completionRows)
	}
	rolloutSession.emitRolloutCompletions([]rolloutMirrorRow{
		{index: 4, entry: SessionStoreEntry(`{"type":"event_msg","payload":{"type":"task_complete"}}`)},
	}, make(chan struct{}))
	if rolloutSession.completionRows != 5 {
		t.Fatalf("completion cursor = %d", rolloutSession.completionRows)
	}
	if !rolloutTaskComplete(SessionStoreEntry(`{"type":"event_msg","payload":{"type":"task_complete"}}`)) ||
		rolloutTaskComplete(SessionStoreEntry(`{"type":"response_item","payload":{"type":"message"}}`)) {
		t.Fatal("rollout task completion detection changed")
	}

	events := make(chan codex.Event, 1)
	rolloutSession.emitRolloutEvents([]rolloutMirrorRow{
		{index: 0, entry: SessionStoreEntry(`{"type":"event_msg","payload":{"type":"agent_message","message":"old"}}`)},
		{index: 4, entry: SessionStoreEntry(`{"type":"event_msg","payload":{"type":"agent_message","message":"visible"}}`)},
	}, events)
	select {
	case event := <-events:
		if event.Kind != codex.EventAgentMessageDelta || event.Text != "visible" || !event.Completed {
			t.Fatalf("rollout event = %#v", event)
		}
	default:
		t.Fatal("agent_message row did not emit visible event")
	}
	if rolloutSession.visibleRows != 5 {
		t.Fatalf("visible cursor = %d", rolloutSession.visibleRows)
	}
	rolloutSession.emitRolloutEvents([]rolloutMirrorRow{
		{index: 5, entry: SessionStoreEntry(`{"type":"event_msg","payload":{"type":"agent_message","message":"dropped"}}`)},
	}, make(chan codex.Event))
	if rolloutSession.visibleRows != 6 {
		t.Fatalf("visible cursor after unbuffered event = %d", rolloutSession.visibleRows)
	}
	if event, ok := rolloutEvent(SessionStoreEntry(`{"type":"event_msg","payload":{"type":"token_count"}}`)); ok || event.Kind != "" {
		t.Fatalf("non-message rollout event = %#v ok=%v", event, ok)
	}
	if event, ok := rolloutEvent(SessionStoreEntry(`not-json`)); ok || event.Kind != "" {
		t.Fatalf("invalid rollout event = %#v ok=%v", event, ok)
	}
}

func TestPrepareRolloutLiveCursors(t *testing.T) {
	rollout := filepath.Join(t.TempDir(), "rollout.jsonl")
	if err := os.WriteFile(rollout, []byte("\n{\"type\":\"one\"}\n{\"type\":\"two\"}\n"), 0o600); err != nil {
		t.Fatalf("write rollout: %v", err)
	}
	rolloutSession := &session{rolloutPath: rollout, completionRows: 1}
	rolloutSession.prepareRolloutLiveCursors()
	if rolloutSession.completionRows != 2 || rolloutSession.visibleRows != 2 {
		t.Fatalf("prepared cursors completion=%d visible=%d", rolloutSession.completionRows, rolloutSession.visibleRows)
	}
	if rows, err := countRolloutRows(""); err != nil || rows != 0 {
		t.Fatalf("empty rollout count rows=%d err=%v", rows, err)
	}
	huge := filepath.Join(t.TempDir(), "huge.jsonl")
	if err := os.WriteFile(huge, []byte(strings.Repeat("x", maxSessionImportLineBytes+1)), 0o600); err != nil {
		t.Fatalf("write huge rollout: %v", err)
	}
	if _, err := countRolloutRows(huge); err == nil {
		t.Fatal("huge rollout count succeeded")
	}

	missing := &session{rolloutPath: filepath.Join(t.TempDir(), "missing.jsonl"), completionRows: 3, visibleRows: 4}
	missing.prepareRolloutLiveCursors()
	if missing.completionRows != 3 || missing.visibleRows != 4 {
		t.Fatalf("missing rollout changed cursors completion=%d visible=%d", missing.completionRows, missing.visibleRows)
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

func withRolloutCompletionFallback(t *testing.T, delay time.Duration) {
	t.Helper()
	original := sessionRolloutCompletionFallback
	sessionRolloutCompletionFallback = delay
	t.Cleanup(func() {
		sessionRolloutCompletionFallback = original
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
