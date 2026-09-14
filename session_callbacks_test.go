package codexacp

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestLateDialogAfterCancellationIsRefused(t *testing.T) {
	t.Parallel()
	for _, state := range []string{"cancelled turn", "timed out", "closed", "disconnected"} {
		t.Run(state, func(t *testing.T) {
			t.Parallel()
			s := &session{rt: &runtime{}, turn: &turn{}}
			switch state {
			case "cancelled turn":
				s.turn.cancelled = true
			case "timed out":
				s.turn.timedOut = true
			case "closed":
				s.closing = true
			case "disconnected":
				s.rt = nil
			}
			ctx, cancel := context.WithCancelCause(t.Context())
			defer cancel(nil)
			release := s.registerDialog("late-native-request", cancel)
			require.ErrorIs(t, context.Cause(ctx), errDialogCancelled)
			release()
			s.callbacks.Wait()
		})
	}
}
