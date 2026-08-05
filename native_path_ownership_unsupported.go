//go:build !linux

package codexacp

import (
	"fmt"
	"os"
	"runtime"
)

func handoffGeneratedNativeTreePlatform(_ string, uid uint32, gid uint32) error {
	if matchesEffectiveIdentity(uid, gid) {
		return nil
	}

	return fmt.Errorf("native path ownership handoff is unsupported on %s", runtime.GOOS)
}

func validateNativeOwnedDirectoryPlatform(_ string, uid uint32, gid uint32) error {
	if matchesEffectiveIdentity(uid, gid) {
		return nil
	}

	return fmt.Errorf("native path ownership validation is unsupported on %s", runtime.GOOS)
}

func matchesEffectiveIdentity(uid uint32, gid uint32) bool {
	return uid == uint32(os.Geteuid()) && gid == uint32(os.Getegid()) //nolint:gosec // Kernel IDs fit the public uint32 identity contract.
}
