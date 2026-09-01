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

	if native == nil {
		return "", ErrHostAuthorityUnavailable
	}

	stdin, stdoutReader, stderrReader := native.Stdin(), native.Stdout(), native.Stderr()
	if stdin == nil || stdoutReader == nil || stderrReader == nil {
		var cleanupErr error

		_, _, cleanupErr = revokeAndWaitNative(native)
		closeErr := closeVersionProbePipes(stdin, stdoutReader, stderrReader, nil, nil)

		return "", errors.Join(
			fmt.Errorf("%w: host returned incomplete native stdio", ErrHostAuthorityUnavailable),
			cleanupErr, closeErr,
		)
	}

	inputErr := processPipeCloseError(stdin.Close())

	var stdout bytes.Buffer

	stdoutDone := make(chan error, 1)
	stderrDone := make(chan error, 1)

	go func() { _, copyErr := io.Copy(&stdout, stdoutReader); stdoutDone <- copyErr }()
	go func() { _, copyErr := io.Copy(io.Discard, stderrReader); stderrDone <- copyErr }()

	result, waitErr := native.Wait(ctx)
	terminalResult := result
	terminal := waitErr == nil

	var settlementErr error
	if waitErr != nil {
		terminalResult, terminal, settlementErr = revokeAndWaitNative(native)
	}

	copyErr := errors.Join(inputErr, settleVersionProbePipes(terminal, stdoutReader, stderrReader, stdoutDone, stderrDone))

	if ctx.Err() != nil {
		return "", errors.Join(ctx.Err(), withoutExactErrorLeaves(waitErr, ctx.Err()), settlementErr, copyErr)
	}

	if !terminal {
		return "", fmt.Errorf("check codex CLI version: %w", errors.Join(waitErr, settlementErr, copyErr))
	}

	if waitErr != nil {
		return "", fmt.Errorf("check codex CLI version: %w", errors.Join(waitErr, settlementErr, copyErr))
	}

	if terminalResult.ExitCode != 0 || terminalResult.Signal != 0 {
		return "", errors.Join(
			fmt.Errorf("check codex CLI version: status %d signal %d", terminalResult.ExitCode, terminalResult.Signal),
			copyErr,
		)
	}

	if copyErr != nil {
		return "", copyErr
	}

	return validateCodexVersionOutput(stdout.String())
}

func settleVersionProbePipes(
	terminal bool,
	stdout, stderr io.ReadCloser,
	stdoutDone, stderrDone <-chan error,
) error {
	if !terminal {
		return closeVersionProbePipes(nil, stdout, stderr, stdoutDone, stderrDone)
	}

	return errors.Join(
		joinVersionProbeCopies(stdoutDone, stderrDone),
		closeVersionProbePipes(nil, stdout, stderr, nil, nil),
	)
}

func joinVersionProbeCopies(stdoutDone, stderrDone <-chan error) error {
	var err error
	if stdoutDone != nil {
		err = processPipeCloseError(<-stdoutDone)
	}

	if stderrDone != nil {
		err = errors.Join(err, processPipeCloseError(<-stderrDone))
	}

	return err
}

func closeVersionProbePipes(
	stdin io.WriteCloser,
	stdout, stderr io.ReadCloser,
	stdoutDone, stderrDone <-chan error,
) error {
	err := closeNativePipes(stdin, stdout, stderr)

	err = errors.Join(err, joinVersionProbeCopies(stdoutDone, stderrDone))

	return err
}
