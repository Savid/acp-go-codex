package homelock

import (
	"os"
	"runtime"
)

// hostFilePerm is the permission mode a host filesystem reports for a file this
// adapter created with mode. Windows carries no POSIX mode bits: os.Stat
// synthesises 0o666 for a writable file and 0o444 for one marked read-only, so
// a POSIX literal is not the property a Windows host can be asked about.
func hostFilePerm(mode os.FileMode) os.FileMode {
	if runtime.GOOS != "windows" {
		return mode
	}

	if mode&0o200 == 0 {
		return 0o444
	}

	return 0o666
}
