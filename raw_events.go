package codexacp

import (
	"encoding/json"
)

const (
	ForkSessionMethod = "_codex/session/fork"
	RawEventMethod    = "_codex/rawEvent"

	codexMetaKey                  = "codex"
	rawEventKey                   = "rawEvent"
	rawEventEnabledKey            = "enabled"
	rawEventCapabilityKey         = "rawEvent"
	rawEventEnabledByPath         = "_meta.codex.rawEvent.enabled"
	rawEventMaxBytes              = 64 * 1024
	structuredOutputCapabilityKey = "structuredOutput"

	rawMarkerTruncated            = "truncated"
	rawMarkerReason               = "reason"
	rawMarkerMaxBytes             = "maxBytes"
	rawMarkerSizeBytes            = "sizeBytes"
	rawMarkerReasonOversize       = "oversize"
	rawMarkerReasonUnserializable = "unserializable"
	outputSchemaConfigPath        = "_meta.codex.options.outputSchema"
	outputSchemaResultPath        = "session/prompt.result._meta.codex.structuredOutput"
	structuredOutputMetaKey       = "structuredOutput"
	codexThreadIDMetaKey          = "codexThreadId"
	codexAccountMetaKey           = "account"
)

type rawMessageConfig struct {
	enabled bool
}

func rawMessageConfigFromMeta(meta map[string]any) rawMessageConfig {
	codexMeta, _ := meta[codexMetaKey].(map[string]any)
	if codexMeta == nil {
		return rawMessageConfig{}
	}

	rawEvent, _ := codexMeta[rawEventKey].(map[string]any)

	enabled, _ := rawEvent[rawEventEnabledKey].(bool)
	if enabled {
		return rawMessageConfig{enabled: true}
	}

	return rawMessageConfig{}
}

func (c rawMessageConfig) Enabled() bool {
	return c.enabled
}

func (c rawMessageConfig) ShouldEmit(raw map[string]any) bool {
	return c.enabled && raw != nil
}

func decodedRawEvent(raw json.RawMessage) map[string]any {
	var out map[string]any
	if len(raw) > 0 {
		_ = json.Unmarshal(raw, &out)
	}

	return out
}

func (s *session) nextRawEventSequence() int64 {
	s.mu.Lock()
	defer s.mu.Unlock()

	s.rawEventSequence++

	return s.rawEventSequence
}

func capRawEventPayload(payload map[string]any) map[string]any {
	encoded, err := json.Marshal(payload)
	if err == nil && len(encoded) <= rawEventMaxBytes {
		return payload
	}

	marker := map[string]any{
		rawMarkerTruncated: true,
		rawMarkerReason:    rawMarkerReasonOversize,
		rawMarkerMaxBytes:  rawEventMaxBytes,
		rawMarkerSizeBytes: len(encoded),
	}
	if err != nil {
		marker = map[string]any{
			rawMarkerTruncated: true,
			rawMarkerReason:    rawMarkerReasonUnserializable,
			rawMarkerMaxBytes:  rawEventMaxBytes,
		}
	}

	return map[string]any{
		jsonFieldSessionID: payload[jsonFieldSessionID],
		jsonFieldSequence:  payload[jsonFieldSequence],
		jsonFieldSource:    payload[jsonFieldSource],
		jsonFieldEvent:     marker,
	}
}

func stringMeta(values map[string]any, key string) string {
	value, _ := values[key].(string)

	return value
}
