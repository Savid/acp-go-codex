//go:build !windows

package codex

import (
	"testing"

	"github.com/stretchr/testify/require"
)

// TestNativeFileURIPathKeepsThePosixSpelling pins the identity this conversion
// is on a posix host: the URI path is already the host path, so a file URI loses
// its scheme and empty authority and nothing else.
func TestNativeFileURIPathKeepsThePosixSpelling(t *testing.T) {
	require.Equal(t, "/repo/main.go", nativeFileURIPath("file:///repo/main.go"))
	require.Equal(t, "/C:/repo/main.go", nativeFileURIPath("file:///C:/repo/main.go"))
}
