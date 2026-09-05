package codex

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

const defaultCodexExecutable = "codex"

const (
	envCodexHome     = "CODEX_HOME"
	envHome          = "HOME"
	envXDGConfigHome = "XDG_CONFIG_HOME"
	minCodexVersion  = "0.144.1"
	appServerCommand = "app-server"
	processExitGrace = 2 * time.Second
)

var processCloseGrace = 2 * time.Second
var processContainmentTimeout = 5 * time.Second

func launchAppServer(
	ctx context.Context,
	options Options,
) (transport *lineTransport, version string, nativePath string, returnErr error) {
	nativeEnv, err := buildMergedEnv(options)
	if err != nil {
		return nil, "", "", err
	}

	selector := strings.TrimSpace(options.CLIPath)
	if selector == "" {
		selector = defaultCodexExecutable
	}

	var packageCleanup func() error
	defer func() {
		if returnErr != nil && packageCleanup != nil {
			returnErr = errors.Join(returnErr, packageCleanup())
		}
	}()

	if options.HostAuthority == nil {
		selector, err = resolveOrdinaryProcessExecutable(selector, nativeEnv)
		if err != nil {
			return nil, "", "", fmt.Errorf("find codex CLI: %w", err)
		}

		selector, nativeEnv, packageCleanup, err = stagePackagedCodex(selector, nativeEnv, options.Scratch)
		if err != nil {
			return nil, "", "", err
		}
	} else if selectorErr := validateManagedSelector(selector); selectorErr != nil {
		return nil, "", "", selectorErr
	}

	version, err = validateCodexVersionOutput(options.NativeVersion)
	if err != nil {
		return nil, "", "", err
	}

	nativePath = searchPathFromEnvironment(nativeEnv)

	request := NativeRequest{
		Executable:  selector,
		Arguments:   appServerArgs(options),
		Environment: nativeEnv,
	}

	spawnStarted := time.Now()

	var native NativeProcess
	if options.HostAuthority != nil {
		native, err = options.HostAuthority.StartNative(ctx, request)
	} else {
		native, err = startOrdinaryNative(ctx, request, options)
	}

	observeCodexStartupStage(ctx, options, "runtime", "spawn", spawnStarted, err)

	if err != nil {
		return nil, "", "", err
	}

	if native == nil {
		return nil, "", "", ErrHostAuthorityUnavailable
	}

	stdin, stdout, stderr := native.Stdin(), native.Stdout(), native.Stderr()
	if stdin == nil || stdout == nil || stderr == nil {
		_, _, cleanupErr := revokeAndWaitNative(native)
		stdioErr := closeNativePipes(stdin, stdout, stderr)

		return nil, "", "", errors.Join(
			fmt.Errorf("%w: host returned incomplete native stdio", ErrHostAuthorityUnavailable),
			cleanupErr, stdioErr,
		)
	}

	stderrDone := make(chan struct{})
	proc := &process{
		native: native, stdin: stdin, stdout: stdout, stderr: stderr, stderrDone: stderrDone,
		packageCleanup: packageCleanup,
	}
	packageCleanup = nil

	go func() {
		defer close(stderrDone)

		_, _ = io.Copy(io.Discard, stderr)
	}()

	return newLineTransport(proc.stdout, proc.stdin, proc), version, nativePath, nil
}

func filepathSlash(path string) string {
	return strings.ReplaceAll(path, `\`, "/")
}

func validateManagedSelector(selector string) error {
	for _, segment := range strings.Split(filepathSlash(strings.TrimSpace(selector)), "/") {
		if segment == "node_modules" {
			return errors.New("managed Codex executable must be staged and pinned by the host before adapter initialization")
		}
	}

	return nil
}

func appServerArgs(options Options) []string {
	args := []string{appServerCommand, "--listen", "stdio://", "--disable", "plugins"}

	keys := make([]string, 0, len(options.Config))
	for key := range options.Config {
		keys = append(keys, key)
	}

	sort.Strings(keys)

	for _, key := range keys {
		args = append(args, "-c", fmt.Sprintf("%s=%s", key, shellValue(options.Config[key])))
	}

	return append(args, options.ExtraArgs...)
}

func validateCodexVersionOutput(output string) (string, error) {
	version := parseCodexVersion(output)
	if version == "" {
		return "", fmt.Errorf("check codex CLI version: could not parse %q", strings.TrimSpace(output))
	}

	if compareSemver(version, minCodexVersion) < 0 {
		return "", fmt.Errorf("codex CLI %s is too old; need >= %s", version, minCodexVersion)
	}

	return version, nil
}

var codexVersionRE = regexp.MustCompile(`\d+\.\d+\.\d+(?:[-+][0-9A-Za-z.-]+)?`)

func parseCodexVersion(output string) string {
	match := codexVersionRE.FindString(output)
	if cut, _, ok := strings.Cut(match, "-"); ok {
		match = cut
	}

	if cut, _, ok := strings.Cut(match, "+"); ok {
		match = cut
	}

	return match
}

func compareSemver(left string, right string) int {
	leftParts := semverParts(left)

	rightParts := semverParts(right)
	for index := range leftParts {
		if leftParts[index] < rightParts[index] {
			return -1
		}

		if leftParts[index] > rightParts[index] {
			return 1
		}
	}

	return 0
}

func semverParts(value string) [3]int {
	var out [3]int

	parts := strings.Split(value, ".")
	for index := 0; index < len(out) && index < len(parts); index++ {
		out[index], _ = strconv.Atoi(parts[index])
	}

	return out
}

func buildMergedEnv(options Options) ([]string, error) {
	managed := map[string]string{}
	if options.CodexHome != "" {
		managed[envCodexHome] = options.CodexHome
	}

	base := options.ImplicitEnvironment
	if options.HostAuthority != nil {
		base = options.HostAuthority.NativeEnvironment()
	}

	return buildProcessEnvironmentFrom(base, withoutManagedRootOverrides(options.Env), managed)
}

func shellValue(value any) string {
	switch typed := value.(type) {
	case string:
		return fmt.Sprintf("%q", typed)
	case bool:
		if typed {
			return otelValueTrue
		}

		return "false"
	default:
		return fmt.Sprint(typed)
	}
}

type process struct {
	native NativeProcess
	stdin  io.WriteCloser
	stdout io.ReadCloser
	stderr io.ReadCloser

	closeMu         sync.Mutex
	inputCloseOnce  sync.Once
	inputCloseErr   error
	streamCloseOnce sync.Once
	streamCloseErr  error
	stderrDone      chan struct{}
	terminalProven  bool
	terminalErr     error

	packageCleanup     func() error
	packageCleanupOnce sync.Once
	packageCleanupErr  error
}

func (p *process) exited(grace time.Duration) bool {
	exited, _ := p.waitTerminalWithin(grace)

	return exited
}

func (p *process) waitTerminalWithin(timeout time.Duration) (bool, error) {
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()

	result, err := p.native.Wait(ctx)

	terminalErr := terminalNativeProcessError(result, err)
	if ctx.Err() != nil {
		return false, terminalErr
	}

	return true, terminalErr
}

func (p *process) waitTerminal() error {
	_, err := p.waitTerminalWithin(processContainmentTimeout)

	return err
}

func (p *process) Close() error {
	if p == nil {
		return nil
	}

	p.closeMu.Lock()
	defer p.closeMu.Unlock()

	inputErr := p.closeInput()

	if p.native == nil {
		return errors.Join(inputErr, p.closeStreams(), p.cleanupPackage())
	}

	if p.terminalProven {
		return errors.Join(inputErr, p.closeStreams(), p.terminalErr, p.cleanupPackage())
	}

	graceCtx, cancelGrace := context.WithTimeout(context.Background(), processCloseGrace)
	result, waitErr := p.native.Wait(graceCtx)
	ownedGraceErr := graceCtx.Err()

	cancelGrace()

	if waitErr == nil {
		p.terminalProven = true
		p.terminalErr = terminalNativeProcessError(result, nil)

		return errors.Join(inputErr, p.closeStreams(), p.terminalErr, p.cleanupPackage())
	}

	independentWaitErr := withoutExactErrorLeaves(waitErr, ownedGraceErr)
	result, terminal, settlementErr := revokeAndWaitNative(p.native)
	streamErr := p.closeStreams()

	if !terminal {
		return errors.Join(inputErr, streamErr, independentWaitErr, settlementErr)
	}

	p.terminalProven = true
	p.terminalErr = terminalNativeProcessError(result, nil)

	return errors.Join(inputErr, streamErr, independentWaitErr, settlementErr, p.terminalErr, p.cleanupPackage())
}

func (p *process) closeInput() error {
	p.inputCloseOnce.Do(func() {
		if p.stdin != nil {
			p.inputCloseErr = processPipeCloseError(p.stdin.Close())
		}
	})

	return p.inputCloseErr
}

func (p *process) closeStreams() error {
	p.streamCloseOnce.Do(func() {
		if p.stdout != nil {
			p.streamCloseErr = processPipeCloseError(p.stdout.Close())
		}

		if p.stderr != nil {
			p.streamCloseErr = errors.Join(p.streamCloseErr, processPipeCloseError(p.stderr.Close()))
		}

		if p.stderrDone != nil {
			<-p.stderrDone
		}
	})

	return p.streamCloseErr
}

func processPipeCloseError(err error) error {
	if errors.Is(err, os.ErrClosed) || errors.Is(err, io.ErrClosedPipe) {
		return nil
	}

	return err
}

func closeNativePipes(stdin io.WriteCloser, stdout, stderr io.ReadCloser) error {
	var err error
	if stdin != nil {
		err = processPipeCloseError(stdin.Close())
	}

	if stdout != nil {
		err = errors.Join(err, processPipeCloseError(stdout.Close()))
	}

	if stderr != nil {
		err = errors.Join(err, processPipeCloseError(stderr.Close()))
	}

	return err
}

func revokeAndWaitNative(native NativeProcess) (NativeResult, bool, error) {
	revokeCtx, cancelRevoke := context.WithTimeout(context.Background(), processContainmentTimeout)
	revokeErr := native.Revoke(revokeCtx)

	cancelRevoke()

	waitCtx, cancelWait := context.WithTimeout(context.Background(), processContainmentTimeout)
	result, waitErr := native.Wait(waitCtx)

	cancelWait()

	if waitErr != nil {
		return result, false, errors.Join(ErrContainmentIncomplete, revokeErr, waitErr)
	}

	return result, true, revokeErr
}

func withoutExactErrorLeaves(err, discarded error) error {
	if err == nil || discarded == nil {
		return err
	}

	if err == discarded {
		return nil
	}

	type joined interface{ Unwrap() []error }

	if combined, ok := err.(joined); ok {
		var retained error
		for _, component := range combined.Unwrap() {
			retained = errors.Join(retained, withoutExactErrorLeaves(component, discarded))
		}

		return retained
	}

	type wrapped interface{ Unwrap() error }
	if single, ok := err.(wrapped); ok {
		return withoutExactErrorLeaves(single.Unwrap(), discarded)
	}

	return err
}

func terminalNativeProcessError(result NativeResult, err error) error {
	if err != nil {
		return errors.Join(ErrContainmentIncomplete, err)
	}

	if result.ExitCode != 0 || result.Signal != 0 {
		return fmt.Errorf("%w: codex app-server exited with status %d signal %d",
			ErrNativeGenerationTerminated, result.ExitCode, result.Signal)
	}

	return nil
}

func (p *process) cleanupPackage() error {
	if p == nil {
		return nil
	}

	p.packageCleanupOnce.Do(func() {
		if p.packageCleanup != nil {
			p.packageCleanupErr = p.packageCleanup()
		}
	})

	return p.packageCleanupErr
}

func observeCodexStartupStage(ctx context.Context, options Options, lifecycle, stage string, started time.Time, err error) {
	if options.ObserveStartupStage != nil {
		options.ObserveStartupStage(ctx, lifecycle, stage, time.Since(started), err)
	}
}
