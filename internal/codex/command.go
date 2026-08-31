package codex

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os/exec"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/savid/acp-go-codex/internal/homelock"
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

	if options.HostAuthority == nil {
		selector, err = resolveOrdinaryProcessExecutable(selector, nativeEnv)
		if err != nil {
			return nil, "", "", fmt.Errorf("find codex CLI: %w", err)
		}

		selector, nativeEnv, err = stagePackagedCodex(selector, nativeEnv, options.Scratch)
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
		if native != nil {
			_ = native.Revoke(context.Background())
			_, _ = native.Wait(context.Background())
		}

		return nil, "", "", fmt.Errorf("%w: host returned incomplete native stdio", ErrHostAuthorityUnavailable)
	}

	proc := &process{native: native, stdin: stdin, stdout: stdout}

	go func() {
		_, _ = io.Copy(io.Discard, stderr)
	}()

	return newLineTransport(proc.stdout, proc.stdin, proc), version, nativePath, nil
}

func filepathSlash(path string) string {
	return strings.ReplaceAll(path, `\`, "/")
}

func validateManagedSelector(selector string) error {
	if strings.Contains(filepathSlash(selector), "/node_modules/") {
		return errors.New("managed Codex executable must be staged and pinned by the host before adapter initialization")
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

	waitOnce sync.Once
	waitDone chan struct{}
	result   NativeResult
	waitErr  error
}

func (p *process) beginWait() {
	p.waitOnce.Do(func() {
		p.waitDone = make(chan struct{})
		go func() {
			defer close(p.waitDone)

			p.result, p.waitErr = p.native.Wait(context.Background())
		}()
	})
}

func (p *process) exited(grace time.Duration) bool {
	p.beginWait()

	select {
	case <-p.waitDone:
		return true
	case <-time.After(grace):
		return false
	}
}

func (p *process) Close() error {
	if p == nil {
		return nil
	}

	if p.stdin != nil {
		_ = p.stdin.Close()
	}

	if p.native == nil {
		return nil
	}

	p.beginWait()

	select {
	case <-p.waitDone:
		return p.waitErr
	case <-time.After(processCloseGrace):
	}

	revokeErr := p.native.Revoke(context.Background())
	<-p.waitDone

	return errors.Join(revokeErr, p.waitErr)
}

func observeCodexStartupStage(ctx context.Context, options Options, lifecycle, stage string, started time.Time, err error) {
	if options.ObserveStartupStage != nil {
		options.ObserveStartupStage(ctx, lifecycle, stage, time.Since(started), err)
	}
}

type ordinaryNativeProcess struct {
	cmd    *exec.Cmd
	stdin  io.WriteCloser
	stdout io.ReadCloser
	stderr io.ReadCloser
	lock   *homelock.Lock

	waitOnce sync.Once
	done     chan struct{}
	result   NativeResult
	waitErr  error
	revoked  bool
	mu       sync.Mutex
}

func startOrdinaryNative(ctx context.Context, request NativeRequest, options Options) (NativeProcess, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	cmd := ordinaryExecCommand(request.Executable, request.Arguments...)
	cmd.Env = append([]string(nil), request.Environment...)
	cmd.Dir = request.WorkingDirectory
	configureProcess(cmd)

	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, err
	}

	stdout, err := cmd.StdoutPipe()
	if err != nil {
		_ = stdin.Close()

		return nil, err
	}

	stderr, err := cmd.StderrPipe()
	if err != nil {
		_ = stdin.Close()
		_ = stdout.Close()

		return nil, err
	}

	var lock *homelock.Lock
	if !options.skipGuardian {
		lockRoot, lockErr := HomeLockRoot(options.ScratchParent, firstNonEmpty(options.WritableHome, options.CodexHome))
		if lockErr != nil {
			return nil, lockErr
		}

		lock, err = homelock.Acquire(lockRoot)
		if err != nil {
			return nil, err
		}
	}

	if err := cmd.Start(); err != nil {
		_ = lock.Release()

		return nil, err
	}

	return &ordinaryNativeProcess{
		cmd: cmd, stdin: stdin, stdout: stdout, stderr: stderr, lock: lock, done: make(chan struct{}),
	}, nil
}

func (p *ordinaryNativeProcess) Stdin() io.WriteCloser { return p.stdin }
func (p *ordinaryNativeProcess) Stdout() io.ReadCloser { return p.stdout }
func (p *ordinaryNativeProcess) Stderr() io.ReadCloser { return p.stderr }

func (p *ordinaryNativeProcess) beginWait() {
	p.waitOnce.Do(func() {
		go func() {
			err := p.cmd.Wait()

			result := NativeResult{}
			if p.cmd.ProcessState != nil {
				result.ExitCode = p.cmd.ProcessState.ExitCode()
				if status, ok := p.cmd.ProcessState.Sys().(syscall.WaitStatus); ok && status.Signaled() {
					result.Signal = int(status.Signal())
				}
			}

			p.mu.Lock()
			result.Revoked = p.revoked
			p.result = result
			p.waitErr = err
			p.mu.Unlock()
			_ = p.lock.Release()
			close(p.done)
		}()
	})
}

func (p *ordinaryNativeProcess) Wait(ctx context.Context) (NativeResult, error) {
	p.beginWait()

	select {
	case <-ctx.Done():
		return NativeResult{}, ctx.Err()
	case <-p.done:
		p.mu.Lock()
		defer p.mu.Unlock()

		return p.result, p.waitErr
	}
}

func (p *ordinaryNativeProcess) Revoke(ctx context.Context) error {
	p.mu.Lock()
	select {
	case <-p.done:
		p.mu.Unlock()

		return nil
	default:
		p.revoked = true
	}
	p.mu.Unlock()

	p.beginWait()

	_ = terminateProcess(p.cmd)
	select {
	case <-p.done:
		return nil
	case <-time.After(processCloseGrace):
		_ = killProcess(p.cmd)
	}

	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-p.done:
		return nil
	}
}
