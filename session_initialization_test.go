package codexacp

import (
	"context"
	"errors"
	"testing"

	"github.com/savid/acp-go-codex/internal/codex"
	"github.com/stretchr/testify/require"
)

func TestNewSessionRequiresDurableInitialization(t *testing.T) {
	failure := errors.New("initial storage unavailable")
	a := newPlaceholderAgent(WithSessionStore(&appendFuncStore{append: func(context.Context, SessionKey, []SessionStoreEntry) error {
		return failure
	}}))
	created, err := a.NewSession(t.Context(), NewSessionRequest(t.TempDir()))
	require.ErrorIs(t, err, failure)
	require.Empty(t, created.SessionId)
	require.Empty(t, a.sessions)
}

func TestInitialRolloutReplacementIsAtomicAndPreservesSubrecords(t *testing.T) {
	store := &initialReplacementStore{InMemorySessionStore: NewInMemorySessionStore()}
	client := newSpyCodexClient()
	a := NewAgent(WithSessionStore(store), withClientFactory(func(context.Context, codex.Options) (codex.Client, error) {
		return client, nil
	}))
	cleanupSessionNativePumps(t, a)
	created, err := a.NewSession(t.Context(), NewSessionRequest(t.TempDir()))
	require.NoError(t, err)
	s := a.activeSession(created.SessionId)
	main := SessionKey{SessionID: string(created.SessionId)}
	initial, err := store.Load(t.Context(), main)
	require.NoError(t, err)
	require.Len(t, initial, 1)
	subkey := SessionKey{SessionID: main.SessionID, Subpath: "image/test"}
	image := []SessionStoreEntry{[]byte(`{"data":"preserved"}`)}
	require.NoError(t, store.Append(t.Context(), subkey, image))
	rows := []SessionStoreEntry{
		[]byte(`{"ordinal":0,"type":"session_meta","payload":{"id":"thread-1","base_instructions":{"text":"native instructions"}}}`),
		[]byte(`{"ordinal":1,"type":"event_msg","payload":{"type":"agent_message","message":"answer"}}`),
	}
	failure := errors.New("replacement unavailable")
	store.replaceErr = failure
	require.ErrorIs(t, s.commitRolloutEntries(t.Context(), store, rows, len(rows)), failure)
	retained, err := store.Load(t.Context(), main)
	require.NoError(t, err)
	require.Equal(t, initial, retained)
	store.replaceErr = nil
	require.NoError(t, s.ensureMirrorSynced(t.Context()))
	committed, err := store.Load(t.Context(), main)
	require.NoError(t, err)
	require.Equal(t, rows, committed)
	savedImage, err := store.Load(t.Context(), subkey)
	require.NoError(t, err)
	require.Equal(t, image, savedImage)
	_, err = a.loadDurableSessionConfig(t.Context(), created.SessionId)
	require.NoError(t, err)
	next := []SessionStoreEntry{[]byte(`{"ordinal":2,"type":"event_msg","payload":{"type":"agent_message","message":"next"}}`)}
	require.NoError(t, s.commitRolloutEntries(t.Context(), store, next, len(rows)+1))
	committed, err = store.Load(t.Context(), main)
	require.NoError(t, err)
	require.Equal(t, rows, committed[:len(rows)])
	require.Equal(t, next, committed[len(rows):])
}

type initialReplacementStore struct {
	*InMemorySessionStore
	replaceErr error
}

func (s *initialReplacementStore) Replace(ctx context.Context, main SessionKey, replacements []SessionStoreReplacement) error {
	if s.replaceErr != nil {
		return s.replaceErr
	}

	return s.InMemorySessionStore.Replace(ctx, main, replacements)
}
