package codexacp

import (
	"testing"

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
