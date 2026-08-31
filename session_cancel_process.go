package codexacp

import (
	"context"
	"errors"

	"github.com/savid/acp-go-codex/internal/codex"
)

// containCancelledTurn completes only the cancelled thread's protocol cleanup.
// The app-server is runtime-owned, so a session boundary never revokes it.
func (s *session) containCancelledTurn(
	ctx context.Context,
	client codex.Client,
	threadID string,
	interruptErr error,
) error {
	return s.containCancelledTurnWithPolicy(ctx, client, threadID, interruptErr, false)
}

func (s *session) containCancelledTurnWithPolicy(
	ctx context.Context,
	client codex.Client,
	threadID string,
	interruptErr error,
	_ bool,
) error {
	containCtx, cancelContain := context.WithTimeout(context.WithoutCancel(ctx), closeTimeout)
	containErr := terminateThreadBackgroundTerminals(containCtx, client, threadID)

	cancelContain()

	if containErr != nil {
		s.setClientDead(true)
	}

	// Native cleanup can finish the rollout after the event terminal. Read it
	// only after targeted protocol cleanup so no thread event can append behind
	// the durable mirror.
	mirrorErr := s.mirrorAndEmitRollout(context.WithoutCancel(ctx))

	return errors.Join(interruptErr, containErr, mirrorErr)
}
