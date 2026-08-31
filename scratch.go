package codexacp

import (
	"os"

	"github.com/savid/acp-go-codex/internal/codex"
)

func init() {
	codex.SetScratchParentResolver(ensureScratchParent)
}

func resolveScratchDir(options Options) string {
	return options.ScratchDir
}

func scratchParent(dir string) string {
	if dir == "" {
		return os.TempDir()
	}

	return dir
}

func ensureScratchParent(dir string) (string, error) {
	parent := scratchParent(dir)
	if err := os.MkdirAll(parent, 0o700); err != nil {
		return "", err
	}

	return parent, nil
}
