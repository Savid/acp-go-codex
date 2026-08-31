package codexacp

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/savid/acp-go-codex/internal/codex"
)

const (
	backgroundTerminalListLimit = 100
	backgroundTerminalRetryWait = 10 * time.Millisecond
)

// terminateThreadBackgroundTerminals is the native containment proof for a
// cancelled turn. The app-server is shared, so cleanup must never cross the
// owning thread boundary or close the runtime generation that serves peers.
//
// A sweep that was offered and did not prove the thread contained is a boundary
// that did not complete, and it says so with the family's stable discriminator:
// this result reaches session close, Agent.Close, and Serve unchanged, and each
// of those surfaces owes the host an error matching
// ErrProcessContainmentIncomplete. A client that offers no thread-scoped
// containment at all is the one failure that is not that: nothing was selected
// here, so the caller's generation fence is the boundary and classifies its own
// result.
func terminateThreadBackgroundTerminals(
	ctx context.Context,
	client codex.Client,
	threadID string,
) error {
	err := sweepThreadBackgroundTerminals(ctx, client, threadID)
	if err == nil || errors.Is(err, codex.ErrBackgroundTerminalsUnsupported) {
		return err
	}

	return fmt.Errorf("%w: %w", codex.ErrProcessContainmentIncomplete, err)
}

func sweepThreadBackgroundTerminals(
	ctx context.Context,
	client codex.Client,
	threadID string,
) error {
	terminals, ok := client.(codex.BackgroundTerminalClient)
	if !ok {
		return fmt.Errorf("required thread-scoped background terminal containment: %w: this client exposes no thread-scoped containment surface",
			codex.ErrBackgroundTerminalsUnsupported)
	}

	for {
		processIDs, err := listThreadBackgroundTerminalIDs(ctx, terminals, threadID)
		if err != nil {
			return err
		}

		if len(processIDs) == 0 {
			return nil
		}

		terminatedAny := false

		for _, processID := range processIDs {
			terminated, terminateErr := terminals.TerminateBackgroundTerminal(
				ctx,
				codex.BackgroundTerminalTerminateRequest{ThreadID: threadID, ProcessID: processID},
			)
			if terminateErr != nil {
				return fmt.Errorf("terminate Codex background terminal %q: %w", processID, terminateErr)
			}

			terminatedAny = terminatedAny || terminated
		}

		if !terminatedAny {
			remaining, listErr := listThreadBackgroundTerminalIDs(ctx, terminals, threadID)
			if listErr != nil {
				return listErr
			}

			if len(remaining) == 0 {
				return nil
			}

			return fmt.Errorf(
				"codex background terminal containment remained unproven for thread %q: %s",
				threadID,
				strings.Join(remaining, ", "),
			)
		}

		timer := time.NewTimer(backgroundTerminalRetryWait)
		select {
		case <-ctx.Done():
			timer.Stop()

			return fmt.Errorf("verify Codex background terminal containment for thread %q: %w", threadID, ctx.Err())
		case <-timer.C:
		}
	}
}

func listThreadBackgroundTerminalIDs(
	ctx context.Context,
	client codex.BackgroundTerminalClient,
	threadID string,
) ([]string, error) {
	if threadID == "" {
		return nil, fmt.Errorf("list Codex background terminals: threadId is required")
	}

	processIDs := make(map[string]struct{})
	seenCursors := make(map[string]struct{})
	cursor := ""

	for {
		page, err := client.ListBackgroundTerminals(ctx, codex.BackgroundTerminalListRequest{
			ThreadID: threadID,
			Cursor:   cursor,
			Limit:    backgroundTerminalListLimit,
		})
		if err != nil {
			return nil, fmt.Errorf("list Codex background terminals for thread %q: %w", threadID, err)
		}

		for _, terminal := range page.Terminals {
			if terminal.ProcessID == "" {
				return nil, fmt.Errorf(
					"list Codex background terminals for thread %q: response item is missing processId",
					threadID,
				)
			}

			processIDs[terminal.ProcessID] = struct{}{}
		}

		if page.NextCursor == "" {
			break
		}

		if _, duplicate := seenCursors[page.NextCursor]; duplicate {
			return nil, fmt.Errorf(
				"list Codex background terminals for thread %q: repeated cursor %q",
				threadID,
				page.NextCursor,
			)
		}

		seenCursors[page.NextCursor] = struct{}{}
		cursor = page.NextCursor
	}

	result := make([]string, 0, len(processIDs))
	for processID := range processIDs {
		result = append(result, processID)
	}

	sort.Strings(result)

	return result, nil
}
