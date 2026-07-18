package codex

import (
	"bytes"
	"context"
	"errors"
	"fmt"
)

const codexVersionArgument = "--version"

// VersionProbeOptions describes one independently contained Codex discovery
// root. Scratch is a fresh generation owned only by this probe.
type VersionProbeOptions struct {
	CLIPath          string
	CodexHome        string
	WritableHome     string
	Scratch          string
	ScratchParent    string
	DarwinBestEffort bool
	Env              map[string]string
}

var versionSupervisorCommand = supervisorCommand
var versionStartProcess = startProcess

// ProbeVersion runs codex --version through its own guardian/liveness pair.
func ProbeVersion(ctx context.Context, options VersionProbeOptions) (string, error) {
	path, err := resolveCodexPath(options.CLIPath)
	if err != nil {
		return "", err
	}

	cmd, proof, err := versionSupervisorCommand(ctx, supervisorConfig{
		NativePath:       path,
		NativeArgs:       []string{codexVersionArgument},
		NativeEnv:        mergedEnv(Options{CodexHome: options.CodexHome, Env: options.Env}),
		Home:             options.WritableHome,
		Scratch:          options.Scratch,
		ScratchParent:    options.ScratchParent,
		LifecycleKind:    lifecycleDiscovery,
		DarwinBestEffort: options.DarwinBestEffort,
		FramedInput:      true,
	})
	if err != nil {
		return "", fmt.Errorf("prepare codex CLI version probe: %w", err)
	}

	var (
		stdout bytes.Buffer
		stderr bytes.Buffer
	)

	cmd.Stdin = bytes.NewReader(nil)
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	if err := versionStartProcess(cmd); err != nil {
		return "", fmt.Errorf("start codex CLI version probe: %w", err)
	}

	waitErr, containmentErr := cmd.Wait(), proof.awaitCompletion()

	if waitErr != nil || containmentErr != nil {
		probeErr := waitErr
		if waitErr != nil && stderr.Len() != 0 {
			probeErr = fmt.Errorf("%w: %s", waitErr, bytes.TrimSpace(stderr.Bytes()))
		}

		return "", fmt.Errorf("check codex CLI version: %w", errors.Join(probeErr, containmentErr))
	}

	return validateCodexVersionOutput(stdout.String())
}
