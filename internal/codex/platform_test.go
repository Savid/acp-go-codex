package codex

import (
	"os"
	"path/filepath"
	"runtime"
	"testing"

	"github.com/stretchr/testify/require"
)

// hostDirPerm is the permission mode a host filesystem reports for a directory
// this adapter created with mode. Windows carries no POSIX mode bits: os.Stat
// reports every directory as 0o777, so a POSIX literal is not the property a
// Windows host can be asked about.
func hostDirPerm(mode os.FileMode) os.FileMode {
	if runtime.GOOS != platformWindows {
		return mode
	}

	return 0o777
}

// writeFakeCLI writes one of the fake native CLI bodies into dir under a name
// this host can launch, and returns its path.
//
// It also points the executable resolver at the host it is actually running on.
// TestMain pins processGOOS to linux so the posix resolution branch is exercised
// on every platform, and that branch asks for an exec bit no Windows file
// carries; a fixture written to be launched needs the branch its host really
// has.
func writeFakeCLI(t *testing.T, dir string, base string, body string) string {
	t.Helper()

	original := processGOOS
	processGOOS = runtime.GOOS

	t.Cleanup(func() { processGOOS = original })

	path := filepath.Join(dir, fakeCLIName(base))
	require.NoError(t, os.WriteFile(path, []byte(body), 0o700))

	return path
}
