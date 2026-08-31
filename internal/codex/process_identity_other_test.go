//go:build !unix

package codex

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func TestCurrentProcessIdentityUnavailable(t *testing.T) {
	_, _, err := currentProcessIdentity()
	require.ErrorContains(t, err, "unavailable")
}
