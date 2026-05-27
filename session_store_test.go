package codexacp

import (
	"context"
	"encoding/json"
	"testing"
	"time"
)

func TestSessionStoreAdditionalBranches(t *testing.T) {
	ctx := context.Background()
	canceled, cancel := context.WithCancel(ctx)
	cancel()
	store := NewInMemorySessionStore()
	key := SessionKey{ProjectKey: "p", SessionID: "s"}
	if err := store.Append(ctx, key, nil); err != nil {
		t.Fatalf("empty Append returned error: %v", err)
	}
	if _, err := store.ListSessions(canceled, "p"); err == nil {
		t.Fatal("canceled ListSessions succeeded")
	}
	if err := store.Delete(canceled, key); err == nil {
		t.Fatal("canceled Delete succeeded")
	}
	if err := store.ReplaceSession(canceled, key, nil); err == nil {
		t.Fatal("canceled ReplaceSession succeeded")
	}
	var nilStore *InMemorySessionStore
	if _, err := nilStore.ListSessions(ctx, "p"); err == nil {
		t.Fatal("nil ListSessions succeeded")
	}
	if err := nilStore.Delete(ctx, key); err == nil {
		t.Fatal("nil Delete succeeded")
	}
	if err := nilStore.ReplaceSession(ctx, key, nil); err == nil {
		t.Fatal("nil ReplaceSession succeeded")
	}
	uninitialized := &InMemorySessionStore{}
	if err := uninitialized.Append(ctx, key, []SessionStoreEntry{SessionStoreEntry(`{"a":1}`)}); err != nil {
		t.Fatalf("uninitialized Append returned error: %v", err)
	}
	if err := uninitialized.ReplaceSession(ctx, key, []SessionStoreReplacement{{Key: key}}); err != nil {
		t.Fatalf("empty replacement returned error: %v", err)
	}
	if _, err := projectKeyForDirectory(""); err == nil {
		t.Fatal("empty project key succeeded")
	}
	if sanitizeSessionProjectPath("") != "-" {
		t.Fatal("empty sanitized path did not use fallback")
	}
}

func TestSessionStoreBranches(t *testing.T) {
	ctx := context.Background()
	store := NewInMemorySessionStore()
	key := SessionKey{ProjectKey: "project", SessionID: "session"}
	sub := SessionKey{ProjectKey: "project", SessionID: "session", Subpath: "sub"}

	if err := store.Append(ctx, key, []SessionStoreEntry{json.RawMessage(`{"type":"a"}`)}); err != nil {
		t.Fatalf("Append returned error: %v", err)
	}
	if err := store.Append(ctx, sub, []SessionStoreEntry{json.RawMessage(`{"type":"b"}`)}); err != nil {
		t.Fatalf("Append sub returned error: %v", err)
	}
	loaded, err := store.Load(ctx, key)
	if err != nil || len(loaded) != 1 {
		t.Fatalf("Load len=%d err=%v", len(loaded), err)
	}
	loaded[0][0] = '{'
	again, _ := store.Load(ctx, key)
	if string(again[0]) != `{"type":"a"}` {
		t.Fatalf("store did not clone entries: %s", again[0])
	}
	if err := store.ReplaceSession(ctx, key, []SessionStoreReplacement{{Key: key, Entries: []SessionStoreEntry{json.RawMessage(`{"type":"c"}`)}}}); err != nil {
		t.Fatalf("ReplaceSession returned error: %v", err)
	}
	summaries, err := store.ListSessions(ctx, "project")
	if err != nil || len(summaries) != 1 {
		t.Fatalf("ListSessions len=%d err=%v", len(summaries), err)
	}
	if err := store.Delete(ctx, key); err != nil {
		t.Fatalf("Delete returned error: %v", err)
	}
	loaded, _ = store.Load(ctx, sub)
	if len(loaded) != 0 {
		t.Fatal("Delete main did not cascade")
	}

	var nilStore *InMemorySessionStore
	if err := nilStore.Append(ctx, key, []SessionStoreEntry{json.RawMessage(`{}`)}); err == nil {
		t.Fatal("nil Append succeeded")
	}
	if err := store.ReplaceSession(ctx, key, []SessionStoreReplacement{{Key: SessionKey{ProjectKey: "other", SessionID: "session"}}}); err == nil {
		t.Fatal("mismatched replacement succeeded")
	}
	if _, err := projectKeyForDirectory("relative"); err == nil {
		t.Fatal("relative project key succeeded")
	}
}

func TestSessionStoreListOrderingAndStoredSessions(t *testing.T) {
	store := NewInMemorySessionStore()
	if err := store.ReplaceSession(context.Background(), SessionKey{ProjectKey: "p", SessionID: "s"}, []SessionStoreReplacement{{Key: SessionKey{ProjectKey: "p", SessionID: "s"}}}); err != nil {
		t.Fatalf("replace empty replacement returned error: %v", err)
	}
	if err := store.Append(context.Background(), SessionKey{ProjectKey: "p", SessionID: "b"}, []SessionStoreEntry{SessionStoreEntry(`{"b":1}`)}); err != nil {
		t.Fatalf("append b: %v", err)
	}
	time.Sleep(time.Millisecond)
	if err := store.Append(context.Background(), SessionKey{ProjectKey: "p", SessionID: "a"}, []SessionStoreEntry{SessionStoreEntry(`{"a":1}`)}); err != nil {
		t.Fatalf("append a: %v", err)
	}
	if err := store.Append(context.Background(), SessionKey{ProjectKey: "p", SessionID: "a", Subpath: "sub"}, []SessionStoreEntry{SessionStoreEntry(`{"sub":1}`)}); err != nil {
		t.Fatalf("append sub: %v", err)
	}
	summaries, err := store.ListSessions(context.Background(), "p")
	if err != nil || len(summaries) != 2 || summaries[0].SessionID != "a" {
		t.Fatalf("summaries=%#v err=%v", summaries, err)
	}
	store.mu.Lock()
	store.mtime[SessionKey{ProjectKey: "p", SessionID: "a"}] = 1
	store.mtime[SessionKey{ProjectKey: "p", SessionID: "b"}] = 1
	store.mu.Unlock()
	if summaries, err := store.ListSessions(context.Background(), "p"); err != nil || len(summaries) != 2 || summaries[0].SessionID != "a" {
		t.Fatalf("tie summaries=%#v err=%v", summaries, err)
	}
	if err := store.Delete(context.Background(), SessionKey{ProjectKey: "p", SessionID: "a", Subpath: "sub"}); err != nil {
		t.Fatalf("delete subpath: %v", err)
	}
	if entries, _ := store.Load(context.Background(), SessionKey{ProjectKey: "p", SessionID: "a"}); len(entries) == 0 {
		t.Fatal("delete subpath removed main session")
	}
	uninitialized := &InMemorySessionStore{}
	if err := uninitialized.ReplaceSession(context.Background(), SessionKey{ProjectKey: "p", SessionID: "s"}, []SessionStoreReplacement{{Key: SessionKey{ProjectKey: "p", SessionID: "s"}, Entries: []SessionStoreEntry{SessionStoreEntry(`{"x":1}`)}}}); err != nil {
		t.Fatalf("uninitialized replace returned error: %v", err)
	}
	if summaries, err := uninitialized.ListSessions(context.Background(), "missing"); err != nil || len(summaries) != 0 {
		t.Fatalf("missing summaries=%#v err=%v", summaries, err)
	}
	storeAgent := NewAgent(WithSessionStore(store))
	projectKey, err := projectKeyForDirectory("/tmp/project")
	if err != nil {
		t.Fatalf("project key: %v", err)
	}
	if err := store.Append(context.Background(), SessionKey{ProjectKey: projectKey, SessionID: "listed"}, []SessionStoreEntry{SessionStoreEntry(`{"listed":1}`)}); err != nil {
		t.Fatalf("append listed: %v", err)
	}
	if stored, err := storeAgent.listStoredSessions(context.Background(), "/tmp/project", nil); err != nil {
		t.Fatalf("listStoredSessions append branch returned error: %v", err)
	} else if len(stored) == 0 {
		t.Fatal("listStoredSessions returned no stored sessions")
	}
}

func TestSessionStoreErrorBranches(t *testing.T) {
	ctx := context.Background()
	var nilStore *InMemorySessionStore
	key := SessionKey{ProjectKey: "p", SessionID: "s"}
	if err := nilStore.Append(ctx, key, []SessionStoreEntry{SessionStoreEntry(`{"x":1}`)}); err == nil {
		t.Fatal("nil Append succeeded")
	}
	if _, err := nilStore.Load(ctx, key); err == nil {
		t.Fatal("nil Load succeeded")
	}
	if _, err := nilStore.ListSessions(ctx, "p"); err == nil {
		t.Fatal("nil ListSessions succeeded")
	}
	if err := nilStore.Delete(ctx, key); err == nil {
		t.Fatal("nil Delete succeeded")
	}
	if err := nilStore.ReplaceSession(ctx, key, nil); err == nil {
		t.Fatal("nil ReplaceSession succeeded")
	}

	canceled := canceledContext()
	store := NewInMemorySessionStore()
	if err := store.Append(canceled, key, nil); err == nil {
		t.Fatal("canceled Append succeeded")
	}
	if _, err := store.Load(canceled, key); err == nil {
		t.Fatal("canceled Load succeeded")
	}
	if _, err := store.ListSessions(canceled, "p"); err == nil {
		t.Fatal("canceled ListSessions succeeded")
	}
	if err := store.Delete(canceled, key); err == nil {
		t.Fatal("canceled Delete succeeded")
	}
	if err := store.ReplaceSession(canceled, key, nil); err == nil {
		t.Fatal("canceled ReplaceSession succeeded")
	}
	if err := store.Append(ctx, key, nil); err != nil {
		t.Fatalf("empty Append returned error: %v", err)
	}
	if err := store.ReplaceSession(ctx, key, []SessionStoreReplacement{{Key: SessionKey{ProjectKey: "other", SessionID: "s"}}}); err == nil {
		t.Fatal("ReplaceSession accepted mismatched key")
	}
	if err := store.Append(ctx, key, []SessionStoreEntry{SessionStoreEntry(`{"a":1}`)}); err != nil {
		t.Fatalf("Append returned error: %v", err)
	}
	if err := store.Append(ctx, SessionKey{ProjectKey: "p", SessionID: "s", Subpath: "sub"}, []SessionStoreEntry{SessionStoreEntry(`{"b":1}`)}); err != nil {
		t.Fatalf("Append subpath returned error: %v", err)
	}
	if err := store.Delete(ctx, key); err != nil {
		t.Fatalf("Delete main returned error: %v", err)
	}
	if entries, _ := store.Load(ctx, SessionKey{ProjectKey: "p", SessionID: "s", Subpath: "sub"}); len(entries) != 0 {
		t.Fatalf("Delete main did not cascade: %#v", entries)
	}
}
