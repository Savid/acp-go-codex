//go:build !windows

package codex

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
)

var (
	browserShimMkdirTemp = os.MkdirTemp
	browserShimWriteFile = os.WriteFile
)

// newBrowserShim materialises the shim beneath parent. os.MkdirTemp creates the
// directory 0700, and every launcher name becomes an executable no-op, so a
// harness that execs one directly reaches this directory before any real
// browser launcher on PATH.
func newBrowserShim(parent string) (*browserShim, error) {
	dir, err := browserShimMkdirTemp(parent, browserShimPrefix)
	if err != nil {
		return nil, fmt.Errorf("create browser shim directory: %w", err)
	}

	for _, name := range browserLauncherNames {
		// #nosec G302 G306 -- a shadowed launcher must be executable by the
		// login child, inside a 0700 scratch directory only this process tree
		// can reach.
		if writeErr := browserShimWriteFile(filepath.Join(dir, name), browserShimScript, 0o700); writeErr != nil {
			return nil, errors.Join(
				fmt.Errorf("write browser shim %s: %w", name, writeErr),
				os.RemoveAll(dir),
			)
		}
	}

	return &browserShim{dir: dir}, nil
}
