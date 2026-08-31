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
	ScratchDir          string
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

	scratchParent, err := accountScratchParent(options.ScratchDir)
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
				return errors.Join(prepareErr, reclaimAccountTrees(options.HostAuthority, prepared), shim.remove())
			}

			prepared = append(prepared, tree)
		}
	}
	defer func() {
		returnErr = errors.Join(returnErr, reclaimAccountTrees(options.HostAuthority, prepared), shim.remove())
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

	if native == nil || native.Stdin() == nil || native.Stdout() == nil || native.Stderr() == nil {
		if native != nil {
			_ = native.Revoke(context.Background())
			_, _ = native.Wait(context.Background())
		}

		return fmt.Errorf("%w: host returned incomplete native stdio", ErrHostAuthorityUnavailable)
	}

	copyDone := make(chan error, 3)

	go func() {
		_, copyErr := io.Copy(native.Stdin(), readerOrEmpty(options.Stdin))
		_ = native.Stdin().Close()

		copyDone <- copyErr
	}()
	go func() { _, copyErr := io.Copy(writerOrDiscard(options.Stdout), native.Stdout()); copyDone <- copyErr }()
	go func() { _, copyErr := io.Copy(writerOrDiscard(options.Stderr), native.Stderr()); copyDone <- copyErr }()

	waitDone := make(chan error, 1)

	go func() {
		result, waitErr := native.Wait(context.WithoutCancel(ctx))
		if waitErr == nil && (result.ExitCode != 0 || result.Signal != 0) {
			waitErr = fmt.Errorf("codex account command exited with status %d signal %d", result.ExitCode, result.Signal)
		}

		waitDone <- waitErr
	}()

	select {
	case waitErr := <-waitDone:
		return errors.Join(waitErr, <-copyDone, <-copyDone, <-copyDone)
	case <-ctx.Done():
		_ = native.Revoke(context.Background())
		waitErr := <-waitDone

		return errors.Join(ctx.Err(), waitErr)
	case <-options.Signals:
		_ = native.Revoke(context.Background())

		return <-waitDone
	}
}

func reclaimAccountTrees(authority HostAuthority, trees []string) error {
	if authority == nil {
		return nil
	}

	var result error

	for index := len(trees) - 1; index >= 0; index-- {
		if err := authority.ReclaimNativeTree(context.Background(), trees[index]); err != nil {
			result = errors.Join(result, fmt.Errorf("%w: %v", ErrContainmentIncomplete, err))
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
