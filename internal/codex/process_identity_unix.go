//go:build unix && !linux

package codex

import (
	"errors"
	"os"
)

var (
	processEffectiveUID = os.Geteuid
	processEffectiveGID = os.Getegid
)

func currentProcessIdentity() (uint32, uint32, error) {
	uid, gid := processEffectiveUID(), processEffectiveGID()
	if uid < 0 || gid < 0 {
		return 0, 0, errors.New("current process identity is unavailable")
	}

	return uint32(uid), uint32(gid), nil //nolint:gosec // Kernel IDs fit the process-isolation wire width.
}
