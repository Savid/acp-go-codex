package codexacp

import (
	"context"
	"fmt"
	"os"
	"path/filepath"

	"github.com/coder/acp-go-sdk"
)

func (a *Agent) materializeStoredRollout(
	ctx context.Context,
	entries []SessionStoreEntry,
	release func(),
) (string, func(), int64, error) {
	if len(entries) == 0 {
		release()

		return "", func() {}, 0, nil
	}

	bytes := materializedRolloutBytes(entries)

	path, err := materializeRollout(a.scratchDir, entries)
	if err != nil {
		release()

		return "", nil, 0, err
	}

	if a.options.HostAuthority != nil {
		if err := a.options.HostAuthority.PrepareNativeTree(ctx, filepath.Dir(path)); err != nil {
			return "", nil, 0, a.retainOpaqueNativeTree(err)
		}
	}

	return path, release, bytes, nil
}

func (a *Agent) hydrateStoredRollout(
	ctx context.Context,
	sessionID acp.SessionId,
	entries []SessionStoreEntry,
) ([]SessionStoreEntry, int64, error) {
	hydrated, err := a.hydrateStoredImageArtifacts(ctx, sessionID, entries)
	if err != nil {
		return nil, 0, err
	}

	return hydrated, materializedRolloutBytes(hydrated), nil
}

func materializedRolloutBytes(entries []SessionStoreEntry) int64 {
	var bytes int64
	for _, entry := range entries {
		bytes += int64(len(entry) + 1)
	}

	return bytes
}

type materializedRolloutFile interface {
	Name() string
	Write([]byte) (int, error)
	Close() error
}

var (
	materializedRolloutTempDirPrefix = "acp-go-codex-rollout-"
	createMaterializedRolloutTemp    = func(scratchDir string) (materializedRolloutFile, error) {
		return createPrivateTempFile(scratchDir, materializedRolloutTempDirPrefix, "rollout-*.jsonl")
	}
	removeMaterializedRolloutFile = os.Remove
)

func materializeRollout(scratchDir string, entries []SessionStoreEntry) (string, error) {
	if len(entries) == 0 {
		return "", nil
	}

	file, err := createMaterializedRolloutTemp(scratchDir)
	if err != nil {
		return "", fmt.Errorf("create materialized rollout: %w", err)
	}

	name := file.Name()

	for _, entry := range entries {
		if _, err := file.Write(entry); err != nil {
			_ = file.Close()
			_ = removeMaterializedRollout(name)

			return "", fmt.Errorf("write materialized rollout: %w", err)
		}

		if _, err := file.Write([]byte{'\n'}); err != nil {
			_ = file.Close()
			_ = removeMaterializedRollout(name)

			return "", fmt.Errorf("write materialized rollout newline: %w", err)
		}
	}

	if err := file.Close(); err != nil {
		_ = removeMaterializedRollout(name)

		return "", fmt.Errorf("close materialized rollout: %w", err)
	}

	return name, nil
}

func removeMaterializedRollout(path string) error {
	return removePrivateTempFile(path, materializedRolloutTempDirPrefix, removeMaterializedRolloutFile)
}
