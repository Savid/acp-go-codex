//go:build windows

package codex

import (
	"path/filepath"
	"strings"
)

// nativeFileURIPath turns a file URI into the host path the native side opens.
// A Windows path is spelled in a file URI with the URI's own root in front of
// the volume — file:///C:/dir/file carries the path /C:/dir/file — and Windows
// has no absolute path that is rooted without a volume. Handing that spelling
// to the native side names nothing, so the URI's root comes off with the scheme
// and the separators become the ones this host writes.
func nativeFileURIPath(uri string) string {
	path := strings.TrimPrefix(uri, fileURIPrefix)
	if len(path) >= 3 && path[0] == '/' && path[2] == ':' {
		path = path[1:]
	}

	return filepath.FromSlash(path)
}
