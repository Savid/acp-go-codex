package codexacp

import (
	"maps"
	"slices"

	"github.com/coder/acp-go-sdk"

	"github.com/savid/acp-go-core/lifecycle"
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
	cloned := maps.Clone(env)

	return func(options *CodexOptions) { options.Env = maps.Clone(cloned) }
}

// WithCodexExtraPathDirs configures the directories prepended to the session PATH.
func WithCodexExtraPathDirs(dirs ...string) CodexOption {
	cloned := slices.Clone(dirs)

	return func(options *CodexOptions) { options.ExtraPathDirs = slices.Clone(cloned) }
}

// WithCodexOutputSchema configures structured output for every turn.
func WithCodexOutputSchema(schema map[string]any) CodexOption {
	cloned := wire.CloneMap(schema)

	return func(options *CodexOptions) { options.OutputSchema = wire.CloneMap(cloned) }
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
	cloned := wire.CloneValue(policy)

	return func(options *CodexOptions) { options.ApprovalPolicy = wire.CloneValue(cloned) }
}

// WithCodexSandboxPolicy configures Codex's sandbox policy.
func WithCodexSandboxPolicy(policy any) CodexOption {
	cloned := wire.CloneValue(policy)

	return func(options *CodexOptions) { options.SandboxPolicy = wire.CloneValue(cloned) }
}

// Meta returns exactly {"codex": {"options": {...}}} with the non-zero fields.
func (options CodexOptions) Meta() map[string]any {
	values := map[string]any{}

	if options.Model != "" {
		values[metaModelKey] = options.Model
	}

	if options.Env != nil {
		values[metaEnvKey] = maps.Clone(options.Env)
	}

	if options.ExtraPathDirs != nil {
		values[metaExtraPathDirsKey] = slices.Clone(options.ExtraPathDirs)
	}

	if options.OutputSchema != nil {
		values[metaOutputSchemaKey] = wire.CloneMap(options.OutputSchema)
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
		values[metaApprovalPolicyKey] = wire.CloneValue(options.ApprovalPolicy)
	}

	if options.SandboxPolicy != nil {
		values[metaSandboxPolicyKey] = wire.CloneValue(options.SandboxPolicy)
	}

	return map[string]any{vendor: map[string]any{metaOptionsKey: values}}
}

func (options CodexOptions) clone() CodexOptions {
	cloned := options
	cloned.Env = maps.Clone(options.Env)
	cloned.ExtraPathDirs = slices.Clone(options.ExtraPathDirs)
	cloned.OutputSchema = wire.CloneMap(options.OutputSchema)
	cloned.ApprovalPolicy = wire.CloneValue(options.ApprovalPolicy)
	cloned.SandboxPolicy = wire.CloneValue(options.SandboxPolicy)

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
		return sessionMeta{}, wire.ParamRefusal(refusal)
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
		return sessionMeta{}, wire.Unsupported(wire.MetaOptionPath(vendor, ""))
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
				return CodexOptions{}, wire.Unsupported(wire.MetaOptionPath(vendor, key))
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
			env, err := wire.StringMapOption(item, wire.MetaOptionPath(vendor, key))
			if err != nil {
				return CodexOptions{}, err
			}

			options.Env = env
		case metaExtraPathDirsKey:
			dirs, err := wire.StringSliceOption(item, wire.MetaOptionPath(vendor, key))
			if err != nil {
				return CodexOptions{}, err
			}

			options.ExtraPathDirs = dirs
		case metaOutputSchemaKey:
			schema, ok := item.(map[string]any)
			if !ok || len(schema) == 0 {
				return CodexOptions{}, wire.Unsupported(wire.MetaOptionPath(vendor, key))
			}

			options.OutputSchema = wire.CloneMap(schema)
		case metaApprovalPolicyKey, metaSandboxPolicyKey:
			policy, ok := policyOption(item)
			if !ok {
				return CodexOptions{}, wire.Unsupported(wire.MetaOptionPath(vendor, key))
			}

			if key == metaApprovalPolicyKey {
				options.ApprovalPolicy = policy
			} else {
				options.SandboxPolicy = policy
			}
		default:
			return CodexOptions{}, wire.Unsupported(wire.MetaOptionPath(vendor, key))
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
		return wire.CloneMap(typed), len(typed) > 0
	default:
		return nil, false
	}
}

func validateCodexOptions(options CodexOptions) *acp.RequestError {
	return wire.ValidateSessionEnvironment(options.Env, options.ExtraPathDirs, wire.MetaOptionPath(vendor, ""))
}
