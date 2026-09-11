package codex

import (
	"context"
	"errors"
	"fmt"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
)

// Native environment variables Codex reads for its home.
const (
	EnvCodexHome = "CODEX_HOME"
	EnvHome      = "HOME"
)

// MinimumVersion is the lowest `codex --version` the adapter accepts: the
// version its behavior was verified against.
const MinimumVersion = "0.153.4"

// Launch describes one app-server process.
type Launch struct {
	// ConfigOverrides are passed as -c key=value, sorted by key.
	ConfigOverrides map[string]any
}

// Args renders the app-server command line.
func (l Launch) Args() []string {
	args := make([]string, 0, 5+2*len(l.ConfigOverrides))
	args = append(args, "app-server", "--listen", "stdio://", "--disable", "plugins")

	for _, key := range slices.Sorted(func(yield func(string) bool) {
		for key := range l.ConfigOverrides {
			if !yield(key) {
				return
			}
		}
	}) {
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

var versionPattern = regexp.MustCompile(`\d+\.\d+\.\d+`)

// ProbeVersion runs `codex --version` against the resolved executable with the
// given environment and returns the reported version.
func ProbeVersion(ctx context.Context, executable string, environment []string) (string, error) {
	command := exec.CommandContext(ctx, executable, "--version")
	command.Env = slices.Clone(environment)

	output, err := command.Output()
	if err != nil {
		return "", fmt.Errorf("probe codex version: %w", err)
	}

	version := versionPattern.FindString(string(output))
	if version == "" {
		return "", fmt.Errorf("probe codex version: could not parse %q", strings.TrimSpace(string(output)))
	}

	return version, nil
}

// CheckMinimumVersion fails when version sorts below minimum.
func CheckMinimumVersion(version string, minimum string) error {
	left, err := versionParts(version)
	if err != nil {
		return err
	}

	right, err := versionParts(minimum)
	if err != nil {
		return err
	}

	for index := range max(len(left), len(right)) {
		leftValue, rightValue := 0, 0
		if index < len(left) {
			leftValue = left[index]
		}

		if index < len(right) {
			rightValue = right[index]
		}

		if leftValue < rightValue {
			return fmt.Errorf("codex version %s is below the minimum supported version %s", version, minimum)
		}

		if leftValue > rightValue {
			return nil
		}
	}

	return nil
}

func versionParts(version string) ([]int, error) {
	trimmed := strings.TrimPrefix(strings.TrimSpace(version), "v")
	if trimmed == "" {
		return nil, errors.New("empty version")
	}

	segments := strings.Split(trimmed, ".")
	parts := make([]int, 0, len(segments))

	for _, segment := range segments {
		value, err := strconv.Atoi(segment)
		if err != nil || value < 0 {
			return nil, fmt.Errorf("invalid version %q", version)
		}

		parts = append(parts, value)
	}

	return parts, nil
}
