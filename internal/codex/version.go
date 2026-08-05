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
	ProcessIsolation *ProcessIsolation
}

var versionSupervisorCommand = supervisorCommand
var versionStartProcess = startProcess

// ProbeVersion runs codex --version through its own guardian/liveness pair.
func ProbeVersion(ctx context.Context, options VersionProbeOptions) (string, error) {
	nativeEnv, err := buildMergedEnv(Options{CodexHome: options.CodexHome, Env: options.Env, ProcessIsolation: options.ProcessIsolation})
	if err != nil {
		return "", err
	}

	path, err := resolveCodexPath(options.CLIPath, nativeEnv)
	if err != nil {
		return "", err
	}

	lockRoot, err := HomeLockRoot(options.ScratchParent, options.WritableHome)
	if err != nil {
		return "", err
	}

	cmd, proof, err := versionSupervisorCommand(ctx, supervisorConfig{
		NativePath:       path,
		NativeArgs:       []string{codexVersionArgument},
		NativeEnv:        nativeEnv,
		Isolation:        options.ProcessIsolation,
		Home:             lockRoot,
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

	// The supervisor reads a hangup on its control input as caller death and
	// abandons agent identity acquisition. This probe sends no control data, so
	// it owns the write end for the probe's lifetime rather than handing the
	// guardian a channel that is already closed before it starts.
	controlRead, controlWrite, err := supervisorPipe()
	if err != nil {
		return "", fmt.Errorf("open codex CLI version probe control input: %w", err)
	}

	defer controlRead.Close()
	defer controlWrite.Close()

	cmd.Stdin = controlRead
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	waiter, err := versionStartProcess(cmd)
	if err != nil {
		_ = proof.closeInherited()

		return "", fmt.Errorf("start codex CLI version probe: %w", err)
	}

	if err := proof.closeInherited(); err != nil {
		_ = cmd.Process.Kill()

		waiter.start()
		<-waiter.result()

		return "", fmt.Errorf("close inherited supervisor config: %w", err)
	}

	waiter.start()
	waitErr, containmentErr := proof.awaitCommand(waiter.result())

	if waitErr != nil || containmentErr != nil {
		probeErr := waitErr
		if waitErr != nil && stderr.Len() != 0 {
			probeErr = fmt.Errorf("%w: %s", waitErr, bytes.TrimSpace(stderr.Bytes()))
		}

		return "", fmt.Errorf("check codex CLI version: %w", errors.Join(probeErr, containmentErr))
	}

	return validateCodexVersionOutput(stdout.String())
}
