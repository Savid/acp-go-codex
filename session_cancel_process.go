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
	containCtx, cancelContain := context.WithTimeout(context.WithoutCancel(ctx), closeTimeout)
	containErr := terminateThreadBackgroundTerminals(containCtx, client, threadID)

	cancelContain()

	if containErr != nil {
		s.setClientDead(true)
	}

	return errors.Join(interruptErr, containErr)
}
