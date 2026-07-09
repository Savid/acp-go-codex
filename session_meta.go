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
	Env                 map[string]string
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
		Env:                 codexOptions.Env,
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
	Env                 map[string]string
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
	if model, _ := optionsMap["model"].(string); model != "" {
		options.Model = model
	}

	if effort, _ := optionsMap["effort"].(string); effort != "" {
		if !validReasoningEffort(effort) {
			return codexOptions{}, fmt.Errorf("_meta.codex.options.effort is unsupported")
		}

		options.ReasoningEffort = effort
	}

	if tier, _ := optionsMap["serviceTier"].(string); tier != "" {
		options.ServiceTier = tier
	}

	if personality, _ := optionsMap["personality"].(string); personality != "" {
		if !validPersonality(personality) {
			return codexOptions{}, fmt.Errorf("_meta.codex.options.personality is unsupported")
		}

		options.Personality = personality
	}

	if rawEnv, ok := optionsMap[metaEnvKey]; ok {
		env, err := stringMapFromMeta(rawEnv)
		if err != nil {
			return codexOptions{}, err
		}

		options.Env = env
	}

	if policy, ok := optionsMap[metaApprovalPolicyKey]; ok {
		options.ApprovalPolicy = cloneAny(policy)
	}

	if policy, ok := optionsMap[metaSandboxPolicyKey]; ok {
		options.SandboxPolicy = cloneAny(policy)
	}

	if schema, ok := optionsMap[metaOutputSchemaKey]; ok {
		if err := validateSchemaObject(schema); err != nil {
			return codexOptions{}, err
		}

		options.OutputSchema = cloneAny(schema)
	}

	if mode, _ := optionsMap["mcpToolApprovalMode"].(string); mode != "" {
		if !codex.ValidMCPApprovalMode(mode) {
			return codexOptions{}, fmt.Errorf("_meta.codex.options.mcpToolApprovalMode is unsupported")
		}

		options.MCPToolApprovalMode = mode
	}

	return options, nil
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
				case "model", metaEnvKey, metaOutputSchemaKey, "effort", "serviceTier", "personality", "approvalPolicy", "sandboxPolicy", "mcpToolApprovalMode":
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

func stringMapFromMeta(value any) (map[string]string, error) {
	switch typed := value.(type) {
	case map[string]string:
		return cloneStringMap(typed), nil
	case map[string]any:
		out := make(map[string]string, len(typed))
		for key, raw := range typed {
			str, ok := raw.(string)
			if !ok {
				return nil, fmt.Errorf("_meta.codex.options.env.%s must be a string", key)
			}

			out[key] = str
		}

		return out, nil
	default:
		return nil, fmt.Errorf("_meta.codex.options.env must be an object")
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
		codexMeta["model"] = snapshot.model
	}

	if snapshot.reasoningEffort != "" {
		codexMeta["effort"] = snapshot.reasoningEffort
	}

	if snapshot.serviceTier != "" {
		codexMeta["serviceTier"] = snapshot.serviceTier
	}

	if snapshot.personality != "" {
		codexMeta["personality"] = snapshot.personality
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
