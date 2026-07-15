package codex

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

const (
	envCodexHome    = "CODEX_HOME"
	minCodexVersion = "0.144.1"
)

var execCommandContext = exec.CommandContext
var processCloseGrace = 2 * time.Second

// launchAppServer starts the codex app-server. The request-scoped ctx bounds
// the version check, while procCtx governs the lifetime of the spawned process
// (see NewAppServerClient): binding the process to procCtx prevents
// exec.CommandContext from SIGKILLing codex when the launching request returns.
func launchAppServer(ctx context.Context, procCtx context.Context, options Options) (*lineTransport, *exec.Cmd, string, error) {
	path, err := resolveCodexPath(options.CLIPath)
	if err != nil {
		return nil, nil, "", err
	}

	version, versionErr := validateCodexVersion(ctx, path)
	if versionErr != nil {
		return nil, nil, "", versionErr
	}

	var cmd *exec.Cmd

	var supervisor *supervisorProof

	if options.skipSupervisor {
		cmd = execCommandContext(procCtx, path, appServerArgs(options)...)
		cmd.Env = mergedEnv(options)
	} else {
		cmd, supervisor, err = supervisorCommand(procCtx, supervisorConfig{
			NativePath: path,
			NativeArgs: appServerArgs(options),
			NativeEnv:  mergedEnv(options),
			Home:       firstNonEmpty(options.WritableHome, options.CodexHome),
			Scratch:    options.SupervisorRoot,
		})
		if err != nil {
			return nil, nil, "", err
		}
	}

	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, nil, "", err
	}

	stdout, err := cmd.StdoutPipe()
	if err != nil {
		_ = stdin.Close()

		return nil, nil, "", err
	}

	stderr := codexStderrWriter(options.Logger)
	cmd.Stderr = stderr

	spawnStarted := time.Now()
	if err := startProcess(cmd); err != nil {
		observeCodexStartupStage(ctx, options, "runtime", "spawn", spawnStarted, err)

		_ = stdin.Close()
		_ = stdout.Close()

		return nil, nil, "", err
	}

	observeCodexStartupStage(ctx, options, "runtime", "spawn", spawnStarted, nil)

	proc := &process{cmd: cmd, stdin: stdin, stdout: stdout, stderr: stderr, supervisor: supervisor, observeProcess: options.ObserveProcess}

	return newLineTransport(stdout, stdin, proc), cmd, version, nil
}

// appServerArgs builds the codex app-server argument list: the base launch
// flags, per-key -c config overrides (emitted in sorted key order for
// deterministic args), and any caller-supplied extra args.
func appServerArgs(options Options) []string {
	args := []string{"app-server", "--listen", "stdio://", "--disable", "plugins"}

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

func resolveCodexPath(path string) (string, error) {
	if strings.TrimSpace(path) != "" {
		return path, nil
	}

	resolved, err := exec.LookPath("codex")
	if err != nil {
		return "", fmt.Errorf("find codex CLI: %w", err)
	}

	return resolved, nil
}

func validateCodexVersion(ctx context.Context, path string) (string, error) {
	cmd := execCommandContext(ctx, path, "--version")

	output, err := cmd.Output()
	if err != nil {
		return "", fmt.Errorf("check codex CLI version: %w", err)
	}

	version := parseCodexVersion(string(output))
	if version == "" {
		return "", fmt.Errorf("check codex CLI version: could not parse %q", strings.TrimSpace(string(output)))
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

func mergedEnv(options Options) []string {
	env := os.Environ()
	if options.CodexHome != "" {
		env = upsertEnv(env, envCodexHome, options.CodexHome)
	}

	for key, value := range options.Env {
		env = upsertEnv(env, key, value)
	}

	return env
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
			return "true"
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
	cmd        *exec.Cmd
	stdin      io.WriteCloser
	stdout     io.ReadCloser
	stderr     *stderrTail
	supervisor *supervisorProof

	waitOnce sync.Once
	waitErr  error
	waitDone chan struct{}

	observationMu       sync.Mutex
	processExited       bool
	supervisorsObserved bool
	observeProcess      func(context.Context, string, int64)
}

func (p *process) markSupervisorsReady(ctx context.Context) {
	if p == nil || p.supervisor == nil || p.observeProcess == nil {
		return
	}

	p.observationMu.Lock()
	if p.processExited || p.supervisorsObserved {
		p.observationMu.Unlock()

		return
	}

	p.supervisorsObserved = true
	p.observationMu.Unlock()
	p.observeProcess(ctx, "home_lock_supervisor", 2)
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

		go func() {
			defer recoverCodexGoroutine(context.Background(), "Codex process waiter")
			defer p.markExited()

			p.waitErr = p.cmd.Wait()
			if p.waitErr != nil && p.supervisor != nil {
				p.waitErr = errors.Join(p.waitErr, p.supervisor.awaitCompletion())
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
		return nil
	}

	p.beginWait()

	if p.supervisor != nil {
		<-p.waitDone

		return processCloseError(p.waitErr)
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
