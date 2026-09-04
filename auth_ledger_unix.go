//go:build !windows

package codexacp

import (
	"errors"
	"fmt"
)

// syncAuthLedgerDirectory flushes the ledger root after a commit, so the rename
// that publishes an entry is durable rather than merely visible to the running
// kernel.
func syncAuthLedgerDirectory(path string) error {
	dir, err := ledgerOpen(path)
	if err != nil {
		return fmt.Errorf("open provider auth ledger root: %w", err)
	}

	return errors.Join(dir.Sync(), dir.Close())
}
