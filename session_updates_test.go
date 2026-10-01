package codexacp

import (
	"testing"

	"github.com/coder/acp-go-sdk"
	"github.com/savid/acp-go-core/wire"
	"github.com/stretchr/testify/require"
)

func TestTextProjectionRejectsStaleAndRepeatedRecords(t *testing.T) {
	t.Parallel()
	for _, script := range []string{"DUPLICATE", "STALE"} {
		t.Run(script, func(t *testing.T) {
			t.Parallel()
			h := newHarness(t)
			h.initialize()
			session := h.newSession()
			_, err := h.prompt(session.SessionId, script, nil)
			require.NoError(t, err)
			require.Equal(t, "Hello world", agentText(h.rec.snapshot()))
		})
	}
}

// TestUsageFollowsEachResponse proves every model request of a turn reports
// the context it left occupied, never the running sum, a restated report
// sends nothing, and the prompt response carries only this turn's summed
// consumption.
func TestUsageFollowsEachResponse(t *testing.T) {
	t.Parallel()

	h := newHarness(t)
	h.initialize()
	session := h.newSession()

	_, err := h.prompt(session.SessionId, "HELLO", nil)
	require.NoError(t, err)

	resp, err := h.prompt(session.SessionId, "MULTI", nil)
	require.NoError(t, err)

	require.Equal(t, []acp.SessionUsageUpdate{
		{Size: 1000, Used: 15},
		{Size: 1000, Used: 1020},
		{Size: 1000, Used: 1130},
		{Size: 1000, Used: 1160},
	}, usageUpdates(h.rec.snapshot()))
	require.Equal(t, &acp.Usage{InputTokens: 3250, OutputTokens: 60, TotalTokens: 3310, CachedReadTokens: new(2100)}, resp.Usage)
}

// TestUsageAfterCompaction proves the figure after a compaction is codex's
// estimate of the compacted history, never the context before it, while the
// summarizing request still counts toward the turn's consumption.
func TestUsageAfterCompaction(t *testing.T) {
	t.Parallel()

	h := newHarness(t)
	h.initialize()
	session := h.newSession()

	resp, err := h.prompt(session.SessionId, "COMPACT", nil)
	require.NoError(t, err)

	require.Equal(t, []acp.SessionUsageUpdate{
		{Size: 1000, Used: 5100},
		{Size: 1000, Used: 5050},
		{Size: 1000, Used: 600},
		{Size: 1000, Used: 720},
	}, usageUpdates(h.rec.snapshot()))
	require.Equal(t, 10870, resp.Usage.TotalTokens)
}

// TestCancelledTurnReportsNoUsageAfterCancel proves a cancelled turn keeps the
// usage it reported before the cancel and sends none after it, while the
// request codex recorded on its way out still counts toward the response.
func TestCancelledTurnReportsNoUsageAfterCancel(t *testing.T) {
	t.Parallel()

	h := newHarness(t)
	h.initialize()
	session := h.newSession()

	done := make(chan acp.PromptResponse, 1)

	go func() {
		resp, _ := h.prompt(session.SessionId, "STEPSLOW", nil)
		done <- resp
	}()

	h.rec.waitFor(t, func(updates []acp.SessionNotification) bool { return len(usageUpdates(updates)) == 1 })
	require.NoError(t, h.conn.Cancel(h.ctx(), wire.CancelRequest(session.SessionId)))

	resp := <-done
	require.Equal(t, acp.StopReasonCancelled, resp.StopReason)
	require.Equal(t, []acp.SessionUsageUpdate{{Size: 1000, Used: 1020}}, usageUpdates(h.rec.snapshot()))
	require.Equal(t, 1035, resp.Usage.TotalTokens)
}

// TestUsageWindowPrefersCatalog proves size is the catalog's window for the
// selected model, and codex's reported window only for a model the catalog
// states none for.
func TestUsageWindowPrefersCatalog(t *testing.T) {
	t.Parallel()

	for model, size := range map[string]int{"vision": 1000, "text-only": 500, "unlisted": 1000} {
		t.Run(model, func(t *testing.T) {
			t.Parallel()

			h := newHarness(t)
			h.initialize()
			session := h.newSession(WithSessionCodexOptions(NewCodexOptions(WithCodexModel(model))))

			_, err := h.prompt(session.SessionId, "HELLO", nil)
			require.NoError(t, err)
			require.Equal(t, []acp.SessionUsageUpdate{{Size: size, Used: 15}}, usageUpdates(h.rec.snapshot()))
		})
	}
}
