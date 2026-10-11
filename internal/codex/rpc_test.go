package codex

import (
	"io"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestDecodeInboundTakesParamsInOnePass(t *testing.T) {
	t.Parallel()

	frame, ok := decodeInbound([]byte(`{"jsonrpc":"2.0","method":"item/agentMessage/delta","params":{"threadId":"t","delta":"hi"}}`))
	require.True(t, ok)
	require.Equal(t, "item/agentMessage/delta", frame.Method)
	require.Equal(t, map[string]any{"threadId": "t", "delta": "hi"}, frame.Params)

	frame, ok = decodeInbound([]byte(`{"jsonrpc":"2.0","id":7,"method":"item/tool/requestUserInput","params":[1,2]}`))
	require.True(t, ok, "params that are not an object refused the frame")
	require.Equal(t, "7", string(frame.ID))
	require.Nil(t, frame.Params)

	_, ok = decodeInbound([]byte(`not json`))
	require.False(t, ok)
}

// A frame larger than any single read is delivered whole, and the stream
// keeps reading after it.
func TestClientDeliversLargeFrames(t *testing.T) {
	t.Parallel()

	large := strings.Repeat("x", 12<<20)
	stream := `{"method":"item/completed","params":{"threadId":"t","result":"` + large + `"}}` + "\n" +
		`{"method":"turn/completed","params":{"threadId":"t"}}` + "\n"

	client := NewClient(io.Discard, strings.NewReader(stream))
	require.NoError(t, client.Start(t.Context()))

	first := <-client.Notifications()
	require.Equal(t, large, first.Params["result"])

	second := <-client.Notifications()
	require.Equal(t, "turn/completed", second.Method)

	_, open := <-client.Notifications()
	require.False(t, open)
	require.NoError(t, client.Err())
}
