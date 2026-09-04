package codexacp

import (
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"testing"
	"time"
)

// runtimeGenerationSnapshot reads the shared app-server generation a test wants
// to prove survived, or did not. The epoch is what distinguishes a generation
// that kept serving from a replacement started after one was fenced.
type runtimeGeneration struct {
	epoch uint64
	dead  bool
}

func (a *Agent) runtimeGenerationSnapshot() runtimeGeneration {
	a.mu.Lock()
	defer a.mu.Unlock()

	return runtimeGeneration{epoch: a.runtimeEpoch, dead: a.runtimeDead}
}

// testSignalTimeout bounds every rendezvous a test waits on. It is generous
// enough that a slow or loaded machine never trips it, and short enough that a
// signal which is never going to arrive is reported as a failure long before the
// package timeout would take the rest of the suite down with it.
const testSignalTimeout = 30 * time.Second

// awaitTestSignal takes one value from a rendezvous a test is waiting on. A
// signal that never arrives fails the test at the point it waited, naming what
// it waited for, instead of parking the package until its timeout and hiding
// every test that had not run yet behind the one that hung.
func awaitTestSignal[T any](t *testing.T, signal <-chan T, what string) T {
	t.Helper()

	select {
	case value := <-signal:
		return value
	case <-time.After(testSignalTimeout):
		t.Fatalf("timed out waiting for %s", what)

		var zero T

		return zero
	}
}

// hostWindows names the GOOS whose spelling of a path, a permission bit, or a
// file URI differs from the POSIX one these helpers translate from.
const hostWindows = "windows"

// absTestPath builds a host-absolute path from POSIX-looking segments, so a
// test states "an absolute working directory" rather than a spelling only
// one platform accepts.
func absTestPath(segments ...string) string {
	root := "/"
	if runtime.GOOS == hostWindows {
		root = `C:\`
	}

	return filepath.Join(append([]string{root}, segments...)...)
}

// absTestPathJSON is absTestPath quoted for embedding in a JSON literal, so a
// stored record and the request that has to match it carry one spelling.
func absTestPathJSON(segments ...string) string {
	return strconv.Quote(absTestPath(segments...))
}

// handoffTestURI spells a host path as the file URI a truthful host would send.
// A Windows path carries its volume after the URI's own root, so the slashed
// form gains the leading slash a POSIX path already has.
func handoffTestURI(path string) string {
	return "file://" + handoffTestURIPath(path)
}

// handoffTestURIPath is the path component a file URI carries for a host path.
// A Windows path carries its volume after the URI's own root, so the slashed
// form gains the leading slash a POSIX path already has.
func handoffTestURIPath(path string) string {
	slashed := filepath.ToSlash(path)
	if filepath.IsAbs(path) && !strings.HasPrefix(slashed, "/") {
		slashed = "/" + slashed
	}

	return slashed
}

// hostFilePerm is the permission mode a host filesystem reports for a file this
// adapter created with mode. Windows carries no POSIX mode bits: os.Stat
// synthesises 0o666 for a writable file and 0o444 for one marked read-only, so
// a POSIX literal is not the property a Windows host can be asked about.
func hostFilePerm(mode os.FileMode) os.FileMode {
	if runtime.GOOS != hostWindows {
		return mode
	}

	if mode&0o200 == 0 {
		return 0o444
	}

	return 0o666
}

// hostDirPerm is the permission mode a host filesystem reports for a directory
// this adapter created with mode. Windows reports every directory as 0o777, for
// the same reason hostFilePerm exists.
func hostDirPerm(mode os.FileMode) os.FileMode {
	if runtime.GOOS != hostWindows {
		return mode
	}

	return 0o777
}
