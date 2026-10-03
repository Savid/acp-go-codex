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
