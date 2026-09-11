package codexacp

import (
	"errors"
	"fmt"
	"slices"

	"github.com/coder/acp-go-sdk"

	"github.com/savid/acp-go-core/lifecycle"
	"github.com/savid/acp-go-core/process"
	"github.com/savid/acp-go-core/wire"
)

const (
	metaOptionsKey        = "options"
	metaRawEventKey       = "rawEvent"
	metaModelKey          = "model"
	metaEnvKey            = "env"
	metaExtraPathDirsKey  = "extraPathDirs"
	metaOutputSchemaKey   = "outputSchema"
	metaEffortKey         = "effort"
	metaServiceTierKey    = "serviceTier"
	metaPersonalityKey    = "personality"
	metaApprovalPolicyKey = "approvalPolicy"
	metaSandboxPolicyKey  = "sandboxPolicy"
	metaEnabledKey        = "enabled"
)

// CodexOptions is the per-session options struct carried at
// _meta.codex.options.
type CodexOptions struct {
	// Model selects the Codex model for this session.
	Model string `json:"model,omitempty"`
	// Env overlays the session's thread environment.
	Env map[string]string `json:"env,omitempty"`
	// ExtraPathDirs are absolute directories prepended, in order, to the PATH
	// of this session's thread.
	ExtraPathDirs []string `json:"extraPathDirs,omitempty"`
	// OutputSchema is the JSON schema every turn's final answer must satisfy.
	OutputSchema map[string]any `json:"outputSchema,omitempty"`
	// Effort is the reasoning effort passed to Codex.
	Effort string `json:"effort,omitempty"`
	// ServiceTier is the service tier passed to Codex.
	ServiceTier string `json:"serviceTier,omitempty"`
	// Personality is the personality passed to Codex.
	Personality string `json:"personality,omitempty"`
	// ApprovalPolicy is Codex's own approval policy, forwarded unchanged.
	ApprovalPolicy any `json:"approvalPolicy,omitempty"`
	// SandboxPolicy is Codex's own sandbox policy, forwarded unchanged.
	SandboxPolicy any `json:"sandboxPolicy,omitempty"`
}

// CodexOption configures CodexOptions values.
type CodexOption func(*CodexOptions)

// NewCodexOptions constructs CodexOptions from functional options.
func NewCodexOptions(opts ...CodexOption) CodexOptions {
	options := CodexOptions{}
	for _, opt := range opts {
		opt(&options)
	}

	return options.clone()
}

// WithCodexModel configures the session model.
func WithCodexModel(model string) CodexOption {
	return func(options *CodexOptions) { options.Model = model }
}

// WithCodexEnv configures the session environment overlay.
func WithCodexEnv(env map[string]string) CodexOption {
	cloned := cloneStringMap(env)

	return func(options *CodexOptions) { options.Env = cloneStringMap(cloned) }
}

// WithCodexExtraPathDirs configures the directories prepended to the session PATH.
func WithCodexExtraPathDirs(dirs ...string) CodexOption {
	cloned := slices.Clone(dirs)

	return func(options *CodexOptions) { options.ExtraPathDirs = slices.Clone(cloned) }
}

// WithCodexOutputSchema configures structured output for every turn.
func WithCodexOutputSchema(schema map[string]any) CodexOption {
	cloned := cloneAnyMap(schema)

	return func(options *CodexOptions) { options.OutputSchema = cloneAnyMap(cloned) }
}

// WithCodexEffort configures the reasoning effort.
func WithCodexEffort(effort string) CodexOption {
	return func(options *CodexOptions) { options.Effort = effort }
}

// WithCodexServiceTier configures the service tier.
func WithCodexServiceTier(tier string) CodexOption {
	return func(options *CodexOptions) { options.ServiceTier = tier }
}

// WithCodexPersonality configures the personality.
func WithCodexPersonality(personality string) CodexOption {
	return func(options *CodexOptions) { options.Personality = personality }
}

// WithCodexApprovalPolicy configures Codex's approval policy.
func WithCodexApprovalPolicy(policy any) CodexOption {
	cloned := cloneAny(policy)

	return func(options *CodexOptions) { options.ApprovalPolicy = cloneAny(cloned) }
}

// WithCodexSandboxPolicy configures Codex's sandbox policy.
func WithCodexSandboxPolicy(policy any) CodexOption {
	cloned := cloneAny(policy)

	return func(options *CodexOptions) { options.SandboxPolicy = cloneAny(cloned) }
}

// Meta returns exactly {"codex": {"options": {...}}} with the non-zero fields.
func (options CodexOptions) Meta() map[string]any {
	values := map[string]any{}

	if options.Model != "" {
		values[metaModelKey] = options.Model
	}

	if options.Env != nil {
		values[metaEnvKey] = cloneStringMap(options.Env)
	}

	if options.ExtraPathDirs != nil {
		values[metaExtraPathDirsKey] = slices.Clone(options.ExtraPathDirs)
	}

	if options.OutputSchema != nil {
		values[metaOutputSchemaKey] = cloneAnyMap(options.OutputSchema)
	}

	if options.Effort != "" {
		values[metaEffortKey] = options.Effort
	}

	if options.ServiceTier != "" {
		values[metaServiceTierKey] = options.ServiceTier
	}

	if options.Personality != "" {
		values[metaPersonalityKey] = options.Personality
	}

	if options.ApprovalPolicy != nil {
		values[metaApprovalPolicyKey] = cloneAny(options.ApprovalPolicy)
	}

	if options.SandboxPolicy != nil {
		values[metaSandboxPolicyKey] = cloneAny(options.SandboxPolicy)
	}

	return map[string]any{vendor: map[string]any{metaOptionsKey: values}}
}

func (options CodexOptions) clone() CodexOptions {
	cloned := options
	cloned.Env = cloneStringMap(options.Env)
	cloned.ExtraPathDirs = slices.Clone(options.ExtraPathDirs)
	cloned.OutputSchema = cloneAnyMap(options.OutputSchema)
	cloned.ApprovalPolicy = cloneAny(options.ApprovalPolicy)
	cloned.SandboxPolicy = cloneAny(options.SandboxPolicy)

	return cloned
}

// ValidateCodexSessionMeta runs the owned-namespace parsing of a session
// lifecycle request's _meta without an Agent and returns the same refusal.
func ValidateCodexSessionMeta(meta map[string]any) error {
	_, err := parseSessionMeta(meta)
	if err != nil {
		return err
	}

	return nil
}

// sessionMeta is what one session lifecycle request's _meta.codex carried.
type sessionMeta struct {
	options   CodexOptions
	rawEvents bool
	// present records which carrier fields the request named, so a load or
	// resume inherits the stored value only for fields it left out.
	presentEnv           bool
	presentExtraPathDirs bool
}

// parseSessionMeta validates the owned _meta.codex namespace of one session
// lifecycle request. Unknown own-namespace keys fail closed; foreign
// namespaces are ignored; the lifecycle literal is refused by name.
func parseSessionMeta(meta map[string]any) (sessionMeta, *acp.RequestError) {
	if refusal := lifecycle.RejectKey(meta); refusal != nil {
		return sessionMeta{}, invalidParam(refusal)
	}

	raw, exists := meta[vendor]
	if !exists {
		return sessionMeta{}, nil
	}

	codexMeta, ok := raw.(map[string]any)
	if !ok {
		return sessionMeta{}, wire.Unsupported("_meta." + vendor)
	}

	parsed := sessionMeta{}

	for key := range codexMeta {
		switch key {
		case metaOptionsKey, metaRawEventKey:
		default:
			return sessionMeta{}, wire.Unsupported("_meta." + vendor + "." + key)
		}
	}

	if rawEvent, ok := codexMeta[metaRawEventKey]; ok {
		values, ok := rawEvent.(map[string]any)
		if !ok {
			return sessionMeta{}, wire.Unsupported("_meta." + vendor + "." + metaRawEventKey)
		}

		for key, item := range values {
			enabled, ok := item.(bool)
			if key != metaEnabledKey || !ok {
				return sessionMeta{}, wire.Unsupported("_meta." + vendor + "." + metaRawEventKey + "." + key)
			}

			parsed.rawEvents = enabled
		}
	}

	rawOptions, hasOptions := codexMeta[metaOptionsKey]
	if !hasOptions {
		return parsed, nil
	}

	values, isObject := rawOptions.(map[string]any)
	if !isObject {
		return sessionMeta{}, wire.Unsupported(metaOptionPath(""))
	}

	options, err := parseCodexOptions(values)
	if err != nil {
		return sessionMeta{}, err
	}

	parsed.options = options
	_, parsed.presentEnv = values[metaEnvKey]
	_, parsed.presentExtraPathDirs = values[metaExtraPathDirsKey]

	return parsed, nil
}

func parseCodexOptions(values map[string]any) (CodexOptions, *acp.RequestError) {
	options := CodexOptions{}

	for key, item := range values {
		switch key {
		case metaModelKey, metaEffortKey, metaServiceTierKey, metaPersonalityKey:
			text, ok := item.(string)
			if !ok || text == "" {
				return CodexOptions{}, wire.Unsupported(metaOptionPath(key))
			}

			switch key {
			case metaModelKey:
				options.Model = text
			case metaEffortKey:
				options.Effort = text
			case metaServiceTierKey:
				options.ServiceTier = text
			default:
				options.Personality = text
			}
		case metaEnvKey:
			env, err := stringMapOption(item, metaOptionPath(key))
			if err != nil {
				return CodexOptions{}, err
			}

			options.Env = env
		case metaExtraPathDirsKey:
			dirs, err := stringSliceOption(item, metaOptionPath(key))
			if err != nil {
				return CodexOptions{}, err
			}

			options.ExtraPathDirs = dirs
		case metaOutputSchemaKey:
			schema, ok := item.(map[string]any)
			if !ok || len(schema) == 0 {
				return CodexOptions{}, wire.Unsupported(metaOptionPath(key))
			}

			options.OutputSchema = cloneAnyMap(schema)
		case metaApprovalPolicyKey, metaSandboxPolicyKey:
			policy, ok := policyOption(item)
			if !ok {
				return CodexOptions{}, wire.Unsupported(metaOptionPath(key))
			}

			if key == metaApprovalPolicyKey {
				options.ApprovalPolicy = policy
			} else {
				options.SandboxPolicy = policy
			}
		default:
			return CodexOptions{}, wire.Unsupported(metaOptionPath(key))
		}
	}

	return options, validateCodexOptions(options)
}

// policyOption accepts Codex's own policy spellings: a non-empty string or an
// object.
func policyOption(item any) (any, bool) {
	switch typed := item.(type) {
	case string:
		return typed, typed != ""
	case map[string]any:
		return cloneAnyMap(typed), len(typed) > 0
	default:
		return nil, false
	}
}

func validateCodexOptions(options CodexOptions) *acp.RequestError {
	if err := process.ValidateNames(options.Env); err != nil {
		var nameErr *process.NameError
		if errors.As(err, &nameErr) {
			return wire.Unsupported(metaOptionPath(metaEnvKey) + "." + nameErr.Key)
		}

		return wire.Unsupported(metaOptionPath(metaEnvKey))
	}

	if err := process.ValidateExtraPathDirs(options.ExtraPathDirs); err != nil {
		var dirErr *process.PathDirError
		if errors.As(err, &dirErr) {
			return wire.Unsupported(fmt.Sprintf("%s[%d]", metaOptionPath(metaExtraPathDirsKey), dirErr.Index))
		}

		return wire.Unsupported(metaOptionPath(metaExtraPathDirsKey))
	}

	return nil
}

func metaOptionPath(key string) string {
	path := "_meta." + vendor + "." + metaOptionsKey
	if key == "" {
		return path
	}

	return path + "." + key
}

func stringMapOption(value any, path string) (map[string]string, *acp.RequestError) {
	switch typed := value.(type) {
	case map[string]string:
		return cloneStringMap(typed), nil
	case map[string]any:
		result := make(map[string]string, len(typed))
		for key, item := range typed {
			text, ok := item.(string)
			if !ok {
				return nil, wire.Unsupported(path + "." + key)
			}

			result[key] = text
		}

		return result, nil
	default:
		return nil, wire.Unsupported(path)
	}
}

func stringSliceOption(value any, path string) ([]string, *acp.RequestError) {
	switch typed := value.(type) {
	case []string:
		return slices.Clone(typed), nil
	case []any:
		result := make([]string, 0, len(typed))
		for index, item := range typed {
			text, ok := item.(string)
			if !ok {
				return nil, wire.Unsupported(fmt.Sprintf("%s[%d]", path, index))
			}

			result = append(result, text)
		}

		return result, nil
	default:
		return nil, wire.Unsupported(path)
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

func cloneAny(value any) any {
	switch typed := value.(type) {
	case map[string]any:
		return cloneAnyMap(typed)
	case []any:
		cloned := make([]any, len(typed))
		for index, item := range typed {
			cloned[index] = cloneAny(item)
		}

		return cloned
	case []string:
		return slices.Clone(typed)
	default:
		return typed
	}
}

func mergeAnyMap(base map[string]any, overlay map[string]any) map[string]any {
	result := cloneAnyMap(base)
	if result == nil {
		result = map[string]any{}
	}

	for key, value := range overlay {
		if valueMap, ok := value.(map[string]any); ok {
			if existing, ok := result[key].(map[string]any); ok {
				result[key] = mergeAnyMap(existing, valueMap)

				continue
			}
		}

		result[key] = cloneAny(value)
	}

	return result
}
