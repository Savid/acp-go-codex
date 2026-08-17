package codex

import (
	"fmt"
	"strconv"
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
