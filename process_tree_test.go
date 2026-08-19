package codexacp

import (
	"errors"
	"fmt"
	"runtime"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/savid/acp-go-codex/internal/homelock"
)

// The home-lock refusal is a construction error a host must classify, so it is
// reachable by name from the package the host imports. Re-exporting must
// preserve identity: the value the lock path wraps is the value `errors.Is`
// answers for through the public name, or a host that only ever sees the public
// surface cannot classify what construction returned.
func TestRuntimeLockUnsupportedClassifiesThroughThePublicName(t *testing.T) {
	require.ErrorIs(t, ErrRuntimeLockUnsupported, homelock.ErrRuntimeLockUnsupported)
	require.ErrorIs(t, homelock.ErrRuntimeLockUnsupported, ErrRuntimeLockUnsupported)

	// The lock path names the platform alongside the sentinel, and construction
	// hands that error up unchanged, so the wrapped form classifies too.
	wrapped := fmt.Errorf("%w: %s", homelock.ErrRuntimeLockUnsupported, runtime.GOOS)
	require.ErrorIs(t, wrapped, ErrRuntimeLockUnsupported)

	// A neighbouring construction sentinel is not this one: classification tells
	// an unusable platform apart from an incomplete containment boundary.
	require.NotErrorIs(t, ErrRuntimeLockUnsupported, ErrProcessContainmentIncomplete)
	require.NotErrorIs(t, errors.New("codex runtime home lock is unsupported on this platform"), ErrRuntimeLockUnsupported)
}
