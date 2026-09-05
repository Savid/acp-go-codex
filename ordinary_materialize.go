package codexacp

import (
	"fmt"
	"os"
	"path/filepath"
)

type materializedRolloutFile interface {
	Name() string
	Write([]byte) (int, error)
	Close() error
}

var (
	createMaterializedRolloutTemp = func(dir string) (materializedRolloutFile, error) {
		// The staging name must not look like a rollout: Codex scans the whole
		// `sessions/` subtree and parses every `rollout-*.jsonl` name it finds.
		return os.CreateTemp(dir, ".acp-go-codex-rollout-*.staging")
	}
	renameMaterializedRolloutFile = os.Rename
	removeMaterializedRolloutFile = os.Remove
	mkdirMaterializedRolloutDir   = os.MkdirAll
)

// materializeRollout writes the stored rows into the home as one whole file:
// the staged copy is renamed into place, so Codex never scans a partial
// rollout for a thread it is about to be asked to resume.
func materializeRollout(home string, entries []SessionStoreEntry) (string, error) {
	if len(entries) == 0 {
		return "", nil
	}

	target, err := nativeRolloutResidence(home, entries)
	if err != nil {
		return "", err
	}

	dir := filepath.Dir(target)
	if mkdirErr := mkdirMaterializedRolloutDir(dir, 0o700); mkdirErr != nil {
		return "", fmt.Errorf("create native rollout directory: %w", mkdirErr)
	}

	file, err := createMaterializedRolloutTemp(dir)
	if err != nil {
		return "", fmt.Errorf("create materialized rollout: %w", err)
	}

	staged := file.Name()

	for _, entry := range entries {
		if _, err := file.Write(entry); err != nil {
			_ = file.Close()
			_ = removeMaterializedRolloutFile(staged)

			return "", fmt.Errorf("write materialized rollout: %w", err)
		}

		if _, err := file.Write([]byte{'\n'}); err != nil {
			_ = file.Close()
			_ = removeMaterializedRolloutFile(staged)

			return "", fmt.Errorf("write materialized rollout newline: %w", err)
		}
	}

	if err := file.Close(); err != nil {
		_ = removeMaterializedRolloutFile(staged)

		return "", fmt.Errorf("close materialized rollout: %w", err)
	}

	if err := renameMaterializedRolloutFile(staged, target); err != nil {
		_ = removeMaterializedRolloutFile(staged)

		return "", fmt.Errorf("place materialized rollout: %w", err)
	}

	return target, nil
}

func removeMaterializedRollout(path string) error {
	if path == "" {
		return nil
	}

	if err := removeMaterializedRolloutFile(path); err != nil && !os.IsNotExist(err) {
		return err
	}

	return nil
}
