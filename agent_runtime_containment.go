package codexacp

import (
	"errors"
	"fmt"
	"os"
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

// containmentEffectiveUID is the seam the shared-identity report is derived
// through. The mode is selected from a faked GOOS in tests, so the identity it
// is compared against has to be selectable there too.
var containmentEffectiveUID = os.Geteuid

// sharedProcessIdentity reports whether the configured native identity is the
// identity this process already runs as. Root never qualifies: a zero effective
// uid is the trusted supervisor identity, and the native uid is required to be
// nonzero.
func sharedProcessIdentity(isolation *ProcessIsolation) bool {
	if isolation == nil {
		return false
	}

	effectiveUID := containmentEffectiveUID()

	return effectiveUID > 0 && uint64(isolation.UID) == uint64(effectiveUID)
}

// provesWholeTreeLifecycle reports whether the selected boundary can prove that
// every process it started has exited. Both Linux boundaries can: they differ
// in whether the agent runs under its own credentials, not in what the
// subreaper observes.
func (mode RuntimeContainmentMode) provesWholeTreeLifecycle() bool {
	return mode == RuntimeContainmentAuthoritative || mode == RuntimeContainmentSharedIdentity
}

func containmentMode(options Options) RuntimeContainmentMode {
	if options.DarwinBestEffortContainment && containmentGOOS != containmentOSDarwin {
		return RuntimeContainmentUnavailable
	}

	switch containmentGOOS {
	case containmentOSLinux:
		if sharedProcessIdentity(options.ProcessIsolation) {
			return RuntimeContainmentSharedIdentity
		}

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
