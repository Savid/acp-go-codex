package codexacp

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	stdimage "image"
	"image/png"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/coder/acp-go-sdk"
	"github.com/stretchr/testify/require"

	"github.com/savid/acp-go-core/wire"
)

func promptRaster(t *testing.T) string {
	t.Helper()

	var b bytes.Buffer
	require.NoError(t, png.Encode(&b, stdimage.NewRGBA(stdimage.Rect(0, 0, 1, 1))))

	return base64.StdEncoding.EncodeToString(b.Bytes())
}
func TestImageInputOrder(t *testing.T) {
	s := &session{agent: NewAgent()}
	blocks := []acp.ContentBlock{acp.TextBlock("before"), acp.ImageBlock(promptRaster(t), "image/png"), acp.TextBlock("after")}
	mapped, err := s.mapPrompt(t.Context(), blocks)
	require.NoError(t, err)
	data, err := json.Marshal(mapped.input)
	require.NoError(t, err)
	var content []map[string]any
	require.NoError(t, json.Unmarshal(data, &content))
	kinds := make([]string, 0, len(content))
	for _, part := range content {
		kind, ok := part["type"].(string)
		require.True(t, ok)
		kinds = append(kinds, kind)
	}
	require.Equal(t, []string{"text", "image", "text"}, kinds)
}

func TestEmptyTextPromptIsRejected(t *testing.T) {
	s := &session{agent: NewAgent()}
	_, err := s.mapPrompt(t.Context(), []acp.ContentBlock{acp.TextBlock("")})
	require.Error(t, err, "empty text prompt was admitted for native dispatch")
}

func TestImageBlobIsForwarded(t *testing.T) {
	s := &session{agent: NewAgent()}
	mime := "image/png"
	block := acp.ResourceBlock(acp.EmbeddedResourceResource{BlobResourceContents: &acp.BlobResourceContents{Blob: promptRaster(t), MimeType: &mime, Uri: "embedded:one"}})
	mapped, err := s.mapPrompt(t.Context(), []acp.ContentBlock{block})
	require.NoError(t, err)
	require.Len(t, mapped.input, 1)
	require.Equal(t, "image", mapped.input[0]["type"])
}

func TestRefusedPeerPromptDoesNotEndTheLiveTurn(t *testing.T) {
	t.Parallel()

	h := newHarness(t)
	h.initialize(withLifecycle(), withFormElicitation())
	session := h.newSession()

	asked := make(chan struct{}, 1)
	release := make(chan struct{})

	h.rec.mu.Lock()
	h.rec.elicit = func(acp.UnstableCreateElicitationRequest) (acp.UnstableCreateElicitationResponse, error) {
		asked <- struct{}{}
		<-release

		return acp.UnstableCreateElicitationResponse{Accept: &acp.UnstableCreateElicitationAccept{Action: "accept", Content: map[string]any{"name": "Ada"}}}, nil
	}
	h.rec.mu.Unlock()

	type result struct {
		resp acp.PromptResponse
		err  error
	}

	done := make(chan result, 1)

	go func() {
		resp, err := h.prompt(session.SessionId, "ASK", promptMeta(1))
		done <- result{resp: resp, err: err}
	}()

	<-asked

	_, err := h.prompt(session.SessionId, "HELLO", promptMeta(2))
	require.Equal(t, "backpressure", requestErrorData(t, err)["error"])

	close(release)

	outcome := <-done
	require.NoError(t, outcome.err)
	require.Equal(t, acp.StopReasonEndTurn, outcome.resp.StopReason, "a refused peer prompt ended the turn the session still owned")
	require.Equal(t, "hi Ada", agentText(h.rec.snapshot()))
}

func TestNonRetriedNativeErrorFailsTheTurn(t *testing.T) {
	t.Parallel()

	h := newHarness(t)
	h.initialize()
	session := h.newSession()

	_, err := h.prompt(session.SessionId, "NOTIFYERROR", nil)
	data := requestErrorData(t, err)
	require.Equal(t, "codex_turn_failed", data["error"])
	require.Equal(t, "provider", data["cause"])
	require.Equal(t, "provider exploded", data["message"])

	// An error the app-server will retry leaves its turn live; only
	// turn/completed ends it.
	resp, err := h.prompt(session.SessionId, "RETRYERROR", nil)
	require.NoError(t, err)
	require.Equal(t, acp.StopReasonEndTurn, resp.StopReason)
}

// A cancel is the session's before the prompt has anything to dispatch, so a
// session/cancel that lands while the thread is still being rebound on a
// replacement app-server ends it there: the prompt answers cancelled, no turn
// starts, and the lifecycle stream carries nothing for it.
func TestPromptCancelledWhileRebinding(t *testing.T) {
	t.Parallel()

	held := filepath.Join(t.TempDir(), "rebind-held")
	h := newHarness(t, WithEnv(map[string]string{fakeCodexEnv: "1", fakeCodexEnvResumeHold: held}))
	h.initialize(withLifecycle())
	session := h.newSession()

	// The app-server dies mid-turn, so the next prompt starts a replacement and
	// resumes the thread on it; the replacement never answers that resume.
	_, err := h.prompt(session.SessionId, "DIE", promptMeta(1))
	require.Equal(t, "process_exit", requestErrorData(t, err)["cause"])

	before := eventTypes(lifecycleEvents(h.rec.snapshot()))

	type result struct {
		resp acp.PromptResponse
		err  error
	}

	done := make(chan result, 1)

	go func() {
		resp, promptErr := h.prompt(session.SessionId, "HELLO", promptMeta(2))
		done <- result{resp, promptErr}
	}()

	require.Eventually(t, func() bool {
		_, statErr := os.Stat(held)

		return statErr == nil
	}, testTimeout, time.Millisecond)

	require.NoError(t, h.conn.Cancel(h.ctx(), wire.CancelRequest(session.SessionId)))

	got := <-done
	require.NoError(t, got.err)
	require.Equal(t, acp.StopReasonCancelled, got.resp.StopReason)
	require.Equal(t, before, eventTypes(lifecycleEvents(h.rec.snapshot())),
		"a prompt the app-server never received opens no incarnation and publishes no acceptance")
}
