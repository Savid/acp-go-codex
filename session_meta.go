package codexacp

import (
	"encoding/json"
	"fmt"

	"github.com/coder/acp-go-sdk"
	"github.com/savid/acp-go-codex/internal/codex"
)

type sessionMeta struct {
	Model               string
	ReasoningEffort     string
	ServiceTier         string
	Personality         string
	ApprovalPolicy      any
	SandboxPolicy       any
	OutputSchema        any
	RawMessages         rawMessageConfig
	MCPToolApprovalMode string
}

func sessionMetaFromLifecycle(meta map[string]any) (sessionMeta, error) {
	if err := validateLifecycleMeta(meta); err != nil {
		return sessionMeta{}, err
	}

	codexOptions, err := codexOptionsFromMeta(meta)
	if err != nil {
		return sessionMeta{}, err
	}

	return sessionMeta{
		Model:               codexOptions.Model,
		ReasoningEffort:     codexOptions.ReasoningEffort,
		ServiceTier:         codexOptions.ServiceTier,
		Personality:         codexOptions.Personality,
		ApprovalPolicy:      codexOptions.ApprovalPolicy,
		SandboxPolicy:       codexOptions.SandboxPolicy,
		OutputSchema:        codexOptions.OutputSchema,
		RawMessages:         rawMessageConfigFromMeta(meta),
		MCPToolApprovalMode: codexOptions.MCPToolApprovalMode,
	}, nil
}

type codexOptions struct {
	Model               string
	ReasoningEffort     string
	ServiceTier         string
	Personality         string
	ApprovalPolicy      any
	SandboxPolicy       any
	OutputSchema        any
	MCPToolApprovalMode string
}

func codexOptionsFromMeta(meta map[string]any) (codexOptions, error) {
	codexMeta, _ := meta[codexMetaKey].(map[string]any)

	optionsMap, _ := codexMeta["options"].(map[string]any)
	if optionsMap == nil {
		return codexOptions{}, nil
	}

	options := codexOptions{}

	model, err := metaOptionString(optionsMap, metaModelKey)
	if err != nil {
		return codexOptions{}, err
	}

	options.Model = model

	effort, err := metaOptionString(optionsMap, metaEffortKey)
	if err != nil {
		return codexOptions{}, err
	}

	if effort != "" {
		if !validReasoningEffort(effort) {
			return codexOptions{}, fmt.Errorf("_meta.codex.options.effort is unsupported")
		}

		options.ReasoningEffort = effort
	}

	tier, err := metaOptionString(optionsMap, metaServiceTierKey)
	if err != nil {
		return codexOptions{}, err
	}

	options.ServiceTier = tier

	personality, err := metaOptionString(optionsMap, metaPersonalityKey)
	if err != nil {
		return codexOptions{}, err
	}

	if personality != "" {
		if !validPersonality(personality) {
			return codexOptions{}, fmt.Errorf("_meta.codex.options.personality is unsupported")
		}

		options.Personality = personality
	}

	if policy, ok := optionsMap[metaApprovalPolicyKey]; ok {
		options.ApprovalPolicy = cloneAny(policy)
	}

	if policy, ok := optionsMap[metaSandboxPolicyKey]; ok {
		options.SandboxPolicy = cloneAny(policy)
	}

	if schema, ok := optionsMap[metaOutputSchemaKey]; ok {
		if schemaErr := validateSchemaObject(schema); schemaErr != nil {
			return codexOptions{}, schemaErr
		}

		options.OutputSchema = cloneAny(schema)
	}

	mode, err := metaOptionString(optionsMap, metaMCPToolApprovalModeKey)
	if err != nil {
		return codexOptions{}, err
	}

	if mode != "" {
		if !codex.ValidMCPApprovalMode(mode) {
			return codexOptions{}, fmt.Errorf("_meta.codex.options.mcpToolApprovalMode is unsupported")
		}

		options.MCPToolApprovalMode = mode
	}

	return options, nil
}

// metaOptionString reads a known string-typed _meta.codex.options value.
// Wrong-typed values are rejected with the uniform invalid-params data shape
// instead of being silently ignored.
func metaOptionString(optionsMap map[string]any, key string) (string, error) {
	raw, ok := optionsMap[key]
	if !ok {
		return "", nil
	}

	value, ok := raw.(string)
	if !ok {
		return "", unsupportedField("_meta.codex.options." + key)
	}

	return value, nil
}

func validateLifecycleMeta(meta map[string]any) error {
	if len(meta) == 0 {
		return nil
	}

	codexMeta, ok := meta[codexMetaKey].(map[string]any)
	if !ok {
		if _, exists := meta[codexMetaKey]; exists {
			return fmt.Errorf("_meta.codex must be an object")
		}

		return nil
	}

	for key, value := range codexMeta {
		switch key {
		case metaOptionsKey:
			optionsMap, ok := value.(map[string]any)
			if !ok {
				return fmt.Errorf("_meta.codex.options must be an object")
			}

			for optionKey := range optionsMap {
				switch optionKey {
				case metaModelKey, metaOutputSchemaKey, metaEffortKey, metaServiceTierKey, metaPersonalityKey, metaApprovalPolicyKey, metaSandboxPolicyKey, metaMCPToolApprovalModeKey:
				default:
					return unsupportedField("_meta.codex.options." + optionKey)
				}
			}
		case rawEventKey:
			rawEvent, ok := value.(map[string]any)
			if !ok {
				return fmt.Errorf("_meta.codex.rawEvent must be an object")
			}

			for rawKey, rawValue := range rawEvent {
				switch rawKey {
				case rawEventEnabledKey:
					if _, ok := rawValue.(bool); !ok {
						return fmt.Errorf("_meta.codex.rawEvent.enabled must be a boolean")
					}
				default:
					return unsupportedField("_meta.codex.rawEvent." + rawKey)
				}
			}
		default:
			return unsupportedField("_meta.codex." + key)
		}
	}

	return nil
}

func unsupportedField(path string) error {
	return acp.NewInvalidParams(map[string]any{
		jsonFieldError: errValueUnsupported,
		jsonFieldField: path,
	})
}

func validateSchemaObject(schema any) error {
	obj, ok := schema.(map[string]any)
	if !ok || len(obj) == 0 {
		return fmt.Errorf("output schema must be a non-empty JSON object")
	}

	if _, err := json.Marshal(obj); err != nil {
		return fmt.Errorf("output schema must be JSON serializable: %w", err)
	}

	return nil
}

func validReasoningEffort(value string) bool {
	switch value {
	case effortValueNone, "minimal", effortValueLow, effortValueMedium, effortValueHigh, "xhigh":
		return true
	default:
		return false
	}
}

func validPersonality(value string) bool {
	switch value {
	case effortValueNone, personalityFriendly, personalityPragmatic:
		return true
	default:
		return false
	}
}

func sessionResponseMeta(snapshot sessionSnapshot) map[string]any {
	codexMeta := map[string]any{
		codexThreadIDMetaKey: snapshot.codexThreadID,
	}
	if snapshot.modelProvider != "" {
		codexMeta["modelProvider"] = snapshot.modelProvider
	}

	if snapshot.model != "" {
		codexMeta[metaModelKey] = snapshot.model
	}

	if snapshot.reasoningEffort != "" {
		codexMeta[metaEffortKey] = snapshot.reasoningEffort
	}

	if snapshot.serviceTier != "" {
		codexMeta[metaServiceTierKey] = snapshot.serviceTier
	}

	if snapshot.personality != "" {
		codexMeta[metaPersonalityKey] = snapshot.personality
	}

	if len(snapshot.accountMeta) > 0 {
		codexMeta[codexAccountMetaKey] = cloneAnyMap(snapshot.accountMeta)
	}

	if snapshot.model != "" {
		codexMeta["modelId"] = snapshot.model
	}

	return map[string]any{
		codexMetaKey: codexMeta,
	}
}

func sessionInfoMeta(snapshot sessionSnapshot) map[string]any {
	raw, _ := sessionResponseMeta(snapshot)[codexMetaKey].(map[string]any)
	codexMeta := cloneAnyMap(raw)

	return map[string]any{
		codexMetaKey: codexMeta,
	}
}

func cloneAnyMap(values map[string]any) map[string]any {
	if values == nil {
		return nil
	}

	cloned := make(map[string]any, len(values))
	for key, value := range values {
		cloned[key] = cloneAny(value)
	}

	return cloned
}

func cloneAnySlice(values []any) []any {
	if values == nil {
		return nil
	}

	cloned := make([]any, len(values))
	for i, value := range values {
		cloned[i] = cloneAny(value)
	}

	return cloned
}

func cloneAny(value any) any {
	switch typed := value.(type) {
	case map[string]any:
		return cloneAnyMap(typed)
	case map[string]string:
		return cloneStringMap(typed)
	case []any:
		return cloneAnySlice(typed)
	default:
		return value
	}
}

func cloneStringMap(values map[string]string) map[string]string {
	if values == nil {
		return nil
	}

	cloned := make(map[string]string, len(values))
	for key, value := range values {
		cloned[key] = value
	}

	return cloned
}
