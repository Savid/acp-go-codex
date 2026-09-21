package codexacp

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/coder/acp-go-sdk"
	"github.com/savid/acp-go-codex/internal/codex"
	acpcore "github.com/savid/acp-go-core"
	"github.com/savid/acp-go-core/sessionlog"
	"github.com/savid/acp-go-core/wire"
	"github.com/stretchr/testify/require"
)

func TestMirrorListLoadResumeDelete(t *testing.T) {
	t.Parallel()

	store := acpcore.NewInMemorySessionStore()
	h := newHarness(t, WithSessionStore(store))
	h.initialize()

	cwd := t.TempDir()

	session, err := h.conn.NewSession(h.ctx(), wire.NewSessionRequest(cwd, WithSessionCodexOptions(NewCodexOptions(WithCodexEffort("low")))))
	require.NoError(t, err)

	_, err = h.prompt(session.SessionId, "ECHO first", nil)
	require.NoError(t, err)

	rows, err := loadEntries(context.Background(), store, acpcore.SessionKey{SessionID: string(session.SessionId)})
	require.NoError(t, err)
	require.Len(t, rows, 3)

	records, err := loadEntries(context.Background(), store, acpcore.SessionKey{SessionID: string(session.SessionId), Subpath: configSubpath})
	require.NoError(t, err)
	require.NotEmpty(t, records)

	var record sessionRecord
	require.NoError(t, json.Unmarshal(records[len(records)-1], &record))
	require.Equal(t, cwd, record.Cwd)
	require.Equal(t, "low", record.Effort)
	require.NotEmpty(t, record.RolloutPath)

	list, err := h.conn.ListSessions(h.ctx(), wire.ListSessionsRequest())
	require.NoError(t, err)
	require.Len(t, list.Sessions, 1)
	require.Equal(t, "ECHO first", *list.Sessions[0].Title)

	_, err = h.conn.CloseSession(h.ctx(), acp.CloseSessionRequest{SessionId: session.SessionId})
	require.NoError(t, err)

	// Load replays the mirrored rows from a fresh session over the same
	// thread; the native rollout wins because it is at least as long.
	load, err := h.conn.LoadSession(h.ctx(), wire.LoadSessionRequest(session.SessionId, cwd))
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

	_, err = h.conn.ResumeSession(h.ctx(), wire.ResumeSessionRequest(session.SessionId, cwd))
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

	_, err = h.conn.UnstableDeleteSession(h.ctx(), wire.DeleteSessionRequest(session.SessionId))
	require.NoError(t, err)

	list, err = h.conn.ListSessions(h.ctx(), wire.ListSessionsRequest())
	require.NoError(t, err)
	require.Empty(t, list.Sessions)

	_, err = h.conn.LoadSession(h.ctx(), wire.LoadSessionRequest(session.SessionId, cwd))
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

	records, err := loadEntries(context.Background(), store, acpcore.SessionKey{SessionID: string(session.SessionId), Subpath: configSubpath})
	require.NoError(t, err)

	var record sessionRecord
	require.NoError(t, json.Unmarshal(records[len(records)-1], &record))

	rows, err := codex.ReadRows(record.RolloutPath)
	require.NoError(t, err)

	rows[1] = []byte(`{"type":"event_msg","payload":{"type":"user_message","message":"tampered"}}`)
	require.NoError(t, codex.WriteRows(record.RolloutPath, rows))

	_, err = h.conn.LoadSession(h.ctx(), wire.LoadSessionRequest(session.SessionId, cwd))
	require.Equal(t, "codex_restore_failed", requestErrorData(t, err)["error"])
}

func TestUnknownAndInvalidSessionIDs(t *testing.T) {
	t.Parallel()

	h := newHarness(t)
	h.initialize()

	_, err := h.conn.LoadSession(h.ctx(), wire.LoadSessionRequest("../escape", t.TempDir()))
	require.Equal(t, "unknown session", requestErrorData(t, err)["error"])

	_, err = h.conn.ResumeSession(h.ctx(), wire.ResumeSessionRequest("missing-thread", t.TempDir()))
	require.Equal(t, "unknown session", requestErrorData(t, err)["error"])

	_, err = h.conn.ListSessions(h.ctx(), wire.ListSessionsRequest(wire.WithListSessionsCursor("!!")))
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
	before, err := loadEntries(t.Context(), store, acpcore.SessionKey{SessionID: string(session.SessionId)})
	require.NoError(t, err)
	_, err = h.conn.SetSessionConfigOption(h.ctx(), SetModelRequest(session.SessionId, "fake/text"))
	require.NoError(t, err)
	records, err := loadEntries(t.Context(), store, acpcore.SessionKey{SessionID: string(session.SessionId), Subpath: configSubpath})
	require.NoError(t, err)
	require.Len(t, records, 1)
	var record sessionRecord
	require.NoError(t, json.Unmarshal(records[0], &record))
	require.Equal(t, "fake/text", record.Model)
	after, err := loadEntries(t.Context(), store, acpcore.SessionKey{SessionID: string(session.SessionId)})
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
	rows, err := loadEntries(t.Context(), store, acpcore.SessionKey{SessionID: string(session.SessionId)})
	require.NoError(t, err)
	main := acpcore.SessionKey{SessionID: string(session.SessionId)}
	require.NoError(t, store.Replace(t.Context(), main, []acpcore.SessionStoreReplacement{
		{Key: main, Entries: rows},
		{Key: acpcore.SessionKey{SessionID: main.SessionID, Subpath: configSubpath}, Entries: []acpcore.SessionStoreEntry{[]byte(`{"sessionId":"wrong"}`)}},
	}))
	_, err = h.conn.LoadSession(h.ctx(), wire.LoadSessionRequest(session.SessionId, t.TempDir()))
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
	_, err := h.conn.NewSession(h.ctx(), wire.NewSessionRequest(t.TempDir()))
	require.Equal(t, "codex_internal_failure", requestErrorData(t, err)["error"])
	listed, err := h.conn.ListSessions(h.ctx(), wire.ListSessionsRequest())
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

func (s *blockedLoadStore) Load(ctx context.Context, sessionID string) (map[string][]acpcore.SessionStoreEntry, error) {
	rows, err := s.SessionStore.Load(ctx, sessionID)
	if err == nil && s.block.CompareAndSwap(true, false) {
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
	session, err := h.conn.NewSession(h.ctx(), wire.NewSessionRequest(cwd))
	require.NoError(t, err)
	_, err = h.prompt(session.SessionId, "HELLO", nil)
	require.NoError(t, err)
	_, err = h.conn.CloseSession(h.ctx(), acp.CloseSessionRequest{SessionId: session.SessionId})
	require.NoError(t, err)
	store.block.Store(true)
	done := make(chan error, 1)
	ctx := h.ctx()
	go func() {
		_, loadErr := h.conn.LoadSession(ctx, wire.LoadSessionRequest(session.SessionId, cwd))
		done <- loadErr
	}()
	select {
	case <-store.entered:
	case <-ctx.Done():
		t.Fatal("load did not reach configuration")
	}
	_, err = h.conn.ResumeSession(h.ctx(), wire.ResumeSessionRequest(session.SessionId, cwd))
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
	session, err := h.conn.NewSession(h.ctx(), wire.NewSessionRequest(cwd, wire.WithSessionAdditionalDirectories(directory)))
	require.NoError(t, err)
	_, err = h.prompt(session.SessionId, "HELLO", nil)
	require.NoError(t, err)
	_, err = h.conn.ResumeSession(h.ctx(), wire.ResumeSessionRequest(session.SessionId, cwd,
		WithSessionCodexOptions(NewCodexOptions(WithCodexModel("text-only")))))
	require.NoError(t, err)
	rows, err := loadEntries(h.ctx(), store, acpcore.SessionKey{SessionID: string(session.SessionId), Subpath: configSubpath})
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
	session, err := h.conn.NewSession(h.ctx(), wire.NewSessionRequest(cwd))
	require.NoError(t, err)
	_, err = h.prompt(session.SessionId, "HELLO", nil)
	require.NoError(t, err)
	_, err = h.conn.CloseSession(h.ctx(), acp.CloseSessionRequest{SessionId: session.SessionId})
	require.NoError(t, err)
	home := t.TempDir()
	restored := newHarness(t, WithSessionStore(store), WithHome(home))
	restored.initialize()
	_, err = restored.conn.LoadSession(restored.ctx(), wire.LoadSessionRequest(session.SessionId, cwd))
	require.NoError(t, err)
	_, err = restored.prompt(session.SessionId, "HELLO", nil)
	require.NoError(t, err)
	rows, err := loadEntries(restored.ctx(), store, acpcore.SessionKey{SessionID: string(session.SessionId), Subpath: configSubpath})
	require.NoError(t, err)
	var record sessionRecord
	require.Len(t, rows, 1)
	require.NoError(t, json.Unmarshal(rows[0], &record))
	relative, err := filepath.Rel(home, record.RolloutPath)
	require.NoError(t, err)
	require.True(t, filepath.IsLocal(relative))
	require.FileExists(t, record.RolloutPath)
}

func TestRestoreAcceptsACommittedEmptyConversation(t *testing.T) {
	t.Parallel()

	store := acpcore.NewInMemorySessionStore()
	h := newHarness(t, WithSessionStore(store))
	h.initialize()

	cwd := t.TempDir()

	session, err := h.conn.NewSession(h.ctx(), wire.NewSessionRequest(cwd))
	require.NoError(t, err)

	_, err = h.prompt(session.SessionId, "HELLO", nil)
	require.NoError(t, err)

	_, err = h.conn.CloseSession(h.ctx(), acp.CloseSessionRequest{SessionId: session.SessionId})
	require.NoError(t, err)

	// A generation whose main record is live but empty is a conversation whose
	// native history is still empty, not a missing one.
	main := acpcore.SessionKey{SessionID: string(session.SessionId)}
	config := acpcore.SessionKey{SessionID: main.SessionID, Subpath: configSubpath}

	records, err := loadEntries(t.Context(), store, config)
	require.NoError(t, err)
	require.NoError(t, store.Replace(t.Context(), main, []acpcore.SessionStoreReplacement{
		{Key: main}, {Key: config, Entries: records},
	}))

	_, err = h.conn.LoadSession(h.ctx(), wire.LoadSessionRequest(session.SessionId, cwd))
	require.NoError(t, err)
}

func TestEmptyConversationCommitsItsConfiguration(t *testing.T) {
	t.Parallel()

	store := &recoveryFaultStore{SessionStore: acpcore.NewInMemorySessionStore()}
	h := newHarness(t, WithSessionStore(store))
	h.initialize()

	cwd := filepath.Join(t.TempDir(), noNativeRowsDir)
	require.NoError(t, os.MkdirAll(cwd, 0o700))

	session, err := h.conn.NewSession(h.ctx(), wire.NewSessionRequest(cwd,
		WithSessionCodexOptions(NewCodexOptions(WithCodexEffort("low")))))
	require.NoError(t, err)

	generation, err := store.Load(t.Context(), string(session.SessionId))
	require.NoError(t, err)

	rows, present := generation[acpcore.SessionStoreMainSubpath]
	require.True(t, present, "an established empty conversation commits an empty main record")
	require.Empty(t, rows)

	var record sessionRecord
	require.NoError(t, json.Unmarshal(generation[configSubpath][0], &record))
	require.Equal(t, "low", record.Effort)

	listed, err := h.conn.ListSessions(h.ctx(), wire.ListSessionsRequest())
	require.NoError(t, err)
	require.Len(t, listed.Sessions, 1)

	_, err = h.conn.CloseSession(h.ctx(), acp.CloseSessionRequest{SessionId: session.SessionId})
	require.NoError(t, err)
	require.NoError(t, os.Remove(record.RolloutPath))
	before, err := store.Load(t.Context(), string(session.SessionId))
	require.NoError(t, err)
	store.fail.Store(true)
	_, err = h.conn.ResumeSession(h.ctx(), wire.ResumeSessionRequest(session.SessionId, cwd))
	require.Equal(t, "codex_restore_failed", requestErrorData(t, err)["error"])
	after, err := store.Load(t.Context(), string(session.SessionId))
	require.NoError(t, err)
	require.Equal(t, before, after, "failed recovery must preserve the committed binding")
	store.fail.Store(false)

	resumed, err := h.conn.ResumeSession(h.ctx(), wire.ResumeSessionRequest(session.SessionId, cwd))
	require.NoError(t, err)
	require.NotEqual(t, session.Meta, resumed.Meta)
	after, err = store.Load(t.Context(), string(session.SessionId))
	require.NoError(t, err)
	require.NoError(t, json.Unmarshal(after[configSubpath][0], &record))
	require.Equal(t, string(session.SessionId), record.SessionID)
	require.Equal(t, wire.NativeSessionMeta(vendor, record.NativeSessionID), resumed.Meta)
	require.Equal(t, "low", record.Effort)
	listed, err = h.conn.ListSessions(h.ctx(), wire.ListSessionsRequest())
	require.NoError(t, err)
	require.Len(t, listed.Sessions, 1)
	require.Equal(t, session.SessionId, listed.Sessions[0].SessionId)
	require.Equal(t, resumed.Meta, listed.Sessions[0].Meta)
}

func TestRestoreAdoptsRowsAppendedOutsideACP(t *testing.T) {
	t.Parallel()

	store := acpcore.NewInMemorySessionStore()
	h := newHarness(t, WithSessionStore(store))
	h.initialize()

	cwd := t.TempDir()

	session, err := h.conn.NewSession(h.ctx(), wire.NewSessionRequest(cwd))
	require.NoError(t, err)

	_, err = h.prompt(session.SessionId, "ECHO first", nil)
	require.NoError(t, err)

	_, err = h.conn.CloseSession(h.ctx(), acp.CloseSessionRequest{SessionId: session.SessionId})
	require.NoError(t, err)

	records, err := loadEntries(t.Context(), store, acpcore.SessionKey{SessionID: string(session.SessionId), Subpath: configSubpath})
	require.NoError(t, err)

	var record sessionRecord
	require.NoError(t, json.Unmarshal(records[0], &record))

	rows, err := codex.ReadRows(record.RolloutPath)
	require.NoError(t, err)

	native := append(rows, []byte(`{"type":"event_msg","payload":{"type":"agent_message","message":"continued natively"}}`))
	require.NoError(t, codex.WriteRows(record.RolloutPath, native))

	before := len(h.rec.snapshot())

	_, err = h.conn.LoadSession(h.ctx(), wire.LoadSessionRequest(session.SessionId, cwd))
	require.NoError(t, err)

	stored, err := loadEntries(t.Context(), store, acpcore.SessionKey{SessionID: string(session.SessionId)})
	require.NoError(t, err)
	require.Len(t, stored, len(native), "rows written to the rollout outside ACP were not adopted")

	require.Contains(t, agentText(h.rec.snapshot()[before:]), "continued natively")
}

func TestNativeHomeLockRefusesASecondRuntime(t *testing.T) {
	t.Parallel()

	home := filepath.Join(t.TempDir(), "home")

	first := newHarness(t, WithHome(home))
	first.initialize()
	first.newSession()

	second := newHarness(t, WithHome(home))
	second.initialize()

	_, err := second.conn.NewSession(second.ctx(), wire.NewSessionRequest(t.TempDir()))
	data := requestErrorData(t, err)
	require.Equal(t, "codex_internal_failure", data["error"])
	require.Equal(t, internalClassNativeStart, data["class"])
}

func TestCommitWithNoRolloutIsAFailure(t *testing.T) {
	t.Parallel()

	s := &session{agent: NewAgent(), id: "sess-1"}
	require.Error(t, s.commitMirror(t.Context()),
		"a commit the session cannot attempt was reported as a durable generation")

	rollout := filepath.Join(t.TempDir(), "rollout.jsonl")
	require.NoError(t, os.WriteFile(rollout, nil, 0o600))

	s.rolloutPath = rollout
	s.cwd = t.TempDir()
	require.NoError(t, s.commitMirror(t.Context()))
}

// Residual native state with no store entry is neither listed nor adopted.
func TestResidualNativeStateIsNeverAdopted(t *testing.T) {
	t.Parallel()

	h := newHarness(t)
	h.initialize()

	orphan := "00000000-0000-4000-8000-00000000abcd"
	cwd := t.TempDir()
	path := codex.RolloutPath(h.home, orphan, time.Now())
	require.NoError(t, codex.WriteRows(path, [][]byte{
		[]byte(`{"type":"session_meta","payload":{"id":"` + orphan + `","timestamp":"2026-09-15T00:00:00Z","cwd":"` + cwd + `"}}`),
	}))

	list, err := h.conn.ListSessions(h.ctx(), wire.ListSessionsRequest())
	require.NoError(t, err)
	require.Empty(t, list.Sessions)

	_, err = h.conn.LoadSession(h.ctx(), wire.LoadSessionRequest(acp.SessionId(orphan), cwd))
	require.Equal(t, "unknown session", requestErrorData(t, err)["error"])

	_, err = h.conn.ResumeSession(h.ctx(), wire.ResumeSessionRequest(acp.SessionId(orphan), cwd))
	require.Equal(t, "unknown session", requestErrorData(t, err)["error"])
}

type recoveryFaultStore struct {
	acpcore.SessionStore
	fail atomic.Bool
}

func (s *recoveryFaultStore) Replace(ctx context.Context, main acpcore.SessionKey, replacements []acpcore.SessionStoreReplacement) error {
	if s.fail.Load() {
		return errors.New("injected store failure")
	}

	return s.SessionStore.Replace(ctx, main, replacements)
}

func TestNativeBindingSurvivesLoadAndResume(t *testing.T) {
	t.Parallel()
	store := acpcore.NewInMemorySessionStore()
	h := newHarness(t, WithSessionStore(store))
	h.initialize()
	cwd := t.TempDir()
	created, err := h.conn.NewSession(h.ctx(), wire.NewSessionRequest(cwd))
	require.NoError(t, err)
	_, err = h.prompt(created.SessionId, "HELLO", nil)
	require.NoError(t, err)
	_, err = h.conn.CloseSession(h.ctx(), acp.CloseSessionRequest{SessionId: created.SessionId})
	require.NoError(t, err)
	var record sessionRecord
	rows, found, err := sessionlog.Load(t.Context(), store, string(created.SessionId), &record)
	require.NoError(t, err)
	require.True(t, found)
	require.NotEmpty(t, record.NativeSessionID)
	require.Equal(t, wire.NativeSessionMeta(vendor, record.NativeSessionID), created.Meta)
	id := acp.SessionId("acp-conversation-independent-of-native-id")
	record.SessionID = string(id)
	require.NoError(t, sessionlog.Commit(t.Context(), store, string(id), rows, record))
	require.NoError(t, store.Delete(t.Context(), acpcore.SessionKey{SessionID: string(created.SessionId)}))

	before := len(h.rec.snapshot())
	loaded, err := h.conn.LoadSession(h.ctx(), wire.LoadSessionRequest(id, cwd))
	require.NoError(t, err)
	require.Equal(t, wire.NativeSessionMeta(vendor, record.NativeSessionID), loaded.Meta)
	_, err = h.prompt(id, "HELLO", nil)
	require.NoError(t, err)
	for _, update := range h.rec.snapshot()[before:] {
		require.Equal(t, id, update.SessionId)
	}
	listed, err := h.conn.ListSessions(h.ctx(), wire.ListSessionsRequest())
	require.NoError(t, err)
	require.Len(t, listed.Sessions, 1)
	require.Equal(t, id, listed.Sessions[0].SessionId)
	require.Equal(t, loaded.Meta, listed.Sessions[0].Meta)
	_, err = h.conn.CloseSession(h.ctx(), acp.CloseSessionRequest{SessionId: id})
	require.NoError(t, err)
	listed, err = h.conn.ListSessions(h.ctx(), wire.ListSessionsRequest())
	require.NoError(t, err)
	require.Len(t, listed.Sessions, 1)
	require.Equal(t, loaded.Meta, listed.Sessions[0].Meta)
	resumed, err := h.conn.ResumeSession(h.ctx(), wire.ResumeSessionRequest(id, cwd))
	require.NoError(t, err)
	require.Equal(t, loaded.Meta, resumed.Meta)
	_, err = h.prompt(id, "HELLO", nil)
	require.NoError(t, err)
	var after sessionRecord
	_, found, err = sessionlog.Load(t.Context(), store, string(id), &after)
	require.NoError(t, err)
	require.True(t, found)
	require.Equal(t, record.NativeSessionID, after.NativeSessionID)
	require.Equal(t, string(id), after.SessionID)
}

type blockedCommitStore struct {
	acpcore.SessionStore
	blockID atomic.Value
	entered chan struct{}
	release chan struct{}
}

func (s *blockedCommitStore) Replace(ctx context.Context, key acpcore.SessionKey, replacements []acpcore.SessionStoreReplacement) error {
	if s.blockID.Load() == key.SessionID {
		select {
		case s.entered <- struct{}{}:
		default:
		}
		select {
		case <-s.release:
		case <-ctx.Done():
			return ctx.Err()
		}
	}

	return s.SessionStore.Replace(ctx, key, replacements)
}

func TestPeerPromptCompletesDuringMirrorCommit(t *testing.T) {
	t.Parallel()
	store := &blockedCommitStore{SessionStore: acpcore.NewInMemorySessionStore(), entered: make(chan struct{}, 1), release: make(chan struct{})}
	h := newHarness(t, WithSessionStore(store))
	h.initialize(withLifecycle())
	first, second := h.newSession(), h.newSession()
	store.blockID.Store(string(first.SessionId))
	firstDone := make(chan error, 1)
	go func() {
		_, err := h.prompt(first.SessionId, "TAIL", promptMeta(1))
		firstDone <- err
	}()
	defer func() {
		close(store.release)
		require.NoError(t, <-firstDone)
	}()
	select {
	case <-store.entered:
	case <-time.After(testTimeout):
		t.Fatal("first prompt did not reach the mirror commit")
	}
	secondDone := make(chan error, 1)
	go func() {
		_, err := h.prompt(second.SessionId, "HELLO", promptMeta(2))
		secondDone <- err
	}()
	select {
	case err := <-secondDone:
		require.NoError(t, err)
	case <-time.After(3 * time.Second):
		t.Fatal("peer prompt waited for another session's mirror commit")
	}
}

func TestFailedConfigChangeDoesNotReachTheNextCommit(t *testing.T) {
	t.Parallel()
	store := &mirrorFaultStore{SessionStore: acpcore.NewInMemorySessionStore()}
	h := newHarness(t, WithSessionStore(store))
	h.initialize()
	session := h.newSession()
	var before sessionRecord
	_, found, err := sessionlog.Load(t.Context(), store, string(session.SessionId), &before)
	require.NoError(t, err)
	require.True(t, found)
	store.fail.Store(true)
	_, err = h.conn.SetSessionConfigOption(h.ctx(), wire.SetConfigOptionRequest(session.SessionId, configModel, "changed"))
	require.Error(t, err)
	store.fail.Store(false)
	_, err = h.conn.SetSessionConfigOption(h.ctx(), wire.SetConfigOptionRequest(session.SessionId, configMode, "plan"))
	require.NoError(t, err)
	var after sessionRecord
	_, found, err = sessionlog.Load(t.Context(), store, string(session.SessionId), &after)
	require.NoError(t, err)
	require.True(t, found)
	require.Equal(t, before.Model, after.Model)
}

type firstMirrorFailureStore struct {
	acpcore.SessionStore
	calls atomic.Int32
}

func (s *firstMirrorFailureStore) Replace(ctx context.Context, key acpcore.SessionKey, rows []acpcore.SessionStoreReplacement) error {
	if s.calls.Add(1) == 1 {
		return errors.New("initial mirror unavailable")
	}

	return s.SessionStore.Replace(ctx, key, rows)
}

func TestFailedNewSessionDoesNotPersistDuringCleanup(t *testing.T) {
	t.Parallel()
	store := &firstMirrorFailureStore{SessionStore: acpcore.NewInMemorySessionStore()}
	h := newHarness(t, WithSessionStore(store))
	h.initialize()
	response, err := h.conn.NewSession(h.ctx(), wire.NewSessionRequest(t.TempDir()))
	require.Error(t, err)
	require.Empty(t, response.SessionId)
	rows, err := store.ListSessions(h.ctx())
	require.NoError(t, err)
	require.Empty(t, rows)
	listed, err := h.conn.ListSessions(h.ctx(), wire.ListSessionsRequest())
	require.NoError(t, err)
	require.Empty(t, listed.Sessions)
}

type firstOpenFailureClient struct {
	*recorder
	failed atomic.Bool
}

func (c *firstOpenFailureClient) SessionUpdate(ctx context.Context, notification acp.SessionNotification) error {
	if c.failed.CompareAndSwap(false, true) {
		return errors.New("initial publication unavailable")
	}

	return c.recorder.SessionUpdate(ctx, notification)
}

func TestFailedSessionOpenReleasesActiveSlot(t *testing.T) {
	t.Parallel()
	a := NewAgent(testOptions(t, WithConcurrencyLimits(ConcurrencyLimits{MaxActiveSessions: 1}))...)
	t.Cleanup(func() { _ = a.Close() })
	a.attach(&firstOpenFailureClient{recorder: newRecorder()}, nil)
	request := acp.InitializeRequest{ProtocolVersion: acp.ProtocolVersionNumber}
	withLifecycle()(&request)
	_, err := a.Initialize(t.Context(), request)
	require.NoError(t, err)
	first, err := a.NewSession(t.Context(), wire.NewSessionRequest(t.TempDir()))
	require.Error(t, err)
	require.Empty(t, first.SessionId)
	_, err = a.NewSession(t.Context(), wire.NewSessionRequest(t.TempDir()))
	require.NoError(t, err)
}

func TestDeleteWinsAgainstPreparedLoad(t *testing.T) {
	t.Parallel()
	store := &blockedLoadStore{SessionStore: acpcore.NewInMemorySessionStore(), entered: make(chan struct{}), release: make(chan struct{})}
	h := newHarness(t, WithSessionStore(store))
	h.initialize()
	cwd := t.TempDir()
	session, err := h.conn.NewSession(h.ctx(), wire.NewSessionRequest(cwd))
	require.NoError(t, err)
	_, err = h.prompt(session.SessionId, "HELLO", nil)
	require.NoError(t, err)
	_, err = h.conn.CloseSession(h.ctx(), acp.CloseSessionRequest{SessionId: session.SessionId})
	require.NoError(t, err)
	store.block.Store(true)
	done := make(chan error, 1)
	ctx := h.ctx()
	go func() {
		_, loadErr := h.conn.LoadSession(ctx, wire.LoadSessionRequest(session.SessionId, cwd))
		done <- loadErr
	}()
	select {
	case <-store.entered:
	case <-ctx.Done():
		t.Fatal("load did not reach stored configuration")
	}
	_, err = h.conn.UnstableDeleteSession(h.ctx(), acp.UnstableDeleteSessionRequest{SessionId: session.SessionId})
	close(store.release)
	require.NoError(t, err)
	require.Equal(t, "unknown session", requestErrorData(t, <-done)["error"])
	list, err := h.conn.ListSessions(h.ctx(), acp.ListSessionsRequest{})
	require.NoError(t, err)
	require.Empty(t, list.Sessions)
}

// countingStore counts the writes a session makes to the store.
type countingStore struct {
	acpcore.SessionStore
	replaces atomic.Int32
	deletes  atomic.Int32
}

func (s *countingStore) Replace(ctx context.Context, main acpcore.SessionKey, replacements []acpcore.SessionStoreReplacement) error {
	s.replaces.Add(1)

	return s.SessionStore.Replace(ctx, main, replacements)
}

func (s *countingStore) Delete(ctx context.Context, key acpcore.SessionKey) error {
	s.deletes.Add(1)

	return s.SessionStore.Delete(ctx, key)
}

func TestEphemeralSessionNeverReachesTheStore(t *testing.T) {
	t.Parallel()

	store := &countingStore{SessionStore: acpcore.NewInMemorySessionStore()}
	h := newHarness(t, WithSessionStore(store))
	h.initialize()

	cwd := t.TempDir()
	ephemeral := wire.WithSessionMeta(wire.SessionMeta{Ephemeral: true}.Apply(nil))

	session, err := h.conn.NewSession(h.ctx(), wire.NewSessionRequest(cwd, ephemeral))
	require.NoError(t, err)

	_, err = h.prompt(session.SessionId, "ECHO first", nil)
	require.NoError(t, err)

	list, err := h.conn.ListSessions(h.ctx(), wire.ListSessionsRequest())
	require.NoError(t, err)
	require.Empty(t, list.Sessions)

	_, err = h.conn.CloseSession(h.ctx(), acp.CloseSessionRequest{SessionId: session.SessionId})
	require.NoError(t, err)

	_, err = h.conn.UnstableDeleteSession(h.ctx(), wire.DeleteSessionRequest(session.SessionId))
	require.NoError(t, err)

	require.Zero(t, store.replaces.Load(), "an ephemeral session is never mirrored")
	require.Zero(t, store.deletes.Load(), "deleting an ephemeral session leaves no tombstone")

	generation, err := store.Load(t.Context(), string(session.SessionId))
	require.NoError(t, err)
	require.Nil(t, generation)

	// A session opened without the flag still mirrors.
	_, err = h.conn.NewSession(h.ctx(), wire.NewSessionRequest(cwd))
	require.NoError(t, err)
	require.Positive(t, store.replaces.Load())
}

func TestSessionMetaIsRefusedOnRestoreAndUnknownFields(t *testing.T) {
	t.Parallel()

	h := newHarness(t, WithSessionStore(acpcore.NewInMemorySessionStore()))
	h.initialize()

	cwd := t.TempDir()

	session, err := h.conn.NewSession(h.ctx(), wire.NewSessionRequest(cwd))
	require.NoError(t, err)

	ephemeral := wire.WithSessionMeta(wire.SessionMeta{Ephemeral: true}.Apply(nil))

	_, err = h.conn.LoadSession(h.ctx(), wire.LoadSessionRequest(session.SessionId, cwd, ephemeral))
	data := requestErrorData(t, err)
	require.Equal(t, "unsupported", data["error"])
	require.Equal(t, "_meta."+wire.SessionMetaKey, data["field"])

	_, err = h.conn.ResumeSession(h.ctx(), wire.ResumeSessionRequest(session.SessionId, cwd, ephemeral))
	data = requestErrorData(t, err)
	require.Equal(t, "unsupported", data["error"])
	require.Equal(t, "_meta."+wire.SessionMetaKey, data["field"])

	unknown := wire.WithSessionMeta(map[string]any{wire.SessionMetaKey: map[string]any{"persist": false}})

	_, err = h.conn.NewSession(h.ctx(), wire.NewSessionRequest(cwd, unknown))
	data = requestErrorData(t, err)
	require.Equal(t, "unsupported", data["error"])
	require.Equal(t, "_meta."+wire.SessionMetaKey+".persist", data["field"])
}
