package codexacp

import (
	"context"

	"github.com/savid/acp-go-codex/internal/codex"
	"github.com/savid/acp-go-core/wire"
)

func (s *session) projectCompaction(ctx context.Context, event codex.Event) error {
	if event.ThreadID != "" && event.ThreadID != s.nativeID {
		return nil
	}

	if event.ItemID == "" {
		return nil
	}

	return s.compactions.Publish(ctx, s.agent.connection(), s.id, event.TurnID+"/"+event.ItemID, wire.Compaction{Status: wire.CompactionCompleted})
}
