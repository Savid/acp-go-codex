package codex

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os/exec"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

const (
	envCodexHome     = "CODEX_HOME"
	envHome          = "HOME"
	envXDGConfigHome = "XDG_CONFIG_HOME"
	minCodexVersion  = "0.144.1"
	appServerCommand = "app-server"
)

var execCommandContext = exec.CommandContext
var processCloseGrace = 2 * time.Second
var processSupervisorCloseWait = supervisorQuiesceWindow + time.Second

// launchAppServer starts the codex app-server. The request-scoped ctx bounds
// the version check, while procCtx governs the lifetime of the spawned process
// (see NewAppServerClient): binding the process to procCtx prevents
// exec.CommandContext from SIGKILLing codex when the launching request returns.
func launchAppServer(ctx context.Context, procCtx context.Context, options Options) (*lineTransport, *exec.Cmd, string, string, error) {
	nativeEnv, err := buildMergedEnv(options)
	if err != nil {
		return nil, nil, "", "", err
	}

	path, err := resolveCodexPath(options.CLIPath, nativeEnv, options.ProcessIsolation)
	if err != nil {
		return nil, nil, "", "", err
	}

	path, nativeEnv, err = stagePackagedCodex(path, nativeEnv, options.SupervisorRoot)
	if err != nil {
		return nil, nil, "", "", err
	}

	nativePath := searchPathFromEnvironment(nativeEnv)

	version, versionErr := validateCodexVersionOutput(options.NativeVersion)
	if versionErr != nil {
		return nil, nil, "", "", versionErr
	}

	var cmd *exec.Cmd

	var supervisor *supervisorProof

	if options.skipSupervisor {
		cmd = execCommandContext(procCtx, path, appServerArgs(options)...)

		cmd.Env = nativeEnv
		if credentialErr := applyProcessCredential(cmd, options.ProcessIsolation); credentialErr != nil {
			return nil, nil, "", "", credentialErr
		}
	} else {
		lockRoot, lockErr := HomeLockRoot(options.SupervisorParent, firstNonEmpty(options.WritableHome, options.CodexHome))
		if lockErr != nil {
			return nil, nil, "", "", lockErr
		}

		cmd, supervisor, err = supervisorCommand(procCtx, supervisorConfig{
			NativePath:       path,
			NativeArgs:       appServerArgs(options),
			NativeEnv:        nativeEnv,
			Isolation:        options.ProcessIsolation,
			Home:             lockRoot,
			Scratch:          options.SupervisorRoot,
			ScratchParent:    options.SupervisorParent,
			LifecycleKind:    lifecycleRuntime,
			DarwinBestEffort: options.DarwinBestEffort,
		})
		if err != nil {
			return nil, nil, "", "", err
		}
	}

	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, nil, "", "", errors.Join(
			err,
			supervisor.closeInherited(),
			supervisor.releaseOrdinaryHomeLock(),
		)
	}

	stdout, err := cmd.StdoutPipe()
	if err != nil {
		_ = stdin.Close()

		return nil, nil, "", "", errors.Join(
			err,
			supervisor.closeInherited(),
			supervisor.releaseOrdinaryHomeLock(),
		)
	}

	stderr := codexStderrWriter(options.Logger)
	cmd.Stderr = stderr

	spawnStarted := time.Now()

	waiter, err := startProcess(cmd)
	if err != nil {
		cleanupErr := errors.Join(
			supervisor.closeInherited(),
			supervisor.releaseOrdinaryHomeLock(),
		)

		observeCodexStartupStage(ctx, options, "runtime", "spawn", spawnStarted, err)

		_ = stdin.Close()
		_ = stdout.Close()

		return nil, nil, "", "", errors.Join(err, cleanupErr)
	}

	if closeErr := supervisor.closeInherited(); closeErr != nil {
		_ = cmd.Process.Kill()

		waiter.start()
		<-waiter.result()

		return nil, nil, "", "", errors.Join(
			fmt.Errorf("close inherited supervisor config: %w", closeErr),
			supervisor.releaseOrdinaryHomeLock(),
		)
	}

	observeCodexStartupStage(ctx, options, "runtime", "spawn", spawnStarted, nil)

	proc := &process{
		cmd:            cmd,
		stdin:          stdin,
		stdout:         stdout,
		stderr:         stderr,
		supervisor:     supervisor,
		processWaiter:  waiter,
		observeProcess: options.ObserveProcess,
	}
	if options.NewProcessSnapshotObserver != nil {
		proc.processSnapshot = options.NewProcessSnapshotObserver(ctx)
	}

	return newLineTransport(stdout, stdin, proc), cmd, version, nativePath, nil
}

// appServerArgs builds the codex app-server argument list: the base launch
// flags, per-key -c config overrides (emitted in sorted key order for
// deterministic args), and any caller-supplied extra args.
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

// stderrTailLimit bounds the retained app-server stderr so a mid-turn crash can
// surface the tail of the process diagnostics without unbounded buffering.
const stderrTailLimit = 4096

// stderrTail logs every app-server stderr chunk and retains a bounded tail so a
// process-death failure can name the real cause instead of a bare transport EOF.
type stderrTail struct {
	logger *slog.Logger

	mu  sync.Mutex
	buf []byte
}

func codexStderrWriter(logger *slog.Logger) *stderrTail {
	if logger == nil {
		logger = slog.Default()
	}

	return &stderrTail{logger: logger}
}

func (w *stderrTail) Write(p []byte) (int, error) {
	text := strings.TrimSpace(string(p))
	if text != "" {
		w.logger.DebugContext(
			context.Background(),
			"Codex app-server stderr",
			slog.String("stderr", strings.TrimRight(string(p), "\r\n")),
		)
	}

	w.mu.Lock()
	w.buf = append(w.buf, p...)

	if len(w.buf) > stderrTailLimit {
		w.buf = append([]byte(nil), w.buf[len(w.buf)-stderrTailLimit:]...)
	}
	w.mu.Unlock()

	return len(p), nil
}

// tail returns the retained stderr tail with surrounding whitespace trimmed.
func (w *stderrTail) tail() string {
	if w == nil {
		return ""
	}

	w.mu.Lock()
	defer w.mu.Unlock()

	return strings.TrimSpace(string(w.buf))
}

func resolveCodexPath(path string, env []string, isolation *ProcessIsolation) (string, error) {
	if strings.TrimSpace(path) == "" {
		path = "codex" //nolint:goconst // Executable identity is distinct from Darwin registry metadata.
	}

	var (
		resolved string
		err      error
	)
	if isolation == nil {
		resolved, err = resolveOrdinaryProcessExecutable(path, env)
	} else {
		resolved, err = resolveProcessExecutable(path, env)
	}

	if err != nil {
		return "", fmt.Errorf("find codex CLI: %w", err)
	}

	return resolved, nil
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
	if match == "" {
		return ""
	}

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

	for i := 0; i < len(leftParts); i++ {
		switch {
		case leftParts[i] < rightParts[i]:
			return -1
		case leftParts[i] > rightParts[i]:
			return 1
		}
	}

	return 0
}

func semverParts(value string) [3]int {
	var out [3]int

	parts := strings.Split(value, ".")
	for i := 0; i < len(out) && i < len(parts); i++ {
		part := parts[i]
		out[i], _ = strconv.Atoi(part)
	}

	return out
}

func buildMergedEnv(options Options) ([]string, error) {
	managed := map[string]string{}
	if options.CodexHome != "" {
		managed[envCodexHome] = options.CodexHome
	}

	return buildProcessEnvironmentFrom(
		options.ProcessIsolation,
		options.ImplicitEnvironment,
		withoutManagedRootOverrides(options.Env),
		managed,
	)
}

func upsertEnv(env []string, key string, value string) []string {
	prefix := key + "="
	for i, entry := range env {
		if strings.HasPrefix(entry, prefix) {
			env[i] = prefix + value

			return env
		}
	}

	return append(env, prefix+value)
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

// processExitGrace bounds how long the transport waits for the app-server
// process to be reaped after its stdout stream ends, so a mid-turn stream EOF
// can be attributed to the real process exit status instead of a bare transport
// fault. Transport death while the process is still running exceeds the grace
// and stays cause:"transport". Each lineTransport captures it at construction;
// there is no shared mutable grace state.
const processExitGrace = 2 * time.Second

// process owns the codex app-server child process: its stdio, its bounded
// stderr tail, and the single cmd.Wait reaper shared by the transport
// process-death detection and the deliberate Close escalation.
type process struct {
	cmd           *exec.Cmd
	stdin         io.WriteCloser
	stdout        io.ReadCloser
	stderr        *stderrTail
	supervisor    *supervisorProof
	processWaiter *supervisorWaiter

	waitOnce sync.Once
	waitErr  error
	waitDone chan struct{}

	observationMu       sync.Mutex
	processExited       bool
	supervisorsObserved bool
	observeProcess      func(context.Context, string, int64)
	processSnapshot     ProcessSnapshotObserver
}

func (p *process) markSupervisorsReady(ctx context.Context) {
	if p == nil || p.supervisor == nil || p.supervisor.ordinaryHomeLock != nil {
		return
	}

	p.observationMu.Lock()
	if p.processExited || p.supervisorsObserved {
		p.observationMu.Unlock()

		return
	}

	p.supervisorsObserved = true
	p.observationMu.Unlock()

	if p.observeProcess != nil {
		p.observeProcess(ctx, "home_lock_supervisor", 2)
	}

	p.observeProviderSnapshot(ctx)
}

func (p *process) observeProviderSnapshot(ctx context.Context) {
	if p == nil || p.supervisor == nil || p.processSnapshot.Observe == nil {
		return
	}

	if count, available := p.supervisor.readProviderSnapshot(); available {
		p.processSnapshot.Observe(ctx, count)
	}
}

func (p *process) finishProviderSnapshot(ctx context.Context, err error) {
	if p == nil {
		return
	}

	if errors.Is(err, ErrProcessContainmentIncomplete) {
		if p.processSnapshot.Unproven != nil {
			p.processSnapshot.Unproven()
		}

		return
	}

	if p.processSnapshot.Quiescent != nil {
		p.processSnapshot.Quiescent(ctx)
	}
}

func (p *process) markExited() {
	if p == nil {
		return
	}

	p.observationMu.Lock()
	p.processExited = true
	observed := p.supervisorsObserved
	p.supervisorsObserved = false
	observe := p.observeProcess
	p.observationMu.Unlock()

	if observed && observe != nil {
		observe(context.Background(), "home_lock_supervisor", -2)
	}
}

// beginWait reaps the process exactly once in the background. Callers gate on
// waitDone before reading waitErr.
func (p *process) beginWait() {
	p.waitOnce.Do(func() {
		p.waitDone = make(chan struct{})

		if p.cmd == nil || p.cmd.Process == nil {
			close(p.waitDone)

			return
		}

		if p.processWaiter == nil {
			p.waitErr = errors.New("codex process waiter is unavailable")
			close(p.waitDone)

			return
		}

		p.processWaiter.start()

		go func() {
			defer recoverCodexGoroutine(context.Background(), "Codex process waiter")
			defer p.markExited()

			if p.supervisor != nil {
				waitErr, proofErr := p.supervisor.awaitCommand(p.processWaiter.result())
				p.waitErr = errors.Join(waitErr, proofErr)
			} else {
				p.waitErr = <-p.processWaiter.result()
			}

			close(p.waitDone)
		}()
	})
}

func observeCodexStartupStage(ctx context.Context, options Options, lifecycle, stage string, started time.Time, err error) {
	if options.ObserveStartupStage != nil {
		options.ObserveStartupStage(ctx, lifecycle, stage, time.Since(started), err)
	}
}

// exited reports the process exit status and its stderr tail when the process
// terminates within grace. It returns ok=false while the process is still
// running, so a live transport fault is not misattributed to a process exit.
func (p *process) exited(grace time.Duration) (status string, stderrTail string, ok bool) {
	p.beginWait()

	select {
	case <-p.waitDone:
		return exitStatus(p.waitErr), p.stderr.tail(), true
	case <-time.After(grace):
		return "", "", false
	}
}

func (p *process) Close() error {
	if p.stdin != nil {
		_ = p.stdin.Close()
	}

	if p.stdout != nil {
		_ = p.stdout.Close()
	}

	if p.cmd == nil || p.cmd.Process == nil {
		return p.supervisor.releaseOrdinaryHomeLock()
	}

	p.beginWait()

	if p.supervisor != nil && p.supervisor.ordinaryHomeLock == nil {
		select {
		case <-p.waitDone:
			closeErr := processCloseError(p.waitErr)
			p.finishProviderSnapshot(context.Background(), closeErr)

			return closeErr
		case <-time.After(processSupervisorCloseWait):
			// The caller is done waiting, so retire the wait goroutine with it
			// rather than leaving it polling markers past teardown.
			p.supervisor.abandon()

			closeErr := fmt.Errorf(
				"%w: supervised process did not finish within %s",
				ErrProcessContainmentIncomplete,
				processSupervisorCloseWait,
			)
			p.finishProviderSnapshot(context.Background(), closeErr)

			return closeErr
		}
	}

	// Escalate: stdin EOF → SIGTERM → SIGKILL. The first grace window lets
	// the app-server exit on its own after stdin closes so in-flight cleanup
	// (e.g. MCP session termination) completes instead of being cut short.
	select {
	case <-p.waitDone:
		return processCloseError(p.waitErr)
	case <-time.After(processCloseGrace):
	}

	if err := terminateProcess(p.cmd); err != nil {
		return err
	}

	select {
	case <-p.waitDone:
		return processCloseError(p.waitErr)
	case <-time.After(processCloseGrace):
		if err := killProcess(p.cmd); err != nil {
			return err
		}

		<-p.waitDone

		return nil
	}
}

// exitStatusZero is the rendered status for a process that exited cleanly.
const exitStatusZero = "exit status 0"

// exitStatus renders a process wait result as a human-readable exit status.
func exitStatus(err error) string {
	if err == nil {
		return exitStatusZero
	}

	return err.Error()
}
