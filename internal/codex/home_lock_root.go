package codex

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
)

var homeLockAbsolutePath = filepath.Abs

// homeLockChmod carries os.Chmod so a test can fault the protection step. A
// directory MkdirAll just created or accepted always takes a mode change from
// its owner, so the guard below is unreachable through the real call.
var homeLockChmod = os.Chmod

func HomeLockRoot(scratchParent string, writableHome string) (string, error) {
	if scratchParent == "" {
		return "", fmt.Errorf("codex home-lock scratch parent is required")
	}

	home, err := homeLockAbsolutePath(writableHome)
	if err != nil {
		return "", fmt.Errorf("resolve codex writable home for locking: %w", err)
	}

	if resolved, resolveErr := filepath.EvalSymlinks(home); resolveErr == nil {
		home = resolved
	}

	sum := sha256.Sum256([]byte(filepath.Clean(home)))
	root := filepath.Join(scratchParent, "acp-go-codex-home-locks", hex.EncodeToString(sum[:]))

	if err := os.MkdirAll(root, 0o700); err != nil {
		return "", fmt.Errorf("create codex trusted home-lock root: %w", err)
	}

	if err := homeLockChmod(root, 0o700); err != nil {
		return "", fmt.Errorf("protect codex trusted home-lock root: %w", err)
	}

	return root, nil
}
