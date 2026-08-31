package codex

import (
	"io"
	"os"
	"testing"
)

// withNeutralSupervisorIdentityHooks installs the platform-neutral identity
// hooks for cases that exercise supervisor dispatch rather than the authority
// itself. Linux replaces these at init with the real agent authority, which
// claims one identity exclusively and hands the claim down an inherited
// descriptor: a single test process cannot establish and release that claim
// repeatedly, so an in-process dispatch case dies on the authority long before
// the branch it names. The authority itself is proven end to end by the
// supervised-native cases in supervisor_linux_test.go.
func withNeutralSupervisorIdentityHooks(t *testing.T) {
	t.Helper()

	acquire := supervisorAcquireIdentityAuthority
	verify := supervisorVerifyTrustedIdentity
	adoptLock := supervisorAdoptIdentityLock
	adoptDomain := supervisorAdoptAuthorityDomain
	validateAdopted := supervisorValidateAdoptedAuthority
	validatePeer := supervisorValidateGuardianPeer

	t.Cleanup(func() {
		supervisorAcquireIdentityAuthority = acquire
		supervisorVerifyTrustedIdentity = verify
		supervisorAdoptIdentityLock = adoptLock
		supervisorAdoptAuthorityDomain = adoptDomain
		supervisorValidateAdoptedAuthority = validateAdopted
		supervisorValidateGuardianPeer = validatePeer
	})

	supervisorAcquireIdentityAuthority = func(
		uint32, uint32, string, string, io.Reader,
	) (supervisorIdentityLock, supervisorIdentityLock, error) {
		return noopSupervisorIdentityLock{}, noopSupervisorIdentityLock{}, nil
	}
	supervisorVerifyTrustedIdentity = func(uint32) error { return nil }
	supervisorAdoptIdentityLock = func(uint32) (supervisorIdentityLock, error) {
		return noopSupervisorIdentityLock{}, nil
	}
	supervisorAdoptAuthorityDomain = func(uint32) (supervisorIdentityLock, error) {
		return noopSupervisorIdentityLock{}, nil
	}
	supervisorValidateAdoptedAuthority = func(supervisorConfig) error { return nil }
	supervisorValidateGuardianPeer = func(*os.File, <-chan struct{}) error { return nil }
}
