//go:build windows

package codex

import (
	"testing"

	"github.com/stretchr/testify/require"
)

// TestNativeFileURIPathDropsTheURIRootBeforeTheVolume pins what a host's file
// URI has to become before the native side can open it. The volume sits behind
// the URI's own root, and no Windows path is absolute without a volume, so a
// verbatim conversion hands the native side a name that reaches nothing.
func TestNativeFileURIPathDropsTheURIRootBeforeTheVolume(t *testing.T) {
	require.Equal(t, `C:\repo\main.go`, nativeFileURIPath("file:///C:/repo/main.go"))
	require.Equal(t, `\repo\main.go`, nativeFileURIPath("file:///repo/main.go"))
}
