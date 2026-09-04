//go:build windows

package codexacp

import "path/filepath"

// handoffOpenFlags carries no platform open flags on Windows, and none are
// owed: the non-blocking open is a Unix flag against a Unix hazard, and Windows
// has no equivalent to ask for. Nothing about containment rests on that, or on
// any argument that the handoff form is unreachable here. The read is confined
// by opening through the root handle and by requiring the descriptor it returns
// to be a regular file, and both of those bind on every platform.
const handoffOpenFlags = 0

// handoffURIFilePath turns the path component of a file URI into a host path.
// A Windows path is spelled in a file URI with the URI's own root in front of
// the volume — file:///C:/dir/file gives /C:/dir/file — and Windows has no
// absolute path that is rooted without a volume, so that leading slash is
// dropped rather than translated into a separator no host would accept.
func handoffURIFilePath(path string) string {
	if len(path) >= 3 && path[0] == '/' && path[2] == ':' {
		path = path[1:]
	}

	return filepath.FromSlash(path)
}
