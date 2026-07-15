package codexacp

import "github.com/savid/acp-go-codex/internal/codex"

// ErrProcessTreeUnproven means shutdown could not prove that every native
// Codex descendant exited. Callers must keep the runtime quarantined.
var ErrProcessTreeUnproven = codex.ErrProcessTreeUnproven
