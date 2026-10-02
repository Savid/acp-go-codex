package codex

import (
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestResponseCompletionCarriesItsUsage(t *testing.T) {
	t.Parallel()

	decode := func(params string) Event {
		return DecodeEvent(Notification{Method: "rawResponse/completed", Params: json.RawMessage(params)})
	}

	// A completion codex 0.159.3 sent for an OpenRouter Responses gateway.
	event := decode(`{"responseId":"gen-1790864938-0aKnVVHJ8cC28luaL5g3","threadId":"01a0f7de-5336-72f2-a715-d9211c5fa1b4",` +
		`"turnId":"01a0f7de-5389-7613-b9a0-d61d0b7d2ebc","usage":{"cacheWriteInputTokens":0,"cachedInputTokens":9472,` +
		`"inputTokens":9537,"outputTokens":18,"reasoningOutputTokens":14,"totalTokens":9555},"usageMetadata":{"amount":null,` +
		`"metadata":{"cost":0.000169762,"input_tokens":9537,"input_tokens_details":{"cache_write_tokens":0,"cached_tokens":9472},` +
		`"is_byok":false,"output_tokens":18,"output_tokens_details":{"reasoning_tokens":14},"total_tokens":9555}}}`)
	require.Equal(t, EventResponseCompleted, event.Kind)
	require.Equal(t, "01a0f7de-5336-72f2-a715-d9211c5fa1b4", event.ThreadID)
	require.Equal(t, "01a0f7de-5389-7613-b9a0-d61d0b7d2ebc", event.TurnID)
	require.Equal(t, "gen-1790864938-0aKnVVHJ8cC28luaL5g3", event.ResponseID)
	require.Equal(t, &Usage{Input: 9537, CachedRead: 9472, Output: 18, Reasoning: 14, Total: 9555}, event.Response)

	event = decode(`{"threadId":"t","turnId":"u","responseId":"r","usage":null,"usageMetadata":null}`)
	require.Equal(t, EventResponseCompleted, event.Kind)
	require.Equal(t, "r", event.ResponseID)
	require.Nil(t, event.Response)
}

// TestStreamedTextNamesNoResponse proves the text codex streams carries no
// response id: a delta names only its item and turn.
func TestStreamedTextNamesNoResponse(t *testing.T) {
	t.Parallel()

	// Deltas codex 0.159.3 sent for the response the completion above names.
	frames := map[string]string{
		notifyReasoningDelta: `{"contentIndex":0,"delta":"The","itemId":"rs_tmp_ihu3dhadkwh",` +
			`"threadId":"01a0f7de-5336-72f2-a715-d9211c5fa1b4","turnId":"01a0f7de-5389-7613-b9a0-d61d0b7d2ebc"}`,
		notifyAgentMessageDelta: `{"delta":"hi","itemId":"msg_tmp_ukwp09jpqa",` +
			`"threadId":"01a0f7de-5336-72f2-a715-d9211c5fa1b4","turnId":"01a0f7de-5389-7613-b9a0-d61d0b7d2ebc"}`,
	}

	for method, params := range frames {
		event := DecodeEvent(Notification{Method: method, Params: json.RawMessage(params)})
		require.NotEmpty(t, event.Text)
		require.Empty(t, event.ResponseID)
	}
}
