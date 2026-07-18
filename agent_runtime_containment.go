package codexacp

import (
	"errors"
	"fmt"
	"runtime"
	"strings"
)

const (
	containmentOSDarwin     = "darwin"
	containmentOSLinux      = "linux"
	containmentOSWindows    = "windows"
	privateAdapterEnvPrefix = "ACP_" + "GO_CODEX_INTERNAL_"
	privateRuntimeIDEnv     = "ACP_GO_CODEX_RUNTIME_ID"
	privateScratchRootEnv   = "ACP_GO_CODEX_SCRATCH_ROOT"
)

var containmentGOOS = runtime.GOOS

func containmentMode(options Options) RuntimeContainmentMode {
	if options.DarwinBestEffortContainment && containmentGOOS != containmentOSDarwin {
		return RuntimeContainmentUnavailable
	}

	switch containmentGOOS {
	case containmentOSLinux, containmentOSWindows:
		return RuntimeContainmentAuthoritative
	case containmentOSDarwin:
		if options.DarwinBestEffortContainment {
			return RuntimeContainmentBestEffort
		}
	}

	return RuntimeContainmentUnavailable
}

func validateContainmentOptions(options Options) error {
	if options.DarwinBestEffortContainment && containmentGOOS != containmentOSDarwin {
		return errors.New("darwin best-effort containment is supported only on darwin")
	}

	for key := range options.Env {
		if reservedCodexEnvKey(key) {
			return fmt.Errorf("environment key %q is reserved for Codex adapter process management", key)
		}
	}

	return nil
}

func reservedCodexEnvKey(key string) bool {
	upperKey := strings.ToUpper(key)

	return strings.HasPrefix(upperKey, privateAdapterEnvPrefix) ||
		upperKey == privateRuntimeIDEnv ||
		upperKey == privateScratchRootEnv
}
