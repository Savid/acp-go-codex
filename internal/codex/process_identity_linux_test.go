//go:build linux

package codex

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func processIdentitySeams(t *testing.T) {
	t.Helper()
	effectiveUID := processEffectiveUID
	effectiveGID := processEffectiveGID
	t.Cleanup(func() {
		processEffectiveUID = effectiveUID
		processEffectiveGID = effectiveGID
	})
}

func TestCurrentProcessIdentityLinux(t *testing.T) {
	processIdentitySeams(t)
	processEffectiveUID = func() int { return 1000 }
	processEffectiveGID = func() int { return 1001 }

	uid, gid, err := currentProcessIdentity()
	require.NoError(t, err)
	require.Equal(t, uint32(1000), uid)
	require.Equal(t, uint32(1001), gid)

	processEffectiveUID = func() int { return -1 }
	_, _, err = currentProcessIdentity()
	require.ErrorContains(t, err, "unavailable")
}
