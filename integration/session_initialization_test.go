//go:build integration

package integration

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/coder/acp-go-sdk"
	codexacp "github.com/savid/acp-go-codex"
	"github.com/stretchr/testify/require"
)

func TestCodexCLIColdResumeBeforeFirstPrompt(t *testing.T) {
	path := integrationCodexPath(t)
	t.Setenv("HOME", t.TempDir())
	ctx, cancel := context.WithTimeout(t.Context(), time.Minute)
	defer cancel()
	cwd := t.TempDir()
	open := func(store codexacp.SessionStore) *codexacp.Agent {
		home := t.TempDir()
		a := codexacp.NewAgent(
			codexacp.WithExecutablePath(path), codexacp.WithHome(home),
			codexacp.WithDefaultModel("gpt-5.4"), codexacp.WithSessionStore(store),
			codexacp.WithEnv(map[string]string{"OPENAI_API_KEY": ""}),
		)
		t.Cleanup(func() {
			require.NoError(t, a.Close())
		})
		_, err := a.Initialize(ctx, acp.InitializeRequest{ProtocolVersion: acp.ProtocolVersionNumber})
		require.NoError(t, err)

		return a
	}
	store := codexacp.NewInMemorySessionStore()
	first := open(store)
	created, err := first.NewSession(ctx, acp.NewSessionRequest{Cwd: cwd})
	require.NoError(t, err)
	id := string(created.SessionId)
	nativeID := codexThreadID(created.Meta)
	require.NotEmpty(t, nativeID)
	main := codexacp.SessionKey{SessionID: id}
	entries, err := store.Load(ctx, main)
	require.NoError(t, err)
	require.Len(t, entries, 1, "opening must persist native initialization before any prompt or close")
	keys, err := store.ListSubkeys(ctx, main)
	require.NoError(t, err)
	bundle := make([]codexacp.SessionStoreReplacement, 1, 1+len(keys))
	bundle[0] = codexacp.SessionStoreReplacement{Key: main, Entries: entries}
	for _, subpath := range keys {
		key := codexacp.SessionKey{SessionID: id, Subpath: subpath}
		rows, loadErr := store.Load(ctx, key)
		require.NoError(t, loadErr)
		bundle = append(bundle, codexacp.SessionStoreReplacement{Key: key, Entries: rows})
	}
	checkpoint := filepath.Join(t.TempDir(), "session.json")
	data, err := json.Marshal(bundle)
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(checkpoint, data, 0o600))
	warm, err := first.ResumeSession(ctx, acp.ResumeSessionRequest{SessionId: created.SessionId, Cwd: cwd})
	require.NoError(t, err)
	require.Equal(t, nativeID, codexThreadID(warm.Meta))
	require.NoError(t, first.Close())

	data, err = os.ReadFile(checkpoint)
	require.NoError(t, err)
	var restored []codexacp.SessionStoreReplacement
	require.NoError(t, json.Unmarshal(data, &restored))
	coldStore := codexacp.NewInMemorySessionStore()
	require.NoError(t, coldStore.Replace(ctx, main, restored))
	cold := open(coldStore)
	resumed, err := cold.ResumeSession(ctx, acp.ResumeSessionRequest{SessionId: created.SessionId, Cwd: cwd})
	require.NoError(t, err)
	require.Equal(t, nativeID, codexThreadID(resumed.Meta))
	rows, err := coldStore.Load(ctx, main)
	require.NoError(t, err)
	require.Equal(t, entries, rows, "cold resume must not invent a turn")
}
