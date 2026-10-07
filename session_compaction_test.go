package codexacp

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	"github.com/savid/acp-go-codex/internal/codex"

	"github.com/coder/acp-go-sdk"
	"github.com/savid/acp-go-core/wire"
	"github.com/stretchr/testify/require"
)

func compactionReports(t *testing.T, notifications []acp.SessionNotification) []wire.Compaction {
	t.Helper()
	var reports []wire.Compaction
	for _, notification := range notifications {
		value, exists := notification.Meta[wire.CompactionKey]
		if !exists {
			continue
		}
		carrier, err := json.Marshal(notification.Update)
		require.NoError(t, err)
		require.JSONEq(t, `{"sessionUpdate":"session_info_update"}`, string(carrier))
		require.Len(t, notification.Meta, 1)
		encoded, err := json.Marshal(value)
		require.NoError(t, err)
		var report wire.Compaction
		require.NoError(t, json.Unmarshal(encoded, &report))
		require.NotEmpty(t, report.CompactionID)
		reports = append(reports, report)
	}

	return reports
}

func TestCompactionTransport(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	h.initialize()
	created := h.newSession()
	for range 2 {
		_, err := h.prompt(created.SessionId, "COMPACT", nil)
		require.NoError(t, err)
	}
	reports := compactionReports(t, h.rec.snapshot())
	require.Len(t, reports, 2)
	require.NotEqual(t, reports[0].CompactionID, reports[1].CompactionID)
	for _, report := range reports {
		require.Equal(t, wire.CompactionCompleted, report.Status)
	}
}

func TestCompactionItemIdentityAndAttribution(t *testing.T) {
	t.Parallel()
	a := NewAgent()
	rec := newRecorder()
	a.attach(rec, nil)
	s := a.newSession(sessionStart{cwd: t.TempDir()})
	s.nativeID = "root"
	c := &cycle{nativeTurnID: "turn"}
	for _, event := range []codex.Event{
		{Kind: codex.EventCompactionCompleted, ThreadID: "child", TurnID: "turn", ItemID: "child-attempt"},
		{Kind: codex.EventCompactionCompleted, TurnID: "turn", ItemID: "first"},
		{Kind: codex.EventCompactionCompleted, TurnID: "turn", ItemID: "first"},
		{Kind: codex.EventCompactionCompleted, TurnID: "turn", ItemID: "next"},
	} {
		_, err := s.projectEvent(t.Context(), c, event)
		require.NoError(t, err)
	}
	reports := compactionReports(t, rec.snapshot())
	require.Len(t, reports, 2)
	require.NotEqual(t, reports[0].CompactionID, reports[1].CompactionID)
	require.Equal(t, wire.CompactionCompleted, reports[1].Status)
	require.Empty(t, reports[1].Trigger)
}

func TestCompactedTranscriptReplay(t *testing.T) {
	t.Parallel()
	a := NewAgent()
	rec := newRecorder()
	a.attach(rec, nil)
	s := a.newSession(sessionStart{cwd: t.TempDir()})
	require.NoError(t, s.replay(t.Context(), [][]byte{
		[]byte(`{"type":"event_msg","payload":{"type":"user_message","message":"continue"}}`),
		[]byte(`{"type":"compacted","payload":{"message":"retained summary"}}`),
		[]byte(`{"type":"event_msg","payload":{"type":"agent_message","message":"retained answer"}}`),
	}, nil))
	require.Empty(t, compactionReports(t, rec.snapshot()))
	require.Equal(t, "retained answer", agentText(rec.snapshot()))
	var thoughts string
	for _, update := range rec.snapshot() {
		if thought := update.Update.AgentThoughtChunk; thought != nil {
			thoughts += thought.Content.Text.Text
		}
	}
	require.Equal(t, "retained summary", thoughts)
}

func TestCompactionOtherTurnDoesNotAdoptPromptIdentity(t *testing.T) {
	t.Parallel()
	a := NewAgent()
	rec := newRecorder()
	a.attach(rec, nil)
	s := a.newSession(sessionStart{cwd: t.TempDir()})
	s.nativeID = "root"
	c := &cycle{}
	for _, event := range []codex.Event{
		{Kind: codex.EventCompactionCompleted, ThreadID: "root", TurnID: "earlier", ItemID: "attempt"},
	} {
		_, err := s.projectEvent(t.Context(), c, event)
		require.NoError(t, err)
	}
	require.Empty(t, c.nativeTurnID)
	c.nativeTurnID = "current"
	_, err := s.projectEvent(t.Context(), c, codex.Event{Kind: codex.EventCompactionCompleted, ThreadID: "root", TurnID: "older", ItemID: "attempt"})
	require.NoError(t, err)
	reports := compactionReports(t, rec.snapshot())
	require.Len(t, reports, 2)
	require.NotEqual(t, reports[0].CompactionID, reports[1].CompactionID)
	require.Equal(t, "current", c.nativeTurnID)
}

type compactionFailureClient struct {
	*recorder
	failed bool
}

func (c *compactionFailureClient) SessionUpdate(ctx context.Context, notification acp.SessionNotification) error {
	if notification.Meta[wire.CompactionKey] != nil && !c.failed {
		c.failed = true

		return errors.New("compaction delivery unavailable")
	}

	return c.recorder.SessionUpdate(ctx, notification)
}

func TestCompactionSendFailureKeepsRuntime(t *testing.T) {
	t.Parallel()
	a := NewAgent()
	rec := &compactionFailureClient{recorder: newRecorder()}
	a.attach(rec, nil)
	s := a.newSession(sessionStart{cwd: t.TempDir()})
	s.nativeID = "root"
	rt := &runtime{}
	s.rt = rt
	for _, id := range []string{"failed", "next"} {
		require.True(t, s.handleEvent(t.Context(), rt, nil, codex.Event{Kind: codex.EventCompactionCompleted, ThreadID: "root", TurnID: "turn", ItemID: id}))
	}
	require.True(t, rec.failed)
	require.Len(t, compactionReports(t, rec.snapshot()), 1)
}

func TestCompactionNativeOutcomes(t *testing.T) {
	t.Parallel()
	a := NewAgent()
	rec := newRecorder()
	a.attach(rec, nil)
	s := a.newSession(sessionStart{cwd: t.TempDir()})
	s.nativeID = "root"
	for _, status := range []string{"failed", "interrupted", "completed"} {
		c := &cycle{nativeTurnID: status}
		item, err := json.Marshal(map[string]any{"threadId": "root", "turnId": status, "item": map[string]any{"id": "compaction", "type": "contextCompaction"}})
		require.NoError(t, err)
		start := codex.DecodeEvent(codex.Notification{Method: "item/started", Params: item})
		_, err = s.projectEvent(t.Context(), c, start)
		require.NoError(t, err)
		if status == "completed" {
			completed := codex.DecodeEvent(codex.Notification{Method: "item/completed", Params: item})
			_, err = s.projectEvent(t.Context(), c, completed)
			require.NoError(t, err)
		}
		payload, err := json.Marshal(map[string]any{"threadId": "root", "turn": map[string]any{"id": status, "status": status}})
		require.NoError(t, err)
		ended := codex.DecodeEvent(codex.Notification{Method: "turn/completed", Params: payload})
		settled, err := s.projectEvent(t.Context(), c, ended)
		require.NoError(t, err)
		require.True(t, settled)
	}
	reports := compactionReports(t, rec.snapshot())
	require.Len(t, reports, 1)
	require.Equal(t, wire.CompactionCompleted, reports[0].Status)
}
