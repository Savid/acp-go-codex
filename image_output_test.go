package codexacp

import (
	"encoding/base64"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

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

// Streamed tool output is delivered once it doubles, so the bytes sent stay
// linear in the output, and the terminal update carries all of it.
func TestToolOutputDeliveryIsLinear(t *testing.T) {
	t.Parallel()

	a := NewAgent()
	rec := newRecorder()
	a.attach(rec, nil)

	s := a.newSession(sessionStart{cwd: t.TempDir()})
	state := &cycleState{}
	delta := strings.Repeat("x", 100)

	for range 1000 {
		require.NoError(t, s.publishToolDelta(t.Context(), state, "call-1", delta))
	}

	require.NoError(t, s.publishToolTerminal(t.Context(), state, codex.ToolEvent{ID: "call-1", Kind: "commandExecution", Status: "completed"}))

	sent, deliveries, last := 0, 0, ""

	for _, notification := range rec.snapshot() {
		update := notification.Update.ToolCallUpdate
		if update == nil || len(update.Content) == 0 {
			continue
		}

		deliveries++
		last = update.Content[0].Content.Content.Text.Text
		sent += len(last)
	}

	require.Equal(t, strings.Repeat(delta, 1000), last)
	require.LessOrEqual(t, deliveries, 12)
	require.LessOrEqual(t, sent, 3*len(last))
}

// Slow tool output that never doubles is still delivered as it grows, at
// most once per interval and within the delivery rate.
func TestSlowToolOutputIsPaced(t *testing.T) {
	t.Parallel()

	a := NewAgent()
	rec := newRecorder()
	a.attach(rec, nil)

	s := a.newSession(sessionStart{cwd: t.TempDir()})
	clock := time.Unix(0, 0)
	state := &cycleState{now: func() time.Time { return clock }}

	deliveries := func() []string {
		var texts []string

		for _, notification := range rec.snapshot() {
			if update := notification.Update.ToolCallUpdate; update != nil && len(update.Content) > 0 {
				texts = append(texts, update.Content[0].Content.Content.Text.Text)
			}
		}

		return texts
	}

	// A progress line every second for a minute, after a large first chunk
	// so no later line doubles the output.
	require.NoError(t, s.publishToolDelta(t.Context(), state, "call-1", strings.Repeat("x", 1000)))

	for range 60 {
		clock = clock.Add(time.Second)
		require.NoError(t, s.publishToolDelta(t.Context(), state, "call-1", "progress\n"))
	}

	texts := deliveries()
	require.Equal(t, 31, len(texts), "grown output was not delivered every two seconds")
	require.Equal(t, 1000+60*len("progress\n"), len(texts[len(texts)-1]))

	// Large output waits longer between timed deliveries.
	state = &cycleState{now: func() time.Time { return clock }}
	require.NoError(t, s.publishToolDelta(t.Context(), state, "call-2", strings.Repeat("y", 640<<10)))

	before := len(deliveries())

	for range 9 {
		clock = clock.Add(time.Second)
		require.NoError(t, s.publishToolDelta(t.Context(), state, "call-2", "z"))
	}

	require.Equal(t, before, len(deliveries()), "640 KiB of output was resent before ten seconds")

	clock = clock.Add(2 * time.Second)
	require.NoError(t, s.publishToolDelta(t.Context(), state, "call-2", "z"))
	require.Equal(t, before+1, len(deliveries()))
}
