package codexacp

import (
	"github.com/coder/acp-go-sdk"

	"github.com/savid/acp-go-core/wire"
)

// WithSessionCodexOptions merges codex-specific options into _meta.codex.options.
func WithSessionCodexOptions(options CodexOptions) wire.SessionRequestOption {
	return wire.WithSessionMetaValue(options.clone().Meta())
}

// WithSessionOutputSchema sets the JSON schema the turn's final answer must
// satisfy; it rides outputSchema on turn/start.
func WithSessionOutputSchema(schema map[string]any) wire.SessionRequestOption {
	return wire.WithSessionMetaValue(CodexOptions{OutputSchema: wire.CloneMap(schema)}.Meta())
}

// WithSessionRawEvents toggles raw codex event emission for the session.
func WithSessionRawEvents(enabled bool) wire.SessionRequestOption {
	return wire.WithSessionMetaValue(map[string]any{
		vendor: map[string]any{metaRawEventKey: map[string]any{metaEnabledKey: enabled}},
	})
}

// SetModelRequest constructs a model selector update.
func SetModelRequest(sessionID acp.SessionId, model string) acp.SetSessionConfigOptionRequest {
	return wire.SetConfigOptionRequest(sessionID, configModel, acp.SessionConfigValueId(model))
}
