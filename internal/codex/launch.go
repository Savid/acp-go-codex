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

// ThreadUnloadDelayKey is the config key the launch sets to unload idle
// unsubscribed threads at once; overrides may not set it.
const ThreadUnloadDelayKey = "thread_unload_delay_secs"

// Launch describes one app-server process.
type Launch struct {
	// ConfigOverrides are passed as -c key=value, sorted by key.
	ConfigOverrides map[string]any
}

// Args renders the app-server command line. The app-server unloads a thread
// as soon as it is idle and no connection subscribes to it, so a closed
// session's history leaves memory at once and a thread unloaded for repair
// is ready to resume without waiting out the default delay.
func (l Launch) Args() []string {
	args := make([]string, 0, 7+2*len(l.ConfigOverrides))
	args = append(args, "app-server", "--listen", "stdio://", "--disable", "plugins", "-c", ThreadUnloadDelayKey+"=0")

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
