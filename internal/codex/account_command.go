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
		ImplicitEnvironment: options.ImplicitEnvironment,
	}

	environment, err := buildMergedEnv(providerOptions)
	if err != nil {
		return err
	}

	selector := strings.TrimSpace(options.CLIPath)
	if selector == "" {
		selector = defaultCodexExecutable
	}

	selector, err = resolveOrdinaryProcessExecutable(selector, environment)
	if err != nil {
		return err
	}

	var shim *browserShim
	if options.Mode == accountCommandLogin {
		shim, err = newBrowserShim(scratchParent)
		if err != nil {
			return err
		}

		environment = shim.environ(environment)
	}

	defer func() {
		returnErr = errors.Join(returnErr, shim.remove())
	}()

	if _, probeErr := accountProbeVersion(ctx, VersionProbeOptions{
		CLIPath: selector, CodexHome: options.CodexHome, Scratch: scratchParent, ScratchParent: scratchParent,
		Env: options.Env, ImplicitEnvironment: options.ImplicitEnvironment,
	}); probeErr != nil {
		return probeErr
	}

	request := NativeRequest{Executable: selector, Arguments: args, Environment: environment}

	native, err := startOrdinaryNative(ctx, request, providerOptions)
	if err != nil {
		return err
	}

	return runAccountNative(ctx, options, native)
}

func runAccountNative(ctx context.Context, options AccountCommandOptions, native NativeProcess) error {
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
		copyErr := errors.Join(accountCopyError(<-copyDone), accountCopyError(<-copyDone))
		result, waitErr := native.Wait(context.WithoutCancel(ctx))

		terminal := waitErr == nil
		if terminal && (result.ExitCode != 0 || result.Signal != 0) {
			waitErr = fmt.Errorf("codex account command exited with status %d signal %d", result.ExitCode, result.Signal)
		}

		waitDone <- waitResult{err: errors.Join(waitErr, copyErr), terminal: terminal}
	}()

	signals := options.Signals

	for {
		select {
		case wait := <-waitDone:
			settleAccountNativePipes(native)

			return wait.err
		case <-ctx.Done():
			revokeErr := native.Revoke(context.Background())
			wait := <-waitDone

			settleAccountNativePipes(native)

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

			settleAccountNativePipes(native)

			if wait.terminal {
				revokeErr = nil
			} else {
				revokeErr = errors.Join(ErrContainmentIncomplete, revokeErr)
			}

			return errors.Join(revokeErr, wait.err)
		}
	}
}

// settleAccountNativePipes releases this process's ends of the child's three
// standard streams. The ordinary backend owns both ends of every pipe outright,
// so nothing closes the parent ends on this process's behalf once each reader
// has drained its stream to EOF.
func settleAccountNativePipes(native NativeProcess) {
	_ = closeNativePipes(native.Stdin(), native.Stdout(), native.Stderr())
}

func accountCopyError(err error) error {
	if errors.Is(err, os.ErrClosed) {
		return nil
	}

	return err
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
