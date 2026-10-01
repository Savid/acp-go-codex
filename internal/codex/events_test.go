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

	event := decode(`{"threadId":"t","turnId":"u","responseId":"r","usage":{"totalTokens":7106,"inputTokens":7043,` +
		`"cachedInputTokens":5632,"cacheWriteInputTokens":0,"outputTokens":63,"reasoningOutputTokens":11},"usageMetadata":null}`)
	require.Equal(t, EventResponseCompleted, event.Kind)
	require.Equal(t, "t", event.ThreadID)
	require.Equal(t, "u", event.TurnID)
	require.Equal(t, &Usage{Input: 7043, CachedRead: 5632, Output: 63, Reasoning: 11, Total: 7106}, event.Response)

	event = decode(`{"threadId":"t","turnId":"u","responseId":"r","usage":null,"usageMetadata":null}`)
	require.Equal(t, EventResponseCompleted, event.Kind)
	require.Nil(t, event.Response)
}
