package codexacp

import (
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
)

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
