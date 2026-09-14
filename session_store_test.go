package codexacp

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"

	"github.com/coder/acp-go-sdk"
	"github.com/stretchr/testify/require"

	"github.com/savid/acp-go-codex/internal/codex"
	acpcore "github.com/savid/acp-go-core"
)

func TestMirrorListLoadResumeDelete(t *testing.T) {
	t.Parallel()

	store := acpcore.NewInMemorySessionStore()
	h := newHarness(t, WithSessionStore(store))
	h.initialize()

	cwd := t.TempDir()

	session, err := h.conn.NewSession(h.ctx(), NewSessionRequest(cwd, WithSessionCodexOptions(NewCodexOptions(WithCodexEffort("low")))))
	require.NoError(t, err)

	_, err = h.prompt(session.SessionId, "ECHO first", nil)
	require.NoError(t, err)

	rows, err := store.Load(context.Background(), acpcore.SessionKey{SessionID: string(session.SessionId)})
	require.NoError(t, err)
	require.Len(t, rows, 3)

	records, err := store.Load(context.Background(), acpcore.SessionKey{SessionID: string(session.SessionId), Subpath: configSubpath})
	require.NoError(t, err)
	require.NotEmpty(t, records)

	var record sessionRecord
	require.NoError(t, json.Unmarshal(records[len(records)-1], &record))
	require.Equal(t, cwd, record.Cwd)
	require.Equal(t, "low", record.Effort)
	require.NotEmpty(t, record.RolloutPath)

	list, err := h.conn.ListSessions(h.ctx(), ListSessionsRequest())
	require.NoError(t, err)
	require.Len(t, list.Sessions, 1)
	require.Equal(t, "ECHO first", *list.Sessions[0].Title)

	_, err = h.conn.CloseSession(h.ctx(), acp.CloseSessionRequest{SessionId: session.SessionId})
	require.NoError(t, err)

	// Load replays the mirrored rows from a fresh session over the same
	// thread; the native rollout wins because it is at least as long.
	load, err := h.conn.LoadSession(h.ctx(), LoadSessionRequest(session.SessionId, cwd))
	require.NoError(t, err)
	require.NotEmpty(t, load.ConfigOptions)

	var userText, agent string

	for _, update := range h.rec.snapshot() {
		switch {
		case update.Update.UserMessageChunk != nil && update.Update.UserMessageChunk.Content.Text != nil:
			userText += update.Update.UserMessageChunk.Content.Text.Text
		case update.Update.AgentMessageChunk != nil && update.Update.AgentMessageChunk.Content.Text != nil:
			agent += update.Update.AgentMessageChunk.Content.Text.Text
		}
	}

	require.Contains(t, userText, "ECHO first")
	require.Equal(t, "ECHO firstECHO first", agent)

	_, err = h.prompt(session.SessionId, "ECHO second", nil)
	require.NoError(t, err)

	_, err = h.conn.CloseSession(h.ctx(), acp.CloseSessionRequest{SessionId: session.SessionId})
	require.NoError(t, err)

	// Deleting the native rollout leaves the store as the only copy; resume
	// materializes it back into the home at the path the app-server resolves.
	require.NoError(t, os.Remove(record.RolloutPath))

	_, err = h.conn.ResumeSession(h.ctx(), ResumeSessionRequest(session.SessionId, cwd))
	require.NoError(t, err)

	matches, err := filepath.Glob(filepath.Join(h.home, "sessions", "*", "*", "*", "rollout-*-"+string(session.SessionId)+".jsonl"))
	require.NoError(t, err)
	require.Len(t, matches, 1)

	materialized, err := codex.ReadRows(matches[0])
	require.NoError(t, err)
	require.Len(t, materialized, 5)

	resp, err := h.prompt(session.SessionId, "ECHO third", nil)
	require.NoError(t, err)
	require.Equal(t, acp.StopReasonEndTurn, resp.StopReason)

	_, err = h.conn.UnstableDeleteSession(h.ctx(), DeleteSessionRequest(session.SessionId))
	require.NoError(t, err)

	list, err = h.conn.ListSessions(h.ctx(), ListSessionsRequest())
	require.NoError(t, err)
	require.Empty(t, list.Sessions)

	_, err = h.conn.LoadSession(h.ctx(), LoadSessionRequest(session.SessionId, cwd))
	require.Equal(t, "unknown session", requestErrorData(t, err)["error"])

	// Native state survives the delete.
	_, err = os.Stat(matches[0])
	require.NoError(t, err)
}

func TestRestoreRefusesDisagreement(t *testing.T) {
	t.Parallel()

	store := acpcore.NewInMemorySessionStore()
	h := newHarness(t, WithSessionStore(store))
	h.initialize()

	cwd := t.TempDir()
	session := h.newSession()

	_, err := h.prompt(session.SessionId, "HELLO", nil)
	require.NoError(t, err)

	_, err = h.conn.CloseSession(h.ctx(), acp.CloseSessionRequest{SessionId: session.SessionId})
	require.NoError(t, err)

	records, err := store.Load(context.Background(), acpcore.SessionKey{SessionID: string(session.SessionId), Subpath: configSubpath})
	require.NoError(t, err)

	var record sessionRecord
	require.NoError(t, json.Unmarshal(records[len(records)-1], &record))

	rows, err := codex.ReadRows(record.RolloutPath)
	require.NoError(t, err)

	rows[1] = []byte(`{"type":"event_msg","payload":{"type":"user_message","message":"tampered"}}`)
	require.NoError(t, codex.WriteRows(record.RolloutPath, rows))

	_, err = h.conn.LoadSession(h.ctx(), LoadSessionRequest(session.SessionId, cwd))
	require.Equal(t, "codex_restore_failed", requestErrorData(t, err)["error"])
}

func TestUnknownAndInvalidSessionIDs(t *testing.T) {
	t.Parallel()

	h := newHarness(t)
	h.initialize()

	_, err := h.conn.LoadSession(h.ctx(), LoadSessionRequest("../escape", t.TempDir()))
	require.Equal(t, "unknown session", requestErrorData(t, err)["error"])

	_, err = h.conn.ResumeSession(h.ctx(), ResumeSessionRequest("missing-thread", t.TempDir()))
	require.Equal(t, "unknown session", requestErrorData(t, err)["error"])

	_, err = h.conn.ListSessions(h.ctx(), ListSessionsRequest(WithListSessionsCursor("!!")))
	require.Equal(t, "cursor", requestErrorData(t, err)["field"])
}

func TestConfigurationCommitsWithoutNewNativeRows(t *testing.T) {
	t.Parallel()
	store := acpcore.NewInMemorySessionStore()
	h := newHarness(t, WithSessionStore(store))
	h.initialize()
	session := h.newSession()
	_, err := h.prompt(session.SessionId, "HELLO", nil)
	require.NoError(t, err)
	before, err := store.Load(t.Context(), acpcore.SessionKey{SessionID: string(session.SessionId)})
	require.NoError(t, err)
	_, err = h.conn.SetSessionConfigOption(h.ctx(), SetModelRequest(session.SessionId, "fake/text"))
	require.NoError(t, err)
	records, err := store.Load(t.Context(), acpcore.SessionKey{SessionID: string(session.SessionId), Subpath: configSubpath})
	require.NoError(t, err)
	require.Len(t, records, 1)
	var record sessionRecord
	require.NoError(t, json.Unmarshal(records[0], &record))
	require.Equal(t, "fake/text", record.Model)
	after, err := store.Load(t.Context(), acpcore.SessionKey{SessionID: string(session.SessionId)})
	require.NoError(t, err)
	require.GreaterOrEqual(t, len(after), len(before))
}

func TestMalformedStoreRecordFailsRestore(t *testing.T) {
	t.Parallel()
	store := acpcore.NewInMemorySessionStore()
	h := newHarness(t, WithSessionStore(store))
	h.initialize()
	session := h.newSession()
	_, err := h.prompt(session.SessionId, "HELLO", nil)
	require.NoError(t, err)
	_, err = h.conn.CloseSession(h.ctx(), acp.CloseSessionRequest{SessionId: session.SessionId})
	require.NoError(t, err)
	rows, err := store.Load(t.Context(), acpcore.SessionKey{SessionID: string(session.SessionId)})
	require.NoError(t, err)
	main := acpcore.SessionKey{SessionID: string(session.SessionId)}
	require.NoError(t, store.Replace(t.Context(), main, []acpcore.SessionStoreReplacement{
		{Key: main, Entries: rows},
		{Key: acpcore.SessionKey{SessionID: main.SessionID, Subpath: configSubpath}, Entries: []acpcore.SessionStoreEntry{[]byte(`{"sessionId":"wrong"}`)}},
	}))
	_, err = h.conn.LoadSession(h.ctx(), LoadSessionRequest(session.SessionId, t.TempDir()))
	require.Equal(t, "codex_restore_failed", requestErrorData(t, err)["error"])
}

type mirrorFaultStore struct {
	acpcore.SessionStore
	fail atomic.Bool
}

func (s *mirrorFaultStore) Replace(ctx context.Context, key acpcore.SessionKey, replacements []acpcore.SessionStoreReplacement) error {
	if s.fail.Load() {
		return errors.New("mirror unavailable")
	}

	return s.SessionStore.Replace(ctx, key, replacements)
}

func TestMirrorFailureFencesTurnAndAllowsRetry(t *testing.T) {
	t.Parallel()
	store := &mirrorFaultStore{SessionStore: acpcore.NewInMemorySessionStore()}
	h := newHarness(t, WithSessionStore(store))
	h.initialize(withLifecycle())
	session := h.newSession()
	store.fail.Store(true)
	_, err := h.prompt(session.SessionId, "HELLO", promptMeta(1))
	require.Equal(t, "codex_turn_failed", requestErrorData(t, err)["error"])
	types := eventTypes(lifecycleEvents(h.rec.snapshot()))
	require.Equal(t, []string{"lifecycle_snapshot", "prompt_accepted", "state_update:running"}, types)
	store.fail.Store(false)
	_, err = h.prompt(session.SessionId, "HELLO", promptMeta(2))
	require.NoError(t, err)
	types = eventTypes(lifecycleEvents(h.rec.snapshot()))
	require.Equal(t, []string{"lifecycle_snapshot", "prompt_accepted", "state_update:running", "lifecycle_snapshot", "prompt_accepted", "state_update:running", "state_update:idle"}, types)
}

func TestNewSessionFailsWhenInitialMirrorFails(t *testing.T) {
	t.Parallel()
	store := &mirrorFaultStore{SessionStore: acpcore.NewInMemorySessionStore()}
	store.fail.Store(true)
	h := newHarness(t, WithSessionStore(store))
	h.initialize()
	_, err := h.conn.NewSession(h.ctx(), NewSessionRequest(t.TempDir()))
	require.Equal(t, "codex_internal_failure", requestErrorData(t, err)["error"])
	listed, err := h.conn.ListSessions(h.ctx(), ListSessionsRequest())
	require.NoError(t, err)
	require.Empty(t, listed.Sessions)
	store.fail.Store(false)
	h.newSession()
}

type blockedLoadStore struct {
	acpcore.SessionStore
	block            atomic.Bool
	entered, release chan struct{}
}

func (s *blockedLoadStore) Load(ctx context.Context, key acpcore.SessionKey) ([]acpcore.SessionStoreEntry, error) {
	rows, err := s.SessionStore.Load(ctx, key)
	if err == nil && key.Subpath == configSubpath && s.block.CompareAndSwap(true, false) {
		close(s.entered)
		select {
		case <-s.release:
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}

	return rows, err
}

func TestConcurrentColdRestoreIsRefusedBeforeBinding(t *testing.T) {
	t.Parallel()
	store := &blockedLoadStore{SessionStore: acpcore.NewInMemorySessionStore(), entered: make(chan struct{}), release: make(chan struct{})}
	h := newHarness(t, WithSessionStore(store))
	h.initialize()
	cwd := t.TempDir()
	session, err := h.conn.NewSession(h.ctx(), NewSessionRequest(cwd))
	require.NoError(t, err)
	_, err = h.prompt(session.SessionId, "HELLO", nil)
	require.NoError(t, err)
	_, err = h.conn.CloseSession(h.ctx(), acp.CloseSessionRequest{SessionId: session.SessionId})
	require.NoError(t, err)
	store.block.Store(true)
	done := make(chan error, 1)
	ctx := h.ctx()
	go func() {
		_, loadErr := h.conn.LoadSession(ctx, LoadSessionRequest(session.SessionId, cwd))
		done <- loadErr
	}()
	select {
	case <-store.entered:
	case <-ctx.Done():
		t.Fatal("load did not reach configuration")
	}
	_, err = h.conn.ResumeSession(h.ctx(), ResumeSessionRequest(session.SessionId, cwd))
	close(store.release)
	require.Equal(t, "session_restore", requestErrorData(t, err)["limit"])
	require.NoError(t, <-done)
	_, err = h.prompt(session.SessionId, "HELLO", nil)
	require.NoError(t, err)
}

func TestLiveResumeAppliesOptionsAndRetainsDirectories(t *testing.T) {
	t.Parallel()
	store := acpcore.NewInMemorySessionStore()
	h := newHarness(t, WithSessionStore(store))
	h.initialize()
	cwd, directory := t.TempDir(), t.TempDir()
	session, err := h.conn.NewSession(h.ctx(), NewSessionRequest(cwd, WithSessionAdditionalDirectories(directory)))
	require.NoError(t, err)
	_, err = h.prompt(session.SessionId, "HELLO", nil)
	require.NoError(t, err)
	_, err = h.conn.ResumeSession(h.ctx(), ResumeSessionRequest(session.SessionId, cwd,
		WithSessionCodexOptions(NewCodexOptions(WithCodexModel("text-only")))))
	require.NoError(t, err)
	rows, err := store.Load(h.ctx(), acpcore.SessionKey{SessionID: string(session.SessionId), Subpath: configSubpath})
	require.NoError(t, err)
	require.Len(t, rows, 1)
	var record sessionRecord
	require.NoError(t, json.Unmarshal(rows[0], &record))
	require.Equal(t, "text-only", record.Model)
	require.Equal(t, []string{directory}, record.AdditionalDirectories)
	_, err = h.prompt(session.SessionId, "HELLO", nil)
	require.NoError(t, err)
}

func TestRestoreIntoDifferentNativeHome(t *testing.T) {
	t.Parallel()
	store := acpcore.NewInMemorySessionStore()
	h := newHarness(t, WithSessionStore(store))
	h.initialize()
	cwd := t.TempDir()
	session, err := h.conn.NewSession(h.ctx(), NewSessionRequest(cwd))
	require.NoError(t, err)
	_, err = h.prompt(session.SessionId, "HELLO", nil)
	require.NoError(t, err)
	_, err = h.conn.CloseSession(h.ctx(), acp.CloseSessionRequest{SessionId: session.SessionId})
	require.NoError(t, err)
	home := t.TempDir()
	restored := newHarness(t, WithSessionStore(store), WithHome(home))
	restored.initialize()
	_, err = restored.conn.LoadSession(restored.ctx(), LoadSessionRequest(session.SessionId, cwd))
	require.NoError(t, err)
	_, err = restored.prompt(session.SessionId, "HELLO", nil)
	require.NoError(t, err)
	rows, err := store.Load(restored.ctx(), acpcore.SessionKey{SessionID: string(session.SessionId), Subpath: configSubpath})
	require.NoError(t, err)
	var record sessionRecord
	require.Len(t, rows, 1)
	require.NoError(t, json.Unmarshal(rows[0], &record))
	relative, err := filepath.Rel(home, record.RolloutPath)
	require.NoError(t, err)
	require.True(t, filepath.IsLocal(relative))
	require.FileExists(t, record.RolloutPath)
}
