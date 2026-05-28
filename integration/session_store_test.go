//go:build integration

package integration

import (
	"context"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/coder/acp-go-sdk"
	codexacp "github.com/savid/acp-go-codex"
)

func TestCodexCLISessionStoreMirrorLoadAndResume(t *testing.T) {
	requireLiveTurn(t)

	ctx, cancel := context.WithTimeout(context.Background(), 6*time.Minute)
	defer cancel()

	cwd := t.TempDir()
	store := newRecordingSessionStore()
	client := &recordingClient{}
	conn := connectLiveAgent(t, ctx, client, acp.InitializeRequest{}, codexacp.WithSessionStore(store))

	session, err := conn.NewSession(ctx, acp.NewSessionRequest{Cwd: cwd, McpServers: []acp.McpServer{}})
	if err != nil {
		t.Fatalf("new session: %v", err)
	}
	threadID := codexThreadID(session.Meta)
	if threadID == "" {
		t.Fatalf("missing thread id: %#v", session.Meta)
	}

	resp := promptWithRefusalRetry(t, func() (acp.PromptResponse, error) {
		return conn.Prompt(ctx, acp.PromptRequest{
			SessionId: session.SessionId,
			Prompt:    []acp.ContentBlock{acp.TextBlock("Reply with exactly ACP_STORE_OK and no punctuation.")},
		})
	})
	if resp.StopReason != acp.StopReasonEndTurn {
		t.Fatalf("stop reason = %s", resp.StopReason)
	}
	if !store.hasMainSession(string(session.SessionId)) {
		t.Fatalf("store did not mirror ACP session %q", session.SessionId)
	}

	if _, err := conn.CloseSession(ctx, acp.CloseSessionRequest{SessionId: session.SessionId}); err != nil {
		t.Fatalf("close session: %v", err)
	}

	client = &recordingClient{}
	conn = connectLiveAgent(t, ctx, client, acp.InitializeRequest{}, codexacp.WithSessionStore(store))
	_, err = conn.LoadSession(ctx, acp.LoadSessionRequest{
		SessionId:  session.SessionId,
		Cwd:        cwd,
		McpServers: []acp.McpServer{},
	})
	if err != nil {
		t.Fatalf("load stored session: %v", err)
	}
	eventually(t, 30*time.Second, 250*time.Millisecond, func() bool {
		return client.text() != ""
	})

	resp = promptWithRefusalRetry(t, func() (acp.PromptResponse, error) {
		client.resetRecordedOutput()

		return conn.Prompt(ctx, acp.PromptRequest{
			SessionId: session.SessionId,
			Prompt:    []acp.ContentBlock{acp.TextBlock("Reply with exactly ACP_STORE_RESUME_OK and no punctuation.")},
		})
	})
	if resp.StopReason != acp.StopReasonEndTurn {
		t.Fatalf("resume stop reason = %s", resp.StopReason)
	}
	if !slices.ContainsFunc(client.updateSnapshot(), func(update acp.SessionUpdate) bool {
		return update.AgentMessageChunk != nil
	}) {
		t.Fatal("resume did not emit agent message updates")
	}

	if _, err := conn.CloseSession(ctx, acp.CloseSessionRequest{SessionId: session.SessionId}); err != nil {
		t.Fatalf("close loaded session: %v", err)
	}
}

type recordingSessionStore struct {
	mu           sync.Mutex
	entries      map[codexacp.SessionKey][]codexacp.SessionStoreEntry
	mtime        map[codexacp.SessionKey]int64
	replaceCalls int
}

var _ codexacp.SessionStore = (*recordingSessionStore)(nil)
var _ codexacp.SessionStoreLister = (*recordingSessionStore)(nil)
var _ codexacp.SessionStoreDeleter = (*recordingSessionStore)(nil)
var _ codexacp.SessionStoreReplacer = (*recordingSessionStore)(nil)

func newRecordingSessionStore() *recordingSessionStore {
	return &recordingSessionStore{
		entries: make(map[codexacp.SessionKey][]codexacp.SessionStoreEntry),
		mtime:   make(map[codexacp.SessionKey]int64),
	}
}

func (s *recordingSessionStore) Append(
	ctx context.Context,
	key codexacp.SessionKey,
	entries []codexacp.SessionStoreEntry,
) error {
	if err := ctx.Err(); err != nil {
		return err
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	for _, entry := range entries {
		s.entries[key] = append(s.entries[key], slices.Clone(entry))
	}
	s.mtime[key] = time.Now().UnixMilli()

	return nil
}

func (s *recordingSessionStore) Load(
	ctx context.Context,
	key codexacp.SessionKey,
) ([]codexacp.SessionStoreEntry, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	return cloneSessionStoreEntries(s.entries[key]), nil
}

func (s *recordingSessionStore) Replace(
	ctx context.Context,
	key codexacp.SessionKey,
	entries []codexacp.SessionStoreEntry,
) error {
	if err := ctx.Err(); err != nil {
		return err
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	s.entries[key] = cloneSessionStoreEntries(entries)
	s.mtime[key] = time.Now().UnixMilli()

	return nil
}

func (s *recordingSessionStore) ListSessions(
	ctx context.Context,
	projectKey string,
) ([]codexacp.SessionSummary, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	summaries := make([]codexacp.SessionSummary, 0)
	for key := range s.entries {
		if key.ProjectKey != projectKey || key.SessionID == "" || key.Subpath != "" {
			continue
		}

		summaries = append(summaries, codexacp.SessionSummary{
			SessionID: key.SessionID,
			MTime:     s.mtime[key],
		})
	}

	return summaries, nil
}

func (s *recordingSessionStore) Delete(ctx context.Context, key codexacp.SessionKey) error {
	if err := ctx.Err(); err != nil {
		return err
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	s.replaceCalls++

	for candidate := range s.entries {
		if candidate.ProjectKey != key.ProjectKey || candidate.SessionID != key.SessionID {
			continue
		}

		if key.Subpath != "" && candidate.Subpath != key.Subpath {
			continue
		}

		delete(s.entries, candidate)
		delete(s.mtime, candidate)
	}

	return nil
}

func (s *recordingSessionStore) ReplaceSession(
	ctx context.Context,
	main codexacp.SessionKey,
	replacements []codexacp.SessionStoreReplacement,
) error {
	if err := ctx.Err(); err != nil {
		return err
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	s.replaceCalls++

	for candidate := range s.entries {
		if candidate.ProjectKey == main.ProjectKey && candidate.SessionID == main.SessionID {
			delete(s.entries, candidate)
			delete(s.mtime, candidate)
		}
	}

	mtime := time.Now().UnixMilli()
	for _, replacement := range replacements {
		if replacement.Key.ProjectKey != main.ProjectKey || replacement.Key.SessionID != main.SessionID {
			continue
		}

		s.entries[replacement.Key] = cloneSessionStoreEntries(replacement.Entries)
		s.mtime[replacement.Key] = mtime
	}

	return nil
}

func (s *recordingSessionStore) replaceCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()

	return s.replaceCalls
}

func (s *recordingSessionStore) hasMainSession(sessionID string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()

	for key, entries := range s.entries {
		if key.SessionID == sessionID && key.Subpath == "" && len(entries) > 0 {
			return true
		}
	}

	return false
}

func (s *recordingSessionStore) mainSessionEntries(sessionID string) []codexacp.SessionStoreEntry {
	s.mu.Lock()
	defer s.mu.Unlock()

	for key, entries := range s.entries {
		if key.SessionID == sessionID && key.Subpath == "" {
			return cloneSessionStoreEntries(entries)
		}
	}

	return nil
}

func cloneSessionStoreEntries(entries []codexacp.SessionStoreEntry) []codexacp.SessionStoreEntry {
	if len(entries) == 0 {
		return nil
	}

	clone := make([]codexacp.SessionStoreEntry, 0, len(entries))
	for _, entry := range entries {
		clone = append(clone, slices.Clone(entry))
	}

	return clone
}
