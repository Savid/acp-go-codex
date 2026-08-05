//go:build !linux

package codex

import (
	"fmt"
	"os"
	"runtime"
)

func handoffGeneratedNativeTree(_ string, isolation *ProcessIsolation) error {
	if isolation == nil {
		return nil
	}

	if matchesEffectiveIdentity(isolation.UID, isolation.GID) {
		return nil
	}

	return fmt.Errorf("native path ownership handoff is unsupported on %s", runtime.GOOS)
}

func validateNativeOwnedDirectory(_ string, isolation *ProcessIsolation) error {
	if isolation == nil {
		return nil
	}

	if matchesEffectiveIdentity(isolation.UID, isolation.GID) {
		return nil
	}

	return fmt.Errorf("native path ownership validation is unsupported on %s", runtime.GOOS)
}

func matchesEffectiveIdentity(uid uint32, gid uint32) bool {
	return uid == uint32(os.Geteuid()) && gid == uint32(os.Getegid()) //nolint:gosec // Kernel IDs fit the public uint32 identity contract.
}
