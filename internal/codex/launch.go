package codex

import (
	"fmt"
	"maps"
	"path/filepath"
	"slices"
	"strconv"
)

// Native environment variables Codex reads for its home.
const (
	EnvCodexHome = "CODEX_HOME"
	EnvHome      = "HOME"
)

// Launch describes one app-server process.
type Launch struct {
	// ConfigOverrides are passed as -c key=value, sorted by key.
	ConfigOverrides map[string]any
}

// Args renders the app-server command line.
func (l Launch) Args() []string {
	args := make([]string, 0, 5+2*len(l.ConfigOverrides))
	args = append(args, "app-server", "--listen", "stdio://", "--disable", "plugins")

	for _, key := range slices.Sorted(maps.Keys(l.ConfigOverrides)) {
		args = append(args, "-c", key+"="+tomlValue(l.ConfigOverrides[key]))
	}

	return args
}

// tomlValue renders one -c override value: strings are quoted, everything
// else is printed as a TOML literal.
func tomlValue(value any) string {
	switch typed := value.(type) {
	case string:
		return strconv.Quote(typed)
	case bool:
		return strconv.FormatBool(typed)
	default:
		return fmt.Sprint(typed)
	}
}

// CodexHome resolves the home the app-server uses under an environment: an
// explicit home, else CODEX_HOME, else $HOME/.codex.
func CodexHome(home string, lookup func(string) (string, bool)) string {
	if home != "" {
		return home
	}

	if dir, ok := lookup(EnvCodexHome); ok && dir != "" {
		return dir
	}

	userHome, _ := lookup(EnvHome)

	return filepath.Join(userHome, ".codex")
}
