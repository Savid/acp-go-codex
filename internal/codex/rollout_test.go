package codex

import (
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// A recorded rollout location is re-rooted under the home by its own file
// name, which keeps the day directory codex derived from the same stamp
// whatever the local zone.
func TestRelocateRolloutKeepsTheRecordedName(t *testing.T) {
	t.Parallel()

	const thread = "01a10006-93a1-70b1-8822-d5aadfbeafd9"

	name := "rollout-2026-10-03T23-59-59-" + thread + ".jsonl"

	path, ok := RelocateRollout("/new/home", filepath.Join("/old/home/sessions/2026/10/03", name), thread)
	require.True(t, ok)
	require.Equal(t, filepath.Join("/new/home/sessions/2026/10/03", name), path)

	recorded := RolloutPath("/old/home", thread, time.Now())
	path, ok = RelocateRollout("/old/home", recorded, thread)
	require.True(t, ok)
	require.Equal(t, recorded, path)

	for _, refused := range []string{
		"/old/home/sessions/2026/10/03/rollout-2026-10-03T23-59-59-other-thread.jsonl",
		"/old/home/sessions/2026/10/03/rollout-2026-13-03T23-59-59-" + thread + ".jsonl",
		"/old/home/sessions/2026/10/03/" + thread + ".jsonl",
		"/old/home/notes.jsonl",
	} {
		_, ok = RelocateRollout("/new/home", refused, thread)
		require.False(t, ok, refused)
	}
}

func TestRepairCallArguments(t *testing.T) {
	t.Parallel()

	valid := []byte(`{"type":"response_item","payload":{"type":"function_call","call_id":"a","arguments":"{\"x\":1}"}}`)
	cut := []byte(`{"timestamp":"t","type":"response_item","payload":{"type":"function_call","call_id":"b","name":"shell","arguments":"{\"command\":[\"ls"}}`)
	output := []byte(`{"type":"response_item","payload":{"type":"function_call_output","call_id":"b","output":"{"}}`)
	event := []byte(`{"type":"event_msg","payload":{"type":"user_message","message":"{"}}`)

	rows, repaired, err := RepairCallArguments([][]byte{valid, cut, output, event})
	require.NoError(t, err)
	require.Equal(t, 1, repaired)
	require.Equal(t, valid, rows[0])
	require.JSONEq(t, `{"timestamp":"t","type":"response_item","payload":{"type":"function_call","call_id":"b","name":"shell","arguments":"{}"}}`, string(rows[1]))
	require.Equal(t, output, rows[2])
	require.Equal(t, event, rows[3])

	_, _, err = RepairCallArguments([][]byte{[]byte(`not json`)})
	require.Error(t, err)

	require.True(t, RefusesInvalidArguments(`{"error":{"message":"openai-responses: function_call call_3e9e has invalid JSON arguments","type":"invalid_request_error"}}`))
	require.False(t, RefusesInvalidArguments("boom"))
}
