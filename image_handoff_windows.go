//go:build windows

package codexacp

// handoffOpenFlags carries no platform open flags on Windows, and none are
// owed: the non-blocking open is a Unix flag against a Unix hazard, and Windows
// has no equivalent to ask for. Nothing about containment rests on that, or on
// any argument that the handoff form is unreachable here. The read is confined
// by opening through the root handle and by requiring the descriptor it returns
// to be a regular file, and both of those bind on every platform.
const handoffOpenFlags = 0
