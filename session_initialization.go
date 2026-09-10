package codexacp

import (
	"context"
	"fmt"

	"github.com/savid/acp-go-codex/internal/codex"
)

func (s *session) initializeDurableThread(ctx context.Context, thread codex.Thread) error {
	entry, err := codex.InitialThreadRollout(thread)
	if err != nil {
		return err
	}

	s.mirrorMu.Lock()
	defer s.mirrorMu.Unlock()

	if err := s.commitRolloutEntries(ctx, s.agent.sessionStore(), []SessionStoreEntry{entry}, 0); err != nil {
		return fmt.Errorf("initialize durable Codex thread: %w", err)
	}

	s.initialRollout = true

	return nil
}

func (s *session) replaceInitialRollout(ctx context.Context, store SessionStore, entries []SessionStoreEntry) error {
	if _, err := validateStoredRolloutEntries(entries); err != nil {
		return err
	}

	if rolloutNativeThreadID(entries) != s.codexThreadID {
		return fmt.Errorf("initial Codex rollout does not match native thread %q", s.codexThreadID)
	}

	ctx, cancel := context.WithTimeout(ctx, sessionRolloutAppendTimeout)
	defer cancel()

	main := SessionKey{SessionID: string(s.id)}

	subpaths, err := store.ListSubkeys(ctx, main)
	if err != nil {
		return err
	}

	replacements := []SessionStoreReplacement{{Key: main, Entries: entries}}

	for _, subpath := range subpaths {
		key := SessionKey{SessionID: string(s.id), Subpath: subpath}

		records, err := store.Load(ctx, key)
		if err != nil {
			return err
		}

		replacements = append(replacements, SessionStoreReplacement{Key: key, Entries: records})
	}

	return store.Replace(ctx, main, replacements)
}
