package codexacp

import (
	"fmt"
	"os"
)

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
