package codexacp

import (
	"maps"
	"path/filepath"
	"slices"
	"testing"

	"github.com/coder/acp-go-sdk"

	"github.com/savid/acp-go-codex/internal/codex"
	acpcore "github.com/savid/acp-go-core"
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

// usagePaths names the two ways a session learns of its model requests: a
// thread it started reports each response as it completes, and a thread it
// resumed reports each request once the request's tools finished.
var usagePaths = []string{"started", "resumed"}

// usageSession opens a session on a model the gateway lists with a
// 1000-token window: a new thread, or, for the resumed path, the thread of a
// session that answered one prompt, was closed, and was loaded again.
func usageSession(t *testing.T, path string) (*harness, acp.SessionId) {
	t.Helper()

	h := newGatewayHarness(t, WithSessionStore(acpcore.NewInMemorySessionStore()))
	h.initialize()

	cwd := t.TempDir()
	created, err := h.conn.NewSession(h.ctx(), wire.NewSessionRequest(cwd, WithSessionCodexOptions(NewCodexOptions(WithCodexModel("gw/wide")))))
	require.NoError(t, err)

	if path == "resumed" {
		_, err = h.prompt(created.SessionId, "HELLO", nil)
		require.NoError(t, err)

		_, err = h.conn.CloseSession(h.ctx(), acp.CloseSessionRequest{SessionId: created.SessionId})
		require.NoError(t, err)

		_, err = h.conn.LoadSession(h.ctx(), wire.LoadSessionRequest(created.SessionId, cwd))
		require.NoError(t, err)
	}

	return h, created.SessionId
}

// usageSince returns the usage updates delivered after the first mark
// notifications, each breakdown without its response id, which
// TestResponseIDNamesOnlyTheReportedCall proves.
func usageSince(h *harness, mark int) []acp.SessionUsageUpdate {
	updates := usageUpdates(h.rec.snapshot()[mark:])

	for index, update := range updates {
		if call, ok := update.Meta[wire.CallUsageKey].(map[string]any); ok {
			call = maps.Clone(call)
			delete(call, "responseId")
			updates[index].Meta = map[string]any{wire.CallUsageKey: call}
		}
	}

	return updates
}

// callUpdate is the update reporting one model request: the context it left
// occupied and its breakdown.
func callUpdate(used, input, cachedRead, cachedWrite, output int) acp.SessionUsageUpdate {
	return acp.SessionUsageUpdate{Size: 1000, Used: used, Meta: map[string]any{wire.CallUsageKey: map[string]any{
		"inputTokens": float64(input), "cachedReadTokens": float64(cachedRead),
		"cachedWriteTokens": float64(cachedWrite), "outputTokens": float64(output),
	}}}
}

// firstIndex is the position of the first notification matching match, -1
// when none does.
func firstIndex(updates []acp.SessionNotification, match func(acp.SessionUpdate) bool) int {
	return slices.IndexFunc(updates, func(update acp.SessionNotification) bool { return match(update.Update) })
}

func isToolCall(id string) func(acp.SessionUpdate) bool {
	return func(update acp.SessionUpdate) bool {
		return update.ToolCall != nil && string(update.ToolCall.ToolCallId) == id
	}
}

// TestUsageFollowsEachResponse proves every model request of a turn reports
// once, with the context it left occupied, never the running sum, and with
// its breakdown; a restated report sends nothing; and the prompt response
// carries this turn's summed consumption. A started thread reports each
// request as its response completes, before the tools it started; a resumed
// thread reports it once those tools finished.
func TestUsageFollowsEachResponse(t *testing.T) {
	t.Parallel()

	for _, path := range usagePaths {
		t.Run(path, func(t *testing.T) {
			t.Parallel()

			h, session := usageSession(t, path)

			_, err := h.prompt(session, "HELLO", nil)
			require.NoError(t, err)

			mark := len(h.rec.snapshot())

			resp, err := h.prompt(session, "MULTI", nil)
			require.NoError(t, err)

			require.Equal(t, []acp.SessionUsageUpdate{
				callUpdate(1020, 1000, 0, 0, 20),
				callUpdate(1130, 100, 900, 100, 30),
				callUpdate(1160, 50, 1100, 0, 10),
			}, usageSince(h, mark))
			require.Equal(t, &acp.Usage{
				InputTokens: 3250, OutputTokens: 60, TotalTokens: 3310,
				CachedReadTokens: new(2000), CachedWriteTokens: new(100), ThoughtTokens: new(12),
			}, resp.Usage)

			turn := h.rec.snapshot()[mark:]
			usage := firstIndex(turn, func(update acp.SessionUpdate) bool { return update.UsageUpdate != nil })
			tool := firstIndex(turn, isToolCall("call-a"))
			require.NotEqual(t, -1, tool)
			require.Equal(t, path == "started", usage < tool)
		})
	}
}

// TestUsageAfterCompaction proves the figure after a compaction is codex's
// estimate of the compacted history, never the context before it, and
// carries no breakdown, while the summarizing request reports and counts
// toward the turn's consumption.
func TestUsageAfterCompaction(t *testing.T) {
	t.Parallel()

	for _, path := range usagePaths {
		t.Run(path, func(t *testing.T) {
			t.Parallel()

			h, session := usageSession(t, path)
			mark := len(h.rec.snapshot())

			resp, err := h.prompt(session, "COMPACT", nil)
			require.NoError(t, err)

			require.Equal(t, []acp.SessionUsageUpdate{
				callUpdate(5100, 1000, 4000, 0, 100),
				callUpdate(5050, 4900, 0, 0, 150),
				{Size: 1000, Used: 600},
				callUpdate(720, 700, 0, 0, 20),
			}, usageSince(h, mark))
			require.Equal(t, 10870, resp.Usage.TotalTokens)
		})
	}
}

// TestEmptyUsageReportsNothing proves a request whose usage reports no token,
// as a gateway's response-cache replay does, and an empty compaction
// estimate send nothing, never a zero figure, and add nothing to the turn's
// consumption.
func TestEmptyUsageReportsNothing(t *testing.T) {
	t.Parallel()

	for _, path := range usagePaths {
		t.Run(path, func(t *testing.T) {
			t.Parallel()

			h, session := usageSession(t, path)
			mark := len(h.rec.snapshot())

			resp, err := h.prompt(session, "ZERO", nil)
			require.NoError(t, err)

			require.Equal(t, []acp.SessionUsageUpdate{callUpdate(1020, 1000, 0, 0, 20)}, usageSince(h, mark))
			require.Equal(t, &acp.Usage{InputTokens: 1000, OutputTokens: 20, TotalTokens: 1020}, resp.Usage)
		})
	}
}

// TestResumedRestatementReportsNothing proves a resumed thread's restatement
// of the usage codex restored from the rollout is not a new request.
func TestResumedRestatementReportsNothing(t *testing.T) {
	t.Parallel()

	h, session := usageSession(t, "resumed")
	mark := len(h.rec.snapshot())

	resp, err := h.prompt(session, "RESTATE", nil)
	require.NoError(t, err)

	require.Equal(t, []acp.SessionUsageUpdate{callUpdate(15, 10, 0, 0, 5)}, usageSince(h, mark))
	require.Equal(t, 15, resp.Usage.TotalTokens)
}

// TestCancelledTurnReportsNoUsageAfterCancel proves a cancelled turn keeps the
// usage it reported before the cancel and sends none after it, while the
// request codex recorded on its way out still counts toward the response.
func TestCancelledTurnReportsNoUsageAfterCancel(t *testing.T) {
	t.Parallel()

	for _, path := range usagePaths {
		t.Run(path, func(t *testing.T) {
			t.Parallel()

			h, session := usageSession(t, path)
			mark := len(h.rec.snapshot())
			done := make(chan acp.PromptResponse, 1)

			go func() {
				resp, _ := h.prompt(session, "STEPSLOW", nil)
				done <- resp
			}()

			h.rec.waitFor(t, func(updates []acp.SessionNotification) bool { return len(usageUpdates(updates[mark:])) == 1 })
			require.NoError(t, h.conn.Cancel(h.ctx(), wire.CancelRequest(session)))

			resp := <-done
			require.Equal(t, acp.StopReasonCancelled, resp.StopReason)
			require.Equal(t, []acp.SessionUsageUpdate{callUpdate(1020, 1000, 0, 0, 20)}, usageSince(h, mark))
			require.Equal(t, 1035, resp.Usage.TotalTokens)
		})
	}
}

// TestCancelDuringToolKeepsResponseUsage proves a request whose tool the
// cancel interrupts reports before the cancel on a started thread, which
// learns of it as its response completes, and nothing on a resumed thread,
// which learns of it only after the cancel; it counts toward the response
// once either way.
func TestCancelDuringToolKeepsResponseUsage(t *testing.T) {
	t.Parallel()

	for _, path := range usagePaths {
		t.Run(path, func(t *testing.T) {
			t.Parallel()

			h, session := usageSession(t, path)
			mark := len(h.rec.snapshot())
			done := make(chan acp.PromptResponse, 1)

			go func() {
				resp, _ := h.prompt(session, "HALTTOOL", nil)
				done <- resp
			}()

			h.rec.waitFor(t, func(updates []acp.SessionNotification) bool {
				return firstIndex(updates[mark:], isToolCall("call-a")) != -1
			})
			require.NoError(t, h.conn.Cancel(h.ctx(), wire.CancelRequest(session)))

			resp := <-done
			require.Equal(t, acp.StopReasonCancelled, resp.StopReason)
			require.Equal(t, &acp.Usage{InputTokens: 1000, OutputTokens: 20, TotalTokens: 1020}, resp.Usage)

			want := []acp.SessionUsageUpdate{callUpdate(1020, 1000, 0, 0, 20)}
			if path == "resumed" {
				want = nil
			}

			require.Equal(t, want, usageSince(h, mark))
		})
	}
}

// TestUsageWindowPrefersGatewayList proves size is the window the gateway's
// model list states for the selected model, and codex's reported window only
// for a model the list states none for, unknown on a new thread's first
// request, whose response completes before codex first reports it.
func TestUsageWindowPrefersGatewayList(t *testing.T) {
	t.Parallel()

	for model, sizes := range map[string][]int{"gw/wide": {1000, 1000}, "gw/narrow": {500, 500}, "gw/unsized": {0, 1000}, "unlisted": {0, 1000}} {
		t.Run(model, func(t *testing.T) {
			t.Parallel()

			h := newGatewayHarness(t)
			h.initialize()
			session := h.newSession(WithSessionCodexOptions(NewCodexOptions(WithCodexModel(model))))

			for range sizes {
				_, err := h.prompt(session.SessionId, "HELLO", nil)
				require.NoError(t, err)
			}

			got := make([]int, 0, len(sizes))
			for _, update := range usageUpdates(h.rec.snapshot()) {
				got = append(got, update.Size)
			}

			require.Equal(t, sizes, got)
		})
	}
}

// chunkMessageIDs lists the messageId of each assistant and thought chunk in
// delivery order, "" for a chunk that carries none.
func chunkMessageIDs(updates []acp.SessionNotification) []string {
	ids := make([]string, 0, len(updates))

	for _, update := range updates {
		var id *string

		switch {
		case update.Update.AgentMessageChunk != nil:
			id = update.Update.AgentMessageChunk.MessageId
		case update.Update.AgentThoughtChunk != nil:
			id = update.Update.AgentThoughtChunk.MessageId
		default:
			continue
		}

		if id == nil {
			id = new("")
		}

		ids = append(ids, *id)
	}

	return ids
}

// TestResponseIDNamesOnlyTheReportedCall proves the gateway's response id
// rides only the breakdown that reports its call. A started thread's
// breakdown carries the id its response completion names, the one the
// rollout records; a resumed thread's reports name none, so its breakdown
// carries none. No chunk carries a messageId: live chunks stream before codex
// names their response, and the rollout cannot tie a replayed row to its
// response with certainty.
func TestResponseIDNamesOnlyTheReportedCall(t *testing.T) {
	t.Parallel()

	for _, path := range usagePaths {
		t.Run(path, func(t *testing.T) {
			t.Parallel()

			h := newGatewayHarness(t, WithSessionStore(acpcore.NewInMemorySessionStore()))
			h.initialize()

			cwd := t.TempDir()
			created, err := h.conn.NewSession(h.ctx(), wire.NewSessionRequest(cwd, WithSessionCodexOptions(NewCodexOptions(WithCodexModel("gw/wide")))))
			require.NoError(t, err)

			session := created.SessionId
			reload := func() []acp.SessionNotification {
				_, closeErr := h.conn.CloseSession(h.ctx(), acp.CloseSessionRequest{SessionId: session})
				require.NoError(t, closeErr)

				mark := len(h.rec.snapshot())
				_, loadErr := h.conn.LoadSession(h.ctx(), wire.LoadSessionRequest(session, cwd))
				require.NoError(t, loadErr)

				return h.rec.snapshot()[mark:]
			}

			if path == "resumed" {
				_, err = h.prompt(session, "HELLO", nil)
				require.NoError(t, err)
				reload()
			}

			mark := len(h.rec.snapshot())
			_, err = h.prompt(session, "THINK", nil)
			require.NoError(t, err)

			live := h.rec.snapshot()[mark:]
			require.Equal(t, []string{"", ""}, chunkMessageIDs(live))

			usage := usageUpdates(live)
			require.Len(t, usage, 1)

			call, ok := usage[0].Meta[wire.CallUsageKey].(map[string]any)
			require.True(t, ok)

			recorded := recordedResponseIDs(t, h, session)
			responseID, named := call["responseId"].(string)
			require.Equal(t, path == "started", named, "only a started thread's report names its response")

			if named {
				require.Equal(t, recorded[len(recorded)-1], responseID)
			}

			replayed := reload()
			ids := chunkMessageIDs(replayed)
			require.NotEmpty(t, ids)
			require.Equal(t, make([]string, len(ids)), ids, "a replayed chunk carries no messageId although the rollout records its turn's response")
		})
	}
}

// recordedResponseIDs lists the response ids the session's rollout records.
func recordedResponseIDs(t *testing.T, h *harness, session acp.SessionId) []string {
	t.Helper()

	matches, err := filepath.Glob(filepath.Join(h.home, "sessions", "*", "*", "*", "rollout-*-"+string(session)+".jsonl"))
	require.NoError(t, err)
	require.Len(t, matches, 1)

	rows, err := codex.ReadRows(matches[0])
	require.NoError(t, err)

	ids := make([]string, 0, len(rows))

	for _, row := range rows {
		decoded, decodeErr := codex.DecodeRow(row)
		require.NoError(t, decodeErr)

		if decoded.Type == "token_usage_record" {
			ids = append(ids, payloadString(decoded.Payload, "response_id"))
		}
	}

	require.NotEmpty(t, ids)

	return ids
}
