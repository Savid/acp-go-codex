package codexacp

import (
	"encoding/json"
)

const (
	rawCodexSDKMessageMethod = "_codex/sdkMessage"

	codexMetaKey                 = "codex"
	packageMetaKey               = "github.com/savid/acp-go-codex"
	emitRawSDKMessagesKey        = "emitRawSDKMessages"
	rawSDKMessagesCapabilityKey  = "sdkMessages"
	rawSDKMessagesEnabledByPath  = "_meta.codex.emitRawSDKMessages"
	rawSDKMessagesMethodKey      = "method"
	rawSDKMessagesEnabledByKey   = "enabledBy"
	outputSchemaCapabilityKey    = "outputSchema"
	outputSchemaConfigPath       = "_meta.codex.options.outputSchema"
	outputSchemaResultPath       = "usage_update._meta.codex.structuredOutput"
	structuredOutputMetaKey      = "structuredOutput"
	capabilityScopeKey           = "scope"
	capabilityScopeSession       = "session"
	codexThreadIDMetaKey         = "codexThreadId"
	codexAccountMetaKey          = "account"
	codexSessionImportFormatJSON = "codex-rollout-jsonl"
)

type rawMessageConfig struct {
	All     bool
	Filters []rawMessageFilter
}

type rawMessageFilter struct {
	Type        string
	PayloadType string
	PayloadRole string
}

func rawMessageConfigFromMeta(meta map[string]any) rawMessageConfig {
	codexMeta, _ := meta[codexMetaKey].(map[string]any)
	if codexMeta == nil {
		return rawMessageConfig{}
	}
	if config, ok := rawMessageConfigFromValue(codexMeta[emitRawSDKMessagesKey]); ok {
		return config
	}

	return rawMessageConfig{}
}

func rawMessageConfigFromValue(value any) (rawMessageConfig, bool) {
	switch typed := value.(type) {
	case bool:
		return rawMessageConfig{All: typed}, true
	case []any:
		filters := make([]rawMessageFilter, 0, len(typed))
		for _, item := range typed {
			raw, _ := item.(map[string]any)
			filter, ok := rawMessageFilterFromMap(raw)
			if ok {
				filters = append(filters, filter)
			}
		}
		return rawMessageConfig{Filters: filters}, true
	default:
		return rawMessageConfig{}, false
	}
}

func rawMessageFilterFromMap(raw map[string]any) (rawMessageFilter, bool) {
	if raw == nil {
		return rawMessageFilter{}, false
	}
	filter := rawMessageFilter{
		Type:        stringMeta(raw, "type"),
		PayloadType: stringMeta(raw, "payloadType"),
		PayloadRole: stringMeta(raw, "payloadRole"),
	}
	if filter.Type == "" {
		return rawMessageFilter{}, false
	}

	return filter, true
}

func (c rawMessageConfig) Enabled() bool {
	return c.All || len(c.Filters) > 0
}

func (c rawMessageConfig) ShouldEmit(raw map[string]any) bool {
	if raw == nil {
		return false
	}
	if c.All {
		return true
	}
	for _, filter := range c.Filters {
		if filter.Matches(raw) {
			return true
		}
	}

	return false
}

func (f rawMessageFilter) Matches(raw map[string]any) bool {
	if f.Type == "" || stringMeta(raw, "type") != f.Type {
		return false
	}
	payload, _ := raw["payload"].(map[string]any)
	if f.PayloadType != "" && stringMeta(payload, "type") != f.PayloadType {
		return false
	}
	if f.PayloadRole != "" && stringMeta(payload, "role") != f.PayloadRole {
		return false
	}

	return true
}

func decodedRawEvent(raw json.RawMessage) map[string]any {
	var out map[string]any
	if len(raw) > 0 {
		_ = json.Unmarshal(raw, &out)
	}

	return out
}

func stringMeta(values map[string]any, key string) string {
	value, _ := values[key].(string)
	return value
}
