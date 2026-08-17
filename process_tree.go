package codexacp

import "github.com/savid/acp-go-codex/internal/codex"

// ErrProcessContainmentIncomplete means the selected native containment
// boundary did not complete. Callers must keep the runtime quarantined.
var ErrProcessContainmentIncomplete = codex.ErrProcessContainmentIncomplete
