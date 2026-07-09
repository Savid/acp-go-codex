package codex

import (
	"fmt"
	"strconv"
	"strings"

	"github.com/coder/acp-go-sdk"
)

// configFlag is the Codex app-server CLI flag that injects a single dotted-TOML
// config override.
const configFlag = "-c"

// TOMLLiteral marks a value that is already rendered as TOML so ConfigArg emits
// it verbatim instead of quoting it as a basic string.
type TOMLLiteral string

// ConfigArg renders a Codex `-c key=value` override. String values are quoted as
// TOML basic strings; TOMLLiteral values are emitted verbatim.
func ConfigArg(key string, value any) []string {
	switch typed := value.(type) {
	case TOMLLiteral:
		return []string{configFlag, key + "=" + string(typed)}
	case string:
		return []string{configFlag, key + "=" + TOMLString(typed)}
	default:
		return []string{configFlag, key + "=" + fmt.Sprint(typed)}
	}
}

// TOMLString quotes a value as a TOML basic string.
func TOMLString(value string) string {
	return strconv.Quote(value)
}

// TOMLStringArray renders a TOML array of quoted strings.
func TOMLStringArray(values []string) string {
	items := make([]string, 0, len(values))
	for _, value := range values {
		items = append(items, TOMLString(value))
	}

	return "[" + strings.Join(items, ", ") + "]"
}

// TOMLEnvTable renders an inline TOML table of environment variables, skipping
// entries with empty names.
func TOMLEnvTable(env []acp.EnvVariable) string {
	items := make([]string, 0, len(env))
	for _, variable := range env {
		if variable.Name == "" {
			continue
		}

		items = append(items, variable.Name+" = "+TOMLString(variable.Value))
	}

	return "{ " + strings.Join(items, ", ") + " }"
}
