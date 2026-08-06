package codex

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"time"
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
	CLIPath          string
	CodexHome        string
	ScratchDir       string
	Mode             string
	DeviceAuth       bool
	DarwinBestEffort bool
	Stdin            io.Reader
	Stdout           io.Writer
	Stderr           io.Writer
	Signals          <-chan os.Signal
	Env              map[string]string
	ProcessIsolation *ProcessIsolation
}

var accountScratchParent func(string) (string, error)
var accountMkdirTemp = os.MkdirTemp
var accountRemoveAll = os.RemoveAll
var accountStartProcess = startProcess
var accountSupervisorCommand = supervisorCommand
var accountProbeVersion = ProbeVersion

// SetScratchParentResolver installs the root package's canonical scratch
// accessor. The internal account-command runner never resolves system temp on
// its own; every production caller enters through the root package first.
func SetScratchParentResolver(resolver func(string) (string, error)) {
	accountScratchParent = resolver
}

// RunAccountCommand performs a login or logout only while it exclusively owns
// the writable Codex home. It returns after the selected containment boundary
// completes.
func RunAccountCommand(ctx context.Context, options AccountCommandOptions) (returnErr error) {
	args, err := accountCommandArgs(options.Mode, options.DeviceAuth)
	if err != nil {
		return err
	}

	if options.CodexHome == "" {
		return errors.New("codex writable home is required for account mutation")
	}

	if validationErr := validateNativeOwnedDirectory(options.CodexHome, options.ProcessIsolation); validationErr != nil {
		return fmt.Errorf("validate codex writable home: %w", validationErr)
	}

	nativeEnv, err := buildProcessEnvironment(
		options.ProcessIsolation,
		withoutManagedRootOverrides(options.Env),
		map[string]string{envCodexHome: options.CodexHome},
	)
	if err != nil {
		return err
	}

	path, err := resolveCodexPath(options.CLIPath, nativeEnv)
	if err != nil {
		return err
	}

	if accountScratchParent == nil {
		return errors.New("codex scratch parent resolver is not configured")
	}

	scratchParent, err := accountScratchParent(options.ScratchDir)
	if err != nil {
		return fmt.Errorf("resolve account-command scratch parent: %w", err)
	}

	// Only the login leg execs a browser launcher. Logout removes a resident
	// credential and opens nothing, so it keeps its environment and stays
	// available on platforms where no launch can be neutralised.
	var shim *browserShim

	if options.Mode == accountCommandLogin {
		shim, err = newBrowserShim(scratchParent)
		if err != nil {
			return err
		}

		if handoffErr := shim.handoff(options.ProcessIsolation); handoffErr != nil {
			return errors.Join(handoffErr, shim.remove())
		}
	}

	defer func() {
		if !errors.Is(returnErr, ErrProcessContainmentIncomplete) {
			returnErr = errors.Join(returnErr, shim.remove())
		}
	}()

	if _, versionErr := runAccountVersionProbe(ctx, path, scratchParent, options); versionErr != nil {
		return versionErr
	}

	scratch, err := accountMkdirTemp(scratchParent, accountCommandScratchPrefix)
	if err != nil {
		return fmt.Errorf("create account-command supervisor scratch: %w", err)
	}
	defer func() {
		if !errors.Is(returnErr, ErrProcessContainmentIncomplete) {
			returnErr = errors.Join(returnErr, accountRemoveAll(scratch))
		}
	}()

	lockRoot, err := HomeLockRoot(scratchParent, options.CodexHome)
	if err != nil {
		return err
	}

	cmd, proof, err := accountSupervisorCommand(ctx, supervisorConfig{
		NativePath:       path,
		NativeArgs:       args,
		NativeEnv:        shim.environ(nativeEnv),
		Isolation:        options.ProcessIsolation,
		Home:             lockRoot,
		Scratch:          scratch,
		ScratchParent:    scratchParent,
		LifecycleKind:    lifecycleDiscovery,
		DarwinBestEffort: options.DarwinBestEffort,
		FramedInput:      true,
	})
	if err != nil {
		return err
	}

	// The guardian reads a hangup on its control input as caller death and
	// abandons agent identity acquisition. Handing the caller's reader straight
	// to exec.Cmd makes os/exec close that pipe as soon as the reader ends, so a
	// caller that supplies no terminal input hangs up on a claim that is still
	// walking /proc and reports a containment failure that never happened. This
	// command owns the write end for the command's lifetime and frames what the
	// caller sends: a zero-length frame ends native stdin, and only the loss of
	// this process closes the pipe.
	controlRead, controlWrite, err := supervisorPipe()
	if err != nil {
		return errors.Join(fmt.Errorf("open account-command control input: %w", err), proof.closeInherited())
	}

	defer controlRead.Close()
	defer controlWrite.Close()

	cmd.Stdin = controlRead
	cmd.Stdout = options.Stdout
	cmd.Stderr = options.Stderr

	waiter, err := accountStartProcess(cmd)
	if err != nil {
		_ = proof.closeInherited()

		return err
	}

	commandInput := options.Stdin
	if commandInput == nil {
		commandInput = bytes.NewReader(nil)
	}

	inputDone := make(chan struct{}, 1)
	go copySupervisorFramedInput(controlWrite, commandInput, inputDone)

	if closeErr := proof.closeInherited(); closeErr != nil {
		_ = cmd.Process.Kill()

		waiter.start()
		<-waiter.result()

		return fmt.Errorf("close inherited supervisor config: %w", closeErr)
	}

	waiter.start()
	waitDone := waiter.result()

	quarantinePoll := time.NewTicker(10 * time.Millisecond)
	defer quarantinePoll.Stop()

	signals := options.Signals

	for {
		select {
		case waitErr := <-waitDone:
			return errors.Join(waitErr, proof.awaitCompletion())
		case <-quarantinePoll.C:
			quarantined, quarantineErr := proof.quarantineDetected()
			if quarantineErr != nil {
				return errors.Join(ErrProcessContainmentIncomplete, quarantineErr)
			}

			if quarantined {
				return proof.awaitCompletion()
			}
		case signalValue, ok := <-signals:
			if !ok {
				signals = nil

				continue
			}

			_ = cmd.Process.Signal(signalValue)
		}
	}
}

func runAccountVersionProbe(
	ctx context.Context,
	path string,
	scratchParent string,
	options AccountCommandOptions,
) (version string, returnErr error) {
	scratch, err := accountMkdirTemp(scratchParent, accountCommandScratchPrefix+"version-")
	if err != nil {
		return "", fmt.Errorf("create account-command version scratch: %w", err)
	}
	defer func() {
		if !errors.Is(returnErr, ErrProcessContainmentIncomplete) {
			returnErr = errors.Join(returnErr, accountRemoveAll(scratch))
		}
	}()

	version, returnErr = accountProbeVersion(ctx, VersionProbeOptions{
		CLIPath:          path,
		CodexHome:        options.CodexHome,
		WritableHome:     options.CodexHome,
		Scratch:          scratch,
		ScratchParent:    scratchParent,
		DarwinBestEffort: options.DarwinBestEffort,
		Env:              options.Env,
		ProcessIsolation: options.ProcessIsolation,
	})

	return version, returnErr
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
