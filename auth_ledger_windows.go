//go:build windows

package codexacp

// syncAuthLedgerDirectory does nothing on Windows, because there is nothing here
// it could do: FlushFileBuffers rejects a directory handle opened through
// os.Open, so asking for the flush would fail every ledger commit rather than
// harden one. What remains durable is the entry itself. Each record is written
// to a temporary file in the ledger root, chmodded, and fsynced before the
// rename that publishes it, so an entry that is visible is an entry whose bytes
// reached the disk. NTFS journals the rename's own metadata, so the directory
// update the flush would have forced is already recovered by the filesystem.
func syncAuthLedgerDirectory(string) error { return nil }
