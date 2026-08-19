package codexacp

import (
	"context"
	"errors"
	"strings"
	"testing"
)

func TestInMemorySessionStoreContract(t *testing.T) {
	ctx := context.Background()
	store := NewInMemorySessionStore()
	main := SessionKey{SessionID: "s"}
	subA := SessionKey{SessionID: "s", Subpath: "a"}
	subB := SessionKey{SessionID: "s", Subpath: "b"}

	if SessionStoreMainSubpath != "" || SessionStoreFormat != "codex-rollout-jsonl-v1" {
		t.Fatalf("store constants = %q %q", SessionStoreMainSubpath, SessionStoreFormat)
	}
	if err := store.Append(ctx, main, []SessionStoreEntry{SessionStoreEntry(`{"one":1}`)}); err != nil {
		t.Fatalf("Append main: %v", err)
	}
	if err := store.Append(ctx, subA, []SessionStoreEntry{SessionStoreEntry(`{"sub":"a"}`)}); err != nil {
		t.Fatalf("Append sub: %v", err)
	}
	if got, err := store.Load(ctx, main); err != nil || len(got) != 1 {
		t.Fatalf("Load main = %q err=%v", got, err)
	}

	if err := store.Replace(ctx, main, []SessionStoreReplacement{
		{Key: main, Entries: []SessionStoreEntry{SessionStoreEntry(`{"two":2}`)}},
		{Key: subB, Entries: []SessionStoreEntry{SessionStoreEntry(`{"sub":"b"}`)}},
	}); err != nil {
		t.Fatalf("Replace: %v", err)
	}
	if oldSub, _ := store.Load(ctx, subA); len(oldSub) != 0 {
		t.Fatalf("old subpath survived Replace: %q", oldSub)
	}
	if subkeys, err := store.ListSubkeys(ctx, main); err != nil || len(subkeys) != 1 || subkeys[0] != "b" {
		t.Fatalf("ListSubkeys = %#v err=%v", subkeys, err)
	}
	summaries, err := store.ListSessions(ctx)
	if err != nil || len(summaries) != 1 || summaries[0].SessionID != "s" || summaries[0].UpdatedAtUnixMilli == 0 {
		t.Fatalf("ListSessions = %#v err=%v", summaries, err)
	}

	if err := store.Delete(ctx, main); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if got, _ := store.Load(ctx, main); len(got) != 0 {
		t.Fatalf("deleted main loaded: %q", got)
	}
	if got, _ := store.Load(ctx, subB); len(got) != 0 {
		t.Fatalf("deleted sub loaded: %q", got)
	}
	if err := store.Append(ctx, main, []SessionStoreEntry{SessionStoreEntry(`{"late":true}`)}); err != nil {
		t.Fatalf("Append tombstoned main: %v", err)
	}
	if got, _ := store.Load(ctx, main); len(got) != 0 {
		t.Fatalf("late append resurrected tombstoned main: %q", got)
	}
	if err := store.Append(ctx, subB, []SessionStoreEntry{SessionStoreEntry(`{"late":"sub"}`)}); err != nil {
		t.Fatalf("Append tombstoned sub: %v", err)
	}
	if got, _ := store.Load(ctx, subB); len(got) != 0 {
		t.Fatalf("late append resurrected tombstoned sub: %q", got)
	}
}

// TestInMemorySessionStoreTombstoneIsFinal pins tombstone finality where it has
// to live: in the store. An adapter-level deletion marker is not a substitute,
// because the store is what a second writer, a late tail, or a restarted
// adapter reaches. Both writing verbs are covered — a whole replacement
// generation resurrects strictly more than an append does.
func TestInMemorySessionStoreTombstoneIsFinal(t *testing.T) {
	ctx := context.Background()
	main := SessionKey{SessionID: "s"}
	sub := SessionKey{SessionID: "s", Subpath: "a"}

	for _, test := range []struct {
		name  string
		write func(*InMemorySessionStore) error
	}{
		{"append", func(store *InMemorySessionStore) error {
			return store.Append(ctx, main, []SessionStoreEntry{SessionStoreEntry(`{"late":true}`)})
		}},
		{"replace", func(store *InMemorySessionStore) error {
			return store.Replace(ctx, main, []SessionStoreReplacement{
				{Key: main, Entries: []SessionStoreEntry{SessionStoreEntry(`{"late":true}`)}},
				{Key: sub, Entries: []SessionStoreEntry{SessionStoreEntry(`{"late":"sub"}`)}},
			})
		}},
	} {
		t.Run(test.name, func(t *testing.T) {
			store := NewInMemorySessionStore()
			if err := store.Append(ctx, main, []SessionStoreEntry{SessionStoreEntry(`{"one":1}`)}); err != nil {
				t.Fatalf("Append: %v", err)
			}

			if err := store.Delete(ctx, main); err != nil {
				t.Fatalf("Delete: %v", err)
			}

			// The deleted state is already the caller's answer, so the refusal is
			// silent rather than an error.
			if err := test.write(store); err != nil {
				t.Fatalf("write over tombstone: %v", err)
			}

			if got, _ := store.Load(ctx, main); len(got) != 0 {
				t.Fatalf("write resurrected the tombstoned main key: %q", got)
			}

			if got, _ := store.Load(ctx, sub); len(got) != 0 {
				t.Fatalf("write resurrected a subpath under the tombstoned session: %q", got)
			}

			summaries, err := store.ListSessions(ctx)
			if err != nil || len(summaries) != 0 {
				t.Fatalf("ListSessions = %#v err=%v", summaries, err)
			}

			subkeys, err := store.ListSubkeys(ctx, main)
			if err != nil || len(subkeys) != 0 {
				t.Fatalf("ListSubkeys = %#v err=%v", subkeys, err)
			}
		})
	}
}

func TestInMemorySessionStoreReplaceValidation(t *testing.T) {
	ctx := context.Background()
	store := NewInMemorySessionStore()
	tests := []struct {
		name         string
		main         SessionKey
		replacements []SessionStoreReplacement
	}{
		{name: "missing session id", main: SessionKey{}, replacements: []SessionStoreReplacement{{Key: SessionKey{}}}},
		{name: "main subpath", main: SessionKey{SessionID: "s", Subpath: "x"}, replacements: []SessionStoreReplacement{{Key: SessionKey{SessionID: "s", Subpath: "x"}}}},
		{name: "missing main", main: SessionKey{SessionID: "s"}, replacements: []SessionStoreReplacement{{Key: SessionKey{SessionID: "s", Subpath: "x"}}}},
		{name: "wrong session", main: SessionKey{SessionID: "s"}, replacements: []SessionStoreReplacement{{Key: SessionKey{SessionID: "other"}}}},
		{name: "duplicate main", main: SessionKey{SessionID: "s"}, replacements: []SessionStoreReplacement{{Key: SessionKey{SessionID: "s"}}, {Key: SessionKey{SessionID: "s"}}}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if err := store.Replace(ctx, test.main, test.replacements); err == nil {
				t.Fatal("Replace accepted invalid generation")
			}
		})
	}
}

func TestInMemorySessionStoreEdgeBranches(t *testing.T) {
	ctx := context.Background()
	canceled, cancel := context.WithCancel(ctx)
	cancel()
	var nilStore *InMemorySessionStore
	key := SessionKey{SessionID: "s"}

	if err := nilStore.Append(ctx, key, []SessionStoreEntry{SessionStoreEntry(`{"x":1}`)}); err == nil {
		t.Fatal("nil Append succeeded")
	}
	if _, err := nilStore.Load(ctx, key); err == nil {
		t.Fatal("nil Load succeeded")
	}
	if err := nilStore.Replace(ctx, key, []SessionStoreReplacement{{Key: key}}); err == nil {
		t.Fatal("nil Replace succeeded")
	}
	if err := nilStore.Delete(ctx, key); err == nil {
		t.Fatal("nil Delete succeeded")
	}
	if _, err := nilStore.ListSessions(ctx); err == nil {
		t.Fatal("nil ListSessions succeeded")
	}
	if _, err := nilStore.ListSubkeys(ctx, key); err == nil {
		t.Fatal("nil ListSubkeys succeeded")
	}

	store := &InMemorySessionStore{}
	if err := store.Append(canceled, key, []SessionStoreEntry{SessionStoreEntry(`{"x":1}`)}); !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled Append err=%v", err)
	}
	if _, err := store.Load(canceled, key); !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled Load err=%v", err)
	}
	if err := store.Replace(canceled, key, []SessionStoreReplacement{{Key: key}}); !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled Replace err=%v", err)
	}
	if err := store.Delete(canceled, key); !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled Delete err=%v", err)
	}
	if _, err := store.ListSessions(canceled); !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled ListSessions err=%v", err)
	}
	if _, err := store.ListSubkeys(canceled, key); !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled ListSubkeys err=%v", err)
	}

	if err := store.Append(ctx, key, nil); err != nil {
		t.Fatalf("empty Append returned error: %v", err)
	}
	if err := store.Append(ctx, key, []SessionStoreEntry{SessionStoreEntry(`{"x":1}`)}); err != nil {
		t.Fatalf("Append with zero-value maps returned error: %v", err)
	}
	loaded, err := store.Load(ctx, key)
	if err != nil || len(loaded) != 1 {
		t.Fatalf("Load after append = %q err=%v", loaded, err)
	}
	loaded[0][0] = '{'
	loadedAgain, err := store.Load(ctx, key)
	if err != nil || string(loadedAgain[0]) != `{"x":1}` {
		t.Fatalf("Load did not clone entries: %q err=%v", loadedAgain, err)
	}
}

// TestInMemorySessionStoreReplaceEmptyEntrySurvives pins the rule that Replace
// keeps exactly the keys it lists: a listed key whose Entries are empty stays
// live and merely loses its content, so emptiness is never read as removal.
// Delete is the only tombstone path, and it is the one that cascades to
// subpaths.
func TestInMemorySessionStoreReplaceEmptyEntrySurvives(t *testing.T) {
	ctx := context.Background()
	store := NewInMemorySessionStore()
	key := SessionKey{SessionID: "s"}

	if err := store.Append(ctx, key, []SessionStoreEntry{SessionStoreEntry(`{"x":1}`)}); err != nil {
		t.Fatalf("Append returned error: %v", err)
	}

	if err := store.Replace(ctx, key, []SessionStoreReplacement{{Key: key}}); err != nil {
		t.Fatalf("Replace empty entries returned error: %v", err)
	}
	if got, err := store.Load(ctx, key); err != nil || len(got) != 0 {
		t.Fatalf("Load empty-entry key = %q err=%v", got, err)
	}
	if summaries, err := store.ListSessions(ctx); err != nil || len(summaries) != 1 || summaries[0].SessionID != "s" {
		t.Fatalf("empty-entry key did not survive Replace: %#v err=%v", summaries, err)
	}

	if err := store.Delete(ctx, key); err != nil {
		t.Fatalf("Delete returned error: %v", err)
	}
	if got, err := store.Load(ctx, key); err != nil || got != nil {
		t.Fatalf("Load tombstoned = %q err=%v", got, err)
	}
	if summaries, err := store.ListSessions(ctx); err != nil || len(summaries) != 0 {
		t.Fatalf("ListSessions after tombstone = %#v err=%v", summaries, err)
	}
	sub := SessionKey{SessionID: "s", Subpath: "sub"}
	if err := store.Append(ctx, sub, []SessionStoreEntry{SessionStoreEntry(`{"sub":true}`)}); err != nil {
		t.Fatalf("Append tombstoned sub returned error: %v", err)
	}
	if subkeys, err := store.ListSubkeys(ctx, key); err != nil || len(subkeys) != 0 {
		t.Fatalf("ListSubkeys tombstoned = %#v err=%v", subkeys, err)
	}
	if !store.isTombstonedLocked(sub) {
		t.Fatal("subpath did not inherit main tombstone")
	}
}

func TestInMemorySessionStoreListAndDeleteBranches(t *testing.T) {
	ctx := context.Background()
	zeroReplace := &InMemorySessionStore{}
	if err := zeroReplace.Replace(ctx, SessionKey{SessionID: "zero"}, []SessionStoreReplacement{{
		Key:     SessionKey{SessionID: "zero"},
		Entries: []SessionStoreEntry{SessionStoreEntry(`{"zero":true}`)},
	}}); err != nil {
		t.Fatalf("zero Replace: %v", err)
	}
	zeroDelete := &InMemorySessionStore{}
	if err := zeroDelete.Delete(ctx, SessionKey{SessionID: "zero"}); err != nil {
		t.Fatalf("zero Delete: %v", err)
	}
	if zeroDelete.isTombstonedLocked(SessionKey{SessionID: "other"}) {
		t.Fatal("unexpected tombstone in zero delete store")
	}

	store := NewInMemorySessionStore()
	mainA := SessionKey{SessionID: "a"}
	mainB := SessionKey{SessionID: "b"}
	subA := SessionKey{SessionID: "a", Subpath: "sub"}
	if err := store.Append(ctx, mainB, []SessionStoreEntry{SessionStoreEntry(`{"b":1}`)}); err != nil {
		t.Fatalf("Append mainB: %v", err)
	}
	if err := store.Append(ctx, mainA, []SessionStoreEntry{SessionStoreEntry(`{"a":1}`)}); err != nil {
		t.Fatalf("Append mainA: %v", err)
	}
	if err := store.Append(ctx, subA, []SessionStoreEntry{SessionStoreEntry(`{"sub":1}`)}); err != nil {
		t.Fatalf("Append subA: %v", err)
	}
	summaries, err := store.ListSessions(ctx)
	if err != nil {
		t.Fatalf("ListSessions: %v", err)
	}
	if len(summaries) != 2 || summaries[0].SessionID == "" || summaries[1].SessionID == "" {
		t.Fatalf("summaries = %#v", summaries)
	}

	if delErr := store.Delete(ctx, subA); delErr != nil {
		t.Fatalf("Delete subA: %v", delErr)
	}
	if got, loadErr := store.Load(ctx, subA); loadErr != nil || len(got) != 0 {
		t.Fatalf("deleted sub load = %q err=%v", got, loadErr)
	}
	if got, loadErr := store.Load(ctx, mainA); loadErr != nil || len(got) != 1 {
		t.Fatalf("main load after sub delete = %q err=%v", got, loadErr)
	}

	missingSub := SessionKey{SessionID: "a", Subpath: "missing"}
	if delErr := store.Delete(ctx, missingSub); delErr != nil {
		t.Fatalf("Delete missing sub: %v", delErr)
	}
	if !store.isTombstonedLocked(missingSub) {
		t.Fatal("missing sub delete did not create tombstone")
	}
	if store.isTombstonedLocked(SessionKey{SessionID: "other", Subpath: "sub"}) {
		t.Fatal("unrelated subpath was tombstoned")
	}

	sortStore := &InMemorySessionStore{
		entries: map[SessionKey][]SessionStoreEntry{
			{SessionID: "older"}: {SessionStoreEntry(`{"older":true}`)},
			{SessionID: "newer"}: {SessionStoreEntry(`{"newer":true}`)},
		},
		updatedAt: map[SessionKey]int64{
			{SessionID: "older"}: 1,
			{SessionID: "newer"}: 2,
		},
	}
	sorted, err := sortStore.ListSessions(ctx)
	if err != nil || len(sorted) != 2 || sorted[0].SessionID != "newer" {
		t.Fatalf("sorted sessions = %#v err=%v", sorted, err)
	}
}

func TestInMemorySessionStoreEmptySessionIDKeys(t *testing.T) {
	ctx := context.Background()
	store := NewInMemorySessionStore()

	if err := store.Append(ctx, SessionKey{}, []SessionStoreEntry{SessionStoreEntry(`{"a":1}`)}); err == nil || !strings.Contains(err.Error(), "session id is required") {
		t.Fatalf("Append with empty session id = %v, want session id is required", err)
	}
	if err := store.Append(ctx, SessionKey{}, nil); err != nil {
		t.Fatalf("empty Append with empty session id = %v, want pure no-op", err)
	}
	if err := store.Replace(ctx, SessionKey{}, nil); err == nil || err.Error() != "session id is required" {
		t.Fatalf("Replace with empty session id = %v, want exact error", err)
	}

	if err := store.Append(ctx, SessionKey{SessionID: "live"}, []SessionStoreEntry{SessionStoreEntry(`{"a":1}`)}); err != nil {
		t.Fatalf("Append returned error: %v", err)
	}

	if err := store.Delete(ctx, SessionKey{}); err != nil {
		t.Fatalf("Delete with empty session id = %v, want pure no-op", err)
	}
	if err := store.Delete(ctx, SessionKey{Subpath: "sub"}); err != nil {
		t.Fatalf("Delete with empty session id and subpath = %v, want pure no-op", err)
	}

	entries, err := store.Load(ctx, SessionKey{SessionID: "live"})
	if err != nil || len(entries) != 1 {
		t.Fatalf("live session after empty-id delete = %#v err=%v", entries, err)
	}

	summaries, err := store.ListSessions(ctx)
	if err != nil || len(summaries) != 1 || summaries[0].SessionID != "live" {
		t.Fatalf("summaries after empty-id delete = %#v err=%v", summaries, err)
	}
}
