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
		extraPathDirs: []string{absTestPath("operation", "one"), absTestPath("operation", "two")},
	}

	require.NoError(t, session.commitDurableSessionConfig(t.Context(), store))
	require.NoError(t, session.commitDurableSessionConfig(t.Context(), store))
	require.True(t, session.durableConfigCommitted)
	require.Equal(t, 1, session.durableConfigRevision)

	stored, err := agent.loadDurableSessionConfig(t.Context(), session.id)
	require.NoError(t, err)
	require.Equal(t, session.env, stored.Env)
	require.Equal(t, session.extraPathDirs, stored.ExtraPathDirs)

	for _, invalid := range []string{
		`1`,
		`{"version":"one","sessionId":"session","revision":1,"env":{},"extraPathDirs":[]}`,
		`{"version":1,"sessionId":"session","revision":1,"env":{},"extraPathDirs":[],"extra":true}`,
		`{"Version":1,"sessionId":"session","revision":1,"env":{},"extraPathDirs":[]}`,
		`{"version":1,"SessionId":"session","revision":1,"env":{},"extraPathDirs":[]}`,
		`{"version":1,"sessionId":"session","Revision":1,"env":{},"extraPathDirs":[]}`,
		`{"version":1,"sessionId":"session","revision":1,"ENV":{},"extraPathDirs":[]}`,
		`{"version":1,"sessionId":"session","revision":1,"env":{},"ExtraPathDirs":[]}`,
		`{"version":1,"sessionId":"session","revision":1,"env":{},"Env":{},"extraPathDirs":[]}`,
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

type durableConfigLoadErrorStore struct {
	*appendFuncStore
	err error
}

func (s durableConfigLoadErrorStore) Load(context.Context, SessionKey) ([]SessionStoreEntry, error) {
	return nil, s.err
}

type durableConfigFixedStore struct {
	*appendFuncStore
	entries []SessionStoreEntry
}

func (s durableConfigFixedStore) Load(context.Context, SessionKey) ([]SessionStoreEntry, error) {
	return s.entries, nil
}

func TestDurableSessionConfigStorageFailures(t *testing.T) {
	injected := errors.New("injected")
	failedCommit := &session{agent: NewAgent(), id: "session"}
	require.ErrorIs(t, failedCommit.commitDurableSessionConfig(t.Context(), &appendFuncStore{
		append: func(context.Context, SessionKey, []SessionStoreEntry) error { return injected },
	}), injected)
	require.False(t, failedCommit.durableConfigCommitted)

	loadAgent := NewAgent(WithSessionStore(durableConfigLoadErrorStore{
		appendFuncStore: &appendFuncStore{}, err: injected,
	}))
	_, err := loadAgent.loadDurableSessionConfig(t.Context(), "session")
	require.ErrorIs(t, err, injected)

	invalidAgent := NewAgent(WithSessionStore(durableConfigFixedStore{
		appendFuncStore: &appendFuncStore{}, entries: []SessionStoreEntry{[]byte(`1`)},
	}))
	_, err = invalidAgent.loadDurableSessionConfig(t.Context(), "session")
	require.ErrorContains(t, err, "session configuration entry 0")
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
		ExtraPathDirs: []string{absTestPath("stored")},
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

func TestResolveActiveSessionCarriersMakesInheritanceAuthoritative(t *testing.T) {
	active := &session{
		env:           map[string]string{"TOKEN": "active"},
		extraPathDirs: []string{absTestPath("active")},
	}

	resolved := resolveActiveSessionCarriers(sessionMeta{}, active)
	require.Equal(t, active.env, resolved.Env)
	require.Equal(t, active.extraPathDirs, resolved.ExtraPathDirs)
	require.True(t, resolved.EnvPresent)
	require.True(t, resolved.ExtraPathDirsPresent)

	stored := durableSessionConfig{
		Env:           map[string]string{"TOKEN": "stale"},
		ExtraPathDirs: []string{absTestPath("stale")},
	}
	resolved, matches := resolveStoredSessionCarriers(resolved, stored)
	require.False(t, matches)
	require.Equal(t, active.env, resolved.Env)
	require.Equal(t, active.extraPathDirs, resolved.ExtraPathDirs)
}

func TestResumeSessionRestoresDurableSessionCarriers(t *testing.T) {
	store := NewInMemorySessionStore()
	entries := []SessionStoreEntry{
		SessionStoreEntry(`{"type":"session_meta","payload":{"id":"native-thread"}}`),
	}
	require.NoError(t, store.Append(t.Context(), SessionKey{SessionID: "logical-session"}, entries))
	appendTestDurableSessionConfig(t, store, "logical-session", map[string]string{
		"TOKEN": "stored-token",
	}, []string{absTestPath("operation", "first"), absTestPath("operation", "second")})

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
	require.Equal(t, []string{absTestPath("operation", "first"), absTestPath("operation", "second")}, request.ExtraPathDirs)

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

func TestConfigurableStoreDefaultsToMissingSessionConfiguration(t *testing.T) {
	store := &configurableStore{entries: []SessionStoreEntry{
		SessionStoreEntry(`{"type":"session_meta","payload":{"id":"native-thread"}}`),
	}}
	agent := NewAgent(WithSessionStore(store))

	_, err := agent.ResumeSession(t.Context(), ResumeSessionRequest("logical-session", t.TempDir()))
	require.ErrorContains(t, err, "stored Codex session configuration is required")
}

func TestMaterializedLifecycleCommitsChangedSessionCarriersBeforeSuccess(t *testing.T) {
	for _, operation := range []struct {
		name string
		run  func(context.Context, *Agent, acp.SessionId, string, ...SessionRequestOption) error
	}{
		{
			name: "resume",
			run: func(ctx context.Context, agent *Agent, id acp.SessionId, cwd string, options ...SessionRequestOption) error {
				_, err := agent.ResumeSession(ctx, ResumeSessionRequest(id, cwd, options...))

				return err
			},
		},
		{
			name: "load",
			run: func(ctx context.Context, agent *Agent, id acp.SessionId, cwd string, options ...SessionRequestOption) error {
				_, err := agent.LoadSession(ctx, LoadSessionRequest(id, cwd, options...))

				return err
			},
		},
	} {
		t.Run(operation.name, func(t *testing.T) {
			store := NewInMemorySessionStore()
			id := acp.SessionId("logical-session")
			require.NoError(t, store.Append(t.Context(), SessionKey{SessionID: string(id)}, []SessionStoreEntry{
				SessionStoreEntry(`{"type":"session_meta","payload":{"id":"native-thread"}}`),
			}))
			appendTestDurableSessionConfig(t, store, id, map[string]string{"TOKEN": "stored"}, []string{absTestPath("stored")})

			client := newSpyCodexClient()
			agent := NewAgent(
				WithSessionStore(store),
				withClientFactory(func(context.Context, codex.Options) (codex.Client, error) {
					return client, nil
				}),
			)
			t.Cleanup(func() { require.NoError(t, agent.Close()) })

			rotatedPath := t.TempDir()
			err := operation.run(t.Context(), agent, id, t.TempDir(), WithSessionMeta(CodexOptions{
				Env:           map[string]string{"TOKEN": "rotated"},
				ExtraPathDirs: []string{rotatedPath},
			}.Meta()))
			require.NoError(t, err)

			stored, err := agent.loadDurableSessionConfig(t.Context(), id)
			require.NoError(t, err)
			require.Equal(t, 2, stored.Revision)
			require.Equal(t, map[string]string{"TOKEN": "rotated"}, stored.Env)
			require.Equal(t, []string{rotatedPath}, stored.ExtraPathDirs)
		})
	}
}

func TestActiveLifecycleCommitsInheritedPendingSessionCarriers(t *testing.T) {
	store := NewInMemorySessionStore()
	id := acp.SessionId("logical-session")
	require.NoError(t, store.Append(t.Context(), SessionKey{SessionID: string(id)}, []SessionStoreEntry{
		SessionStoreEntry(`{"type":"session_meta","payload":{"id":"native-thread"}}`),
	}))
	appendTestDurableSessionConfig(t, store, id, map[string]string{"TOKEN": "stored"}, []string{absTestPath("stored")})

	client := newSpyCodexClient()
	agent := NewAgent(WithSessionStore(store))
	activeMeta := sessionMeta{
		Env:           map[string]string{"TOKEN": "active"},
		ExtraPathDirs: []string{absTestPath("active")},
	}
	active := newSession(agent, id, absTestPath("work"), nil, codex.Thread{
		ID:   "native-thread",
		Path: "/native/rollout.jsonl",
	}, client, activeMeta, nil)
	active.fingerprint = codexSessionStartFingerprint(codexSessionStart{
		Cwd:      absTestPath("work"),
		Meta:     activeMeta,
		ResumeID: string(id),
	})
	active.durableConfigRevision = 1
	active.durableConfigCommitted = false
	agent.sessions[id] = active
	agent.runtimeClient = client
	t.Cleanup(func() { active.fenceSession() })

	_, err := agent.ResumeSession(t.Context(), ResumeSessionRequest(id, absTestPath("work")))
	require.NoError(t, err)

	stored, err := agent.loadDurableSessionConfig(t.Context(), id)
	require.NoError(t, err)
	require.Equal(t, 2, stored.Revision)
	require.Equal(t, activeMeta.Env, stored.Env)
	require.Equal(t, activeMeta.ExtraPathDirs, stored.ExtraPathDirs)
}

func TestActiveRebindCommitsChangedSessionCarriersBeforeSuccess(t *testing.T) {
	store := NewInMemorySessionStore()
	id := acp.SessionId("logical-session")
	require.NoError(t, store.Append(t.Context(), SessionKey{SessionID: string(id)}, []SessionStoreEntry{
		SessionStoreEntry(`{"type":"session_meta","payload":{"id":"native-thread"}}`),
	}))
	storedPath := t.TempDir()
	appendTestDurableSessionConfig(t, store, id, map[string]string{"TOKEN": "stored"}, []string{storedPath})

	client := newSpyCodexClient()
	agent := NewAgent(WithSessionStore(store))
	activeMeta := sessionMeta{
		Env:           map[string]string{"TOKEN": "stored"},
		ExtraPathDirs: []string{storedPath},
	}
	active := newSession(agent, id, absTestPath("work"), nil, codex.Thread{
		ID:   "native-thread",
		Path: "/native/rollout.jsonl",
	}, client, activeMeta, nil)
	active.fingerprint = codexSessionStartFingerprint(codexSessionStart{
		Cwd:      absTestPath("work"),
		Meta:     activeMeta,
		ResumeID: string(id),
	})
	active.durableConfigRevision = 1
	active.durableConfigCommitted = true
	agent.sessions[id] = active
	agent.runtimeClient = client
	t.Cleanup(func() { active.fenceSession() })

	rotatedPath := t.TempDir()
	_, err := agent.ResumeSession(t.Context(), ResumeSessionRequest(id, absTestPath("work"), WithSessionMeta(CodexOptions{
		Env:           map[string]string{"TOKEN": "rotated"},
		ExtraPathDirs: []string{rotatedPath},
	}.Meta())))
	require.NoError(t, err)

	stored, err := agent.loadDurableSessionConfig(t.Context(), id)
	require.NoError(t, err)
	require.Equal(t, 2, stored.Revision)
	require.Equal(t, map[string]string{"TOKEN": "rotated"}, stored.Env)
	require.Equal(t, []string{rotatedPath}, stored.ExtraPathDirs)
}

func TestActiveRebindRetriesFailedConfigurationBeforeLaterSuccess(t *testing.T) {
	withRolloutAppendSettings(t, time.Second, []time.Duration{0})

	store := &failNextConfigAppendStore{InMemorySessionStore: NewInMemorySessionStore()}
	id := acp.SessionId("logical-session")
	require.NoError(t, store.InMemorySessionStore.Append(t.Context(), SessionKey{SessionID: string(id)}, []SessionStoreEntry{
		SessionStoreEntry(`{"type":"session_meta","payload":{"id":"native-thread"}}`),
	}))
	appendTestDurableSessionConfig(t, store.InMemorySessionStore, id, map[string]string{"TOKEN": "stored"}, []string{absTestPath("stored")})

	client := newSpyCodexClient()
	agent := NewAgent(WithSessionStore(store))
	activeMeta := sessionMeta{
		Env:           map[string]string{"TOKEN": "stored"},
		ExtraPathDirs: []string{absTestPath("stored")},
	}
	active := newSession(agent, id, absTestPath("work"), nil, codex.Thread{
		ID:   "native-thread",
		Path: "/native/rollout.jsonl",
	}, client, activeMeta, nil)
	active.fingerprint = codexSessionStartFingerprint(codexSessionStart{
		Cwd:      absTestPath("work"),
		Meta:     activeMeta,
		ResumeID: string(id),
	})
	active.durableConfigRevision = 1
	active.durableConfigCommitted = true
	agent.sessions[id] = active
	agent.runtimeClient = client
	t.Cleanup(func() { active.fenceSession() })

	store.fail = true
	_, err := agent.ResumeSession(t.Context(), ResumeSessionRequest(id, absTestPath("work"), WithSessionMeta(CodexOptions{
		Env:           map[string]string{"TOKEN": "rotated"},
		ExtraPathDirs: []string{absTestPath("rotated")},
	}.Meta())))
	require.ErrorContains(t, err, "configuration append failed")
	require.True(t, active.pendingDurableSessionConfig(1))

	_, err = agent.ResumeSession(t.Context(), ResumeSessionRequest(id, absTestPath("work")))
	require.NoError(t, err)

	stored, err := agent.loadDurableSessionConfig(t.Context(), id)
	require.NoError(t, err)
	require.Equal(t, 2, stored.Revision)
	require.Equal(t, map[string]string{"TOKEN": "rotated"}, stored.Env)
	require.Equal(t, []string{absTestPath("rotated")}, stored.ExtraPathDirs)
}

type failNextConfigAppendStore struct {
	*InMemorySessionStore
	fail bool
}

func (s *failNextConfigAppendStore) Append(ctx context.Context, key SessionKey, entries []SessionStoreEntry) error {
	if key.Subpath == sessionConfigStoreSubpath && s.fail {
		s.fail = false

		return errors.New("configuration append failed")
	}

	return s.InMemorySessionStore.Append(ctx, key, entries)
}

func TestRetainedResumeCommitsChangedSessionCarriersBeforePublication(t *testing.T) {
	store := NewInMemorySessionStore()
	id := acp.SessionId("logical-session")
	require.NoError(t, store.Append(t.Context(), SessionKey{SessionID: string(id)}, []SessionStoreEntry{
		SessionStoreEntry(`{"type":"session_meta","payload":{"id":"native-thread"}}`),
	}))
	appendTestDurableSessionConfig(t, store, id, map[string]string{"TOKEN": "stored"}, []string{absTestPath("stored")})

	client := newSpyCodexClient()
	agent := NewAgent(WithSessionStore(store))
	agent.runtimeClient = client
	agent.runtimeEpoch = 7
	agent.retainedThreads[id] = &retainedRuntimeThread{
		sessionID: id,
		threadID:  "native-thread",
		path:      "/native/rollout.jsonl",
		client:    client,
		epoch:     7,
	}
	t.Cleanup(func() { require.NoError(t, agent.Close()) })

	rotatedPath := t.TempDir()
	_, err := agent.ResumeSession(t.Context(), ResumeSessionRequest(id, absTestPath("work"), WithSessionMeta(CodexOptions{
		Env:           map[string]string{"TOKEN": "rotated"},
		ExtraPathDirs: []string{rotatedPath},
	}.Meta())))
	require.NoError(t, err)
	require.NotNil(t, agent.activeSession(id))

	stored, err := agent.loadDurableSessionConfig(t.Context(), id)
	require.NoError(t, err)
	require.Equal(t, 2, stored.Revision)
	require.Equal(t, map[string]string{"TOKEN": "rotated"}, stored.Env)
	require.Equal(t, []string{rotatedPath}, stored.ExtraPathDirs)
}

func TestCloseCommitsChangedSessionCarriersWithoutPromptRows(t *testing.T) {
	store := NewInMemorySessionStore()
	id := acp.SessionId("logical-session")
	appendTestDurableSessionConfig(t, store, id, map[string]string{"TOKEN": "stored"}, []string{absTestPath("stored")})

	agent := NewAgent(WithSessionStore(store))
	session := &session{
		agent:                  agent,
		id:                     id,
		env:                    map[string]string{"TOKEN": "rotated"},
		extraPathDirs:          []string{absTestPath("rotated")},
		closeContained:         true,
		durableConfigRevision:  1,
		durableConfigCommitted: false,
	}

	require.NoError(t, session.Close(t.Context()))
	stored, err := agent.loadDurableSessionConfig(t.Context(), id)
	require.NoError(t, err)
	require.Equal(t, 2, stored.Revision)
	require.Equal(t, session.env, stored.Env)
	require.Equal(t, session.extraPathDirs, stored.ExtraPathDirs)
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
		extraPathDirs:         []string{absTestPath("rotated")},
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
