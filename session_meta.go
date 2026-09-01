package codexacp

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"

	"github.com/coder/acp-go-sdk"
	"github.com/savid/acp-go-codex/internal/codex"
)

type sessionMeta struct {
	Model                string
	ReasoningEffort      string
	ServiceTier          string
	Personality          string
	Env                  map[string]string
	EnvPresent           bool
	ExtraPathDirs        []string
	ExtraPathDirsPresent bool
	ApprovalPolicy       any
	SandboxPolicy        any
	OutputSchema         any
	RawMessages          rawMessageConfig
	MCPToolApprovalMode  string
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
		Model:                codexOptions.Model,
		ReasoningEffort:      codexOptions.ReasoningEffort,
		ServiceTier:          codexOptions.ServiceTier,
		Personality:          codexOptions.Personality,
		Env:                  cloneStringMap(codexOptions.Env),
		EnvPresent:           codexOptions.EnvPresent,
		ExtraPathDirs:        cloneStrings(codexOptions.ExtraPathDirs),
		ExtraPathDirsPresent: codexOptions.ExtraPathDirsPresent,
		ApprovalPolicy:       codexOptions.ApprovalPolicy,
		SandboxPolicy:        codexOptions.SandboxPolicy,
		OutputSchema:         codexOptions.OutputSchema,
		RawMessages:          rawMessageConfigFromMeta(meta),
		MCPToolApprovalMode:  codexOptions.MCPToolApprovalMode,
	}, nil
}

type codexOptions struct {
	Model                string
	ReasoningEffort      string
	ServiceTier          string
	Personality          string
	Env                  map[string]string
	EnvPresent           bool
	ExtraPathDirs        []string
	ExtraPathDirsPresent bool
	ApprovalPolicy       any
	SandboxPolicy        any
	OutputSchema         any
	MCPToolApprovalMode  string
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

	effort, err := nonEmptyMetaOptionString(optionsMap, metaEffortKey)
	if err != nil {
		return codexOptions{}, err
	}

	options.ReasoningEffort = effort

	tier, err := metaOptionString(optionsMap, metaServiceTierKey)
	if err != nil {
		return codexOptions{}, err
	}

	options.ServiceTier = tier

	personality, err := nonEmptyMetaOptionString(optionsMap, metaPersonalityKey)
	if err != nil {
		return codexOptions{}, err
	}

	options.Personality = personality

	if rawEnv, ok := optionsMap[metaEnvKey]; ok {
		env, envErr := stringMapFromMeta(rawEnv)
		if envErr != nil {
			return codexOptions{}, envErr
		}

		options.Env = env
		options.EnvPresent = true
	}

	if rawPathDirs, ok := optionsMap[metaExtraPathDirsKey]; ok {
		dirs, dirsErr := extraPathDirsFromMeta(rawPathDirs)
		if dirsErr != nil {
			return codexOptions{}, dirsErr
		}

		options.ExtraPathDirs = dirs
		options.ExtraPathDirsPresent = true
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
			return codexOptions{}, unsupportedField("_meta.codex.options." + metaMCPToolApprovalModeKey)
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

// nonEmptyMetaOptionString distinguishes an omitted option from a present
// empty select value. An omitted value leaves native defaults intact; an empty value has
// no value to pass through and is an input-shape error.
func nonEmptyMetaOptionString(optionsMap map[string]any, key string) (string, error) {
	value, err := metaOptionString(optionsMap, key)
	if err != nil {
		return "", err
	}

	if _, present := optionsMap[key]; present && value == "" {
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
			return unsupportedField("_meta.codex")
		}

		return nil
	}

	for key, value := range codexMeta {
		switch key {
		case metaOptionsKey:
			optionsMap, ok := value.(map[string]any)
			if !ok {
				return unsupportedField("_meta.codex." + metaOptionsKey)
			}

			for optionKey := range optionsMap {
				switch optionKey {
				case metaModelKey, metaEnvKey, metaExtraPathDirsKey, metaOutputSchemaKey, metaEffortKey, metaServiceTierKey, metaPersonalityKey, metaApprovalPolicyKey, metaSandboxPolicyKey, metaMCPToolApprovalModeKey:
				default:
					return unsupportedField("_meta.codex.options." + optionKey)
				}
			}
		case rawEventKey:
			rawEvent, ok := value.(map[string]any)
			if !ok {
				return unsupportedField("_meta.codex." + rawEventKey)
			}

			for rawKey, rawValue := range rawEvent {
				switch rawKey {
				case rawEventEnabledKey:
					if _, ok := rawValue.(bool); !ok {
						return unsupportedField("_meta.codex.rawEvent." + rawEventEnabledKey)
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

func stringMapFromMeta(value any) (map[string]string, error) {
	switch typed := value.(type) {
	case map[string]string:
		return validatedSessionEnv(cloneStringMap(typed))
	case map[string]any:
		out := make(map[string]string, len(typed))
		for key, raw := range typed {
			str, ok := raw.(string)
			if !ok {
				return nil, unsupportedField("_meta.codex.options." + metaEnvKey + "." + key)
			}

			out[key] = str
		}

		return validatedSessionEnv(out)
	default:
		return nil, unsupportedField("_meta.codex.options." + metaEnvKey)
	}
}

// validatedSessionEnv rejects the two classes of session environment key the
// adapter owns: its private names and PATH. The thread PATH is
// derived from extraPathDirs plus the app-server's native PATH, so a raw
// session PATH would be a second, silently losing owner of the same value.
func validatedSessionEnv(env map[string]string) (map[string]string, error) {
	for key := range env {
		if isPathEnvKey(key) || reservedCodexEnvKey(key) {
			return nil, unsupportedField("_meta.codex.options." + metaEnvKey + "." + key)
		}
	}

	return env, nil
}

// pathEnvKey is the search-path variable this adapter derives for every native
// thread. Windows environment keys are case insensitive, so "Path" is the same
// owner there; on other platforms only the exact name is.
const pathEnvKey = "PATH"

var caseInsensitiveEnvKeys = runtime.GOOS == "windows"

func isPathEnvKey(key string) bool {
	return key == pathEnvKey || (caseInsensitiveEnvKeys && strings.EqualFold(key, pathEnvKey))
}

// extraPathDirsFromMeta accepts both decoded forms of an ordered directory
// list: the Go-native []string an embedded caller supplies and the []any a JSON
// decoder produces. Order and duplicates are preserved because native lookup
// order is the whole point of the field.
func extraPathDirsFromMeta(value any) ([]string, error) {
	var raw []any

	switch typed := value.(type) {
	case []string:
		return validatedExtraPathDirs(cloneStrings(typed))
	case []any:
		raw = typed
	default:
		return nil, unsupportedField("_meta.codex.options." + metaExtraPathDirsKey)
	}

	dirs := make([]string, 0, len(raw))

	for index, element := range raw {
		dir, ok := element.(string)
		if !ok {
			return nil, unsupportedField(extraPathDirField(index))
		}

		dirs = append(dirs, dir)
	}

	return validatedExtraPathDirs(dirs)
}

// validatedExtraPathDirs rejects a directory the adapter cannot splice into a
// native PATH: a list separator would smuggle a second entry past this check,
// and only an absolute path resolves identically from the adapter's cwd and the
// harness's. The empty string fails the absolute test, so it needs no case.
func validatedExtraPathDirs(dirs []string) ([]string, error) {
	for index, dir := range dirs {
		if strings.ContainsRune(dir, os.PathListSeparator) || !filepath.IsAbs(dir) {
			return nil, unsupportedField(extraPathDirField(index))
		}
	}

	return dirs, nil
}

func extraPathDirField(index int) string {
	return fmt.Sprintf("_meta.codex.options.%s[%d]", metaExtraPathDirsKey, index)
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
		return unsupportedField(outputSchemaConfigPath)
	}

	// An embedded Go caller can hand over a value no JSON encoder accepts, and
	// the schema is forwarded verbatim into the native request.
	if _, err := json.Marshal(obj); err != nil {
		return unsupportedField(outputSchemaConfigPath)
	}

	return nil
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

func cloneStrings(values []string) []string {
	if values == nil {
		return nil
	}

	return append([]string{}, values...)
}
