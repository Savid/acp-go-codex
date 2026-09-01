package codex

import (
	"context"
	"errors"
	"fmt"
	"io"
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
		_, cleanupErr := revokeAndWaitNative(native)

		return nil, "", "", errors.Join(
			fmt.Errorf("%w: host returned incomplete native stdio", ErrHostAuthorityUnavailable),
			cleanupErr,
		)
	}

	proc := &process{native: native, stdin: stdin, stdout: stdout, packageCleanup: packageCleanup}
	packageCleanup = nil

	go func() {
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

	if p.stdin != nil {
		_ = p.stdin.Close()
	}

	if p.native == nil {
		return p.cleanupPackage()
	}

	graceCtx, cancelGrace := context.WithTimeout(context.Background(), processCloseGrace)
	result, waitErr := p.native.Wait(graceCtx)
	graceExpired := graceCtx.Err() != nil

	cancelGrace()

	if !graceExpired {
		terminalErr := terminalNativeProcessError(result, waitErr)
		if errors.Is(terminalErr, ErrContainmentIncomplete) {
			return terminalErr
		}

		return errors.Join(terminalErr, p.cleanupPackage())
	}

	result, containmentErr := revokeAndWaitNative(p.native)
	if containmentErr != nil {
		return containmentErr
	}

	return errors.Join(terminalNativeProcessError(result, nil), p.cleanupPackage())
}

func revokeAndWaitNative(native NativeProcess) (NativeResult, error) {
	revokeCtx, cancelRevoke := context.WithTimeout(context.Background(), processContainmentTimeout)
	revokeErr := native.Revoke(revokeCtx)

	cancelRevoke()

	waitCtx, cancelWait := context.WithTimeout(context.Background(), processContainmentTimeout)
	result, waitErr := native.Wait(waitCtx)

	cancelWait()

	if waitErr != nil {
		return result, errors.Join(ErrContainmentIncomplete, revokeErr, waitErr)
	}

	return result, nil
}

func terminalNativeProcessError(result NativeResult, err error) error {
	if err != nil {
		return errors.Join(ErrContainmentIncomplete, err)
	}

	if result.ExitCode != 0 || result.Signal != 0 {
		return fmt.Errorf("codex app-server exited with status %d signal %d", result.ExitCode, result.Signal)
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
