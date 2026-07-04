package codexacp

import (
	"context"
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

func TestInMemorySessionStoreReplaceValidation(t *testing.T) {
	ctx := context.Background()
	store := NewInMemorySessionStore()
	tests := []struct {
		name         string
		main         SessionKey
		replacements []SessionStoreReplacement
	}{
		{name: "main subpath", main: SessionKey{SessionID: "s", Subpath: "x"}, replacements: []SessionStoreReplacement{{Key: SessionKey{SessionID: "s", Subpath: "x"}}}},
		{name: "missing main", main: SessionKey{SessionID: "s"}, replacements: []SessionStoreReplacement{{Key: SessionKey{SessionID: "s", Subpath: "x"}}}},
		{name: "wrong session", main: SessionKey{SessionID: "s"}, replacements: []SessionStoreReplacement{{Key: SessionKey{SessionID: "other"}}}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if err := store.Replace(ctx, test.main, test.replacements); err == nil {
				t.Fatal("Replace accepted invalid generation")
			}
		})
	}
}
