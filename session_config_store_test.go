package codexacp

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/coder/acp-go-sdk"
	"github.com/savid/acp-go-codex/internal/codex"
	"github.com/stretchr/testify/require"
)

func appendTestDurableSessionConfig(
	t *testing.T,
	store SessionStore,
	sessionID acp.SessionId,
	env map[string]string,
	extraPathDirs []string,
) {
	t.Helper()

	entry, err := json.Marshal(durableSessionConfig{
		Version:       sessionConfigVersion,
		SessionID:     string(sessionID),
		Revision:      1,
		Env:           nonNilStringMap(env),
		ExtraPathDirs: nonNilStrings(extraPathDirs),
	})
	require.NoError(t, err)
	require.NoError(t, store.Append(t.Context(), SessionKey{
		SessionID: string(sessionID),
		Subpath:   sessionConfigStoreSubpath,
	}, []SessionStoreEntry{entry}))
}

func testDurableSessionConfigReplacement(
	t *testing.T,
	sessionID acp.SessionId,
	env map[string]string,
	extraPathDirs []string,
) SessionStoreReplacement {
	t.Helper()

	entry, err := json.Marshal(durableSessionConfig{
		Version:       sessionConfigVersion,
		SessionID:     string(sessionID),
		Revision:      1,
		Env:           nonNilStringMap(env),
		ExtraPathDirs: nonNilStrings(extraPathDirs),
	})
	require.NoError(t, err)

	return SessionStoreReplacement{
		Key: SessionKey{
			SessionID: string(sessionID),
			Subpath:   sessionConfigStoreSubpath,
		},
		Entries: []SessionStoreEntry{entry},
	}
}

func TestDurableSessionConfigRoundTripAndStrictReader(t *testing.T) {
	store := NewInMemorySessionStore()
	agent := NewAgent(WithSessionStore(store))
	session := &session{
		agent:         agent,
		id:            "session",
		env:           map[string]string{"TOKEN": "rotated"},
		extraPathDirs: []string{"/operation/one", "/operation/two"},
	}

	require.NoError(t, session.commitDurableSessionConfig(t.Context(), store))
	require.True(t, session.durableConfigCommitted)
	require.Equal(t, 1, session.durableConfigRevision)

	stored, err := agent.loadDurableSessionConfig(t.Context(), session.id)
	require.NoError(t, err)
	require.Equal(t, session.env, stored.Env)
	require.Equal(t, session.extraPathDirs, stored.ExtraPathDirs)

	for _, invalid := range []string{
		`{"version":1,"sessionId":"session","revision":1,"env":{},"extraPathDirs":[],"extra":true}`,
		`{"version":1,"sessionId":"session","revision":1,"env":{"A":"one","A":"two"},"extraPathDirs":[]}`,
		`{"version":1,"sessionId":"session","revision":1,"env":null,"extraPathDirs":[]}`,
		`{"version":1,"sessionId":"session","revision":1,"env":{},"extraPathDirs":null}`,
		`{"version":1,"sessionId":"session","revision":1,"env":{"PATH":"bad"},"extraPathDirs":[]}`,
		`{"version":1,"sessionId":"session","revision":1,"env":{},"extraPathDirs":["relative"]}`,
		`{"version":1,"sessionId":"session","revision":1,"env":{},"extraPathDirs":[]} {}`,
	} {
		_, err := decodeDurableSessionConfig(SessionStoreEntry(invalid))
		require.Error(t, err, invalid)
	}
}

func TestLoadDurableSessionConfigRejectsIdentityAndRevisionDrift(t *testing.T) {
	encode := func(sessionID string, revision int) SessionStoreEntry {
		entry, err := json.Marshal(durableSessionConfig{
			Version:       sessionConfigVersion,
			SessionID:     sessionID,
			Revision:      revision,
			Env:           map[string]string{},
			ExtraPathDirs: []string{},
		})
		require.NoError(t, err)

		return entry
	}

	for _, entries := range [][]SessionStoreEntry{
		{encode("other", 1)},
		{encode("session", 2)},
		{encode("session", 1), encode("session", 1)},
	} {
		store := NewInMemorySessionStore()
		require.NoError(t, store.Append(context.Background(), SessionKey{
			SessionID: "session",
			Subpath:   sessionConfigStoreSubpath,
		}, entries))

		agent := NewAgent(WithSessionStore(store))
		_, err := agent.loadDurableSessionConfig(t.Context(), "session")
		require.Error(t, err)
	}
}

func TestResolveStoredSessionCarriersUsesOnlyOmittedValues(t *testing.T) {
	stored := durableSessionConfig{
		Env:           map[string]string{"TOKEN": "stored"},
		ExtraPathDirs: []string{"/stored"},
	}

	omitted, matches := resolveStoredSessionCarriers(sessionMeta{}, stored)
	require.True(t, matches)
	require.Equal(t, stored.Env, omitted.Env)
	require.Equal(t, stored.ExtraPathDirs, omitted.ExtraPathDirs)

	explicit, matches := resolveStoredSessionCarriers(sessionMeta{
		Env:                  map[string]string{},
		EnvPresent:           true,
		ExtraPathDirs:        []string{},
		ExtraPathDirsPresent: true,
	}, stored)
	require.False(t, matches)
	require.Empty(t, explicit.Env)
	require.Empty(t, explicit.ExtraPathDirs)
}

func TestResumeSessionRestoresDurableSessionCarriers(t *testing.T) {
	store := NewInMemorySessionStore()
	entries := []SessionStoreEntry{
		SessionStoreEntry(`{"type":"session_meta","payload":{"id":"native-thread"}}`),
	}
	require.NoError(t, store.Append(t.Context(), SessionKey{SessionID: "logical-session"}, entries))
	appendTestDurableSessionConfig(t, store, "logical-session", map[string]string{
		"TOKEN": "stored-token",
	}, []string{"/operation/first", "/operation/second"})

	client := newSpyCodexClient()
	agent := NewAgent(
		WithSessionStore(store),
		withClientFactory(func(context.Context, codex.Options) (codex.Client, error) {
			return client, nil
		}),
	)
	t.Cleanup(func() { require.NoError(t, agent.Close()) })

	_, err := agent.ResumeSession(t.Context(), ResumeSessionRequest("logical-session", t.TempDir()))
	require.NoError(t, err)

	client.mu.Lock()
	request := client.resume
	client.mu.Unlock()

	require.Equal(t, map[string]string{"TOKEN": "stored-token"}, request.Environment)
	require.Equal(t, []string{"/operation/first", "/operation/second"}, request.ExtraPathDirs)

	active := agent.activeSession("logical-session")
	require.NotNil(t, active)
	require.Equal(t, 1, active.durableConfigRevision)
	require.True(t, active.durableConfigCommitted)
}

func TestResumeSessionRejectsMissingDurableSessionConfigBeforeNative(t *testing.T) {
	store := NewInMemorySessionStore()
	require.NoError(t, store.Append(t.Context(), SessionKey{SessionID: "logical-session"}, []SessionStoreEntry{
		SessionStoreEntry(`{"type":"session_meta","payload":{"id":"native-thread"}}`),
	}))

	client := newSpyCodexClient()
	agent := NewAgent(
		WithSessionStore(store),
		withClientFactory(func(context.Context, codex.Options) (codex.Client, error) {
			return client, nil
		}),
	)
	t.Cleanup(func() { require.NoError(t, agent.Close()) })

	_, err := agent.ResumeSession(t.Context(), ResumeSessionRequest("logical-session", t.TempDir()))
	require.ErrorContains(t, err, "stored Codex session configuration is required")

	client.mu.Lock()
	defer client.mu.Unlock()
	require.Empty(t, client.resume.ThreadID)
}

func TestRolloutCommitPlacesChangedConfigRevisionBeforeMainRows(t *testing.T) {
	var keys []SessionKey
	store := &appendFuncStore{append: func(_ context.Context, key SessionKey, _ []SessionStoreEntry) error {
		keys = append(keys, key)

		return nil
	}}

	session := &session{
		id:                    "session",
		env:                   map[string]string{"TOKEN": "rotated"},
		extraPathDirs:         []string{"/rotated"},
		durableConfigRevision: 1,
	}
	row := SessionStoreEntry(`{"type":"session_meta","payload":{"id":"native-thread"}}`)
	require.NoError(t, session.commitRolloutEntries(t.Context(), store, []SessionStoreEntry{row}, 1))
	require.Equal(t, []SessionKey{
		{SessionID: "session", Subpath: sessionConfigStoreSubpath},
		{SessionID: "session"},
	}, keys)
	require.Equal(t, 2, session.durableConfigRevision)
	require.True(t, session.durableConfigCommitted)
}

func TestRolloutConfigFailureRetainsMainPrefix(t *testing.T) {
	withRolloutAppendSettings(t, time.Second, []time.Duration{0})

	configFailure := errors.New("configuration append failed")
	var keys []SessionKey
	store := &appendFuncStore{append: func(_ context.Context, key SessionKey, _ []SessionStoreEntry) error {
		keys = append(keys, key)
		if key.Subpath == sessionConfigStoreSubpath {
			return configFailure
		}

		return nil
	}}

	session := &session{id: "session"}
	row := SessionStoreEntry(`{"type":"session_meta","payload":{"id":"native-thread"}}`)
	err := session.commitRolloutEntries(t.Context(), store, []SessionStoreEntry{row}, 1)
	require.ErrorIs(t, err, configFailure)
	require.Equal(t, []SessionKey{{SessionID: "session", Subpath: sessionConfigStoreSubpath}}, keys)
	require.Equal(t, []SessionStoreEntry{row}, session.unsyncedEntries)
	require.Equal(t, 1, session.unsyncedRow)
}
