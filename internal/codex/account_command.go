package codex

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
)

const (
	accountCommandScratchPrefix = "acp-go-codex-account-"
	accountCommandLogin         = "login"
	accountCommandLogout        = "logout"
)

// AccountCommandOptions describes one terminal Codex account mutation. The
// command is run through the same home-lock supervisor and process-tree
// containment used by app-server runtimes.
type AccountCommandOptions struct {
	CLIPath    string
	CodexHome  string
	ScratchDir string
	Mode       string
	DeviceAuth bool
	Stdin      io.Reader
	Stdout     io.Writer
	Stderr     io.Writer
	Signals    <-chan os.Signal
}

var accountScratchParent func(string) (string, error)
var accountMkdirTemp = os.MkdirTemp
var accountRemoveAll = os.RemoveAll
var accountStartProcess = startProcess
var accountSupervisorCommand = supervisorCommand

// SetScratchParentResolver installs the root package's canonical scratch
// accessor. The internal account-command runner never resolves system temp on
// its own; every production caller enters through the root package first.
func SetScratchParentResolver(resolver func(string) (string, error)) {
	accountScratchParent = resolver
}

// RunAccountCommand performs a login or logout only while it exclusively owns
// the writable Codex home. It returns after the contained native process tree
// has been reaped and proven quiescent.
func RunAccountCommand(ctx context.Context, options AccountCommandOptions) (returnErr error) {
	args, err := accountCommandArgs(options.Mode, options.DeviceAuth)
	if err != nil {
		return err
	}

	if options.CodexHome == "" {
		return errors.New("codex writable home is required for account mutation")
	}

	path, err := resolveCodexPath(options.CLIPath)
	if err != nil {
		return err
	}

	if _, versionErr := validateCodexVersion(ctx, path); versionErr != nil {
		return versionErr
	}

	if accountScratchParent == nil {
		return errors.New("codex scratch parent resolver is not configured")
	}

	scratchParent, err := accountScratchParent(options.ScratchDir)
	if err != nil {
		return fmt.Errorf("resolve account-command scratch parent: %w", err)
	}

	scratch, err := accountMkdirTemp(scratchParent, accountCommandScratchPrefix)
	if err != nil {
		return fmt.Errorf("create account-command supervisor scratch: %w", err)
	}
	defer func() { returnErr = errors.Join(returnErr, accountRemoveAll(scratch)) }()

	cmd, proof, err := accountSupervisorCommand(ctx, supervisorConfig{
		NativePath:  path,
		NativeArgs:  args,
		NativeEnv:   upsertEnv(os.Environ(), envCodexHome, options.CodexHome),
		Home:        options.CodexHome,
		Scratch:     scratch,
		FramedInput: true,
	})
	if err != nil {
		return err
	}

	cmd.Stdin = options.Stdin
	cmd.Stdout = options.Stdout
	cmd.Stderr = options.Stderr

	if err := accountStartProcess(cmd); err != nil {
		return err
	}

	waitDone := make(chan error, 1)
	go func() { waitDone <- cmd.Wait() }()

	signals := options.Signals

	for {
		select {
		case waitErr := <-waitDone:
			return errors.Join(waitErr, proof.awaitCompletion())
		case signalValue, ok := <-signals:
			if !ok {
				signals = nil

				continue
			}

			_ = cmd.Process.Signal(signalValue)
		}
	}
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
