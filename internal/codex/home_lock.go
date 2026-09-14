package codex

import (
	"path/filepath"

	"github.com/savid/acp-go-core/process"
)

// LockHome reserves this native home until the app-server has been waited on.
func LockHome(home string) (*process.FileLock, error) {
	return process.LockFile(filepath.Join(home, ".acp-go-codex.lock"))
}
