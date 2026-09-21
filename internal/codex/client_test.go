package codex

import (
	"testing"

	"github.com/stretchr/testify/require"
)

// thread/start and thread/resume answer with the same Thread shape, and the
// rollout path it names is the adapter's only mirror source, so both routes
// refuse a thread they could not mirror.
func TestThreadResponseNamesItsIDAndRollout(t *testing.T) {
	t.Parallel()

	thread, err := threadFromResponse(map[string]any{
		"thread": map[string]any{"id": "thread-1", "path": "/home/sessions/rollout.jsonl", "model": "gpt-5"},
	})
	require.NoError(t, err)
	require.Equal(t, Thread{ID: "thread-1", Path: "/home/sessions/rollout.jsonl", Model: "gpt-5"}, thread)

	_, err = threadFromResponse(map[string]any{"thread": map[string]any{"path": "/home/sessions/rollout.jsonl"}})
	require.Error(t, err, "a thread with no id was bound")

	_, err = threadFromResponse(map[string]any{"thread": map[string]any{"id": "thread-1"}})
	require.Error(t, err, "a thread whose rollout the adapter cannot read was bound")

	_, err = threadFromResponse(map[string]any{"thread": map[string]any{"id": "thread-1", "path": ""}})
	require.Error(t, err, "a thread whose rollout the adapter cannot read was bound")
}
