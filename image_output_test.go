package codexacp

import (
	"encoding/base64"
	"os"
	"path/filepath"
	"testing"

	"github.com/coder/acp-go-sdk"
	"github.com/savid/acp-go-codex/internal/codex"
	acpcore "github.com/savid/acp-go-core"
	"github.com/stretchr/testify/require"

	"github.com/savid/acp-go-core/wire"
)

func TestSavedImageReplaysAfterOriginalFileIsDeleted(t *testing.T) {
	t.Parallel()
	store := acpcore.NewInMemorySessionStore()
	a := NewAgent(testOptions(t, WithSessionStore(store))...)
	t.Cleanup(func() { _ = a.Close() })
	rec := newRecorder()
	a.attach(rec, nil)
	_, err := a.Initialize(t.Context(), acp.InitializeRequest{ProtocolVersion: acp.ProtocolVersionNumber})
	require.NoError(t, err)
	cwd := t.TempDir()
	created, err := a.NewSession(t.Context(), wire.NewSessionRequest(cwd))
	require.NoError(t, err)
	s, err := a.session(t.Context(), created.SessionId)
	require.NoError(t, err)
	data := promptRaster(t)
	decoded, err := base64.StdEncoding.DecodeString(data)
	require.NoError(t, err)
	path := filepath.Join(cwd, "output.png")
	require.NoError(t, os.WriteFile(path, decoded, 0o600))
	state := &cycleState{}
	require.NoError(t, s.publishImageTerminal(t.Context(), state, codex.ImageEvent{ID: "image-view", Kind: itemTypeImageView, SavedPath: path}))
	require.NoError(t, s.commitMirror(t.Context()))
	_, err = a.CloseSession(t.Context(), acp.CloseSessionRequest{SessionId: created.SessionId})
	require.NoError(t, err)
	require.NoError(t, os.Remove(path))
	before := len(rec.snapshot())
	_, err = a.LoadSession(t.Context(), wire.LoadSessionRequest(created.SessionId, cwd))
	require.NoError(t, err)
	var images []string
	for _, update := range rec.snapshot()[before:] {
		if tool := update.Update.ToolCallUpdate; tool != nil && tool.Content != nil {
			for _, item := range tool.Content {
				if item.Content != nil && item.Content.Content.Image != nil {
					images = append(images, item.Content.Content.Image.Data)
				}
			}
		}
	}
	require.Equal(t, []string{data}, images)
}
