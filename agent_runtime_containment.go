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
	managedCodexHomeEnv     = "CODEX_HOME"
	managedHomeEnv          = "HOME"
	managedXDGConfigHomeEnv = "XDG_CONFIG_HOME"
)

var containmentGOOS = runtime.GOOS

// provesWholeTreeLifecycle reports whether the selected boundary can prove that
// every process it started has exited. Ordinary current-identity execution and
// Darwin best effort are deliberately non-authoritative.
func (mode RuntimeContainmentMode) provesWholeTreeLifecycle() bool {
	return mode == RuntimeContainmentAuthoritative
}

func containmentMode(options Options) RuntimeContainmentMode {
	if options.DarwinBestEffortContainment &&
		(containmentGOOS != containmentOSDarwin || options.ProcessIsolation != nil) {
		return RuntimeContainmentUnavailable
	}

	if options.DarwinBestEffortContainment {
		return RuntimeContainmentBestEffort
	}

	if options.ProcessIsolation == nil {
		return RuntimeContainmentSharedIdentity
	}

	if containmentGOOS == containmentOSLinux {
		return RuntimeContainmentAuthoritative
	}

	return RuntimeContainmentUnavailable
}

func validateContainmentOptions(options Options) error {
	if options.DarwinBestEffortContainment && options.ProcessIsolation != nil {
		return errors.New("darwin best-effort containment and explicit process isolation are mutually exclusive")
	}

	if options.DarwinBestEffortContainment && containmentGOOS != containmentOSDarwin {
		return errors.New("darwin best-effort containment is supported only on darwin")
	}

	if options.ProcessIsolation != nil && containmentGOOS != containmentOSLinux {
		return errors.New("explicit process isolation is supported only on linux")
	}

	for key := range options.Env {
		if reservedCodexEnvKey(key) {
			return fmt.Errorf("environment key %q is reserved for Codex adapter process management", key)
		}
	}

	return nil
}

func normalizeStandaloneHome(options *Options) error {
	if options == nil || options.ProcessIsolation == nil {
		return nil
	}

	isolation := options.ProcessIsolation
	if isolation.IdentityLock != nil || isolation.AuthorityDomain != nil || isolation.StandaloneStateRoot == "" {
		return nil
	}

	if options.Home == "" {
		options.Home = isolation.StandaloneStateRoot

		return nil
	}

	if options.Home != isolation.StandaloneStateRoot {
		return fmt.Errorf("WithHome must equal ProcessIsolation.StandaloneStateRoot %q", isolation.StandaloneStateRoot)
	}

	return nil
}

func reservedCodexEnvKey(key string) bool {
	upperKey := strings.ToUpper(key)

	return strings.HasPrefix(upperKey, privateAdapterEnvPrefix) ||
		upperKey == privateRuntimeIDEnv ||
		upperKey == privateScratchRootEnv ||
		managedCodexRootEnvKey(upperKey)
}

func managedCodexRootEnvKey(key string) bool {
	switch strings.ToUpper(key) {
	case managedCodexHomeEnv, managedHomeEnv,
		"XDG_CACHE_HOME", managedXDGConfigHomeEnv, "XDG_DATA_HOME", "XDG_RUNTIME_DIR", "XDG_STATE_HOME":
		return true
	default:
		return false
	}
}
