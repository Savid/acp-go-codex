//go:build linux

package codex

// validateProcessIsolationIdentity enforces the standalone/borrowed identity
// disposition. Only Linux owns the agent identity lock and authority domain, so
// only Linux has a disposition to validate.
func validateProcessIsolationIdentity(isolation *ProcessIsolation) error {
	return validateStandaloneIdentityDisposition(isolation)
}
