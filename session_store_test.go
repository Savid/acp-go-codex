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
