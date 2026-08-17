package codexacp

import (
	"context"
	"errors"

	"github.com/savid/acp-go-codex/internal/codex"
)

// containCancelledTurn proves that a cancelled native turn cannot keep writing
// through a background terminal. The normal path is scoped to one Codex
// thread; a failed interrupt or scoped proof exceptionally fences the shared
// generation so an uncontained native process cannot escape the turn epoch.
func (s *session) containCancelledTurn(
	ctx context.Context,
	client codex.Client,
	threadID string,
	interruptErr error,
) error {
	containCtx, cancelContain := context.WithTimeout(context.WithoutCancel(ctx), closeTimeout)
	containErr := terminateThreadBackgroundTerminals(containCtx, client, threadID)

	cancelContain()

	var fenceErr error

	if interruptErr != nil || containErr != nil {
		fenceCtx, cancelFence := context.WithTimeout(context.WithoutCancel(ctx), closeTimeout)
		fenceErr = s.agent.quiesceRuntimeAfterCancel(fenceCtx, client)

		cancelFence()
	}

	// Native cleanup can finish the rollout after the live tail stopped. Read it
	// only after targeted containment or its generation-wide fallback so no
	// process can append behind the durable mirror.
	mirrorErr := s.mirrorAndEmitRollout(context.WithoutCancel(ctx))

	return errors.Join(interruptErr, containErr, fenceErr, mirrorErr)
}
