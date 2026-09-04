//go:build windows

package codexacp

import (
	"testing"

	"github.com/stretchr/testify/require"
)

// TestHandoffURIFilePathDropsTheURIRootBeforeTheVolume pins what a host's file
// URI has to become before the handoff read can open it. The volume sits behind
// the URI's own root, and no Windows path is absolute without a volume, so a
// verbatim conversion produces a path filepath.IsAbs refuses and the whole
// handoff form would be unreachable on this platform.
func TestHandoffURIFilePathDropsTheURIRootBeforeTheVolume(t *testing.T) {
	require.Equal(t, `C:\images\valid.png`, handoffURIFilePath("/C:/images/valid.png"))
	require.Equal(t, `\images\valid.png`, handoffURIFilePath("/images/valid.png"))
}
