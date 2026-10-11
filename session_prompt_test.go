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

	"github.com/savid/acp-go-codex/internal/codex"
	acpcore "github.com/savid/acp-go-core"
	"github.com/savid/acp-go-core/sessionlog"
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

// A provider refusal of a cut-off function call in the history repairs the
// rollout, so the next turn runs instead of being refused again.
func TestInvalidCallArgumentsAreRepaired(t *testing.T) {
	t.Parallel()

	store := acpcore.NewInMemorySessionStore()
	h := newHarness(t, WithSessionStore(store))
	h.initialize()

	session := h.newSession()

	_, err := h.prompt(session.SessionId, "RUNAWAY", nil)
	data := requestErrorData(t, err)
	require.Contains(t, data["message"], "has invalid JSON arguments")
	require.Equal(t, "repaired", data["historyRepair"])

	var record sessionRecord
	_, found, err := sessionlog.Load(t.Context(), store, string(session.SessionId), &record)
	require.NoError(t, err)
	require.True(t, found)
	require.Empty(t, invalidCall(record.NativePath), "the rollout still holds the cut-off call")

	native, err := readRollout(record.NativePath)
	require.NoError(t, err)

	stored, err := loadEntries(t.Context(), store, acpcore.SessionKey{SessionID: string(session.SessionId)})
	require.NoError(t, err)
	require.Len(t, stored, len(native))

	for index := range native {
		require.JSONEq(t, string(native[index]), string(stored[index]))
	}

	response, err := h.prompt(session.SessionId, "ECHO after", nil)
	require.NoError(t, err)
	require.Equal(t, acp.StopReasonEndTurn, response.StopReason)
}

// A refusal the rollout gives no cause for reports a failed repair, and the
// thread stays usable.
func TestUnfoundInvalidCallReportsAFailedRepair(t *testing.T) {
	t.Parallel()

	h := newHarness(t)
	h.initialize()
	session := h.newSession()

	_, err := h.prompt(session.SessionId, "REFUSEONLY", nil)
	data := requestErrorData(t, err)
	require.Equal(t, "failed", data["historyRepair"])

	response, err := h.prompt(session.SessionId, "ECHO after", nil)
	require.NoError(t, err)
	require.Equal(t, acp.StopReasonEndTurn, response.StopReason)
}

// A store that still holds the cut-off call a repaired rollout no longer
// holds restores and is republished repaired.
func TestRestoreRepairsAStoredCutOffCall(t *testing.T) {
	t.Parallel()

	store := acpcore.NewInMemorySessionStore()
	h := newHarness(t, WithSessionStore(store))
	h.initialize()

	cwd := t.TempDir()
	session, err := h.conn.NewSession(h.ctx(), wire.NewSessionRequest(cwd))
	require.NoError(t, err)

	_, err = h.prompt(session.SessionId, "RUNAWAY", nil)
	require.Error(t, err)

	var record sessionRecord
	rows, found, err := sessionlog.Load(t.Context(), store, string(session.SessionId), &record)
	require.NoError(t, err)
	require.True(t, found)

	_, err = h.conn.CloseSession(h.ctx(), acp.CloseSessionRequest{SessionId: session.SessionId})
	require.NoError(t, err)

	// The store keeps the row as codex wrote it, before the repair.
	unrepaired := make([][]byte, len(rows))

	for index, row := range rows {
		unrepaired[index] = row

		var decoded, payload map[string]json.RawMessage
		require.NoError(t, json.Unmarshal(row, &decoded))

		if json.Unmarshal(decoded["payload"], &payload) != nil || string(payload["call_id"]) != `"call-runaway"` || string(payload["type"]) != `"function_call"` {
			continue
		}

		payload["arguments"], err = json.Marshal(`{"command":["ls`)
		require.NoError(t, err)
		decoded["payload"], err = json.Marshal(payload)
		require.NoError(t, err)
		unrepaired[index], err = json.Marshal(decoded)
		require.NoError(t, err)
	}

	require.NoError(t, sessionlog.NewMirror(store, string(session.SessionId)).Commit(t.Context(), sessionlog.Rows(unrepaired),
		func(sessionlog.Tail) (any, error) { return record, nil }))

	_, err = h.conn.LoadSession(h.ctx(), wire.LoadSessionRequest(session.SessionId, cwd))
	require.NoError(t, err)

	restored, _, err := sessionlog.Load(t.Context(), store, string(session.SessionId), &record)
	require.NoError(t, err)

	_, cutOff, err := codex.RepairCallArguments(restored)
	require.NoError(t, err)
	require.Zero(t, cutOff, "the store still holds the cut-off call")

	native, err := readRollout(record.NativePath)
	require.NoError(t, err)
	require.Len(t, restored, len(native))

	response, err := h.prompt(session.SessionId, "ECHO after", nil)
	require.NoError(t, err)
	require.Equal(t, acp.StopReasonEndTurn, response.StopReason)
}
