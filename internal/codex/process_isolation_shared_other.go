//go:build !linux

package codex

// The shared arm belongs to the Linux supervisor. Only Linux descends from a
// trusted root identity to the native one, so only Linux has a descent to skip
// when the two identities are the same; every other platform states its own
// boundary and is left as it is.

func sharedSupervisorIdentity(uint32) bool { return false }

func sharedProcessIdentity(*ProcessIsolation) bool { return false }

func sharedProcessCredential(*ProcessIsolation) (bool, error) { return false, nil }

// validateSupervisorIdentityDisposition has nothing to cross-check where no arm
// can be selected: the parent stamps the disposition it decided, and off Linux
// that decision is always the isolated one.
func validateSupervisorIdentityDisposition(supervisorConfig) error { return nil }
