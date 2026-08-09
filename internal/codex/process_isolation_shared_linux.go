//go:build linux

package codex

import (
	"errors"
	"fmt"
	"os"
)

// Seams for the identity the shared arm is selected by. They are deliberately
// not effectiveUIDSource: the gates that stand for the privilege boundary read
// their own euid through that seam, and their cases fake it to reach refusals a
// real euid cannot produce. Reading the same seam here would hand those cases
// the shared arm instead of the refusal they pin.
var (
	processEffectiveUID = os.Geteuid
	processEffectiveGID = os.Getegid
)

// sharedSupervisorIdentity reports whether uid names the identity this process
// already runs as. Nothing separates the two ends of the launch in that shape,
// so every step that exists to cross the boundary has nothing to cross. A zero
// effective uid never qualifies: the supervisor holds the trusted identity
// there, and a nonzero native uid is required everywhere, so the two can never
// name the same identity.
func sharedSupervisorIdentity(uid uint32) bool {
	effectiveUID := processEffectiveUID()

	return effectiveUID > 0 && uint64(uid) == uint64(effectiveUID)
}

// sharedProcessIdentity reports the same answer for a whole isolation shape.
func sharedProcessIdentity(isolation *ProcessIsolation) bool {
	return isolation != nil && sharedSupervisorIdentity(isolation.UID)
}

// validateSupervisorIdentityDisposition makes each member of the supervisor
// pair re-derive the arm it is running on from its own identity and refuse a
// sealed config that disagrees. The parent stamps what it decided; a child that
// is not the identity the stamp claims, or that is the identity a stamp denies,
// has been handed a launch it cannot perform, so it fails closed either way.
// The shared arm also writes no authority record, so a stamp that promises one
// is refused with it.
func validateSupervisorIdentityDisposition(config supervisorConfig) error {
	if config.SharedIdentity != sharedSupervisorIdentity(config.IsolationUID) {
		return errors.New("codex supervisor identity disposition does not match the identity it runs as")
	}

	if !config.SharedIdentity {
		return nil
	}

	if config.IdentityLock || config.AuthorityDomain || config.StandaloneAuthority ||
		config.StandaloneOwnerID != "" || config.StandaloneStateRoot != "" {
		return errors.New("codex shared supervisor identity disposition is invalid")
	}

	return nil
}

// sharedProcessCredential reports whether the launch must request no credential
// change at all. That is the only honest instruction when the native identity
// is already the running one: the supplementary groups belong to the account
// the supervisor was started under, and an unprivileged process can neither
// shed them nor re-enter them. A native group the supervisor is not already in
// is still refused, because emitting nothing would silently run the agent
// somewhere else.
func sharedProcessCredential(isolation *ProcessIsolation) (bool, error) {
	if !sharedProcessIdentity(isolation) {
		return false, nil
	}

	effectiveGID := processEffectiveGID()
	if effectiveGID < 0 || uint64(isolation.GID) != uint64(effectiveGID) {
		return false, fmt.Errorf(
			"native group %d cannot be entered from group %d; %s",
			isolation.GID, effectiveGID, sharedIdentitySupervisorRemedy,
		)
	}

	return true, nil
}
