//go:build !windows

package codex

import "strings"

// nativeFileURIPath turns a file URI into the host path the native side opens.
// On a posix host the URI path and the host path are the same string, so only
// the scheme and authority come off.
func nativeFileURIPath(uri string) string {
	return strings.TrimPrefix(uri, fileURIPrefix)
}
