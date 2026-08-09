//go:build linux

package codex

import (
	"os/exec"
	"testing"

	"github.com/stretchr/testify/require"
)

// sharedIdentitySeams restores the identity the shared arm is selected by. The
// arm is unreachable for the trusted root supervisor by construction, so the
// only way to prove both arms from one test process is to state the identity it
// runs as.
func sharedIdentitySeams(t *testing.T) {
	t.Helper()
	effectiveUID := processEffectiveUID
	effectiveGID := processEffectiveGID
	t.Cleanup(func() {
		processEffectiveUID = effectiveUID
		processEffectiveGID = effectiveGID
	})
}

// TestSharedProcessIdentityNamesOnlyTheIdentityTheSupervisorRunsAs pins the one
// predicate the whole arm hangs off. It has to be true for exactly the shape
// that has no privilege boundary to cross and false everywhere else, and a
// trusted root supervisor must never be able to reach it: root is the identity
// the boundary descends from, and the native uid is required to be nonzero, so
// the two can never name the same identity.
func TestSharedProcessIdentityNamesOnlyTheIdentityTheSupervisorRunsAs(t *testing.T) {
	sharedIdentitySeams(t)

	processEffectiveUID = func() int { return 1000 }
	require.False(t, sharedProcessIdentity(nil))
	require.True(t, sharedProcessIdentity(&ProcessIsolation{UID: 1000, GID: 1000}))
	// Decision 2 judges the boundary on the UID alone.
	require.True(t, sharedProcessIdentity(&ProcessIsolation{UID: 1000, GID: 1001}))
	require.False(t, sharedProcessIdentity(&ProcessIsolation{UID: 65534, GID: 65534}))
	require.True(t, sharedSupervisorIdentity(1000))
	require.False(t, sharedSupervisorIdentity(65534))

	processEffectiveUID = func() int { return 0 }
	require.False(t, sharedProcessIdentity(&ProcessIsolation{UID: 1000, GID: 1000}))
	require.False(t, sharedProcessIdentity(&ProcessIsolation{UID: 0, GID: 0}))
	require.False(t, sharedSupervisorIdentity(0))
}

// TestSharedIdentityIsolationCarriesNoStandaloneOwnerFields proves the
// disposition the arm accepts and the one it refuses. The durable standalone
// record proves an identity no live task holds; a supervisor asking to run the
// native process as its own identity is such a task, so the record can never be
// written and fields promising one are a description of a launch that will not
// happen. The isolated arm keeps demanding them word for word.
func TestSharedIdentityIsolationCarriesNoStandaloneOwnerFields(t *testing.T) {
	sharedIdentitySeams(t)
	processEffectiveUID = func() int { return 1000 }

	require.NoError(t, validateStandaloneIdentityDisposition(&ProcessIsolation{UID: 1000, GID: 1000}))

	for name, isolation := range map[string]ProcessIsolation{
		"owner id":   {UID: 1000, GID: 1000, StandaloneOwnerID: "deployment-1"},
		"state root": {UID: 1000, GID: 1000, StandaloneStateRoot: "/var/lib/acp-go-codex"},
	} {
		t.Run(name, func(t *testing.T) {
			err := validateStandaloneIdentityDisposition(&isolation)
			require.ErrorContains(t, err, "standalone owner fields describe an identity the supervisor already holds")
			require.ErrorContains(t, err, sharedIdentitySupervisorRemedy)
		})
	}

	require.EqualError(t,
		validateStandaloneIdentityDisposition(&ProcessIsolation{UID: 65534, GID: 65534}),
		"standalone owner id must be 1..256 canonical ASCII bytes",
	)

	// A capability pair still names the borrowed arm, whatever identity the
	// supervisor runs as.
	capability := testProcessIdentityCapability{}
	require.NoError(t, validateStandaloneIdentityDisposition(
		&ProcessIsolation{UID: 1000, GID: 1000, IdentityLock: capability, AuthorityDomain: capability},
	))
}

// TestSharedIdentityCredentialRequestsNoIdentityChange proves the launch asks
// the kernel for nothing it cannot do. An unprivileged process can neither
// re-enter its own identity nor shed the supplementary groups of the account it
// was started under, so the honest instruction is no credential at all. A
// native group it is not already in is still refused rather than silently
// ignored, and the isolated arm still drops to the target identity with an
// empty group list.
func TestSharedIdentityCredentialRequestsNoIdentityChange(t *testing.T) {
	sharedIdentitySeams(t)
	processEffectiveUID = func() int { return 1000 }
	processEffectiveGID = func() int { return 1000 }

	environment := map[string]string{"PATH": "/usr/bin:/bin"}

	cmd := exec.Command("/bin/true")
	require.NoError(t, applyProcessCredential(cmd, &ProcessIsolation{UID: 1000, GID: 1000, BaseEnvironment: environment}))
	require.Nil(t, cmd.SysProcAttr.Credential)

	for name, effectiveGID := range map[string]int{"another group": 1001, "unrepresentable group": -1} {
		t.Run(name, func(t *testing.T) {
			processEffectiveGID = func() int { return effectiveGID }
			refused := exec.Command("/bin/true")
			err := applyProcessCredential(refused, &ProcessIsolation{UID: 1000, GID: 1000, BaseEnvironment: environment})
			require.ErrorContains(t, err, "native group 1000 cannot be entered from group")
			require.ErrorContains(t, err, sharedIdentitySupervisorRemedy)
			require.Nil(t, refused.SysProcAttr.Credential)
		})
	}

	processEffectiveGID = func() int { return 1000 }
	isolated := exec.Command("/bin/true")
	require.NoError(t, applyProcessCredential(isolated, &ProcessIsolation{
		UID: 65534, GID: 65534, BaseEnvironment: environment,
		StandaloneOwnerID: "test-owner", StandaloneStateRoot: "/var/lib/acp-go-codex-test",
	}))
	require.Equal(t, uint32(65534), isolated.SysProcAttr.Credential.Uid)
	require.Equal(t, uint32(65534), isolated.SysProcAttr.Credential.Gid)
	require.Empty(t, isolated.SysProcAttr.Credential.Groups)
	require.False(t, isolated.SysProcAttr.Credential.NoSetGroups)
}
