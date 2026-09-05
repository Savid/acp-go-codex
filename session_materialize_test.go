package codexacp

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/coder/acp-go-sdk"
	"github.com/savid/acp-go-codex/internal/codex"
	"github.com/stretchr/testify/require"
)

func testStoredRolloutEntries(threadID string, timestamp string) []SessionStoreEntry {
	meta := map[string]any{"id": threadID}
	if timestamp != "" {
		meta["timestamp"] = timestamp
	}

	row, err := json.Marshal(map[string]any{"type": "session_meta", "payload": meta})
	if err != nil {
		panic(err)
	}

	return []SessionStoreEntry{SessionStoreEntry(row)}
}

// A stored rollout is adoptable only where Codex itself would have written it:
// under `sessions/` in the app-server's own home, named for the session's own
// start time and its native thread id.
func TestNativeRolloutResidenceReconstructsTheCodexPath(t *testing.T) {
	home := filepath.Join(t.TempDir(), "home")

	stamp := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
	entries := testStoredRolloutEntries("01a0-thread_9", stamp.Format(time.RFC3339))

	path, err := nativeRolloutResidence(home, entries)
	require.NoError(t, err)

	local := stamp.Local()
	require.Equal(t, filepath.Join(
		home,
		nativeSessionsDirName,
		local.Format("2006"), local.Format("01"), local.Format("02"),
		"rollout-"+local.Format(nativeRolloutStampLayout)+"-01a0-thread_9.jsonl",
	), path)

	again, err := nativeRolloutResidence(home, entries)
	require.NoError(t, err)
	require.Equal(t, path, again, "one stored session resolved to two residences")
}

// Without a home there is no app-server home to be resident in, and without a
// usable native thread id there is no name the app-server would resolve.
func TestNativeRolloutResidenceRefusesUnplaceableRollouts(t *testing.T) {
	_, err := nativeRolloutResidence("", testStoredRolloutEntries("thread", ""))
	require.ErrorContains(t, err, "codex home is unknown")

	for name, threadID := range map[string]string{
		"empty":     "",
		"separator": "../escape",
		"dot":       "thread.1",
		"oversize":  strings.Repeat("a", 129),
	} {
		t.Run(name, func(t *testing.T) {
			_, err := nativeRolloutResidence(t.TempDir(), testStoredRolloutEntries(threadID, ""))
			require.ErrorContains(t, err, "native thread id")
		})
	}
}

// A rollout whose own rows do not date it still resolves to one path; the clock
// stands in for the session start the rows never recorded.
func TestNativeRolloutResidenceStampFallsBackToTheClock(t *testing.T) {
	original := materializedRolloutNow
	fixed := time.Date(2031, 7, 8, 9, 10, 11, 0, time.Local)
	materializedRolloutNow = func() time.Time { return fixed }

	t.Cleanup(func() { materializedRolloutNow = original })

	for name, entries := range map[string][]SessionStoreEntry{
		"absent timestamp": testStoredRolloutEntries("thread", ""),
		"unparsable":       testStoredRolloutEntries("thread", "yesterday"),
		"no session_meta": {
			SessionStoreEntry(`{"type":"event_msg","payload":{}}`),
			SessionStoreEntry(`{"type":"session_meta","payload":{"id":"thread"}}`),
		},
	} {
		t.Run(name, func(t *testing.T) {
			require.Equal(t, fixed, nativeRolloutResidenceStamp(entries))
		})
	}
}

func TestMaterializeRolloutPlacesAWholeFile(t *testing.T) {
	home := filepath.Join(t.TempDir(), "home")
	entries := append(
		testStoredRolloutEntries("thread-1", "2026-01-02T03:04:05Z"),
		SessionStoreEntry(`{"type":"response_item","payload":{"type":"message"}}`),
	)

	path, err := materializeRollout(home, entries)
	require.NoError(t, err)

	expected, err := nativeRolloutResidence(home, entries)
	require.NoError(t, err)
	require.Equal(t, expected, path)

	data, err := os.ReadFile(path)
	require.NoError(t, err)
	require.Equal(t, string(entries[0])+"\n"+string(entries[1])+"\n", string(data))

	// Nothing but the finished rollout is left in the day directory, so a scan
	// never sees a staging file it would try to read as one.
	names, err := os.ReadDir(filepath.Dir(path))
	require.NoError(t, err)
	require.Len(t, names, 1)

	require.NoError(t, removeMaterializedRollout(path))
	require.NoFileExists(t, path)
	require.NoError(t, removeMaterializedRollout(path))
	require.NoError(t, removeMaterializedRollout(""))
}

func TestMaterializeRolloutEmptyEntries(t *testing.T) {
	path, err := materializeRollout(t.TempDir(), nil)
	require.NoError(t, err)
	require.Empty(t, path)
}

func TestMaterializeRolloutFileHookErrorBranches(t *testing.T) {
	origCreate := createMaterializedRolloutTemp
	origRemove := removeMaterializedRolloutFile
	origRename := renameMaterializedRolloutFile
	origMkdir := mkdirMaterializedRolloutDir

	t.Cleanup(func() {
		createMaterializedRolloutTemp = origCreate
		removeMaterializedRolloutFile = origRemove
		renameMaterializedRolloutFile = origRename
		mkdirMaterializedRolloutDir = origMkdir
	})

	home := t.TempDir()
	entries := testStoredRolloutEntries("thread-1", "2026-01-02T03:04:05Z")

	mkdirMaterializedRolloutDir = func(string, os.FileMode) error { return errors.New("mkdir failed") }
	_, err := materializeRollout(home, entries)
	require.ErrorContains(t, err, "create native rollout directory")

	mkdirMaterializedRolloutDir = origMkdir
	createMaterializedRolloutTemp = func(string) (materializedRolloutFile, error) {
		return nil, errors.New("create failed")
	}
	_, err = materializeRollout(home, entries)
	require.ErrorContains(t, err, "create materialized rollout")

	removed := []string{}
	removeMaterializedRolloutFile = func(path string) error {
		removed = append(removed, path)

		return nil
	}

	createMaterializedRolloutTemp = func(string) (materializedRolloutFile, error) {
		return &fakeRolloutFile{name: "staged", failWriteAt: 1}, nil
	}
	_, err = materializeRollout(home, entries)
	require.ErrorContains(t, err, "write materialized rollout:")

	createMaterializedRolloutTemp = func(string) (materializedRolloutFile, error) {
		return &fakeRolloutFile{name: "staged", failWriteAt: 2}, nil
	}
	_, err = materializeRollout(home, entries)
	require.ErrorContains(t, err, "write materialized rollout newline")

	createMaterializedRolloutTemp = func(string) (materializedRolloutFile, error) {
		return &fakeRolloutFile{name: "staged", closeErr: errors.New("close failed")}, nil
	}
	_, err = materializeRollout(home, entries)
	require.ErrorContains(t, err, "close materialized rollout")

	createMaterializedRolloutTemp = func(string) (materializedRolloutFile, error) {
		return &fakeRolloutFile{name: "staged"}, nil
	}
	renameMaterializedRolloutFile = func(string, string) error { return errors.New("rename failed") }
	_, err = materializeRollout(home, entries)
	require.ErrorContains(t, err, "place materialized rollout")
	require.Equal(t, []string{"staged", "staged", "staged", "staged"}, removed)

	removeMaterializedRolloutFile = func(string) error { return os.ErrNotExist }
	require.NoError(t, removeMaterializedRollout("missing"))

	removeMaterializedRolloutFile = func(string) error { return errors.New("remove failed") }
	require.Error(t, removeMaterializedRollout("bad"))
}

func TestMaterializeStoredRolloutReleasesReservationOnFailure(t *testing.T) {
	released := false
	agent := NewAgent(WithHome(""))
	agent.options.Home = ""
	agent.options.implicitEnvironment = map[string]string{}

	path, release, bytes, err := agent.materializeStoredRollout(
		t.Context(),
		testStoredRolloutEntries("thread-1", ""),
		func() { released = true },
	)
	require.Error(t, err)
	require.Empty(t, path)
	require.Nil(t, release)
	require.Zero(t, bytes)
	require.True(t, released)
}

func TestManagedMaterializeUsesAuthorityAndNeverOpensTheResidence(t *testing.T) {
	home := filepath.Join(t.TempDir(), "inaccessible-home")
	// An ordinary write would fail: the home's path is occupied by a file.
	require.NoError(t, os.WriteFile(home, []byte("untouched"), 0o600))
	entries := testStoredRolloutEntries("thread-managed", "2026-09-05T01:02:03Z")
	entries = append(entries, SessionStoreEntry("  {\"type\":\"event_msg\",\"payload\":{}} \t"))
	target, err := nativeRolloutResidence(home, entries)
	require.NoError(t, err)
	refusal := errors.New("write refused")
	for _, outcome := range []string{"accepted", "refusal", "panic", "invalid thread"} {
		t.Run(outcome, func(t *testing.T) {
			calls := 0
			ctx := t.Context()
			authority := authorityCoverageHost{
				environment: func() map[string]string { return map[string]string{"HOME": home} },
				write: func(gotCtx context.Context, path string, records [][]byte) error {
					calls++
					require.Equal(t, ctx, gotCtx)
					require.Equal(t, target, path)
					require.Equal(t, [][]byte{[]byte(entries[0]), []byte(entries[1])}, records)
					records[0][0] = '!'
					if outcome == "panic" {
						panic("write panicked")
					}
					if outcome == "refusal" {
						return refusal
					}

					return nil
				},
			}
			agent := NewAgent(WithHostAuthority(authority), WithHome(home))
			input := entries
			if outcome == "invalid thread" {
				input = testStoredRolloutEntries("../escape", "")
			}
			released := false
			path, release, size, writeErr := agent.materializeStoredRollout(ctx, input, func() { released = true })
			require.Equal(t, byte('{'), entries[0][0], "the authority mutated store records")
			if outcome == "accepted" {
				require.NoError(t, writeErr)
				require.Equal(t, target, path)
				require.Equal(t, materializedRolloutBytes(entries), size)
				require.False(t, released)
				release()
			} else {
				require.Error(t, writeErr)
				require.Empty(t, path)
				require.Zero(t, size)
				require.Nil(t, release)
				if outcome == "refusal" {
					require.ErrorIs(t, writeErr, refusal)
				}
				if outcome == "panic" {
					require.ErrorIs(t, writeErr, ErrHostAuthorityUnavailable)
				}
			}
			require.True(t, released)
			if outcome == "invalid thread" {
				require.Zero(t, calls)
			} else {
				require.Equal(t, 1, calls)
			}
			contents, readErr := os.ReadFile(home)
			require.NoError(t, readErr)
			require.Equal(t, "untouched", string(contents))
		})
	}
}

func TestMaterializeStoredRolloutEmptyEntries(t *testing.T) {
	released := false
	agent := NewAgent(WithHome(t.TempDir()))

	path, release, bytes, err := agent.materializeStoredRollout(t.Context(), nil, func() { released = true })
	require.NoError(t, err)
	require.Empty(t, path)
	require.NotNil(t, release)
	require.Zero(t, bytes)
	require.True(t, released)
	release()
}

type fakeRolloutFile struct {
	name        string
	writes      int
	failWriteAt int
	closeErr    error
}

func (f *fakeRolloutFile) Name() string { return f.name }
func (f *fakeRolloutFile) Write([]byte) (int, error) {
	f.writes++
	if f.failWriteAt == f.writes {
		return 0, errors.New("write failed")
	}

	return 1, nil
}
func (f *fakeRolloutFile) Close() error { return f.closeErr }

// The resident rollout is the store's own rows written back out. A mirror that
// read it from the top would append the whole restored history to the store a
// second time — the store would then hold two `session_meta` rows and refuse
// every later load of that session.
func TestRestoredSessionMirrorsOnlyWhatTheNativeSideAddsAfterIt(t *testing.T) {
	for name, restore := range map[string]func(context.Context, *Agent, acp.SessionId, string) error{
		"resume": func(ctx context.Context, agent *Agent, id acp.SessionId, cwd string) error {
			_, err := agent.ResumeSession(ctx, ResumeSessionRequest(id, cwd))

			return err
		},
		"load": func(ctx context.Context, agent *Agent, id acp.SessionId, cwd string) error {
			_, err := agent.LoadSession(ctx, LoadSessionRequest(id, cwd))

			return err
		},
	} {
		t.Run(name, func(t *testing.T) {
			ctx := t.Context()
			store := NewInMemorySessionStore()
			id := acp.SessionId("logical-session")
			entries := append(
				testStoredRolloutEntries("native-thread", "2026-01-02T03:04:05Z"),
				SessionStoreEntry(`{"type":"event_msg","payload":{"type":"task_complete"}}`),
			)
			require.NoError(t, store.Append(ctx, SessionKey{SessionID: string(id)}, entries))
			appendTestDurableSessionConfig(t, store, id, nil, nil)

			client := newSpyCodexClient()
			agent := NewAgent(
				WithSessionStore(store),
				WithHome(t.TempDir()),
				withClientFactory(func(context.Context, codex.Options) (codex.Client, error) { return client, nil }),
			)

			t.Cleanup(func() { require.NoError(t, agent.Close()) })

			require.NoError(t, restore(ctx, agent, id, t.TempDir()))

			session := agent.activeSession(id)
			require.NotNil(t, session)
			require.Equal(t, len(entries), session.mirroredRows)

			// One mirror pass over the untouched resident rollout owes the store
			// nothing, so the stored generation is still exactly what was
			// restored.
			require.NoError(t, session.mirrorAndEmitRollout(ctx))

			after, err := store.Load(ctx, SessionKey{SessionID: string(id)})
			require.NoError(t, err)
			require.Len(t, after, len(entries))
			require.NoError(t, validateStoredRolloutEntriesRoundTrip(after))
		})
	}
}

func validateStoredRolloutEntriesRoundTrip(entries []SessionStoreEntry) error {
	_, err := validateStoredRolloutEntries(entries)

	return err
}
