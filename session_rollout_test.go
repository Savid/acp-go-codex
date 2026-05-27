package codexacp

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/savid/acp-go-codex/internal/codex"
)

func TestRawRolloutMirrorsAndEmits(t *testing.T) {
	client := newSpyCodexClient()
	agent := NewAgent(
		WithSessionStore(NewInMemorySessionStore()),
		withClientFactory(func(context.Context, codex.Options) (codex.Client, error) { return client, nil }),
	)
	conn := newRecordingAgentClient()
	agent.setAgentClient(conn)
	ctx := context.Background()

	rollout := filepath.Join(t.TempDir(), "rollout.jsonl")
	if err := os.WriteFile(rollout, []byte("{\"type\":\"response_item\",\"payload\":{\"type\":\"message\",\"role\":\"assistant\"}}\n"), 0o600); err != nil {
		t.Fatalf("write rollout: %v", err)
	}
	client.thread.Path = rollout

	resp, err := agent.NewSession(ctx, NewSessionRequest("/tmp/project", WithSessionRawSDKMessages(true)))
	if err != nil {
		t.Fatalf("NewSession returned error: %v", err)
	}
	if err := agent.sessionMust(resp.SessionId).mirrorAndEmitRollout(ctx); err != nil {
		t.Fatalf("mirror returned error: %v", err)
	}
	if len(conn.extensions) != 1 || conn.extensions[0].method != rawCodexSDKMessageMethod {
		t.Fatalf("extensions = %#v", conn.extensions)
	}
}

func TestRolloutMirrorDoesNotDuplicateDurableRowsWhenRawFails(t *testing.T) {
	store := NewInMemorySessionStore()
	agent := NewAgent(WithSessionStore(store))
	agent.setAgentClient(&extensionErrorClient{recordingAgentClient: newRecordingAgentClient()})
	rollout := filepath.Join(t.TempDir(), "rollout.jsonl")
	if err := os.WriteFile(rollout, []byte("{\"type\":\"one\"}\n{\"type\":\"two\"}\n"), 0o600); err != nil {
		t.Fatalf("write rollout: %v", err)
	}
	session := &Session{
		agent:         agent,
		id:            "session",
		cwd:           "/tmp/project",
		codexThreadID: "thread",
		rolloutPath:   rollout,
		rawMessages:   rawMessageConfig{All: true},
	}

	if err := session.mirrorAndEmitRollout(context.Background()); err != nil {
		t.Fatalf("mirror with raw failure returned error: %v", err)
	}
	projectKey, err := projectKeyForDirectory("/tmp/project")
	if err != nil {
		t.Fatalf("project key: %v", err)
	}
	entries, err := store.Load(context.Background(), SessionKey{ProjectKey: projectKey, SessionID: "thread"})
	if err != nil || len(entries) != 2 {
		t.Fatalf("durable entries after raw failure len=%d err=%v", len(entries), err)
	}
	if session.mirroredRows != 2 || session.emittedRawRows != 0 {
		t.Fatalf("cursors after raw failure mirrored=%d raw=%d", session.mirroredRows, session.emittedRawRows)
	}

	conn := newRecordingAgentClient()
	agent.setAgentClient(conn)
	if err := session.mirrorAndEmitRollout(context.Background()); err != nil {
		t.Fatalf("mirror after raw recovery returned error: %v", err)
	}
	entries, err = store.Load(context.Background(), SessionKey{ProjectKey: projectKey, SessionID: "thread"})
	if err != nil || len(entries) != 2 {
		t.Fatalf("durable entries were duplicated len=%d err=%v", len(entries), err)
	}
	if len(conn.extensions) != 2 || session.emittedRawRows != 2 {
		t.Fatalf("raw recovery extensions=%#v rawCursor=%d", conn.extensions, session.emittedRawRows)
	}
}

func TestEmitRawRolloutRowsSkipsAlreadyEmittedRows(t *testing.T) {
	agent := NewAgent()
	conn := newRecordingAgentClient()
	agent.setAgentClient(conn)
	session := &Session{
		agent:          agent,
		id:             "session",
		rawMessages:    rawMessageConfig{All: true},
		emittedRawRows: 1,
	}

	session.emitRawRolloutRows(context.Background(), []rolloutMirrorRow{
		{index: 0, entry: SessionStoreEntry(`{"type":"already"}`)},
		{index: 1, entry: SessionStoreEntry(`{"type":"new"}`)},
	})
	if len(conn.extensions) != 1 || session.emittedRawRows != 2 {
		t.Fatalf("raw rollout skip extensions=%#v cursor=%d", conn.extensions, session.emittedRawRows)
	}
}

func TestAppendRolloutEntriesRetriesAndBoundsStoreCalls(t *testing.T) {
	withRolloutAppendSettings(t, time.Millisecond, []time.Duration{0, 0})
	var attempts int
	transient := errors.New("transient")
	store := &appendFuncStore{append: func(context.Context, SessionKey, []SessionStoreEntry) error {
		attempts++
		if attempts == 1 {
			return transient
		}
		return nil
	}}
	err := appendRolloutEntries(context.Background(), store, SessionKey{ProjectKey: "p", SessionID: "s"}, []SessionStoreEntry{SessionStoreEntry(`{"type":"one"}`)})
	if err != nil || attempts != 2 {
		t.Fatalf("retry append err=%v attempts=%d", err, attempts)
	}

	attempts = 0
	store = &appendFuncStore{append: func(ctx context.Context, _ SessionKey, _ []SessionStoreEntry) error {
		attempts++
		<-ctx.Done()
		return ctx.Err()
	}}
	err = appendRolloutEntries(context.Background(), store, SessionKey{ProjectKey: "p", SessionID: "s"}, []SessionStoreEntry{SessionStoreEntry(`{"type":"one"}`)})
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
	err = appendRolloutEntries(ctx, store, SessionKey{ProjectKey: "p", SessionID: "s"}, []SessionStoreEntry{SessionStoreEntry(`{"type":"one"}`)})
	if !errors.Is(err, context.Canceled) || attempts != 1 {
		t.Fatalf("canceled backoff append err=%v attempts=%d", err, attempts)
	}
	if err := appendRolloutEntries(context.Background(), store, SessionKey{}, nil); err != nil {
		t.Fatalf("empty append returned error: %v", err)
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
