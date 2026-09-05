package codexacp

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/coder/acp-go-sdk"
)

// Codex 0.153.2 threads are paginated: the app-server resolves a thread by id
// against the rollout files under its own CODEX_HOME and refuses a rollout
// handed to it by path. Restoring a stored session therefore means making the
// stored rows resident in the home the way Codex writes them itself — under
// `sessions/`, named `rollout-<timestamp>-<threadId>.jsonl` — and then resuming
// by thread id alone.
const (
	nativeSessionsDirName    = "sessions"
	nativeRolloutNamePrefix  = "rollout-"
	nativeRolloutNameSuffix  = ".jsonl"
	nativeRolloutStampLayout = "2006-01-02T15-04-05"
	nativeRolloutDayLayout   = "2006/01/02"
)

var materializedRolloutNow = time.Now

func (a *Agent) materializeStoredRollout(
	entries []SessionStoreEntry,
	release func(),
) (string, func(), int64, error) {
	if len(entries) == 0 {
		release()

		return "", func() {}, 0, nil
	}

	bytes := materializedRolloutBytes(entries)

	path, err := materializeRollout(a.resolvedCodexHome(), entries)
	if err != nil {
		release()

		return "", nil, 0, err
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
	createMaterializedRolloutTemp = func(dir string) (materializedRolloutFile, error) {
		// The staging name must not look like a rollout: Codex scans the whole
		// `sessions/` subtree and parses every `rollout-*.jsonl` name it finds.
		return os.CreateTemp(dir, ".acp-go-codex-rollout-*.staging")
	}
	renameMaterializedRolloutFile = os.Rename
	removeMaterializedRolloutFile = os.Remove
	mkdirMaterializedRolloutDir   = os.MkdirAll
)

// nativeRolloutResidence is the exact path a stored rollout must occupy inside
// the app-server's home for `thread/resume` to adopt it by thread id.
func nativeRolloutResidence(home string, entries []SessionStoreEntry) (string, error) {
	if home == "" {
		return "", errors.New("codex home is unknown, so a stored rollout cannot be made resident")
	}

	threadID := rolloutNativeThreadID(entries)
	if !validNativeThreadIDForResidence(threadID) {
		return "", errors.New("stored rollout does not name a native thread id that can be made resident")
	}

	stamp := nativeRolloutResidenceStamp(entries)
	name := nativeRolloutNamePrefix + stamp.Format(nativeRolloutStampLayout) + "-" + threadID + nativeRolloutNameSuffix

	return filepath.Join(home, nativeSessionsDirName, filepath.FromSlash(stamp.Format(nativeRolloutDayLayout)), name), nil
}

// validNativeThreadIDForResidence keeps a host-supplied thread id from naming
// anything but one file in the day directory this rollout belongs to.
func validNativeThreadIDForResidence(threadID string) bool {
	if threadID == "" || len(threadID) > 128 {
		return false
	}

	for _, r := range threadID {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9':
		case r == '-', r == '_':
		default:
			return false
		}
	}

	return true
}

// nativeRolloutResidenceStamp reads the session's own start time so one stored
// session always resolves to one path. Codex names the file in local time, so
// the reconstruction does too.
func nativeRolloutResidenceStamp(entries []SessionStoreEntry) time.Time {
	for _, entry := range entries {
		row, err := decodeRolloutRow(entry)
		if err != nil || row.Type != rolloutTypeSessionMeta {
			continue
		}

		stamp := strings.TrimSpace(stringFromAny(row.Payload["timestamp"]))
		if stamp == "" {
			break
		}

		parsed, parseErr := time.Parse(time.RFC3339, stamp)
		if parseErr != nil {
			break
		}

		return parsed.Local()
	}

	return materializedRolloutNow()
}

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
