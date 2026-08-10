//go:build !linux

package codexacp

import (
	"fmt"
	"runtime"
)

func handoffGeneratedNativeTreePlatform(_ string, _ uint32, _ uint32) error {
	return fmt.Errorf("native path ownership handoff is unsupported on %s", runtime.GOOS)
}

func validateNativeOwnedDirectoryPlatform(_ string, _ uint32, _ uint32) error {
	return fmt.Errorf("native path ownership validation is unsupported on %s", runtime.GOOS)
}
