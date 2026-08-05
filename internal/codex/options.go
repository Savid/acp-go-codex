package codex

import (
	"context"
	"log/slog"
	"os"
	"time"
)

// Options configures the Codex provider boundary.
type Options struct {
	CLIPath                    string
	CodexHome                  string
	WritableHome               string
	SupervisorRoot             string
	SupervisorParent           string
	DarwinBestEffort           bool
	NativeVersion              string
	DefaultModel               string
	Env                        map[string]string
	ProcessIsolation           *ProcessIsolation
	Config                     map[string]any
	ExtraArgs                  []string
	Logger                     *slog.Logger
	RequestHandler             RequestHandler
	EventHandler               func(context.Context, Event)
	LaunchTimeout              time.Duration
	ObserveProcess             func(context.Context, string, int64)
	ObserveStartupStage        func(context.Context, string, string, time.Duration, error)
	NewProcessSnapshotObserver func(context.Context) ProcessSnapshotObserver

	// skipSupervisor is restricted to package tests that exercise low-level
	// command error branches. Production launches always use the supervisor
	// pair and fail closed without its private scratch root.
	skipSupervisor bool
}

// ProcessIsolation is the mandatory credential and complete environment base
// applied to every provider process.
type ProcessIdentityLockCapability interface {
	Duplicate() (*os.File, error)
}

type ProcessIsolation struct {
	UID                      uint32
	GID                      uint32
	BaseEnvironment          map[string]string
	StandaloneOwnerID        string
	StandaloneStateRoot      string
	IdentityLock             ProcessIdentityLockCapability
	AuthorityDomain          ProcessIdentityLockCapability
	identityAuthorityAdopted bool
}

// ProcessSnapshotObserver reports containment-proven absolute inventory for
// one native root. Unproven suppresses future aggregates after a failed proof.
type ProcessSnapshotObserver struct {
	Observe   func(context.Context, int)
	Quiescent func(context.Context)
	Unproven  func()
}
