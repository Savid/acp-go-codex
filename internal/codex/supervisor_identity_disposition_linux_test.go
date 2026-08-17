//go:build linux

package codex

import (
	"context"
	"errors"
	"os"
	"testing"

	"github.com/stretchr/testify/require"
)

// TestSupervisorRefusesAHalfProvidedCapabilityPairPlatformValidationDoesNot
// proves the supervisor makes its own check that the UID lock and the authority
// domain arrive together, independently of the isolation policy.
//
// On Linux validateProcessIsolation already refuses a half-provided pair through
// validateStandaloneIdentityDisposition, which is the identity validator only
// the Linux build carries; every other platform accepts whatever disposition it
// is handed and nothing upstream of supervisorCommand looks at the pair at all.
// The supervisor's own check is therefore the only thing standing between a
// caller and a supervisor started with a lock but no domain — one that would go
// on to record IdentityLock without AuthorityDomain and be refused much later,
// inside the child, by runSupervisor's consistency check.
//
// The case installs the identity validator every non-Linux platform has, hands
// the supervisor each half in turn, and requires the supervisor's own refusal by
// its exact text, so a refusal that came from the isolation policy instead could
// not be mistaken for it. Nothing may be built before that refusal: no config
// descriptor is written, and no command comes back.
func TestSupervisorRefusesAHalfProvidedCapabilityPairPlatformValidationDoesNot(t *testing.T) {
	for _, testCase := range []struct {
		name  string
		apply func(*ProcessIsolation)
	}{
		{
			name: "lock without domain",
			apply: func(isolation *ProcessIsolation) {
				isolation.IdentityLock = supervisorIdentityCapability{}
			},
		},
		{
			name: "domain without lock",
			apply: func(isolation *ProcessIsolation) {
				isolation.AuthorityDomain = supervisorIdentityCapability{}
			},
		},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			preserveSupervisorGlobals(t)
			validateIdentity := processIsolationValidateIdentity
			writeConfig := supervisorWriteConfig
			processIsolationValidateIdentity = func(*ProcessIsolation) error { return nil }
			t.Cleanup(func() {
				processIsolationValidateIdentity = validateIdentity
				supervisorWriteConfig = writeConfig
			})

			isolation := testProcessIsolation()
			isolation.StandaloneOwnerID = ""
			isolation.StandaloneStateRoot = ""
			testCase.apply(isolation)

			config := supervisorConfig{Scratch: t.TempDir(), Isolation: isolation}
			require.NoError(t, validateProcessIsolation(config.Isolation),
				"the fixture must be one this platform's isolation policy accepts")

			written := 0
			supervisorWriteConfig = func(string, supervisorConfig) (*os.File, error) {
				written++

				return nil, errors.New("the supervisor must not reach the config write")
			}

			cmd, proof, err := supervisorCommand(context.Background(), config)
			require.EqualError(t, err, "codex supervisor requires the UID lock and authority domain together")
			require.Nil(t, cmd)
			require.Nil(t, proof)
			require.Zero(t, written, "a half-provided capability pair must be refused before anything is built")
		})
	}
}
