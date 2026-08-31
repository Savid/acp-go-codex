package codex

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"strings"
)

const codexVersionArgument = "--version"

type VersionProbeOptions struct {
	CLIPath             string
	CodexHome           string
	Scratch             string
	ScratchParent       string
	Env                 map[string]string
	ImplicitEnvironment map[string]string
	HostAuthority       HostAuthority
}

func ProbeVersion(ctx context.Context, options VersionProbeOptions) (string, error) {
	providerOptions := Options{
		CLIPath: options.CLIPath, CodexHome: options.CodexHome, WritableHome: options.CodexHome,
		Scratch: options.Scratch, ScratchParent: options.ScratchParent,
		Env: options.Env, ImplicitEnvironment: options.ImplicitEnvironment, HostAuthority: options.HostAuthority,
	}

	nativeEnv, err := buildMergedEnv(providerOptions)
	if err != nil {
		return "", err
	}

	selector := strings.TrimSpace(options.CLIPath)
	if selector == "" {
		selector = defaultCodexExecutable
	}

	if options.HostAuthority == nil {
		selector, err = resolveOrdinaryProcessExecutable(selector, nativeEnv)
		if err != nil {
			return "", err
		}
	} else if selectorErr := validateManagedSelector(selector); selectorErr != nil {
		return "", selectorErr
	}

	request := NativeRequest{Executable: selector, Arguments: []string{codexVersionArgument}, Environment: nativeEnv}

	var native NativeProcess
	if options.HostAuthority != nil {
		native, err = options.HostAuthority.StartNative(ctx, request)
	} else {
		native, err = startOrdinaryNative(ctx, request, providerOptions)
	}

	if err != nil {
		return "", fmt.Errorf("start codex CLI version probe: %w", err)
	}

	if native == nil || native.Stdin() == nil || native.Stdout() == nil || native.Stderr() == nil {
		var cleanupErr error

		if native != nil {
			revokeErr := native.Revoke(context.Background())

			_, waitErr := native.Wait(context.Background())
			if waitErr != nil {
				cleanupErr = errors.Join(ErrContainmentIncomplete, revokeErr, waitErr)
			}
		}

		return "", errors.Join(
			fmt.Errorf("%w: host returned incomplete native stdio", ErrHostAuthorityUnavailable),
			cleanupErr,
		)
	}

	_ = native.Stdin().Close()

	var stdout bytes.Buffer

	stdoutDone := make(chan error, 1)
	stderrDone := make(chan error, 1)

	go func() { _, copyErr := io.Copy(&stdout, native.Stdout()); stdoutDone <- copyErr }()
	go func() { _, copyErr := io.Copy(io.Discard, native.Stderr()); stderrDone <- copyErr }()

	result, waitErr := native.Wait(ctx)
	if ctx.Err() != nil {
		revokeErr := native.Revoke(context.Background())

		_, terminalErr := native.Wait(context.Background())
		if terminalErr == nil {
			revokeErr = nil
		} else {
			revokeErr = errors.Join(ErrContainmentIncomplete, revokeErr)
		}

		return "", errors.Join(ctx.Err(), revokeErr, terminalErr)
	}

	copyErr := errors.Join(<-stdoutDone, <-stderrDone)
	if waitErr != nil || result.ExitCode != 0 || result.Signal != 0 {
		return "", fmt.Errorf("check codex CLI version: %w", errors.Join(waitErr, copyErr))
	}

	if copyErr != nil {
		return "", copyErr
	}

	return validateCodexVersionOutput(stdout.String())
}
