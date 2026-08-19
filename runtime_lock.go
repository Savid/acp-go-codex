package codexacp

import "github.com/savid/acp-go-codex/internal/homelock"

// ErrRuntimeLockUnsupported means the platform carries no writable-home lock
// primitive, so the runtime home's exclusivity cannot be established. Home
// exclusivity is load-bearing rather than advisory — it is what keeps two
// runtimes off one Codex home — so construction fails closed with this sentinel
// instead of launching unprotected. A host classifies it the same way on every
// such platform: the configuration is unusable here, and no retry against the
// same home will change that.
var ErrRuntimeLockUnsupported = homelock.ErrRuntimeLockUnsupported
