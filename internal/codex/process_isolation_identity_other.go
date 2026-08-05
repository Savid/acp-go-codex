//go:build !linux

package codex

// validateProcessIsolationIdentity accepts every disposition off Linux: the
// agent identity lock and authority domain are Linux-only capabilities, so no
// platform outside Linux can present one to validate.
func validateProcessIsolationIdentity(*ProcessIsolation) error { return nil }
