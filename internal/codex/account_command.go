package codex

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
)

const (
	accountCommandLogin  = "login"
	accountCommandLogout = "logout"
)

type AccountCommandOptions struct {
	CLIPath             string
	CodexHome           string
	Scratch             string
	Mode                string
	DeviceAuth          bool
	Stdin               io.Reader
	Stdout              io.Writer
	Stderr              io.Writer
	Signals             <-chan os.Signal
	Env                 map[string]string
	ImplicitEnvironment map[string]string
	HostAuthority       HostAuthority
}

var accountScratchParent func(string) (string, error)
var accountProbeVersion = ProbeVersion

func SetScratchParentResolver(resolver func(string) (string, error)) {
	accountScratchParent = resolver
}

func RunAccountCommand(ctx context.Context, options AccountCommandOptions) (returnErr error) {
	args, err := accountCommandArgs(options.Mode, options.DeviceAuth)
	if err != nil {
		return err
	}

	if options.CodexHome == "" {
		return errors.New("codex writable home is required for account mutation")
	}

	if accountScratchParent == nil {
		return errors.New("codex scratch parent resolver is not configured")
	}

	scratchParent, err := accountScratchParent(options.Scratch)
	if err != nil {
		return err
	}

	providerOptions := Options{
		CLIPath: options.CLIPath, CodexHome: options.CodexHome, WritableHome: options.CodexHome,
		Scratch: scratchParent, ScratchParent: scratchParent, Env: options.Env,
		ImplicitEnvironment: options.ImplicitEnvironment, HostAuthority: options.HostAuthority,
	}

	environment, err := buildMergedEnv(providerOptions)
	if err != nil {
		return err
	}

	selector := strings.TrimSpace(options.CLIPath)
	if selector == "" {
		selector = defaultCodexExecutable
	}

	if options.HostAuthority == nil {
		selector, err = resolveOrdinaryProcessExecutable(selector, environment)
		if err != nil {
			return err
		}
	} else if selectorErr := validateManagedSelector(selector); selectorErr != nil {
		return selectorErr
	}

	var shim *browserShim
	if options.Mode == accountCommandLogin {
		shim, err = newBrowserShim(scratchParent)
		if err != nil {
			return err
		}

		environment = shim.environ(environment)
	}

	prepared := make([]string, 0, 2)

	if options.HostAuthority != nil {
		for _, tree := range []string{options.CodexHome, shimDirectory(shim)} {
			if tree == "" {
				continue
			}

			if prepareErr := options.HostAuthority.PrepareNativeTree(ctx, tree); prepareErr != nil {
				if errors.Is(prepareErr, ErrContainmentIncomplete) {
					prepared = append(prepared, tree)
				}

				return errors.Join(prepareErr, cleanupAccountTrees(options.HostAuthority, prepared, shim))
			}

			prepared = append(prepared, tree)
		}
	}
	defer func() {
		returnErr = errors.Join(returnErr, cleanupAccountTrees(options.HostAuthority, prepared, shim))
	}()

	if _, probeErr := accountProbeVersion(ctx, VersionProbeOptions{
		CLIPath: selector, CodexHome: options.CodexHome, Scratch: scratchParent, ScratchParent: scratchParent,
		Env: options.Env, ImplicitEnvironment: options.ImplicitEnvironment, HostAuthority: options.HostAuthority,
	}); probeErr != nil {
		return probeErr
	}

	request := NativeRequest{Executable: selector, Arguments: args, Environment: environment}

	var native NativeProcess
	if options.HostAuthority != nil {
		native, err = options.HostAuthority.StartNative(ctx, request)
	} else {
		native, err = startOrdinaryNative(ctx, request, providerOptions)
	}

	if err != nil {
		return err
	}

	return runAccountNative(ctx, options, native)
}

func runAccountNative(ctx context.Context, options AccountCommandOptions, native NativeProcess) error {
	if native == nil {
		return ErrHostAuthorityUnavailable
	}

	if native.Stdin() == nil || native.Stdout() == nil || native.Stderr() == nil {
		revokeErr := native.Revoke(context.Background())
		_, waitErr := native.Wait(context.Background())

		var cleanupErr error
		if waitErr != nil {
			cleanupErr = errors.Join(ErrContainmentIncomplete, revokeErr, waitErr)
		}

		return errors.Join(
			fmt.Errorf("%w: host returned incomplete native stdio", ErrHostAuthorityUnavailable),
			cleanupErr,
		)
	}

	copyDone := make(chan error, 2)

	go func() {
		_, _ = io.Copy(native.Stdin(), readerOrEmpty(options.Stdin))
		_ = native.Stdin().Close()
	}()
	go func() { _, copyErr := io.Copy(writerOrDiscard(options.Stdout), native.Stdout()); copyDone <- copyErr }()
	go func() { _, copyErr := io.Copy(writerOrDiscard(options.Stderr), native.Stderr()); copyDone <- copyErr }()

	type waitResult struct {
		err      error
		terminal bool
	}

	waitDone := make(chan waitResult, 1)

	go func() {
		result, waitErr := native.Wait(context.WithoutCancel(ctx))

		terminal := waitErr == nil
		if terminal && (result.ExitCode != 0 || result.Signal != 0) {
			waitErr = fmt.Errorf("codex account command exited with status %d signal %d", result.ExitCode, result.Signal)
		}

		waitDone <- waitResult{err: waitErr, terminal: terminal}
	}()

	signals := options.Signals

	for {
		select {
		case wait := <-waitDone:
			_ = native.Stdin().Close()

			return errors.Join(wait.err, <-copyDone, <-copyDone)
		case <-ctx.Done():
			revokeErr := native.Revoke(context.Background())
			wait := <-waitDone
			_ = native.Stdin().Close()

			if wait.terminal {
				revokeErr = nil
			} else {
				revokeErr = errors.Join(ErrContainmentIncomplete, revokeErr)
			}

			return errors.Join(ctx.Err(), revokeErr, wait.err)
		case _, ok := <-signals:
			if !ok {
				signals = nil

				continue
			}

			revokeErr := native.Revoke(context.Background())
			wait := <-waitDone
			_ = native.Stdin().Close()

			if wait.terminal {
				revokeErr = nil
			} else {
				revokeErr = errors.Join(ErrContainmentIncomplete, revokeErr)
			}

			return errors.Join(revokeErr, wait.err)
		}
	}
}

func cleanupAccountTrees(authority HostAuthority, trees []string, shim *browserShim) error {
	if err := reclaimAccountTrees(authority, trees); err != nil {
		return err
	}

	return shim.remove()
}

func reclaimAccountTrees(authority HostAuthority, trees []string) error {
	if authority == nil {
		return nil
	}

	var result error

	for index := len(trees) - 1; index >= 0; index-- {
		if err := authority.ReclaimNativeTree(context.Background(), trees[index]); err != nil {
			if errors.Is(err, ErrNativeTreeBusy) {
				result = errors.Join(result, err)

				continue
			}

			result = errors.Join(result, fmt.Errorf("%w: %w", ErrContainmentIncomplete, err))
		}
	}

	return result
}

func shimDirectory(shim *browserShim) string {
	if shim == nil {
		return ""
	}

	return shim.dir
}

func readerOrEmpty(reader io.Reader) io.Reader {
	if reader == nil {
		return strings.NewReader("")
	}

	return reader
}

func writerOrDiscard(writer io.Writer) io.Writer {
	if writer == nil {
		return io.Discard
	}

	return writer
}

func accountCommandArgs(mode string, deviceAuth bool) ([]string, error) {
	switch mode {
	case accountCommandLogin:
		args := []string{accountCommandLogin}
		if deviceAuth {
			args = append(args, "--device-auth")
		}

		return args, nil
	case accountCommandLogout:
		return []string{accountCommandLogout}, nil
	default:
		return nil, fmt.Errorf("unsupported command %q", mode)
	}
}
