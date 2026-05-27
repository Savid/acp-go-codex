//go:build integration

package integration

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/coder/acp-go-sdk"
	codexacp "github.com/savid/acp-go-codex"
)

const (
	codexSessionImportChunkMethod        = "_codex/session/importChunk"
	codexSessionCommitImportMethod       = "_codex/session/commitImport"
	codexSessionImportFormat             = "codex-rollout-jsonl"
	resumeFromFileImportedResumeSentinel = "ACP_IMPORTED_RESUME_OK"
)

func TestCodexCLISessionImportChunkCommitReplaceAndLoad(t *testing.T) {
	requireLiveTurn(t)

	ctx, cancel := context.WithTimeout(context.Background(), 6*time.Minute)
	defer cancel()

	cwd := t.TempDir()
	sourceStore := newRecordingSessionStore()
	sourceClient := &recordingClient{}
	sourceConn := connectLiveAgent(t, ctx, sourceClient, acp.InitializeRequest{}, codexacp.WithSessionStore(sourceStore))

	source, err := sourceConn.NewSession(ctx, acp.NewSessionRequest{Cwd: cwd, McpServers: []acp.McpServer{}})
	if err != nil {
		t.Fatalf("new source session: %v", err)
	}
	sourceThreadID := codexThreadID(source.Meta)
	if sourceThreadID == "" {
		t.Fatalf("missing source thread id: %#v", source.Meta)
	}

	resp := promptWithRefusalRetry(t, func() (acp.PromptResponse, error) {
		return sourceConn.Prompt(ctx, acp.PromptRequest{
			SessionId: source.SessionId,
			Prompt:    []acp.ContentBlock{acp.TextBlock("Reply exactly ACP_IMPORT_SEED.")},
		})
	})
	if resp.StopReason != acp.StopReasonEndTurn {
		t.Fatalf("seed stop reason = %s", resp.StopReason)
	}
	eventually(t, 30*time.Second, 250*time.Millisecond, func() bool {
		return sourceStore.hasMainSession(sourceThreadID)
	})
	entries := sourceStore.mainSessionEntries(sourceThreadID)
	if len(entries) == 0 {
		t.Fatal("source store did not capture rollout entries")
	}
	if _, err := sourceConn.CloseSession(ctx, acp.CloseSessionRequest{SessionId: source.SessionId}); err != nil {
		t.Fatalf("close source session: %v", err)
	}

	targetStore := newRecordingSessionStore()
	client := &recordingClient{}
	conn := connectLiveAgent(t, ctx, client, acp.InitializeRequest{}, codexacp.WithSessionStore(targetStore))

	importSessionEntries(t, ctx, conn, "integration-import-1", sourceThreadID, cwd, entries)
	importSessionEntries(t, ctx, conn, "integration-import-2", sourceThreadID, cwd, entries)
	if targetStore.replaceCount() != 1 {
		t.Fatalf("replace count = %d, want 1", targetStore.replaceCount())
	}

	_, err = conn.LoadSession(ctx, acp.LoadSessionRequest{
		SessionId:  acp.SessionId(sourceThreadID),
		Cwd:        cwd,
		McpServers: []acp.McpServer{},
	})
	if err != nil {
		t.Fatalf("load imported session: %v", err)
	}
	eventually(t, 30*time.Second, 250*time.Millisecond, func() bool {
		return strings.Contains(client.text(), "ACP_IMPORT_SEED")
	})

	resp = promptWithRefusalRetry(t, func() (acp.PromptResponse, error) {
		client.resetRecordedOutput()

		return conn.Prompt(ctx, acp.PromptRequest{
			SessionId: acp.SessionId(sourceThreadID),
			Prompt: []acp.ContentBlock{
				acp.TextBlock("Reply exactly " + resumeFromFileImportedResumeSentinel + " and do not use tools."),
			},
		})
	})
	if resp.StopReason != acp.StopReasonEndTurn {
		t.Fatalf("stop reason = %s", resp.StopReason)
	}
	if !strings.Contains(client.text(), resumeFromFileImportedResumeSentinel) {
		t.Fatalf("fresh load text = %q", client.text())
	}

	if _, err := conn.CloseSession(ctx, acp.CloseSessionRequest{SessionId: acp.SessionId(sourceThreadID)}); err != nil {
		t.Fatalf("close loaded session: %v", err)
	}
}

func importSessionEntries(
	t *testing.T,
	ctx context.Context,
	conn *acp.ClientSideConnection,
	importID string,
	sessionID string,
	cwd string,
	entries []codexacp.SessionStoreEntry,
) {
	t.Helper()

	raw, err := conn.CallExtension(ctx, codexSessionImportChunkMethod, map[string]any{
		"importId":  importID,
		"sessionId": sessionID,
		"cwd":       cwd,
		"format":    codexSessionImportFormat,
		"offset":    0,
		"entries":   entries,
	})
	if err != nil {
		t.Fatalf("import chunk: %v", err)
	}

	var chunk struct {
		ImportID string `json:"importId"`
		Offset   int    `json:"offset"`
		Entries  int    `json:"entries"`
	}
	if err := json.Unmarshal(raw, &chunk); err != nil {
		t.Fatalf("decode chunk result: %v", err)
	}
	if chunk.ImportID != importID || chunk.Offset != len(entries) || chunk.Entries != len(entries) {
		t.Fatalf("chunk result = %#v", chunk)
	}

	raw, err = conn.CallExtension(ctx, codexSessionCommitImportMethod, map[string]any{"importId": importID})
	if err != nil {
		t.Fatalf("commit import: %v", err)
	}

	var commit struct {
		ImportID  string `json:"importId"`
		SessionID string `json:"sessionId"`
		Entries   int    `json:"entries"`
		SHA256    string `json:"sha256"`
	}
	if err := json.Unmarshal(raw, &commit); err != nil {
		t.Fatalf("decode commit result: %v", err)
	}
	if commit.ImportID != importID || commit.SessionID != sessionID ||
		commit.Entries != len(entries) || commit.SHA256 == "" {
		t.Fatalf("commit result = %#v", commit)
	}
}
